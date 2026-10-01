package kvm

import (
	"bytes"
	"errors"
	"image"
	"io"
	"math/rand"
	"testing"
)

// ---- helpers ---------------------------------------------------------------

// aspeedBitWriter builds a bitstream the way the ASpeed engine emits it:
// MSB first into 32-bit words, stored little-endian. bytes() pads to a
// word boundary and appends one zero word so the reader's lookahead never
// runs dry before the frame-end code.
type aspeedBitWriter struct{ bits []bool }

func (w *aspeedBitWriter) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, v>>uint(i)&1 == 1)
	}
}

func (w *aspeedBitWriter) bytes() []byte {
	nw := (len(w.bits)+31)/32 + 1
	words := make([]uint32, nw)
	for i, b := range w.bits {
		if b {
			words[i/32] |= 1 << uint(31-i%32)
		}
	}
	out := make([]byte, 0, nw*4)
	for _, x := range words {
		out = append(out, byte(x), byte(x>>8), byte(x>>16), byte(x>>24))
	}
	return out
}

// huffCode returns the canonical code and length of sym in t.
func huffCode(t *aspeedHuffTable, sym uint8) (code uint32, length int) {
	for l := 1; l <= 16; l++ {
		if t.minCode[l] == 0xFFFF {
			continue
		}
		for c := t.minCode[l]; c <= t.maxCode[l]; c++ {
			if t.vals[l][c-t.minCode[l]] == sym {
				return uint32(c), l
			}
		}
	}
	panic("symbol not in table")
}

func (w *aspeedBitWriter) putSym(t *aspeedHuffTable, sym uint8) {
	c, l := huffCode(t, sym)
	w.put(c, l)
}

// putDC writes a DC difference: category symbol then magnitude bits.
func (w *aspeedBitWriter) putDC(t *aspeedHuffTable, v int) {
	if v == 0 {
		w.putSym(t, 0)
		return
	}
	mag := v
	if mag < 0 {
		mag = -mag
	}
	cat := 0
	for m := mag; m > 0; m >>= 1 {
		cat++
	}
	w.putSym(t, uint8(cat))
	if v < 0 {
		v += 1<<uint(cat) - 1
	}
	w.put(uint32(v), cat)
}

// aspeedPayload frames a 0x86 payload (12-byte header + data).
func aspeedPayload(mode, w, h int, flags, tables byte, cursor uint16, data []byte) []byte {
	p := make([]byte, 12, 12+len(data))
	p[0] = byte(mode)
	p[4], p[5] = byte(h>>8), byte(h)
	p[6], p[7] = byte(w>>8), byte(w)
	p[8] = flags
	p[9], p[10] = byte(cursor>>8), byte(cursor)
	p[11] = tables
	return append(p, data...)
}

// dcOnlyJPEGBlock writes a JPEG block (no-skip code) with DC-only Y blocks
// (ny = 1 or 4) and zero Cb/Cr.
func dcOnlyJPEGBlock(w *aspeedBitWriter, yDiffs []int) {
	w.put(aspeedCodeJPEG, aspeedCodeBits)
	for _, d := range yDiffs {
		w.putDC(&aspeedDCTables[0], d)
		w.putSym(&aspeedACTables[0], 0x00) // EOB
	}
	for i := 0; i < 2; i++ {
		w.putDC(&aspeedDCTables[1], 0)
		w.putSym(&aspeedACTables[1], 0x00)
	}
}

// pxARGB is px() with the opaque alpha the decoder writes, for comparing
// against aspeedYUV() results.
func pxARGB(t *testing.T, fb *Framebuffer, x, y int) uint32 {
	t.Helper()
	return 0xFF000000 | px(t, fb, x, y)
}

func newTestASpeed(t *testing.T, w, h int) (*aspeedDecoder, *Framebuffer) {
	t.Helper()
	fb := NewFramebuffer()
	fb.Resize(w, h)
	return newASpeedDecoder(fb, nil), fb
}

// ---- bit reader ---------------------------------------------------------------

