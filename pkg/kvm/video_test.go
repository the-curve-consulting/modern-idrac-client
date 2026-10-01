package kvm

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

// vidPkt frames a video-socket packet as the iDRAC6 sends it (type high
// byte 0x86, which the reader ignores).
func vidPkt(typ byte, payload []byte) []byte {
	total := vidHeaderLen + len(payload)
	b := []byte{0, 0, 0, 0, 0x86, typ, byte(total >> 8), byte(total)}
	return append(b, payload...)
}

// dvcPkt builds a DVC video packet (com.avocent.kvm.b.a.hb layout).
func dvcPkt(mode byte, w, h int, bof, eof bool, checksum uint16, data []byte) []byte {
	p := make([]byte, 12, 12+len(data))
	p[4], p[5] = byte(h>>8), byte(h)
	p[6], p[7] = byte(w>>8), byte(w)
	if bof {
		p[8] |= 1
	}
	if eof {
		p[8] |= 2
	}
	p[10], p[11] = byte(checksum>>8), byte(checksum)
	return vidPkt(mode, append(p, data...))
}

type rwBuf struct {
	r *bytes.Reader
	w bytes.Buffer
}

func (b *rwBuf) Read(p []byte) (int, error)  { return b.r.Read(p) }
func (b *rwBuf) Write(p []byte) (int, error) { return b.w.Write(p) }

// runStream feeds data through a VideoStream until EOF.
func runStream(t *testing.T, fb *Framebuffer, data []byte, cfg func(*VideoStream)) (*VideoStream, *rwBuf, error) {
	t.Helper()
	rw := &rwBuf{r: bytes.NewReader(data)}
	v := NewVideoStream(rw, fb, nil)
	if cfg != nil {
		cfg(v)
	}
	err := v.Run(context.Background())
	return v, rw, err
}

func px(t *testing.T, fb *Framebuffer, x, y int) uint32 {
	t.Helper()
	img := fb.Snapshot()
	if !(image.Point{x, y}).In(img.Rect) {
		t.Fatalf("pixel (%d,%d) outside %v", x, y, img.Rect)
	}
	o := img.PixOffset(x, y)
	return uint32(img.Pix[o])<<16 | uint32(img.Pix[o+1])<<8 | uint32(img.Pix[o+2])
}

func expectPx(t *testing.T, fb *Framebuffer, x, y int, want uint32) {
	t.Helper()
	if got := px(t, fb, x, y); got != want {
		t.Errorf("pixel (%d,%d) = %06x, want %06x", x, y, got, want)
	}
}

const red15 = 0xF80000 // DVC15 make-pixel 0xFC 0x00

// ---- pure functions ---------------------------------------------------------

func TestDVCRunLength(t *testing.T) {
	cases := []struct {
		in       []byte
		n, used  int
		complete bool
	}{
		{[]byte{0x05}, 5, 1, false},                                // may continue in next packet
		{[]byte{0x05, 0x80}, 5, 1, true},                           // next byte is another opcode
		{[]byte{0x05, 0x01}, 5 | 1<<5, 2, false},                   // extension, maybe more
		{[]byte{0x25, 0x21, 0x80}, 5 | 1<<5, 2, true},              // copy-left opcode carried in extension
		{[]byte{0x25, 0x01, 0x80}, 5, 1, true},                     // different opcode -> not an extension
		{[]byte{0x1F, 0x1F, 0x1F, 0x1F, 0x1F}, 1<<25 - 1, 5, true}, // max 5 bytes
		{[]byte{0x1F, 0x1F, 0x1F, 0x1F, 0x1F, 0x1F}, 1<<25 - 1, 5, true},
	}
	for i, c := range cases {
		n, used, complete := dvcRunLength(c.in)
		if n != c.n || used != c.used || complete != c.complete {
			t.Errorf("case %d: got (%d,%d,%v) want (%d,%d,%v)", i, n, used, complete, c.n, c.used, c.complete)
		}
	}
}

