package kvm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/des"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"sync"
	"time"
)

// This file implements an RFB 3.8 (VNC) server that exposes a console
// Framebuffer to any VNC viewer and forwards keyboard/mouse events to an
// InputSink. It lets the iDRAC console be used from TigerVNC, Remmina,
// macOS Screen Sharing, noVNC, ... with no Java involved.

// InputSink receives events from VNC clients. Session implements it.
type InputSink interface {
	// KeyEvent receives an X11 keysym (as sent by RFB KeyEvent).
	KeyEvent(keysym uint32, down bool) error
	// PointerEvent receives absolute framebuffer coordinates and the RFB
	// button mask (bit0 left, bit1 middle, bit2 right, bit3/4 wheel up/down).
	PointerEvent(x, y int, buttonMask uint8) error
}

// FramebufferSource is what the VNC server needs from a Framebuffer.
type FramebufferSource interface {
	Size() (w, h int)
	CopyRect(dst *image.RGBA, r image.Rectangle)
	Subscribe(fn func(r image.Rectangle)) (unsubscribe func())
}

// VNCServer serves one framebuffer to any number of viewers.
type VNCServer struct {
	FB       FramebufferSource
	Input    InputSink // may be nil for view-only
	Password string    // empty = no authentication (security type None)
	Name     string    // desktop name shown by viewers
	Logger   Logger
	ViewOnly bool
	// OnClientChange, if set, is called with the number of connected clients.
	OnClientChange func(n int)

	mu      sync.Mutex
	clients map[*vncClient]struct{}
}

// ServeListener accepts viewers until ctx is done or the listener fails.
func (s *VNCServer) ServeListener(ctx context.Context, ln net.Listener) error {
	if s.Logger == nil {
		s.Logger = nopLogger{}
	}
	if s.Name == "" {
		s.Name = "iDRAC console"
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serveConn(ctx, conn)
	}
}

// ListenAndServe listens on addr (e.g. ":5901") and serves viewers.
func (s *VNCServer) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.ServeListener(ctx, ln)
}

// ClientCount returns the number of connected viewers.
func (s *VNCServer) ClientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.clients)
}

func (s *VNCServer) track(c *vncClient, add bool) {
	s.mu.Lock()
	if s.clients == nil {
		s.clients = map[*vncClient]struct{}{}
	}
	if add {
		s.clients[c] = struct{}{}
	} else {
		delete(s.clients, c)
	}
	n := len(s.clients)
	s.mu.Unlock()
	if s.OnClientChange != nil {
		s.OnClientChange(n)
	}
}

// pixelFormat is the RFB PIXEL_FORMAT structure.
type pixelFormat struct {
	BPP, Depth, BigEndian, TrueColour uint8
	RedMax, GreenMax, BlueMax         uint16
	RedShift, GreenShift, BlueShift   uint8
}

var serverPixelFormat = pixelFormat{BPP: 32, Depth: 24, BigEndian: 0, TrueColour: 1, RedMax: 255, GreenMax: 255, BlueMax: 255, RedShift: 16, GreenShift: 8, BlueShift: 0}

func (p pixelFormat) encode() []byte {
	b := make([]byte, 16)
	b[0], b[1], b[2], b[3] = p.BPP, p.Depth, p.BigEndian, p.TrueColour
	binary.BigEndian.PutUint16(b[4:], p.RedMax)
	binary.BigEndian.PutUint16(b[6:], p.GreenMax)
	binary.BigEndian.PutUint16(b[8:], p.BlueMax)
	b[10], b[11], b[12] = p.RedShift, p.GreenShift, p.BlueShift
	return b
}

func decodePixelFormat(b []byte) pixelFormat {
	return pixelFormat{
		BPP: b[0], Depth: b[1], BigEndian: b[2], TrueColour: b[3],
		RedMax: binary.BigEndian.Uint16(b[4:]), GreenMax: binary.BigEndian.Uint16(b[6:]), BlueMax: binary.BigEndian.Uint16(b[8:]),
		RedShift: b[10], GreenShift: b[11], BlueShift: b[12],
	}
}

const (
	encRaw         = 0
	encCopyRect    = 1
	encDesktopSize = -223
)