func TestASpeedBitReader(t *testing.T) {
	initASpeedTables()
	// Words 0xA5A5A5A5, 0x0F0F0F0F, 0x12345678, 0 (little-endian on the wire).
	data := []byte{0xA5, 0xA5, 0xA5, 0xA5, 0x0F, 0x0F, 0x0F, 0x0F, 0x78, 0x56, 0x34, 0x12, 0, 0, 0, 0}
	r, err := newASpeedBitReader(data)
	if err != nil {
		t.Fatal(err)
	}
	if r.window() != 0xA5A5A5A5 {
		t.Fatalf("window = %#x", r.window())
	}
	if got := r.peek(4); got != 0xA {
		t.Fatalf("peek(4) = %#x", got)
	}
	if got := r.peek(16); got != 0xA5A5 {
		t.Fatalf("peek(16) = %#x", got)
	}
	if err := r.consume(4); err != nil {
		t.Fatal(err)
	}
	if r.window() != 0x5A5A5A50 {
		t.Fatalf("after consume(4): window = %#x", r.window())
	}
	// Cross the first word boundary: 4 + 27 = 31 bits consumed.
	if err := r.consume(27); err != nil {
		t.Fatal(err)
	}
	// Remaining: last bit of word 0 (1) then word 1 (0x0F0F0F0F).
	if want := uint32(1<<31 | 0x0F0F0F0F>>1); r.window() != want {
		t.Fatalf("after consume(31): window = %#x want %#x", r.window(), want)
	}
	if err := r.consume(1); err != nil {
		t.Fatal(err)
	}
	if r.window() != 0x0F0F0F0F {
		t.Fatalf("after consume(32): window = %#x", r.window())
	}
	// Consume through word 1. Like the Java (d.a: "ob - n <= 0"), the
	// reader fetches the next word as soon as the lookahead is exactly
	// used up, so word 3 is pulled here although no bit of it is needed.
	if err := r.consume(20); err != nil {
		t.Fatal(err)
	}
	if err := r.consume(12); err != nil {
		t.Fatal(err)
	}
	if r.window() != 0x12345678 || r.pos != 4 || r.avail != 32 {
		t.Fatalf("after consume(64): window = %#x pos %d avail %d", r.window(), r.pos, r.avail)
	}
	if err := r.consume(31); err != nil {
		t.Fatal(err)
	}
	// Now the word array is exhausted: the next refill fails without a panic.
	if err := r.consume(1); !errors.Is(err, errASpeedTruncated) {
		t.Fatalf("consume past end: err = %v", err)
	}
	if err := r.consume(0); err == nil {
		t.Fatal("consume(0) must be rejected")
	}

	if _, err := newASpeedBitReader(make([]byte, 7)); err == nil {
		t.Fatal("7 bytes must be rejected (two words needed)")
	}

	// receiveExtend: 3-bit "010" = 2 -> -5, "110" = 6 -> +6.
	var w aspeedBitWriter
	w.put(0b010, 3)
	w.put(0b110, 3)
	r, _ = newASpeedBitReader(w.bytes())
	if v, err := r.receiveExtend(3); err != nil || v != -5 {
		t.Fatalf("receiveExtend = %d, %v", v, err)
	}
	if v, err := r.receiveExtend(3); err != nil || v != 6 {
		t.Fatalf("receiveExtend = %d, %v", v, err)
	}
	if _, err := r.receiveExtend(16); err == nil {
		t.Fatal("receiveExtend(16) must be rejected")
	}
}

// ---- Huffman ----------------------------------------------------------------------