func TestDVCSeriesExtent(t *testing.T) {
	if used, ok := dvcSeriesExtent([]byte{0x6A}); used != 1 || !ok {
		t.Errorf("no continuation: %d %v", used, ok)
	}
	if used, ok := dvcSeriesExtent([]byte{0x7A}); used != 1 || ok {
		t.Errorf("continuation missing: %d %v", used, ok)
	}
	if used, ok := dvcSeriesExtent([]byte{0x7A, 0x81, 0x01, 0xFF}); used != 3 || !ok {
		t.Errorf("two continuation bytes: %d %v", used, ok)
	}
	if used, ok := dvcSeriesExtent([]byte{0x7A, 0x81}); used != 2 || ok {
		t.Errorf("open continuation: %d %v", used, ok)
	}
}

func TestDVCMakePixel(t *testing.T) {
	if got := dvcMakePixel(dvcMode15, 0xFC, 0x00, 0); got != 0xFF000000|red15 {
		t.Errorf("DVC15 red = %08x", got)
	}
	if got := dvcMakePixel(dvcMode15, 0xFF, 0xFF, 0); got != 0xFFF8F8F8 { // 0x7FFF -> R=31,G=31,B=31
		t.Errorf("DVC15 white = %08x", got)
	}
	if got := dvcMakePixel(dvcMode23, 0xFF, 0xFF, 0xFF); got != 0xFFFFFFFE { // B has 7 bits
		t.Errorf("DVC23 white = %08x", got)
	}
	if got := dvcMakePixel(dvcMode23, 0x80|0x7F, 0x80, 0x00); got != 0xFFFF0000 {
		t.Errorf("DVC23 red = %08x", got)
	}
	if got := dvcMakePixel(dvcMode7Gray, 0xFF, 0, 0); got != 0xFFFEFEFE {
		t.Errorf("grey = %08x", got)
	}
	pal := DVCPalette()
	if pal[0] != 0xFF000000 || pal[1] != 0xFF000046 || pal[127] != 0xFFDFDFDF {
		t.Errorf("palette endpoints: %08x %08x %08x", pal[0], pal[1], pal[127])
	}
	// wire index 16 -> base[1] -> lv[0]<<0 | lv[0]<<8 | lv[1]<<16 = red 70
	if pal[16] != 0xFF460000 {
		t.Errorf("palette[16] = %08x", pal[16])
	}
	if got := dvcMakePixel(dvcMode7, 0x81, 0, 0); got != pal[1] {
		t.Errorf("DVC7 idx1 = %08x", got)
	}
}

// ---- decoder ------------------------------------------------------------------