type vncClient struct {
	srv    *VNCServer
	conn   net.Conn
	rd     *bufio.Reader
	wr     *bufio.Writer
	wmu    sync.Mutex
	pf     pixelFormat
	encs   map[int32]bool
	w, h   int
	dirty  image.Rectangle
	dmu    sync.Mutex
	wantUp bool // an update request is outstanding
	notify chan struct{}
}

func (s *VNCServer) serveConn(ctx context.Context, conn net.Conn) {
	c := &vncClient{srv: s, conn: conn, rd: bufio.NewReaderSize(conn, 64<<10), wr: bufio.NewWriterSize(conn, 256<<10), pf: serverPixelFormat, encs: map[int32]bool{encRaw: true}, notify: make(chan struct{}, 1)}
	defer conn.Close()
	if err := c.handshake(); err != nil {
		s.Logger.Printf("vnc %s: handshake: %v", conn.RemoteAddr(), err)
		return
	}
	s.Logger.Printf("vnc %s: viewer connected", conn.RemoteAddr())
	s.track(c, true)
	defer s.track(c, false)

	c.w, c.h = s.FB.Size()
	unsub := s.FB.Subscribe(func(r image.Rectangle) {
		c.dmu.Lock()
		c.dirty = c.dirty.Union(r)
		c.dmu.Unlock()
		select {
		case c.notify <- struct{}{}:
		default:
		}
	})
	defer unsub()

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.updateLoop(cctx)
	if err := c.readLoop(cctx); err != nil && !errors.Is(err, io.EOF) && cctx.Err() == nil {
		s.Logger.Printf("vnc %s: %v", conn.RemoteAddr(), err)
	}
	s.Logger.Printf("vnc %s: viewer disconnected", conn.RemoteAddr())
}

func (c *vncClient) handshake() error {
	c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer c.conn.SetDeadline(time.Time{})
	if _, err := c.conn.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	ver := make([]byte, 12)
	if _, err := io.ReadFull(c.rd, ver); err != nil {
		return err
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(ver), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return fmt.Errorf("unsupported client version %q", ver)
	}
	if minor < 7 {
		// RFB 3.3: server picks the security type.
		st := uint32(1)
		if c.srv.Password != "" {
			st = 2
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], st)
		if _, err := c.conn.Write(b[:]); err != nil {
			return err
		}
		if st == 2 {
			if err := c.vncAuth(); err != nil {
				return err
			}
		}
	} else {
		sec := byte(1)
		if c.srv.Password != "" {
			sec = 2
		}
		if _, err := c.conn.Write([]byte{1, sec}); err != nil {
			return err
		}
		chosen, err := c.rd.ReadByte()
		if err != nil {
			return err
		}
		if chosen != sec {
			c.securityResult(false, "unsupported security type")
			return fmt.Errorf("client chose security type %d", chosen)
		}
		if sec == 2 {
			if err := c.vncAuth(); err != nil {
				return err
			}
		}
		if err := c.securityResult(true, ""); err != nil {
			return err
		}
	}
	// ClientInit
	if _, err := c.rd.ReadByte(); err != nil { // shared flag
		return err
	}
	w, h := c.srv.FB.Size()
	if w == 0 || h == 0 {
		w, h = 1024, 768
	}
	var init bytes.Buffer
	binary.Write(&init, binary.BigEndian, uint16(w))
	binary.Write(&init, binary.BigEndian, uint16(h))
	init.Write(serverPixelFormat.encode())
	binary.Write(&init, binary.BigEndian, uint32(len(c.srv.Name)))
	init.WriteString(c.srv.Name)
	_, err := c.conn.Write(init.Bytes())
	return err
}

func (c *vncClient) securityResult(ok bool, reason string) error {
	var b bytes.Buffer
	if ok {
		binary.Write(&b, binary.BigEndian, uint32(0))
	} else {
		binary.Write(&b, binary.BigEndian, uint32(1))
		binary.Write(&b, binary.BigEndian, uint32(len(reason)))
		b.WriteString(reason)
	}
	_, err := c.conn.Write(b.Bytes())
	return err
}