func TestASpeedHuffmanTablesConsistent(t *testing.T) {
	initASpeedTables()
	cases := []struct {
		name string
		bits [17]uint8
		vals []uint8
	}{
		{"DC luma", aspeedDCLumaBits, aspeedDCLumaVals},
		{"DC chroma", aspeedDCChromaBits, aspeedDCChromaVals},
		{"AC luma", aspeedACLumaBits, aspeedACLumaVals},
		{"AC chroma", aspeedACChromaBits, aspeedACChromaVals},
	}
	for _, c := range cases {
		n := 0
		for l := 1; l <= 16; l++ {
			n += int(c.bits[l])
		}
		if n != len(c.vals) {
			t.Errorf("%s: %d codes but %d values", c.name, n, len(c.vals))
		}
	}
	// Standard JPEG codes: DC luma category 0 = "00", category 2 = "011";
	// AC luma EOB (0x00) = "1010", 0x01 = "00".
	if c, l := huffCode(&aspeedDCTables[0], 0); c != 0b00 || l != 2 {
		t.Errorf("DC luma 0: %b/%d", c, l)
	}
	if c, l := huffCode(&aspeedDCTables[0], 2); c != 0b011 || l != 3 {
		t.Errorf("DC luma 2: %b/%d", c, l)
	}
	if c, l := huffCode(&aspeedACTables[0], 0x00); c != 0b1010 || l != 4 {
		t.Errorf("AC luma EOB: %b/%d", c, l)
	}
	if c, l := huffCode(&aspeedACTables[0], 0xF0); l != 11 {
		t.Errorf("AC luma ZRL: %b/%d", c, l)
	}
}

func TestASpeedHuffmanDecodeBlock(t *testing.T) {
	d, _ := newTestASpeed(t, 8, 8)
	var w aspeedBitWriter
	// Block 1: DC +2 ("011" + "10"), AC run0/size1 ("00" + "1" = +1) at
	// zig-zag 1, AC run0/size2 ("01" + "01" = -2) at zig-zag 2, then
	// ZRL (16 zeros) and run 3 / size 1 (+1) at zig-zag 3+16+3 = 22, EOB.
	w.put(0b011, 3)
	w.put(0b10, 2)
	w.put(0b00, 2)
	w.put(1, 1)
	w.put(0b01, 2)
	w.put(0b01, 2)
	w.putSym(&aspeedACTables[0], 0xF0)
	w.putSym(&aspeedACTables[0], 0x31)
	w.put(1, 1)
	w.putSym(&aspeedACTables[0], 0x00)
	// Block 2: DC category 0 (predictor carries), EOB.
	w.put(0b00, 2)
	w.putSym(&aspeedACTables[0], 0x00)

	r, err := newASpeedBitReader(w.bytes())
	if err != nil {
		t.Fatal(err)
	}
	var pred int16
	if err := d.decodeCoefBlock(r, &aspeedDCTables[0], &aspeedACTables[0], &pred); err != nil {
		t.Fatal(err)
	}
	natural := func(zz int) int {
		for i, z := range aspeedZigzag {
			if int(z) == zz {
				return i
			}
		}
		t.Fatalf("zigzag %d not found", zz)
		return 0
	}
	want := map[int]int16{0: 2, natural(1): 1, natural(2): -2, natural(22): 1}
	for i := 0; i < 64; i++ {
		if d.coef[i] != want[i] {
			t.Errorf("block 1 coef[%d] = %d want %d", i, d.coef[i], want[i])
		}
	}
	if natural(1) != 1 || natural(2) != 8 {
		t.Errorf("zigzag table: natural(1)=%d natural(2)=%d", natural(1), natural(2))
	}
	if pred != 2 {
		t.Fatalf("pred = %d", pred)
	}
	if err := d.decodeCoefBlock(r, &aspeedDCTables[0], &aspeedACTables[0], &pred); err != nil {
		t.Fatal(err)
	}
	if d.coef[0] != 2 || d.coef[1] != 0 || pred != 2 {
		t.Fatalf("block 2: coef[0]=%d coef[1]=%d pred=%d", d.coef[0], d.coef[1], pred)
	}

	// All-ones is not a code in any table.
	r, _ = newASpeedBitReader(bytes.Repeat([]byte{0xFF}, 16))
	if _, err := aspeedDCTables[0].decode(r); err == nil {
		t.Fatal("all-ones must not decode")
	}
	var p int16
	if err := d.decodeCoefBlock(r, &aspeedDCTables[0], &aspeedACTables[0], &p); err == nil {
		t.Fatal("garbage block must fail")
	}
}

// ---- IDCT ---------------------------------------------------------------------------

