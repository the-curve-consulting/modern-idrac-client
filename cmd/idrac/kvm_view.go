package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"idrac/pkg/kvm"
	"idrac/pkg/viewer"
)

// kvmView opens the console window on the target.
func kvmView(ctx context.Context, g *globals, o kvmOpts) error {
	// Ask for the password on the terminal before the window opens.
	if viewer.Available {
		if _, err := g.passwd(ctx); err != nil {
			return err
		}
	}
	title := g.target.Name
	if g.target.Name != g.target.Address {
		title = fmt.Sprintf("%s (%s)", g.target.Name, g.target.Address)
	}
	return viewer.Run(viewer.Options{
		Title:     title,
		ViewOnly:  o.viewOnly,
		Logger:    g.logger,
		ExitAfter: o.exitAfter,
		Actions:   viewerActions(g),
		Connect:   func(ctx context.Context) (viewer.Backend, error) { return connectBackend(ctx, g, o) },
	})
}

// kvmDemo opens the window on a synthetic test screen.
func kvmDemo(g *globals, o kvmOpts) error {
	return viewer.Run(viewer.Options{
		Title: "demo", Logger: g.logger, ExitAfter: o.exitAfter,
		Connect: func(ctx context.Context) (viewer.Backend, error) { return newDemoBackend(), nil },
	})
}

// kvmReplayWindow plays a recording in the window, looping.
func kvmReplayWindow(g *globals, o kvmOpts, path string) error {
	return viewer.Run(viewer.Options{
		Title: "replay " + path, Logger: g.logger, ExitAfter: o.exitAfter,
		Connect: func(ctx context.Context) (viewer.Backend, error) { return newReplayBackend(path, o.speed, g) },
	})
}

// ---- live backend ----

type consoleBackend struct {
	c       *kvm.Console
	record  string
	started time.Time
}

func connectBackend(ctx context.Context, g *globals, o kvmOpts) (viewer.Backend, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c, err := openConsole(cctx, g, o)
	if err != nil {
		return nil, err
	}
	b := &consoleBackend{c: c, record: o.record, started: time.Now()}
	if err := c.WaitRunning(cctx); err != nil {
		b.Close()
		return nil, err
	}
	c.Session.MouseOrigin()
	if c.Video != nil {
		c.Video.RequestRefresh()
	}
	return b, nil
}

func (b *consoleBackend) Framebuffer() *kvm.Framebuffer { return b.c.FB }
func (b *consoleBackend) KeyDown(u uint16) error        { return b.c.Session.KeyDown(u) }
func (b *consoleBackend) KeyUp(u uint16) error          { return b.c.Session.KeyUp(u) }
func (b *consoleBackend) Chord(u ...uint16) error       { return b.c.Session.Chord(u...) }
func (b *consoleBackend) ReleaseAllKeys() error         { return b.c.Session.ReleaseAllKeys() }
func (b *consoleBackend) Done() <-chan struct{}         { return b.c.Done() }
func (b *consoleBackend) Err() error                    { return b.c.Err() }
func (b *consoleBackend) PointerEvent(x, y int, m uint8) error {
	return b.c.Session.PointerEvent(x, y, m)
}
func (b *consoleBackend) TypeString(s string, d time.Duration) error {
	return b.c.Session.TypeString(s, d)
}
func (b *consoleBackend) Refresh() error {
	if b.c.Video == nil {
		return b.c.Session.RefreshScreen()
	}
	return b.c.Video.RequestRefresh()
}
func (b *consoleBackend) Close() error { return b.c.Close() }

