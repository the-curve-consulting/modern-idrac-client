package kvm

import (
	"bufio"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeFB is a minimal FramebufferSource for the VNC server tests.
type fakeFB struct {
	mu   sync.Mutex
	img  *image.RGBA
	subs []func(image.Rectangle)
}

func (f *fakeFB) Size() (int, int) { return f.img.Rect.Dx(), f.img.Rect.Dy() }
func (f *fakeFB) CopyRect(dst *image.RGBA, r image.Rectangle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			dst.Set(x, y, f.img.At(x, y))
		}
	}
}
func (f *fakeFB) Subscribe(fn func(image.Rectangle)) func() {
	f.mu.Lock()
	f.subs = append(f.subs, fn)
	f.mu.Unlock()
	return func() {}
}

type recInput struct {
	mu   sync.Mutex
	keys []uint32
	ptr  []int
}

func (r *recInput) KeyEvent(k uint32, down bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, k)
	return nil
}
func (r *recInput) PointerEvent(x, y int, m uint8) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ptr = append(r.ptr, x, y, int(m))
	return nil
}

func TestVNCEncryptChallengeKnownVector(t *testing.T) {
	// Vector cross-checked with `openssl enc -des-ecb -K 0e86ceceeef64e26 -nopad` (bit-reversed "password"), zero challenge.
	got, err := vncEncryptChallenge("password", make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0xff, 0x97, 0x50, 0x2e, 0x94, 0x22, 0xf0, 0x89, 0xff, 0x97, 0x50, 0x2e, 0x94, 0x22, 0xf0, 0x89}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("challenge response mismatch:\n got % x\nwant % x", got, want)
		}
	}
}

func TestVNCServerHandshakeAndUpdate(t *testing.T) {
	fb := &fakeFB{img: image.NewRGBA(image.Rect(0, 0, 4, 2))}
	fb.img.Set(1, 0, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	in := &recInput{}
	srv := &VNCServer{FB: fb, Input: in, Name: "test"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.ServeListener(ctx, ln)

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	rd := bufio.NewReader(conn)
	ver := make([]byte, 12)
	io.ReadFull(rd, ver)
	if string(ver) != "RFB 003.008\n" {
		t.Fatalf("version %q", ver)
	}
	conn.Write([]byte("RFB 003.008\n"))
	n, _ := rd.ReadByte()
	types := make([]byte, n)
	io.ReadFull(rd, types)
	if n != 1 || types[0] != 1 {
		t.Fatalf("security types %v", types)
	}
	conn.Write([]byte{1})
	var res uint32
	binary.Read(rd, binary.BigEndian, &res)
	if res != 0 {
		t.Fatalf("security result %d", res)
	}
	conn.Write([]byte{1}) // ClientInit shared
	init := make([]byte, 24)
	io.ReadFull(rd, init)
	if binary.BigEndian.Uint16(init) != 4 || binary.BigEndian.Uint16(init[2:]) != 2 {
		t.Fatalf("server init size %v", init[:4])
	}
	nameLen := binary.BigEndian.Uint32(init[20:])
	name := make([]byte, nameLen)
	io.ReadFull(rd, name)
	if string(name) != "test" {
		t.Fatalf("name %q", name)
	}
	// Full update request.
	req := []byte{3, 0, 0, 0, 0, 0, 0, 4, 0, 2}
	conn.Write(req)
	hdr := make([]byte, 4)
	io.ReadFull(rd, hdr)
	if hdr[0] != 0 || binary.BigEndian.Uint16(hdr[2:]) != 1 {
		t.Fatalf("update header %v", hdr)
	}
	rect := make([]byte, 12)
	io.ReadFull(rd, rect)
	if binary.BigEndian.Uint16(rect[4:]) != 4 || binary.BigEndian.Uint16(rect[6:]) != 2 || binary.BigEndian.Uint32(rect[8:]) != 0 {
		t.Fatalf("rect %v", rect)
	}
	pix := make([]byte, 4*2*4)
	io.ReadFull(rd, pix)
	// pixel (1,0) is red: little-endian 0x00RRGGBB => bytes B,G,R,0
	if pix[4] != 0 || pix[5] != 0 || pix[6] != 255 {
		t.Fatalf("pixel bytes % x", pix[4:8])
	}
	// Key + pointer events.
	key := []byte{4, 1, 0, 0, 0, 0, 0xff, 0x0d}
	conn.Write(key)
	ptr := []byte{5, 1, 0, 3, 0, 1}
	conn.Write(ptr)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		in.mu.Lock()
		ok := len(in.keys) == 1 && len(in.ptr) == 3
		in.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	if len(in.keys) != 1 || in.keys[0] != 0xff0d {
		t.Fatalf("keys %v", in.keys)
	}
	if len(in.ptr) != 3 || in.ptr[0] != 3 || in.ptr[1] != 1 || in.ptr[2] != 1 {
		t.Fatalf("pointer %v", in.ptr)
	}
}