func TestASpeedIDCTDCOnly(t *testing.T) {
	initASpeedTables()
	var q [64]float32
	aspeedBuildQuant(&aspeedQuantLuma[7], aspeedQuantScale, &q) // q[0] = 2
	if q[0] != 2 {
		t.Fatalf("q[0] = %v", q[0])
	}
	// AAN scaling at [1] = 1 * 1.3870399 (table value 1 -> clamped to 1).
	if q[1] != 1*aspeedAANScale[0]*aspeedAANScale[1] {
		t.Fatalf("q[1] = %v", q[1])
	}
	cases := []struct {
		dc   int16
		want uint8
	}{
		{0, 128},    // level shift only
		{40, 138},   // (40*2)>>3 = 10
		{-200, 78},  // (-400)>>3 = -50
		{2000, 255}, // saturates high
		{-2000, 0},  // saturates low
	}
	for _, c := range cases {
		var in [64]int16
		var ws [64]int32
		var out [64]uint8
		in[0] = c.dc
		aspeedIDCT(&in, &q, &ws, &out)
		for i, v := range out {
			if v != c.want {
				t.Fatalf("dc %d: out[%d] = %d want %d", c.dc, i, v, c.want)
			}
		}
	}
	// A non-DC coefficient must break the uniformity (exercises the full
	// butterfly path) and stay in range.
	var in [64]int16
	var ws [64]int32
	var out [64]uint8
	in[0], in[1], in[8] = 40, 30, -20
	aspeedIDCT(&in, &q, &ws, &out)
	if out[0] == out[7] && out[0] == out[56] {
		t.Fatalf("AC coefficients had no effect: %v", out)
	}
}

// ---- colour ---------------------------------------------------------------------

func TestASpeedYUV(t *testing.T) {
	initASpeedTables()
	if c := aspeedYUV(255, 128, 128); c != 0xFFFFFFFF {
		t.Errorf("white = %#x", c)
	}
	if c := aspeedYUV(0, 128, 128); c != 0xFF000000 {
		t.Errorf("black = %#x", c)
	}
	if c := aspeedYUV(128, 128, 128); c != 0xFF828282 {
		t.Errorf("grey = %#x", c)
	}
	// Pure red in BT.601 studio range is roughly Y=81, Cb=90, Cr=240.
	c := aspeedYUV(81, 90, 240)
	r, g, b := c>>16&0xFF, c>>8&0xFF, c&0xFF
	if r < 230 || g > 20 || b > 20 {
		t.Errorf("red = %#x", c)
	}
}

// ---- VQ blocks ------------------------------------------------------------------