// vncAuth performs the classic DES challenge (RFB security type 2).
func (c *vncClient) vncAuth() error {
	challenge := make([]byte, 16)
	if _, err := rand.Read(challenge); err != nil {
		return err
	}
	if _, err := c.conn.Write(challenge); err != nil {
		return err
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(c.rd, resp); err != nil {
		return err
	}
	expected, err := vncEncryptChallenge(c.srv.Password, challenge)
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, resp) {
		c.securityResult(false, "authentication failed")
		return errors.New("vnc authentication failed")
	}
	return nil
}

// vncEncryptChallenge applies the RFB DES variant: the key is the password
// padded/truncated to 8 bytes with each byte's bits reversed.
func vncEncryptChallenge(password string, challenge []byte) ([]byte, error) {
	key := make([]byte, 8)
	copy(key, password)
	for i := range key {
		b := key[i]
		var r byte
		for j := 0; j < 8; j++ {
			r = r<<1 | b&1
			b >>= 1
		}
		key[i] = r
	}
	blk, err := des.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	blk.Encrypt(out[:8], challenge[:8])
	blk.Encrypt(out[8:], challenge[8:])
	return out, nil
}

func (c *vncClient) readLoop(ctx context.Context) error {
	for {
		t, err := c.rd.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0: // SetPixelFormat
			b := make([]byte, 19)
			if _, err := io.ReadFull(c.rd, b); err != nil {
				return err
			}
			pf := decodePixelFormat(b[3:])
			if pf.TrueColour == 0 {
				return errors.New("colour-map pixel formats are not supported")
			}
			if pf.BPP != 8 && pf.BPP != 16 && pf.BPP != 32 {
				return fmt.Errorf("unsupported bits-per-pixel %d", pf.BPP)
			}
			c.wmu.Lock()
			c.pf = pf
			c.wmu.Unlock()
			c.markAllDirty()
		case 2: // SetEncodings
			b := make([]byte, 3)
			if _, err := io.ReadFull(c.rd, b); err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint16(b[1:]))
			encs := map[int32]bool{encRaw: true}
			for i := 0; i < n; i++ {
				var e int32
				if err := binary.Read(c.rd, binary.BigEndian, &e); err != nil {
					return err
				}
				encs[e] = true
			}
			c.wmu.Lock()
			c.encs = encs
			c.wmu.Unlock()
		case 3: // FramebufferUpdateRequest
			b := make([]byte, 9)
			if _, err := io.ReadFull(c.rd, b); err != nil {
				return err
			}
			incremental := b[0] != 0
			r := image.Rect(int(binary.BigEndian.Uint16(b[1:])), int(binary.BigEndian.Uint16(b[3:])), 0, 0)
			r.Max = image.Pt(r.Min.X+int(binary.BigEndian.Uint16(b[5:])), r.Min.Y+int(binary.BigEndian.Uint16(b[7:])))
			c.dmu.Lock()
			if !incremental {
				c.dirty = c.dirty.Union(r)
			}
			c.wantUp = true
			c.dmu.Unlock()
			select {
			case c.notify <- struct{}{}:
			default:
			}
		case 4: // KeyEvent
			b := make([]byte, 7)
			if _, err := io.ReadFull(c.rd, b); err != nil {
				return err
			}
			if c.srv.Input != nil && !c.srv.ViewOnly {
				if err := c.srv.Input.KeyEvent(binary.BigEndian.Uint32(b[3:]), b[0] != 0); err != nil {
					c.srv.Logger.Printf("vnc: key event: %v", err)
				}
			}
		case 5: // PointerEvent
			b := make([]byte, 5)
			if _, err := io.ReadFull(c.rd, b); err != nil {
				return err
			}
			if c.srv.Input != nil && !c.srv.ViewOnly {
				if err := c.srv.Input.PointerEvent(int(binary.BigEndian.Uint16(b[1:])), int(binary.BigEndian.Uint16(b[3:])), b[0]); err != nil {
					c.srv.Logger.Printf("vnc: pointer event: %v", err)
				}
			}
		case 6: // ClientCutText
			b := make([]byte, 7)
			if _, err := io.ReadFull(c.rd, b); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(b[3:])
			if n > 1<<20 {
				return errors.New("cut text too large")
			}
			if _, err := io.CopyN(io.Discard, c.rd, int64(n)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown client message type %d", t)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

func (c *vncClient) markAllDirty() {
	w, h := c.srv.FB.Size()
	c.dmu.Lock()
	c.dirty = image.Rect(0, 0, w, h)
	c.dmu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// updateLoop sends a FramebufferUpdate whenever the client has asked for one
// and there is something dirty, coalescing bursts of decoder updates.
func (c *vncClient) updateLoop(ctx context.Context) {
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.notify:
		case <-ticker.C:
		}
		c.dmu.Lock()
		want := c.wantUp
		w, h := c.srv.FB.Size()
		resized := w != c.w || h != c.h
		dirty := c.dirty
		if resized {
			dirty = image.Rect(0, 0, w, h)
		}
		if !want || (dirty.Empty() && !resized) || w == 0 || h == 0 {
			c.dmu.Unlock()
			continue
		}
		c.dirty = image.Rectangle{}
		c.wantUp = false
		c.dmu.Unlock()
		if err := c.sendUpdate(dirty.Intersect(image.Rect(0, 0, w, h)), resized, w, h); err != nil {
			c.conn.Close()
			return
		}
	}
}

func (c *vncClient) sendUpdate(r image.Rectangle, resized bool, w, h int) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var rects [][]byte
	if resized {
		c.w, c.h = w, h
		if c.encs[encDesktopSize] {
			hdr := make([]byte, 12)
			binary.BigEndian.PutUint16(hdr[4:], uint16(w))
			binary.BigEndian.PutUint16(hdr[6:], uint16(h))
			var enc int32 = encDesktopSize
			binary.BigEndian.PutUint32(hdr[8:], uint32(enc))
			rects = append(rects, hdr)
		}
		r = image.Rect(0, 0, w, h)
	}
	if !r.Empty() {
		// Cap oversize rects to the client's known geometry when DesktopSize is
		// unsupported, otherwise the viewer would reject the update.
		r = r.Intersect(image.Rect(0, 0, c.w, c.h))
		if !r.Empty() {
			img := image.NewRGBA(r)
			c.srv.FB.CopyRect(img, r)
			hdr := make([]byte, 12)
			binary.BigEndian.PutUint16(hdr[0:], uint16(r.Min.X))
			binary.BigEndian.PutUint16(hdr[2:], uint16(r.Min.Y))
			binary.BigEndian.PutUint16(hdr[4:], uint16(r.Dx()))
			binary.BigEndian.PutUint16(hdr[6:], uint16(r.Dy()))
			binary.BigEndian.PutUint32(hdr[8:], encRaw)
			rects = append(rects, append(hdr, c.translate(img)...))
		}
	}
	if len(rects) == 0 {
		return nil
	}
	head := []byte{0, 0, 0, 0}
	binary.BigEndian.PutUint16(head[2:], uint16(len(rects)))
	if _, err := c.wr.Write(head); err != nil {
		return err
	}
	for _, rc := range rects {
		if _, err := c.wr.Write(rc); err != nil {
			return err
		}
	}
	return c.wr.Flush()
}

// translate converts RGBA pixels into the client's pixel format.
func (c *vncClient) translate(img *image.RGBA) []byte {
	pf := c.pf
	w, h := img.Rect.Dx(), img.Rect.Dy()
	bpp := int(pf.BPP) / 8
	out := make([]byte, w*h*bpp)
	o := 0
	for y := 0; y < h; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+w*4]
		for x := 0; x < w; x++ {
			r, g, b := uint32(row[x*4]), uint32(row[x*4+1]), uint32(row[x*4+2])
			v := (r*uint32(pf.RedMax)/255)<<pf.RedShift | (g*uint32(pf.GreenMax)/255)<<pf.GreenShift | (b*uint32(pf.BlueMax)/255)<<pf.BlueShift
			switch bpp {
			case 4:
				if pf.BigEndian != 0 {
					binary.BigEndian.PutUint32(out[o:], v)
				} else {
					binary.LittleEndian.PutUint32(out[o:], v)
				}
			case 2:
				if pf.BigEndian != 0 {
					binary.BigEndian.PutUint16(out[o:], uint16(v))
				} else {
					binary.LittleEndian.PutUint16(out[o:], uint16(v))
				}
			case 1:
				out[o] = byte(v)
			}
			o += bpp
		}
	}
	return out
}
