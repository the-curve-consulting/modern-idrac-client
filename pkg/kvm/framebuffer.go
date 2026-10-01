package kvm

import (
	"image"
	"image/color"
	"image/png"
	"io"
	"sync"
	"sync/atomic"
)

// Framebuffer is the decoded remote screen: an RGBA image protected by a
// mutex, with change notification for consumers such as the VNC bridge.
//
// It mirrors the Avocent viewer's "video model" (com.avocent.kvm.c.i /
// com.avocent.kvm.c.j: a linear int[] of ARGB pixels plus a dirty
// rectangle). Decoders write through the internal helpers at the bottom of
// this file; everything exported is safe for concurrent use.
type Framebuffer struct {
	mu     sync.RWMutex
	img    *image.RGBA
	w, h   int
	frames atomic.Uint64

	subMu   sync.Mutex
	subs    map[int]func(image.Rectangle)
	nextSub int
}

// DefaultFramebufferWidth/Height are the sizes the Java model starts with
// before the first video packet announces the real resolution
// (com.avocent.kvm.c.j constructor: a(820, 620)). We use a more common VGA
// size so a viewer connecting before video arrives sees something sane.
const (
	DefaultFramebufferWidth  = 1024
	DefaultFramebufferHeight = 768
)

// NewFramebuffer returns a black framebuffer of the default size.
func NewFramebuffer() *Framebuffer {
	fb := &Framebuffer{subs: map[int]func(image.Rectangle){}}
	fb.w, fb.h = DefaultFramebufferWidth, DefaultFramebufferHeight
	fb.img = newBlackRGBA(fb.w, fb.h)
	return fb
}

func newBlackRGBA(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 0xFF
	}
	return img
}

// Size returns the current width and height in pixels.
func (fb *Framebuffer) Size() (w, h int) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	return fb.w, fb.h
}

// Frames returns the number of complete frames received (end-of-frame
// markers seen by the decoder).
func (fb *Framebuffer) Frames() uint64 { return fb.frames.Load() }

// Resize replaces the image with a black one of the given size and
// notifies subscribers with a full-screen rectangle. Sizes <= 0 are ignored.
func (fb *Framebuffer) Resize(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	fb.mu.Lock()
	fb.w, fb.h = w, h
	fb.img = newBlackRGBA(w, h)
	fb.mu.Unlock()
	fb.notify(image.Rect(0, 0, w, h))
}

// Snapshot returns a deep copy of the current image.
func (fb *Framebuffer) Snapshot() *image.RGBA {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	dst := image.NewRGBA(fb.img.Rect)
	copy(dst.Pix, fb.img.Pix)
	return dst
}

// CopyRect copies region r of the framebuffer into dst (same coordinates)
// while holding the read lock. Parts of r outside either image are clipped.
func (fb *Framebuffer) CopyRect(dst *image.RGBA, r image.Rectangle) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	r = r.Intersect(fb.img.Rect).Intersect(dst.Rect)
	if r.Empty() {
		return
	}
	rowLen := r.Dx() * 4
	for y := r.Min.Y; y < r.Max.Y; y++ {
		so := fb.img.PixOffset(r.Min.X, y)
		do := dst.PixOffset(r.Min.X, y)
		copy(dst.Pix[do:do+rowLen], fb.img.Pix[so:so+rowLen])
	}
}

// Subscribe registers fn to be called (synchronously, from the decoder
// goroutine) after each decoded update with the dirty rectangle. The
// returned function removes the subscription.
func (fb *Framebuffer) Subscribe(fn func(r image.Rectangle)) (unsubscribe func()) {
	fb.subMu.Lock()
	id := fb.nextSub
	fb.nextSub++
	fb.subs[id] = fn
	fb.subMu.Unlock()
	return func() {
		fb.subMu.Lock()
		delete(fb.subs, id)
		fb.subMu.Unlock()
	}
}

// WritePNG encodes a snapshot of the framebuffer as PNG.
func (fb *Framebuffer) WritePNG(w io.Writer) error {
	return png.Encode(w, fb.Snapshot())
}

// notify calls every subscriber with r (if non-empty).
func (fb *Framebuffer) notify(r image.Rectangle) {
	if r.Empty() {
		return
	}
	fb.subMu.Lock()
	fns := make([]func(image.Rectangle), 0, len(fb.subs))
	for _, fn := range fb.subs {
		fns = append(fns, fn)
	}
	fb.subMu.Unlock()
	for _, fn := range fns {
		fn(r)
	}
}

// ---- internal write helpers used by the decoders -------------------------

// modify runs fn with exclusive access to the backing image. fn returns the
// rectangle it changed; subscribers are notified after the lock is released.
func (fb *Framebuffer) modify(fn func(img *image.RGBA) image.Rectangle) {
	fb.mu.Lock()
	r := fn(fb.img)
	fb.mu.Unlock()
	fb.notify(r)
}

// endFrame counts a completed frame.
func (fb *Framebuffer) endFrame() { fb.frames.Add(1) }

// clear paints the whole framebuffer with c and notifies (used by the
// "video stopped" handler, equivalent to com.avocent.kvm.c.j.i()).
func (fb *Framebuffer) clear(c color.RGBA) {
	fb.modify(func(img *image.RGBA) image.Rectangle {
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = c.R, c.G, c.B, 0xFF
		}
		return img.Rect
	})
}

// blitARGB copies a w x h block of 0xAARRGGBB pixels to (x, y). Rows that
// would run past the end of the image are skipped, exactly like
// com.avocent.kvm.c.k.a(int,int,int,int,int[]). Returns the touched rect.
func blitARGB(img *image.RGBA, x, y, w, h int, pix []uint32) image.Rectangle {
	W := img.Rect.Dx()
	n := len(img.Pix) / 4
	if w <= 0 || h <= 0 || x < 0 || y < 0 {
		return image.Rectangle{}
	}
	for row := 0; row < h; row++ {
		base := (y+row)*W + x
		if base+w >= n || base < 0 {
			continue
		}
		src := pix[row*w : row*w+w]
		for i, c := range src {
			putARGB(img.Pix, base+i, c)
		}
	}
	return image.Rect(x, y, x+w, y+h).Intersect(img.Rect)
}

// putARGB stores a 0xAARRGGBB value at linear pixel index i (alpha forced
// opaque).
func putARGB(pix []byte, i int, c uint32) {
	o := i * 4
	pix[o] = byte(c >> 16)
	pix[o+1] = byte(c >> 8)
	pix[o+2] = byte(c)
	pix[o+3] = 0xFF
}

// getRGB returns the 0x00RRGGBB value at linear pixel index i.
func getRGB(pix []byte, i int) uint32 {
	o := i * 4
	return uint32(pix[o])<<16 | uint32(pix[o+1])<<8 | uint32(pix[o+2])
}