func TestASpeedVQBlockPlacement(t *testing.T) {
	d, fb := newTestASpeed(t, 32, 32)
	var dirty image.Rectangle
	fb.Subscribe(func(r image.Rectangle) { dirty = dirty.Union(r) })

	white := aspeedYUV(0xFF, 0x80, 0x80)
	black := aspeedYUV(0x00, 0x80, 0x80)
	grey := aspeedYUV(0x80, 0x80, 0x80)

	var w aspeedBitWriter
	// Block A (0,0): VQ 1 colour, load slot 0 = white.
	w.put(aspeedCodeVQ1, aspeedCodeBits)
	w.put(1, 1)
	w.put(0, 2)
	w.put(0xFF8080, 24)
	// Block B at (1,1) via skip: VQ 2 colours, entry 0 reuses slot 0,
	// entry 1 loads slot 1 = black, then a 1-bit checkerboard.
	w.put(aspeedCodeVQ2Skip, aspeedCodeBits)
	w.put(1, 8)
	w.put(1, 8)
	w.put(0, 1)
	w.put(0, 2)
	w.put(1, 1)
	w.put(1, 2)
	w.put(0x008080, 24)
	for k := 0; k < 64; k++ {
		w.put(uint32((k+k/8)%2), 1)
	}
	// Block C: no skip -> advances to (2,1): VQ 1 colour, slot 2 = grey.
	w.put(aspeedCodeVQ1, aspeedCodeBits)
	w.put(1, 1)
	w.put(2, 2)
	w.put(0x808080, 24)
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)

	first := aspeedPayload(0, 32, 32, 1, 0x00, 0, w.bytes())
	if frame, err := d.handle(first); frame || err != nil {
		t.Fatalf("first fragment: frame=%v err=%v", frame, err)
	}
	// The frame is decoded when the next first-fragment arrives.
	var end aspeedBitWriter
	end.put(aspeedCodeFrameEnd, aspeedCodeBits)
	if frame, err := d.handle(aspeedPayload(0, 32, 32, 1, 0, 0, end.bytes())); !frame || err != nil {
		t.Fatalf("second first-fragment: frame=%v err=%v", frame, err)
	}

	checks := []struct {
		x, y int
		want uint32
	}{
		{0, 0, white}, {7, 7, white}, {3, 4, white},
		{8, 8, white}, {9, 8, black}, {8, 9, black}, {9, 9, white}, {15, 15, white}, {14, 15, black},
		{16, 8, grey}, {23, 15, grey},
		{16, 0, 0xFF000000}, {0, 16, 0xFF000000}, {24, 8, 0xFF000000},
	}
	for _, c := range checks {
		if got := pxARGB(t, fb, c.x, c.y); got != c.want {
			t.Errorf("pixel (%d,%d) = %#x want %#x", c.x, c.y, got, c.want)
		}
	}
	if want := image.Rect(0, 0, 24, 16); dirty != want {
		t.Errorf("dirty = %v want %v", dirty, want)
	}
	s := d.stats
	if s.Frames != 1 || s.Blocks != 3 || s.VQBlocks != 3 || s.SkipCodes != 1 || s.Errors != 0 {
		t.Errorf("stats = %+v", s)
	}
	if fb.Frames() != 1 {
		t.Errorf("fb frames = %d", fb.Frames())
	}
	if d.blockX != 3 || d.blockY != 1 {
		t.Errorf("cursor after frame = (%d,%d)", d.blockX, d.blockY)
	}
}