func (b *consoleBackend) Stats() string {
	var sb strings.Builder
	maj, min := b.c.Session.ProtocolVersion()
	w, h := b.c.FB.Size()
	fmt.Fprintf(&sb, "Session state:    %v\n", b.c.Session.State())
	fmt.Fprintf(&sb, "Protocol:         %d.%d\n", maj, min)
	if p := b.c.Session.Platform(); p != "" {
		fmt.Fprintf(&sb, "Platform:         %s\n", p)
	}
	fmt.Fprintf(&sb, "Connected for:    %s\n", time.Since(b.started).Round(time.Second))
	fmt.Fprintf(&sb, "Resolution:       %d x %d\n", w, h)
	fmt.Fprintf(&sb, "Frames decoded:   %d\n", b.c.FB.Frames())
	if b.c.Video != nil {
		st := b.c.Video.Stats()
		fmt.Fprintf(&sb, "Video packets:    %d (%.1f MB)\n", st.Packets, float64(st.Bytes)/1e6)
		fmt.Fprintf(&sb, "Acks sent:        %d\n", st.AcksSent)
		fmt.Fprintf(&sb, "Decode errors:    %d\n", st.DecodeErrors)
		fmt.Fprintf(&sb, "Unknown packets:  %d\n", st.Unknown)
		if st.ASpeedPackets > 0 {
			fmt.Fprintf(&sb, "ASpeed:           %d packets, %d frames, %d errors\n", st.ASpeedPackets, st.ASpeedFrames, st.ASpeedErrors)
			fmt.Fprintf(&sb, "  blocks:         %d (JPEG %d, VQ %d, skip codes %d)\n", st.ASpeed.Blocks, st.ASpeed.JPEGBlocks, st.ASpeed.VQBlocks, st.ASpeed.SkipCodes)
		}
		if st.DVC.Frames > 0 {
			fmt.Fprintf(&sb, "DVC:              %d frames, %d checksum errors\n", st.DVC.Frames, st.DVC.ChecksumErrors)
		}
		if st.TextPackets > 0 {
			fmt.Fprintf(&sb, "Text mode:        %d packets\n", st.TextPackets)
		}
	}
	if b.record != "" {
		fmt.Fprintf(&sb, "Recording to:     %s\n", b.record)
	}
	return sb.String()
}

// viewerActions wires the Power / Next Boot / Identify menus to the
// out-of-band API of whatever generation the target is.
func viewerActions(g *globals) viewer.Actions {
	with := func(ctx context.Context, fn func(Device) error) error {
		d, err := openDevice(ctx, g)
		if err != nil {
			return err
		}
		defer d.Close(context.WithoutCancel(ctx))
		return fn(d)
	}
	return viewer.Actions{
		PowerState: func(ctx context.Context) (st string, err error) {
			err = with(ctx, func(d Device) error { st, err = d.PowerState(ctx); return err })
			return st, err
		},
		Power: func(ctx context.Context, a string) error {
			return with(ctx, func(d Device) error { return d.Power(ctx, a) })
		},
		BootOnce: func(ctx context.Context, t string) error {
			return with(ctx, func(d Device) error { return d.BootOnce(ctx, t) })
		},
		Identify: func(ctx context.Context, on bool) error {
			return with(ctx, func(d Device) error { return d.Identify(ctx, on) })
		},
	}
}

// ---- offline backends (demo and replay) ----

// offlineBackend serves a framebuffer with no remote end; input is logged.
type offlineBackend struct {
	fb     *kvm.Framebuffer
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
	events int
	last   string
	px, py int
	log    func(string, ...any)
	stats  func() string
}

func (b *offlineBackend) note(format string, args ...any) error {
	b.mu.Lock()
	b.events++
	b.last = fmt.Sprintf(format, args...)
	b.mu.Unlock()
	if b.log != nil {
		b.log("viewer input: "+format, args...)
	}
	return nil
}
func (b *offlineBackend) Framebuffer() *kvm.Framebuffer { return b.fb }
func (b *offlineBackend) KeyDown(u uint16) error        { return b.note("key down %#02x", u) }
func (b *offlineBackend) KeyUp(u uint16) error          { return b.note("key up %#02x", u) }
func (b *offlineBackend) Chord(u ...uint16) error       { return b.note("chord %#02x", u) }
func (b *offlineBackend) ReleaseAllKeys() error         { return nil }
func (b *offlineBackend) Refresh() error                { return nil }
func (b *offlineBackend) Done() <-chan struct{}         { return b.done }
func (b *offlineBackend) PointerEvent(x, y int, m uint8) error {
	b.mu.Lock()
	b.px, b.py = x, y
	b.mu.Unlock()
	if m != 0 {
		return b.note("pointer %d,%d buttons %#x", x, y, m)
	}
	return nil
}
func (b *offlineBackend) TypeString(s string, _ time.Duration) error {
	return b.note("type %q", s)
}
func (b *offlineBackend) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}
func (b *offlineBackend) Close() error {
	b.cancel()
	return nil
}
func (b *offlineBackend) finish(err error) {
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
	b.once.Do(func() { close(b.done) })
}
func (b *offlineBackend) Stats() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	w, h := b.fb.Size()
	s := fmt.Sprintf("Offline source\nResolution:      %d x %d\nFrames:          %d\nInput events:    %d\nLast input:      %s\n", w, h, b.fb.Frames(), b.events, b.last)
	if b.stats != nil {
		s += b.stats()
	}
	return s
}