func TestDVC15MakePixelCopyLeftCopyAbove(t *testing.T) {
	fb := NewFramebuffer()
	var dirty []image.Rectangle
	fb.Subscribe(func(r image.Rectangle) { dirty = append(dirty, r) })
	data := []byte{
		0xFC, 0x00, // make pixel red
		0x20 | 3, // copy left x3   -> row 0 all red
		0x40 | 4, // copy above x4  -> row 1 all red
	}
	v, _, err := runStream(t, fb, dvcPkt(dvcMode15, 4, 2, true, true, 0, data), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	if w, h := fb.Size(); w != 4 || h != 2 {
		t.Fatalf("size %dx%d", w, h)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 4; x++ {
			expectPx(t, fb, x, y, red15)
		}
	}
	if fb.Frames() != 1 {
		t.Errorf("frames = %d", fb.Frames())
	}
	if len(dirty) < 2 || dirty[len(dirty)-1] != image.Rect(0, 0, 4, 2) {
		t.Errorf("dirty rects = %v", dirty)
	}
	if s := v.Stats(); s.DVC.Commands != 3 || s.DVC.Pixels != 8 || s.DecodeErrors != 0 {
		t.Errorf("stats = %+v", s)
	}
}

func TestDVC7RunLengthBiasAndNoChange(t *testing.T) {
	fb := NewFramebuffer()
	data := []byte{
		0x81,     // palette index 1 = 0x000046
		0x20 | 1, // copy left: 1 + 2 bias = 3
		0x00 | 2, // no change: 2 + 2 = 4 -> cursor 8
		0x90,     // palette index 16 (red 70) at pixel 8
	}
	_, _, err := runStream(t, fb, dvcPkt(dvcMode7, 10, 1, true, true, 0, data), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	for x := 0; x < 4; x++ {
		expectPx(t, fb, x, 0, 0x000046)
	}
	for x := 4; x < 8; x++ {
		expectPx(t, fb, x, 0, 0)
	}
	expectPx(t, fb, 8, 0, 0x460000)
	expectPx(t, fb, 9, 0, 0)
}

func TestDVCGrayMakeSeries(t *testing.T) {
	fb := NewFramebuffer()
	data := []byte{
		0xFF,                     // grey 0xFE
		0x80,                     // black  -> A = black, B = 0xFEFEFE
		0x60 | 0x0A,              // MS 1010 -> B A B A  (pixels 2..5)
		0x70 | 0x05,              // MS with continuation, 0101 -> A B A B (6..9)
		0x41,                     // continuation, no more: bits 6 and 0 -> B A A A A A B (10..16)
		0x40 | 0x1F, 0x40 | 0x00, // copy above (run 31, with extension byte) on first line: ignored
	}
	_, _, err := runStream(t, fb, dvcPkt(dvcMode7Gray, 20, 1, true, true, 0, data), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	const W, K = 0xFEFEFE, 0x000000
	want := []uint32{W, K, W, K, W, K, K, W, K, W, W, K, K, K, K, K, W, K, K, K}
	for x, c := range want {
		expectPx(t, fb, x, 0, c)
	}
}

func TestDVC23AndCopyAboveFirstLineIgnored(t *testing.T) {
	fb := NewFramebuffer()
	data := []byte{
		0x40 | 2,         // copy above on first line: ignored, cursor stays 0
		0xFF, 0xFF, 0xFF, // DVC23 white (0xFFFFFE)
		0x20 | 1, // copy left 1 -> pixels 0,1
		0x00 | 1, // skip 1 -> cursor 3
		0x40 | 3, // copy above (row 1): pixels 3,4,5 <- 0,1,2
	}
	_, _, err := runStream(t, fb, dvcPkt(dvcMode23, 3, 2, true, true, 0, data), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	expectPx(t, fb, 0, 0, 0xFFFFFE)
	expectPx(t, fb, 1, 0, 0xFFFFFE)
	expectPx(t, fb, 2, 0, 0)
	expectPx(t, fb, 0, 1, 0xFFFFFE)
	expectPx(t, fb, 1, 1, 0xFFFFFE)
	expectPx(t, fb, 2, 1, 0)
}

func TestDVCCommandsSpanPackets(t *testing.T) {
	fb := NewFramebuffer()
	var stream []byte
	// Frame 1: a make-pixel split across packets, then a run-length whose
	// extension byte is in the next packet.
	stream = append(stream, dvcPkt(dvcMode15, 64, 1, true, false, 0, []byte{0xFC})...)
	stream = append(stream, dvcPkt(dvcMode15, 64, 1, false, false, 0, []byte{0x00, 0x01})...)                // b1 of pixel, then skip 1 (maybe extended)
	stream = append(stream, dvcPkt(dvcMode15, 64, 1, false, true, 0, []byte{0x01, 0xFC, 0x00, 0x20 | 1})...) // extension -> skip 33; red at 34, 35
	_, _, err := runStream(t, fb, stream, nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	expectPx(t, fb, 0, 0, red15)
	expectPx(t, fb, 1, 0, 0)
	expectPx(t, fb, 33, 0, 0)
	expectPx(t, fb, 34, 0, red15)
	expectPx(t, fb, 35, 0, red15)
	expectPx(t, fb, 36, 0, 0)
	if fb.Frames() != 1 {
		t.Errorf("frames = %d", fb.Frames())
	}
}

func TestDVCBOFResetsCursorAndSkipsUntilFirstBOF(t *testing.T) {
	fb := NewFramebuffer()
	var stream []byte
	// Packet without BOF before any frame: must be ignored.
	stream = append(stream, dvcPkt(dvcMode15, 4, 1, false, true, 0, []byte{0xFC, 0x00, 0x20 | 3})...)
	// Frame 1: red, red, black, black
	stream = append(stream, dvcPkt(dvcMode15, 4, 1, true, true, 0, []byte{0xFC, 0x00, 0x20 | 1})...)
	// Frame 2: white at 0 (cursor reset), then skip.
	stream = append(stream, dvcPkt(dvcMode15, 4, 1, true, true, 0, []byte{0xFF, 0xFF})...)
	_, _, err := runStream(t, fb, stream, nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	expectPx(t, fb, 0, 0, 0xF8F8F8)
	expectPx(t, fb, 1, 0, red15)
	expectPx(t, fb, 2, 0, 0)
	if fb.Frames() != 2 {
		t.Errorf("frames = %d", fb.Frames())
	}
}

func TestDVCChecksum(t *testing.T) {
	// 4 red pixels: 4 * 0x7C00 = 0x1F000 & 0xFFFF = 0xF000
	data := []byte{0xFC, 0x00, 0x20 | 3}
	fb := NewFramebuffer()
	v, _, err := runStream(t, fb, dvcPkt(dvcMode15, 4, 1, true, true, 0xF000, data), nil)
	if !errors.Is(err, io.EOF) || v.Stats().DecodeErrors != 0 || v.Stats().DVC.ChecksumErrors != 0 {
		t.Fatalf("good checksum rejected: %v %+v", err, v.Stats())
	}
	fb = NewFramebuffer()
	v, _, err = runStream(t, fb, dvcPkt(dvcMode15, 4, 1, true, true, 0x1234, data), nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	if v.Stats().DVC.ChecksumErrors != 1 || v.Stats().DecodeErrors != 1 {
		t.Errorf("bad checksum not counted: %+v", v.Stats())
	}
	expectPx(t, fb, 3, 0, red15) // frame still applied
}

func TestDVCResizeAndOversize(t *testing.T) {
	fb := NewFramebuffer()
	var stream []byte
	stream = append(stream, dvcPkt(dvcMode15, 8, 4, true, true, 0, []byte{0xFC, 0x00})...)
	stream = append(stream, dvcPkt(dvcMode15, 3000, 4, true, true, 0, []byte{0xFC, 0x00})...) // ignored (>2000)
	stream = append(stream, dvcPkt(dvcMode15, 0, 0, true, true, 0, []byte{0xFC, 0x00})...)    // bad size
	_, _, err := runStream(t, fb, stream, nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	if w, h := fb.Size(); w != 8 || h != 4 {
		t.Errorf("size %dx%d", w, h)
	}
	expectPx(t, fb, 0, 0, red15)
}

// ---- stream level -------------------------------------------------------------

type fakeControl struct {
	sent []struct {
		typ     uint16
		payload []byte
	}
}

func (f *fakeControl) SendControlPacket(typ uint16, payload []byte) error {
	f.sent = append(f.sent, struct {
		typ     uint16
		payload []byte
	}{typ, append([]byte(nil), payload...)})
	return nil
}

func TestVideoAckEveryTwentyPackets(t *testing.T) {
	var stream []byte
	for i := 0; i < 45; i++ {
		stream = append(stream, vidPkt(vidTypeNop, make([]byte, 8))...)
	}
	v, rw, err := runStream(t, NewFramebuffer(), stream, nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	want := append(EncodeVideoAck(20), EncodeVideoAck(20)...)
	if !bytes.Equal(rw.w.Bytes(), want) {
		t.Errorf("acks written:\n%s\nwant:\n%s", HexDump(rw.w.Bytes(), 0), HexDump(want, 0))
	}
	if v.Stats().AcksSent != 2 || v.Stats().Packets != 45 {
		t.Errorf("stats %+v", v.Stats())
	}
}

func TestSetDVCColorDepthAndRefreshGoToControlChannel(t *testing.T) {
	ctl := &fakeControl{}
	var stream []byte
	stream = append(stream, dvcPkt(dvcMode15, 4, 1, true, true, 0, []byte{0xFC, 0x00})...)
	stream = append(stream, dvcPkt(dvcMode15, 4, 1, true, true, 0, []byte{0xFC, 0x00})...)
	stream = append(stream, dvcPkt(dvcMode7, 4, 1, true, true, 0, []byte{0x81})...)
	v, _, err := runStream(t, NewFramebuffer(), stream, func(v *VideoStream) { v.Control = ctl })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	if len(ctl.sent) != 2 {
		t.Fatalf("control packets = %d, want 2 (one per mode change)", len(ctl.sent))
	}
	for _, s := range ctl.sent {
		if s.typ != vidCtlSetDVCColorDepth || !bytes.Equal(s.payload, []byte{1, 0, 0, 0, 0, 0, 0, 0}) {
			t.Errorf("unexpected control packet %04x %x", s.typ, s.payload)
		}
	}
	if err := v.RequestRefresh(); err != nil {
		t.Fatal(err)
	}
	if last := ctl.sent[len(ctl.sent)-1]; last.typ != vidCtlScreenRefresh || len(last.payload) != 8 {
		t.Errorf("refresh packet %04x %x", last.typ, last.payload)
	}
	v2 := NewVideoStream(&rwBuf{r: bytes.NewReader(nil)}, nil, nil)
	if err := v2.RequestRefresh(); !errors.Is(err, ErrNoControlChannel) {
		t.Errorf("RequestRefresh without control: %v", err)
	}
}

func TestVideoStoppedAndConnectStatus(t *testing.T) {
	fb := NewFramebuffer()
	var stream []byte
	stream = append(stream, vidPkt(vidTypeConnectStatus, make([]byte, 8))...)
	stream = append(stream, dvcPkt(dvcMode15, 4, 1, true, true, 0, []byte{0xFC, 0x00, 0x20 | 3})...)
	stream = append(stream, vidPkt(vidTypeVideoStopped, []byte{3, 0, 0, 0, 0, 0, 0, 0})...)
	var connected, stopped = -1, -1
	_, _, err := runStream(t, fb, stream, func(v *VideoStream) {
		v.OnConnectStatus = func(ok bool) {
			if ok {
				connected = 1
			} else {
				connected = 0
			}
		}
		v.OnVideoStopped = func(reason int) { stopped = reason }
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	if connected != 1 || stopped != 3 {
		t.Errorf("callbacks: connected=%d stopped=%d", connected, stopped)
	}
	expectPx(t, fb, 0, 0, 0) // cleared to black
	if fb.Frames() != 2 {
		t.Errorf("frames = %d", fb.Frames())
	}
}

func TestUnknownAndOversizePackets(t *testing.T) {
	var stream []byte
	stream = append(stream, vidPkt(0x7B, make([]byte, 8))...)
	stream = append(stream, vidPkt(vidTypeASpeedJPEG, make([]byte, 20))...)
	v, _, err := runStream(t, NewFramebuffer(), stream, nil)
	if !errors.Is(err, io.EOF) || v.Stats().Unknown != 1 || v.Stats().ASpeedPackets != 1 {
		t.Errorf("err=%v stats=%+v", err, v.Stats())
	}
	// length below the minimum is a framing error
	bad := []byte{0, 0, 0, 0, 0x86, 0x80, 0, 12, 0, 0, 0, 0}
	_, _, err = runStream(t, NewFramebuffer(), bad, nil)
	if err == nil || errors.Is(err, io.EOF) {
		t.Errorf("short length accepted: %v", err)
	}
	big := []byte{0, 0, 0, 0, 0x86, 0x80, 0xFF, 0xFF}
	_, _, err = runStream(t, NewFramebuffer(), big, nil)
	if err == nil || errors.Is(err, io.EOF) {
		t.Errorf("huge length accepted: %v", err)
	}
}

func TestTruncatedStreamReturnsError(t *testing.T) {
	var full []byte
	full = append(full, vidPkt(vidTypeConnectStatus, make([]byte, 8))...)
	full = append(full, dvcPkt(dvcMode15, 8, 2, true, false, 0, []byte{0xFC, 0x00, 0x20 | 3, 0x40})...)
	full = append(full, dvcPkt(dvcMode7, 8, 2, false, true, 0x10, []byte{0x04, 0x6A, 0x7F, 0x81, 0x00})...)
	full = append(full, vidPkt(vidTypeTextMode, make([]byte, 12))...)
	for cut := 1; cut < len(full); cut++ {
		_, _, err := runStream(t, NewFramebuffer(), full[:cut], nil)
		if err == nil {
			t.Fatalf("cut at %d: no error", cut)
		}
		if errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			// a clean EOF is only acceptable on a packet boundary
			onBoundary := false
			for _, b := range []int{16, 16 + 24, 16 + 24 + 25, len(full)} {
				if cut == b {
					onBoundary = true
				}
			}
			if !onBoundary {
				t.Errorf("cut at %d: clean EOF inside a packet", cut)
			}
		}
	}
}

func TestDecoderGarbageDoesNotPanic(t *testing.T) {
	// Every byte value as a command, in every mode, over a tiny frame.
	for _, mode := range []byte{dvcMode15, dvcMode7, dvcMode7Gray, dvcMode23} {
		data := make([]byte, 512)
		for i := range data {
			data[i] = byte(i*7 + 3)
		}
		var stream []byte
		stream = append(stream, dvcPkt(mode, 5, 3, true, false, 0, data[:200])...)
		stream = append(stream, dvcPkt(mode, 5, 3, false, false, 0, data[200:400])...)
		stream = append(stream, dvcPkt(mode, 5, 3, false, true, 0xFFFF, data[400:])...)
		if _, _, err := runStream(t, NewFramebuffer(), stream, nil); !errors.Is(err, io.EOF) {
			t.Errorf("mode %02x: %v", mode, err)
		}
	}
}

// ---- text mode ------------------------------------------------------------------

func TestTextMode(t *testing.T) {
	fb := NewFramebuffer()
	// Font: glyph 0x41 row 0 = 0x80 (leftmost pixel), other rows 0.
	// Glyph 0xC4 (box drawing '-') all rows 0xFF to check 9th-column replication.
	font := make([]byte, 4+textFontTableSize)
	font[0], font[1] = 0, 1 // table 0, one font
	font[4+0x41*32+0] = 0x80
	for r := 0; r < 32; r++ {
		font[4+0xC4*32+r] = 0xFF
	}
	// Palette: 16 entries, index i = (i*16, i*16, i*16) except 1 = blue, 15 = white.
	pal := make([]byte, 4+16*4)
	pal[1] = 16
	for i := 0; i < 16; i++ {
		v := byte(i * 16)
		pal[4+i*4], pal[4+i*4+1], pal[4+i*4+2] = v, v, v
	}
	pal[4+1*4], pal[4+1*4+1], pal[4+1*4+2] = 0, 0, 0xAA
	pal[4+15*4], pal[4+15*4+1], pal[4+15*4+2] = 0xFF, 0xFF, 0xFF
	const rows, cols = 25, 80
	cells := make([]byte, rows*cols*2)
	cells[0], cells[1] = 0x41, 0x1F // 'A', white on blue
	cells[2], cells[3] = 0xC4, 0x07 // '-', grey 7 on black
	// text packet: 720x400, BOF|EOF|lineGraphics, underline row 0, cursor at cell 0 rows 14..15
	hdr := make([]byte, 12)
	hdr[4], hdr[5] = 400>>8, 400&0xFF
	hdr[6], hdr[7] = 720>>8, 720&0xFF
	hdr[8] = 1 | 2 | 0x10
	hdr[10], hdr[11] = rows, cols
	payload := append(hdr, cells...)
	payload = append(payload, 0, 0, 14, 15)

	var stream []byte
	stream = append(stream, vidPkt(vidTypeFontTable, font)...)
	stream = append(stream, vidPkt(vidTypeColorPalette, pal)...)
	stream = append(stream, vidPkt(vidTypeTextMode, payload)...)
	v, _, err := runStream(t, fb, stream, nil)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	if s := v.Stats(); s.DecodeErrors != 0 || s.FontPackets != 1 || s.PalettePackets != 1 || s.TextPackets != 1 {
		t.Fatalf("stats %+v", s)
	}
	if w, h := fb.Size(); w != 720 || h != 400 {
		t.Fatalf("size %dx%d", w, h)
	}
	const white, blue, grey7 = 0xFFFFFF, 0x0000AA, 0x707070
	expectPx(t, fb, 0, 0, white) // glyph pixel
	expectPx(t, fb, 1, 0, blue)  // background
	expectPx(t, fb, 8, 0, blue)  // 9th column = bg for non line-graphics glyph
	expectPx(t, fb, 0, 1, blue)  // row 1 empty
	// cursor: bottom 2 rows (14,15) of cell 0 in fg
	expectPx(t, fb, 4, 13, blue)
	expectPx(t, fb, 4, 14, white)
	expectPx(t, fb, 4, 15, white)
	// cell 1: line-graphics glyph replicates column 7 into column 8
	expectPx(t, fb, 9, 5, grey7)
	expectPx(t, fb, 9+8, 5, grey7)
	// cell 2 (empty, attr 0): palette index 0 on 0 = black
	expectPx(t, fb, 18, 5, 0)
	if fb.Frames() != 1 {
		t.Errorf("frames = %d", fb.Frames())
	}

	// Cursor-only update moves the cursor to cell 1; cell 0 is repainted.
	cur := make([]byte, 16)
	copy(cur, hdr)
	cur[8] = 1 | 2
	cur[12], cur[13], cur[14], cur[15] = 0, 1, 14, 15
	rw := &rwBuf{r: bytes.NewReader(vidPkt(vidTypeTextMode, cur))}
	v2 := NewVideoStream(rw, fb, nil)
	v2.text = v.text
	v2.inTextMode = true
	if err := v2.Run(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	expectPx(t, fb, 4, 14, blue)    // old cursor erased
	expectPx(t, fb, 9+4, 14, grey7) // new cursor drawn in cell 1's fg (already fg from glyph)
	expectPx(t, fb, 9+4, 15, grey7)
}

func TestTextPacketParsing(t *testing.T) {
	p, err := parseTextPacket(make([]byte, 12))
	if err != nil || !p.empty {
		t.Errorf("empty: %v %+v", err, p)
	}
	hdr := make([]byte, 16)
	hdr[8] = 3
	hdr[12], hdr[13], hdr[14], hdr[15] = 0x01, 0x02, 5, 9
	p, err = parseTextPacket(hdr)
	if err != nil || !p.cursorOnly || p.cursorPos != 0x102 || p.cursorStart != 5 || p.cursorEnd != 9 {
		t.Errorf("cursor only: %v %+v", err, p)
	}
	if _, err := parseTextPacket(make([]byte, 5)); err == nil {
		t.Error("short payload accepted")
	}
	if _, err := parseTextPalette([]byte{0, 9, 0, 0, 1, 2, 3, 4}); err == nil {
		t.Error("palette overrun accepted")
	}
}

// ---- framebuffer --------------------------------------------------------------------

func TestFramebufferBasics(t *testing.T) {
	fb := NewFramebuffer()
	if w, h := fb.Size(); w != DefaultFramebufferWidth || h != DefaultFramebufferHeight {
		t.Fatalf("default size %dx%d", w, h)
	}
	var got []image.Rectangle
	unsub := fb.Subscribe(func(r image.Rectangle) { got = append(got, r) })
	fb.Resize(16, 8)
	if len(got) != 1 || got[0] != image.Rect(0, 0, 16, 8) {
		t.Errorf("resize notification %v", got)
	}
	fb.modify(func(img *image.RGBA) image.Rectangle {
		putARGB(img.Pix, 5, 0xFF102030)
		return image.Rect(5, 0, 6, 1)
	})
	dst := image.NewRGBA(image.Rect(0, 0, 16, 8))
	fb.CopyRect(dst, image.Rect(0, 0, 16, 1))
	if dst.Pix[5*4] != 0x10 || dst.Pix[5*4+1] != 0x20 || dst.Pix[5*4+2] != 0x30 || dst.Pix[5*4+3] != 0xFF {
		t.Errorf("CopyRect pixel %v", dst.Pix[5*4:5*4+4])
	}
	snap := fb.Snapshot()
	snap.Pix[5*4] = 0
	if px(t, fb, 5, 0) != 0x102030 {
		t.Error("Snapshot is not a deep copy")
	}
	unsub()
	fb.Resize(4, 4)
	if len(got) != 2 {
		t.Errorf("unsubscribe failed: %v", got)
	}
	var buf bytes.Buffer
	if err := fb.WritePNG(&buf); err != nil || !bytes.HasPrefix(buf.Bytes(), []byte("\x89PNG")) {
		t.Errorf("WritePNG: %v", err)
	}
}