func TestASpeedVQIn420ModeIsSkipped(t *testing.T) {
	d, fb := newTestASpeed(t, 32, 32)
	var w aspeedBitWriter
	w.put(aspeedCodeVQ1, aspeedCodeBits)
	w.put(1, 1)
	w.put(0, 2)
	w.put(0xFF8080, 24)
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(1, 32, 32, 1, 0, 0, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	if got := pxARGB(t, fb, 0, 0); got != 0xFF000000 {
		t.Errorf("pixel drawn in 4:2:0 VQ mode: %#x", got)
	}
	if d.stats.Blocks != 1 || d.stats.Frames != 1 {
		t.Errorf("stats = %+v", d.stats)
	}
}

func mustParseASpeed(t *testing.T, payload []byte) *aspeedPacket {
	t.Helper()
	p, err := parseASpeedPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- JPEG blocks ----------------------------------------------------------------------

func TestASpeedJPEGBlocks444And420(t *testing.T) {
	// 4:4:4, Y table 7 (q0 = 2), UV table 7: DC +40 -> Y sample 138.
	d, fb := newTestASpeed(t, 16, 16)
	var w aspeedBitWriter
	dcOnlyJPEGBlock(&w, []int{40})
	// Second block without skip lands at (1,0) and carries DC diff -40
	// (predictor back to 0 -> sample 128).
	dcOnlyJPEGBlock(&w, []int{-40})
	// Low-quality skip code to (0,1) with DC +40 again (quant table 0:
	// q0 = 20 -> 800>>3 = 100 -> 228).
	w.put(aspeedCodeLowJPEGSkip, aspeedCodeBits)
	w.put(0, 8)
	w.put(1, 8)
	w.putDC(&aspeedDCTables[0], 40)
	w.putSym(&aspeedACTables[0], 0x00)
	for i := 0; i < 2; i++ {
		w.putDC(&aspeedDCTables[1], 0)
		w.putSym(&aspeedACTables[1], 0x00)
	}
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 16, 16, 1, 0x77, 0, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	if got, want := pxARGB(t, fb, 0, 0), aspeedYUV(138, 128, 128); got != want {
		t.Errorf("(0,0) = %#x want %#x", got, want)
	}
	if got, want := pxARGB(t, fb, 7, 7), aspeedYUV(138, 128, 128); got != want {
		t.Errorf("(7,7) = %#x want %#x", got, want)
	}
	if got, want := pxARGB(t, fb, 8, 0), aspeedYUV(128, 128, 128); got != want {
		t.Errorf("(8,0) = %#x want %#x", got, want)
	}
	if got, want := pxARGB(t, fb, 0, 8), aspeedYUV(228, 128, 128); got != want {
		t.Errorf("(0,8) = %#x want %#x", got, want)
	}
	if got := pxARGB(t, fb, 8, 8); got != 0xFF000000 {
		t.Errorf("(8,8) = %#x want untouched", got)
	}
	if d.stats.JPEGBlocks != 3 || d.stats.SkipCodes != 1 {
		t.Errorf("stats = %+v", d.stats)
	}

	// 4:2:0: one 16x16 macroblock, Y quadrants 138, 128, 128, 138.
	d, fb = newTestASpeed(t, 16, 16)
	w = aspeedBitWriter{}
	dcOnlyJPEGBlock(&w, []int{40, -40, 0, 40})
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(1, 16, 16, 1, 0x77, 0, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	bright, mid := aspeedYUV(138, 128, 128), aspeedYUV(128, 128, 128)
	for _, c := range []struct {
		x, y int
		want uint32
	}{{0, 0, bright}, {7, 7, bright}, {8, 0, mid}, {15, 7, mid}, {0, 8, mid}, {7, 15, mid}, {8, 8, bright}, {15, 15, bright}} {
		if got := pxARGB(t, fb, c.x, c.y); got != c.want {
			t.Errorf("4:2:0 (%d,%d) = %#x want %#x", c.x, c.y, got, c.want)
		}
	}
	if !d.mode420 || d.padHeight != 16 {
		t.Errorf("mode420=%v padHeight=%d", d.mode420, d.padHeight)
	}
}

// ---- header, resize, cursor -------------------------------------------------------

func TestASpeedParsePacket(t *testing.T) {
	payload := []byte{
		0x01,             // mode: 4:2:0
		0x12, 0x34, 0x56, // cursor x = 0x123, y = 0x456
		0x02, 0x58, // height 600
		0x03, 0x20, // width 800
		0x11,       // flags: first + pad
		0xF7, 0xB9, // hot x = 0x3D, hot y = 0x3B, cursor id 9
		0x53,       // Y table 5, UV table 3
		1, 2, 3, 4, // data
	}
	p, err := parseASpeedPacket(payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.mode != 1 || p.width != 800 || p.height != 600 || !p.first || !p.padTo16 ||
		p.cursorX != 0x123 || p.cursorY != 0x456 || p.hotX != 0x3D || p.hotY != 0x3B || p.cursorID != 9 ||
		p.yTable != 5 || p.uvTable != 3 || len(p.data) != 4 {
		t.Fatalf("parsed %+v", *p)
	}
	if _, err := parseASpeedPacket(payload[:11]); err == nil {
		t.Fatal("short payload must fail")
	}

	// 600 lines in 4:2:0 pad to 608 (d.a(int,int)); otherwise flag 0x10
	// rounds up to a multiple of 16.
	d, fb := newTestASpeed(t, 8, 8)
	var w aspeedBitWriter
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(1, 800, 600, 0x01, 0, 0, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	if fw, fh := fb.Size(); fw != 800 || fh != 600 || d.padHeight != 608 || d.stats.Resizes != 1 {
		t.Errorf("size %dx%d padHeight %d resizes %d", fw, fh, d.padHeight, d.stats.Resizes)
	}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 1000, 700, 0x11, 0, 0, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	if fw, fh := fb.Size(); fw != 1000 || fh != 700 || d.padHeight != 704 {
		t.Errorf("size %dx%d padHeight %d", fw, fh, d.padHeight)
	}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 1000, 700, 0x01, 0, 0, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	if d.stats.Resizes != 2 {
		t.Errorf("unchanged size caused a resize: %d", d.stats.Resizes)
	}
	// Cursor placement is carried by every frame; id 0 hides it.
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 1000, 700, 0x01, 0, 0x0C35, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	if !d.cursor.visible || d.cursor.id != 5 || d.cursor.hotX != 3 || d.cursor.hotY != 3 {
		t.Errorf("cursor = %+v", d.cursor)
	}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 1000, 700, 0x01, 0, 0, w.bytes()))); err != nil {
		t.Fatal(err)
	}
	if d.cursor.visible {
		t.Error("cursor still visible")
	}
}

func TestASpeedCursorImage(t *testing.T) {
	d, _ := newTestASpeed(t, 8, 8)
	// 2x1 ARGB4444 cursor, id 3: opaque red, half-transparent green.
	pix := []byte{0x00, 0xFF, 0xF0, 0x80}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(3, 2, 1, 1, 0, 3, pix))); err != nil {
		t.Fatal(err)
	}
	img := d.cursors[3]
	if img == nil || img.w != 2 || img.h != 1 {
		t.Fatalf("cursor image = %+v", img)
	}
	if img.pix[0] != 0xFFFF0000 || img.pix[1] != 0x8800FF00 {
		t.Errorf("ARGB4444 pixels = %#x %#x", img.pix[0], img.pix[1])
	}
	// XOR cursor: opaque, transparent, inverted.
	pix = []byte{0xF0, 0x0F, 0x00, 0x80, 0x00, 0xC0}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(2, 3, 1, 1, 0, 4, pix))); err != nil {
		t.Fatal(err)
	}
	img = d.cursors[4]
	if img.pix[0] != 0xFFFFFF00 || img.pix[1] != 0 || img.pix[2]>>24 != 0xFF || img.pix[2] == 0xFF000000 {
		t.Errorf("XOR pixels = %#x %#x %#x", img.pix[0], img.pix[1], img.pix[2])
	}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(3, 4, 4, 1, 0, 1, pix))); err == nil {
		t.Error("undersized cursor data must fail")
	}
	if d.stats.CursorPackets != 2 {
		t.Errorf("CursorPackets = %d", d.stats.CursorPackets)
	}
}