// newDemoBackend animates colour bars, a moving block and a marker under the
// last pointer position, so rendering, scaling and input mapping can be
// checked with no iDRAC.
func newDemoBackend() viewer.Backend {
	ctx, cancel := context.WithCancel(context.Background())
	b := &offlineBackend{fb: kvm.NewFramebuffer(), cancel: cancel, done: make(chan struct{})}
	b.fb.Resize(1024, 768)
	go func() {
		defer b.finish(nil)
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		bars := [][3]uint8{{192, 192, 192}, {192, 192, 0}, {0, 192, 192}, {0, 192, 0}, {192, 0, 192}, {192, 0, 0}, {0, 0, 192}, {16, 16, 16}}
		for n := 0; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			b.mu.Lock()
			px, py := b.px, b.py
			b.mu.Unlock()
			b.fb.Update(true, func(img *image.RGBA) image.Rectangle {
				w, h := img.Rect.Dx(), img.Rect.Dy()
				for y := 0; y < h; y++ {
					row := img.Pix[y*img.Stride:]
					for x := 0; x < w; x++ {
						c := bars[x*len(bars)/w]
						if y > h*3/4 { // grey ramp
							v := uint8(x * 255 / w)
							c = [3]uint8{v, v, v}
						}
						row[x*4], row[x*4+1], row[x*4+2], row[x*4+3] = c[0], c[1], c[2], 255
					}
				}
				// moving block
				bx := int((math.Sin(float64(n)/20)+1)/2*float64(w-80)) + 0
				by := h/2 - 40
				fill(img, image.Rect(bx, by, bx+80, by+80), 255, 255, 255)
				// pointer marker
				fill(img, image.Rect(px-12, py-1, px+13, py+2), 255, 64, 64)
				fill(img, image.Rect(px-1, py-12, px+2, py+13), 255, 64, 64)
				// 1px border to make edge clipping visible
				fill(img, image.Rect(0, 0, w, 1), 255, 255, 0)
				fill(img, image.Rect(0, h-1, w, h), 255, 255, 0)
				fill(img, image.Rect(0, 0, 1, h), 255, 255, 0)
				fill(img, image.Rect(w-1, 0, w, h), 255, 255, 0)
				return img.Rect
			})
		}
	}()
	return b
}

func fill(img *image.RGBA, r image.Rectangle, cr, cg, cb uint8) {
	r = r.Intersect(img.Rect)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = cr, cg, cb, 255
		}
	}
}

// newReplayBackend decodes a recording on a loop.
func newReplayBackend(path string, speed float64, g *globals) (viewer.Backend, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &offlineBackend{fb: kvm.NewFramebuffer(), cancel: cancel, done: make(chan struct{}), log: g.logger.Printf}
	var smu sync.Mutex
	var last *kvm.VideoStats
	b.stats = func() string {
		smu.Lock()
		defer smu.Unlock()
		if last == nil {
			return "Replay:          first pass in progress\n"
		}
		return fmt.Sprintf("Replay pass:     %d packets, %d decode errors, %d ASpeed errors\n", last.Packets, last.DecodeErrors, last.ASpeedErrors)
	}
	go func() {
		for {
			f, err := os.Open(path)
			if err != nil {
				b.finish(err)
				return
			}
			st, err := kvm.Replay(ctx, f, b.fb, g.logger, speed)
			f.Close()
			if st != nil {
				smu.Lock()
				last = st
				smu.Unlock()
			}
			if ctx.Err() != nil {
				b.finish(nil)
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				b.finish(fmt.Errorf("replay: %w", err))
				return
			}
			select {
			case <-ctx.Done():
				b.finish(nil)
				return
			case <-time.After(1500 * time.Millisecond): // pause on the last frame, then loop
			}
		}
	}()
	return b, nil
}