// ---- robustness ------------------------------------------------------------------

func TestASpeedTruncatedAndGarbage(t *testing.T) {
	d, fb := newTestASpeed(t, 32, 32)

	// Short payload.
	if _, err := d.handle(make([]byte, 5)); err == nil {
		t.Error("short payload accepted")
	}
	// Continuation without a frame start is dropped silently.
	if frame, err := d.handle(aspeedPayload(0, 32, 32, 0, 0, 0, make([]byte, 20))); frame || err != nil {
		t.Errorf("stray continuation: frame=%v err=%v", frame, err)
	}
	if d.stats.Dropped != 1 {
		t.Errorf("Dropped = %d", d.stats.Dropped)
	}

	// A JPEG block with all-zero data never reaches a frame end: the AC
	// loop eats "00"+"0" triples until the word array is exhausted.
	err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 32, 32, 1, 0, 0, make([]byte, 8))))
	if !errors.Is(err, errASpeedTruncated) {
		t.Errorf("zero data: err = %v", err)
	}
	// Frame data shorter than two words.
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 32, 32, 1, 0, 0, make([]byte, 7)))); err == nil {
		t.Error("7-byte frame accepted")
	}
	// Unknown block code 1.
	var w aspeedBitWriter
	w.put(1, aspeedCodeBits)
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 32, 32, 1, 0, 0, w.bytes()))); err == nil {
		t.Error("unknown block code accepted")
	}
	// Bad sizes and table selectors.
	w = aspeedBitWriter{}
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 0, 32, 1, 0, 0, w.bytes()))); err == nil {
		t.Error("zero width accepted")
	}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 5000, 32, 1, 0, 0, w.bytes()))); err == nil {
		t.Error("5000 width accepted")
	}
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 32, 32, 1, 0x9F, 0, w.bytes()))); err == nil {
		t.Error("table selector 9 accepted")
	}
	// Oversized but "valid" sizes are decoded without resizing (c.i.a).
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 2100, 2100, 1, 0, 0, w.bytes()))); err != nil {
		t.Errorf("2100x2100: %v", err)
	}
	if fw, fh := fb.Size(); fw != 32 || fh != 32 {
		t.Errorf("framebuffer resized to %dx%d", fw, fh)
	}
	// Skip code pointing far outside the image is clipped, not a panic.
	w = aspeedBitWriter{}
	w.put(aspeedCodeVQ1Skip, aspeedCodeBits)
	w.put(255, 8)
	w.put(255, 8)
	w.put(0, 3)
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)
	if err := d.decodeFrame(mustParseASpeed(t, aspeedPayload(0, 32, 32, 1, 0, 0, w.bytes()))); err != nil {
		t.Errorf("out-of-image block: %v", err)
	}

	// Random garbage: must never panic, errors are fine.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 400; i++ {
		n := 12 + rng.Intn(300)
		payload := make([]byte, n)
		rng.Read(payload)
		payload[0] = byte(rng.Intn(4))
		wd, ht := 8+rng.Intn(120), 8+rng.Intn(120)
		payload[4], payload[5] = byte(ht>>8), byte(ht)
		payload[6], payload[7] = byte(wd>>8), byte(wd)
		payload[8] |= 1
		payload[11] &= 0x77
		_, _ = d.handle(payload)
	}
	if d.stats.Packets < 400 {
		t.Errorf("stats = %+v", d.stats)
	}
}

// ---- VideoStream wiring ----------------------------------------------------------

func TestASpeedVideoStreamWiring(t *testing.T) {
	white := aspeedYUV(0xFF, 0x80, 0x80)
	var w aspeedBitWriter
	w.put(aspeedCodeVQ1, aspeedCodeBits)
	w.put(1, 1)
	w.put(0, 2)
	w.put(0xFF8080, 24)
	w.put(aspeedCodeFrameEnd, aspeedCodeBits)
	data := w.bytes()
	var end aspeedBitWriter
	end.put(aspeedCodeFrameEnd, aspeedCodeBits)

	// Frame A split across a first fragment and a continuation, then the
	// first fragment of frame B which triggers A's decode.
	var stream []byte
	stream = append(stream, vidPkt(vidTypeASpeedJPEG, aspeedPayload(0, 24, 16, 1, 0, 0, data[:5]))...)
	stream = append(stream, vidPkt(vidTypeASpeedJPEG, aspeedPayload(0, 24, 16, 0, 0, 0, data[5:]))...)
	stream = append(stream, vidPkt(vidTypeASpeedJPEG, aspeedPayload(0, 24, 16, 1, 0, 0, end.bytes()))...)

	fb := NewFramebuffer()
	v, rw, err := runStream(t, fb, stream, func(v *VideoStream) { v.AckInterval = 2 })
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	s := v.Stats()
	if s.ASpeedPackets != 3 || s.ASpeedFrames != 1 || s.ASpeedErrors != 0 || s.DecodeErrors != 0 {
		t.Errorf("stats = %+v", s)
	}
	if s.ASpeed.Fragments != 1 || s.ASpeed.Frames != 1 || s.ASpeed.Resizes != 1 {
		t.Errorf("aspeed stats = %+v", s.ASpeed)
	}
	if fw, fh := fb.Size(); fw != 24 || fh != 16 {
		t.Errorf("size %dx%d", fw, fh)
	}
	if got := pxARGB(t, fb, 3, 3); got != white {
		t.Errorf("pixel = %#x want %#x", got, white)
	}
	if got := pxARGB(t, fb, 12, 3); got != 0xFF000000 {
		t.Errorf("pixel outside block = %#x", got)
	}
	// The packets count towards the Video Ack like every other video type.
	if s.AcksSent != 1 || !bytes.Contains(rw.w.Bytes(), EncodeVideoAck(2)) {
		t.Errorf("acks = %d, wrote %x", s.AcksSent, rw.w.Bytes())
	}
	// flush() decodes the pending frame B on demand.
	if frame, err := v.aspeed.flush(); !frame || err != nil {
		t.Errorf("flush: frame=%v err=%v", frame, err)
	}
	if v.aspeed.stats.Frames != 2 {
		t.Errorf("frames after flush = %d", v.aspeed.stats.Frames)
	}
}
