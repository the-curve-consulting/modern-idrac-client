package kvm

import (
	"errors"
	"fmt"
	"image"
	"sync"
)

// ASpeed JPEG video decoder (packet type 0x86, "ASpeed JPEG Video"), ported
// from the viewer classes
//
//	com.avocent.kvm.b.a.gb      (iDRAC6) / com.avocent.kvm.a.c.g (iDRAC8)   packet
//	com.avocent.kvm.b.n.a(gb)   (iDRAC6) / com.avocent.d.g.a.b.a(g)         reassembly
//	com.avocent.kvm.a.a.a       (iDRAC6) / com.avocent.kvm.a.c.e            block loop ("d()" / "b()")
//	com.avocent.kvm.a.a.d       (iDRAC6) / com.avocent.kvm.a.c.b            "BishaDecode": bit reader,
//	                                                                        Huffman, IDCT, colour conversion
//	com.avocent.kvm.a.a.e/f     (iDRAC6) / com.avocent.kvm.a.c.c/d          VQ palette, Huffman table
//	com.avocent.kvm.a.a.b       (iDRAC6) / com.avocent.kvm.a.c.a            cursor bitmap (mode 2/3)
//
// The iDRAC8 codec classes are the iDRAC6 ones renamed; the logic is
// identical, so Java references below use the iDRAC6 names.
//
// Payload layout (after the 8-byte video header), gb.a(byte[],byte[],int):
//
//	0      mode: bit0 = 0 -> YUV 4:4:4, 8x8 blocks; 1 -> YUV 4:2:0, 16x16 blocks.
//	       Values 2 and 3 carry a hardware cursor bitmap instead of video.
//	1..3   24 bits: cursor x (high 12) / cursor y (low 12)
//	4..5   u16be height
//	6..7   u16be width
//	8      flags: bit0 first fragment of a frame; bit4 pad height to a
//	       multiple of 16 blocks; bits 1..3 parsed but unused by the viewer
//	9..10  u16be: hotspot x (bits 15..10) / hotspot y (bits 9..4) / cursor id (bits 3..0)
//	11     high nibble: luminance quantisation table (0..7); low nibble: chrominance table
//	12..   compressed data (little-endian 32-bit words, bits consumed MSB first)
//
// Fragments of a frame are appended to the first one; the frame is decoded
// when the NEXT first-fragment packet arrives (n.a(gb)), so the picture is
// always one frame behind the wire. There is no other frame delimiter.
//
// Block grammar (a.d()): each block starts with a 4-bit code read from the
// top of the 32-bit window:
//
//	0  JPEG                    1 (4:4:4) or 4 (4:2:0) Y blocks, then Cb, Cr, Huffman coded
//	4  LOW_JPEG                same, quantised with the fixed coarse tables (index 0)
//	5  VQ_1_COLOR              one palette entry, 64 pixels, no index bits
//	6  VQ_2_COLOR              two palette entries, 64 x 1-bit indices
//	7  VQ_4_COLOR              four palette entries, 64 x 2-bit indices
//	8,12,13,14,15              the "_SKIP" variants of 0,4,5,6,7: the code is followed
//	                           by 8 bits block column and 8 bits block row (20 bits in all)
//	9  FRAME_END               stop
//
// Non-skip blocks go to the block after the previous one (row-major over
// width/bs columns and padHeight/bs rows). A VQ palette entry is 3 bits
// (bit 31 = "load new colour", bits 30..29 = palette slot) optionally
// followed by 24 bits of Y,Cb,Cr; the palette persists within a frame.

// Block codes (constants d.p/q/r/s/t/u/v/w/x/y/z and d.h).
const (
	aspeedCodeJPEG        = 0  // JPEG_NO_SKIP_CODE
	aspeedCodeLowJPEG     = 4  // LOW_JPEG_NO_SKIP_CODE
	aspeedCodeVQ1         = 5  // VQ_NO_SKIP_1_COLOR_CODE
	aspeedCodeVQ2         = 6  // VQ_NO_SKIP_2_COLOR_CODE
	aspeedCodeVQ4         = 7  // VQ_NO_SKIP_4_COLOR_CODE
	aspeedCodeJPEGSkip    = 8  // JPEG_SKIP_CODE
	aspeedCodeFrameEnd    = 9  // frame end (d.t)
	aspeedCodeLowJPEGSkip = 12 // LOW_JPEG_SKIP_CODE
	aspeedCodeVQ1Skip     = 13 // VQ_SKIP_1_COLOR_CODE
	aspeedCodeVQ2Skip     = 14 // VQ_SKIP_2_COLOR_CODE
	aspeedCodeVQ4Skip     = 15 // VQ_SKIP_4_COLOR_CODE

	aspeedCodeBits     = 4  // d.A
	aspeedSkipBits     = 20 // d.B: code + column + row
	aspeedVQReuseBits  = 3  // d.n: flag + slot
	aspeedVQColourBits = 27 // d.m: flag + slot + 24-bit colour
	aspeedQuantScale   = 16 // d.S..V: fixed "quality" divisor, never changed by the viewer
)

// Cursor bitmap modes (payload[0] values handled by class b).
const (
	aspeedModeCursorXOR  = 2 // 16-bit: bit15 = transparent, bit14 = inverted, RGB444
	aspeedModeCursorARGB = 3 // 16-bit ARGB4444
)

// ---- packet ---------------------------------------------------------------

// aspeedPacket is a parsed 0x86 payload (com.avocent.kvm.b.a.gb).
type aspeedPacket struct {
	mode             int  // payload[0]
	width, height    int  // payload[6:8], payload[4:6]
	flags            byte // payload[8]
	first            bool // flags bit0 (gb.r() / g.j())
	padTo16          bool // flags bit4 (gb.s() / g.v())
	yTable, uvTable  int  // payload[11] >> 4, payload[11] & 0xF
	cursorX, cursorY int  // payload[1:4] high/low 12 bits
	hotX, hotY       int  // payload[9:11] bits 15..10 / 9..4
	cursorID         int  // payload[9:11] bits 3..0
	data             []byte
}

// parseASpeedPacket mirrors gb.a(byte[],byte[],int).
func parseASpeedPacket(payload []byte) (*aspeedPacket, error) {
	if len(payload) < 12 {
		return nil, fmt.Errorf("aspeed: video packet payload too short (%d bytes)", len(payload))
	}
	p := &aspeedPacket{
		mode:   int(payload[0]),
		height: int(be16(payload[4:])),
		width:  int(be16(payload[6:])),
		flags:  payload[8],
		data:   payload[12:],
	}
	pos := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	p.cursorX = pos >> 12 & 0xFFF
	p.cursorY = pos & 0xFFF
	p.first = p.flags&1 != 0
	p.padTo16 = p.flags&0x10 != 0
	hs := int(be16(payload[9:]))
	p.hotX = hs >> 10 & 0x3F
	p.hotY = hs >> 4 & 0x3F
	p.cursorID = hs & 0xF
	p.yTable = int(payload[11] >> 4)
	p.uvTable = int(payload[11] & 0xF)
	return p, nil
}

// ---- bit reader (d.a(int), d.b(int), d.c(int), d.d(int)) -------------------

var errASpeedTruncated = errors.New("aspeed: bitstream ended before the frame-end code")

// aspeedBitReader is the viewer's two-word bit window: cur (d.nb) holds the
// next 32 bits MSB first, next (d.mb) the following word shifted so that its
// avail (d.ob) valid bits are at the top.
type aspeedBitReader struct {
	words []uint32 // d.ib: little-endian 32-bit words of the frame data
	pos   int      // d.hb: next word to fetch
	cur   uint32   // d.nb
	next  uint32   // d.mb
	avail int      // d.ob
}

// newASpeedBitReader loads data as little-endian words (d.a(byte[],int,int));
// a trailing partial word is dropped. The reader needs two whole words.
func newASpeedBitReader(data []byte) (*aspeedBitReader, error) {
	n := len(data) / 4
	if n < 2 {
		return nil, fmt.Errorf("aspeed: frame data too short (%d bytes)", len(data))
	}
	w := make([]uint32, n)
	for i := range w {
		w[i] = uint32(data[4*i]) | uint32(data[4*i+1])<<8 | uint32(data[4*i+2])<<16 | uint32(data[4*i+3])<<24
	}
	return &aspeedBitReader{words: w, pos: 2, cur: w[0], next: w[1], avail: 32}, nil
}

// window returns the 32-bit lookahead (d.nb) used for the block headers.
func (r *aspeedBitReader) window() uint32 { return r.cur }

// peek returns the next n (1..16) bits without consuming them (d.b).
func (r *aspeedBitReader) peek(n int) uint32 { return (r.cur >> uint(32-n)) & 0xFFFF }

// consume drops n (1..31) bits, refilling from the word array (d.a / d.c).
func (r *aspeedBitReader) consume(n int) error {
	if n <= 0 || n > 31 {
		return fmt.Errorf("aspeed: internal error: consume(%d)", n)
	}
	if r.avail-n <= 0 {
		if r.pos >= len(r.words) {
			return errASpeedTruncated
		}
		w := r.words[r.pos]
		r.pos++
		r.cur = r.cur<<uint(n) | (r.next|w>>uint(r.avail))>>uint(32-n)
		r.next = w << uint(n-r.avail)
		r.avail = 32 + r.avail - n
		return nil
	}
	r.cur = r.cur<<uint(n) | r.next>>uint(32-n)
	r.next <<= uint(n)
	r.avail -= n
	return nil
}

// receiveExtend reads an n-bit (1..15) magnitude and sign-extends it per the
// JPEG EXTEND procedure (d.d(int)).
func (r *aspeedBitReader) receiveExtend(n int) (int16, error) {
	if n < 1 || n > 15 {
		return 0, fmt.Errorf("aspeed: bad coefficient size %d", n)
	}
	v := int32(r.peek(n))
	if v&(1<<uint(n-1)) == 0 {
		v += int32(aspeedExtendOffset[n])
	}
	if err := r.consume(n); err != nil {
		return 0, err
	}
	return int16(v), nil
}

// ---- Huffman tables (class f, filled by d.a(f,char[],char[])) ---------------

type aspeedHuffTable struct {
	minCode [17]int        // f.b, per code length; 0xFFFF when no codes of that length
	maxCode [17]int        // f.c
	vals    [17][256]uint8 // f.d, indexed [length][code-minCode]
}

func newASpeedHuffTable(bits [17]uint8, vals []uint8) aspeedHuffTable {
	var t aspeedHuffTable
	k := 0
	for l := 1; l <= 16; l++ {
		for i := 0; i < int(bits[l]); i++ {
			if k < len(vals) && i < 256 {
				t.vals[l][i] = vals[k]
			}
			k++
		}
	}
	code := 0
	for l := 1; l <= 16; l++ {
		t.minCode[l] = code & 0xFFFF
		code += int(bits[l])
		t.maxCode[l] = (code - 1) & 0xFFFF
		code *= 2
		if bits[l] == 0 {
			t.minCode[l] = 0xFFFF
			t.maxCode[l] = 0
		}
	}
	return t
}

// decode reads one symbol: the first code length whose code range contains
// the peeked bits wins (the loop in d.a(int,int,short[])).
func (t *aspeedHuffTable) decode(r *aspeedBitReader) (uint8, error) {
	for l := 1; l <= 16; l++ {
		c := int(r.peek(l))
		if c <= t.maxCode[l] && c >= t.minCode[l] {
			if err := r.consume(l); err != nil {
				return 0, err
			}
			idx := c - t.minCode[l]
			if idx >= 256 {
				return 0, errors.New("aspeed: invalid Huffman code")
			}
			return t.vals[l][idx], nil
		}
	}
	return 0, errors.New("aspeed: invalid Huffman code")
}

// ---- static tables (d.a(), d.b(), d.c()) -----------------------------------

var (
	aspeedTablesOnce sync.Once
	aspeedDCTables   [2]aspeedHuffTable // d.tb[0] luma, d.tb[1] chroma
	aspeedACTables   [2]aspeedHuffTable // d.ub[0] luma, d.ub[1] chroma
	aspeedClamp      [1408]uint8        // d.qb: index 256+v -> clamp(v) with wrap-around regions
	aspeedCrToR      [256]int           // d.E
	aspeedCbToB      [256]int           // d.F
	aspeedCrToG      [256]int           // d.G (16.16 fixed point, unshifted)
	aspeedCbToG      [256]int           // d.H (16.16 fixed point incl. rounding term)
	aspeedYScale     [256]int           // d.I
)

func initASpeedTables() {
	aspeedTablesOnce.Do(func() {
		aspeedDCTables[0] = newASpeedHuffTable(aspeedDCLumaBits, aspeedDCLumaVals)
		aspeedDCTables[1] = newASpeedHuffTable(aspeedDCChromaBits, aspeedDCChromaVals)
		aspeedACTables[0] = newASpeedHuffTable(aspeedACLumaBits, aspeedACLumaVals)
		aspeedACTables[1] = newASpeedHuffTable(aspeedACChromaBits, aspeedACChromaVals)

		// d.a(): colour conversion tables. fix(d) = (int)(d*65536 + 0.5).
		fix := func(d float64) int { return int(d*65536 + 0.5) }
		for i, n := 0, -128; i < 256; i, n = i+1, n+1 {
			aspeedCrToR[i] = (fix(1.597656)*n + 32768) >> 16
			aspeedCbToB[i] = (fix(2.015625)*n + 32768) >> 16
			aspeedCrToG[i] = -fix(0.8125) * n
			aspeedCbToG[i] = -fix(0.390625)*n + 32768
		}
		for i, n := 0, -16; i < 256; i, n = i+1, n+1 {
			aspeedYScale[i] = (fix(1.164)*n + 32768) >> 16
		}

		// d.b(): the clamp table.
		for i := 0; i < 256; i++ {
			aspeedClamp[i] = 0
			aspeedClamp[256+i] = uint8(i)
		}
		for i := 256; i < 640; i++ {
			aspeedClamp[256+i] = 255
		}
		for i := 0; i < 384; i++ {
			aspeedClamp[256+640+i] = 0
		}
		for i := 0; i < 128; i++ {
			aspeedClamp[256+1024+i] = uint8(i)
		}
	})
}

// aspeedClampAt is the guarded lookup used by the colour conversion.
func aspeedClampAt(i int) uint32 {
	if i < 0 || i >= len(aspeedClamp) {
		return 0
	}
	return uint32(aspeedClamp[i])
}

// aspeedYUV converts one Y,Cb,Cr triple to 0xFFRRGGBB exactly like the inner
// loop of d.a(int,int,char[][],int[]).
func aspeedYUV(y, cb, cr uint8) uint32 {
	iy := aspeedYScale[y]
	b := aspeedClampAt(256 + iy + aspeedCbToB[cb])
	g := aspeedClampAt(256 + iy + ((aspeedCbToG[cb] + aspeedCrToG[cr]) >> 16))
	r := aspeedClampAt(256 + iy + aspeedCrToR[cr])
	return 0xFF000000 | r<<16 | g<<8 | b
}

// aspeedBuildQuant is d.a(char[],byte,char[]) followed by the AAN scaling of
// d.a(float[]): q*16/scale clamped to 1..255, in natural order, multiplied by
// the row and column scale factors.
func aspeedBuildQuant(src *[64]uint8, scale int, dst *[64]float32) {
	var zz [64]uint8
	for i := 0; i < 64; i++ {
		v := int(src[i]) * 16 / scale
		if v <= 0 {
			v = 1
		} else if v > 255 {
			v = 255
		}
		zz[aspeedZigzag[i]] = uint8(v)
	}
	for i := 0; i < 64; i++ {
		dst[i] = float32(zz[aspeedZigzag[i]])
	}
	n := 0
	for i := 0; i < 8; i++ {
		for j := 0; j < 8; j++ {
			dst[n] = dst[n] * (aspeedAANScale[i] * aspeedAANScale[j])
			n++
		}
	}
}

// ---- IDCT (d.a(short[],char[],char)) ----------------------------------------

// aspeedMul is d.b(int,int): fixed-point multiply with 8 fraction bits.
func aspeedMul(a, b int32) int32 { return a * b >> 8 }

// aspeedIDCT dequantises in (natural order) with q and writes the 8x8
// samples (level shifted, clamped) to out. ws is the column-pass workspace
// (d.pb). Integer arithmetic is int32 to match Java overflow behaviour.
func aspeedIDCT(in *[64]int16, q *[64]float32, ws *[64]int32, out *[64]uint8) {
	// Column pass.
	for c := 0; c < 8; c++ {
		if in[c+8]|in[c+16]|in[c+24]|in[c+32]|in[c+40]|in[c+48]|in[c+56] == 0 {
			dc := int32(float32(in[c]) * q[c])
			for i := 0; i < 8; i++ {
				ws[c+8*i] = dc
			}
			continue
		}
		x0 := int32(float32(in[c]) * q[c])
		x2 := int32(float32(in[c+16]) * q[c+16])
		x4 := int32(float32(in[c+32]) * q[c+32])
		x6 := int32(float32(in[c+48]) * q[c+48])
		x1 := int32(float32(in[c+8]) * q[c+8])
		x3 := int32(float32(in[c+24]) * q[c+24])
		x5 := int32(float32(in[c+40]) * q[c+40])
		x7 := int32(float32(in[c+56]) * q[c+56])
		var o [8]int32
		aspeedIDCT1D(x0, x1, x2, x3, x4, x5, x6, x7, &o)
		for i := 0; i < 8; i++ {
			ws[c+8*i] = o[i]
		}
	}
	// Row pass with output clamp: qb[(384 + (v >> 3)) & 0x3FF].
	for r := 0; r < 8; r++ {
		base := r * 8
		var o [8]int32
		aspeedIDCT1D(ws[base], ws[base+1], ws[base+2], ws[base+3], ws[base+4], ws[base+5], ws[base+6], ws[base+7], &o)
		for i := 0; i < 8; i++ {
			out[base+i] = aspeedClamp[(384+int(o[i]>>3))&0x3FF]
		}
	}
}

// aspeedIDCT1D is the butterfly shared by both passes of d.a(short[],...).
// Variable names follow the Java locals (n2..n18).
func aspeedIDCT1D(x0, x1, x2, x3, x4, x5, x6, x7 int32, o *[8]int32) {
	// Even part.
	n14 := x0 + x4
	n13 := x0 - x4
	n12 := x2 + x6
	n11 := aspeedMul(x2-x6, 362) - n12
	n18 := n14 + n12
	n15 := n14 - n12
	n17 := n13 + n11
	n16 := n13 - n11
	// Odd part.
	n6 := x5 + x3
	n5 := x5 - x3
	n4 := x1 + x7
	n3 := x1 - x7
	n7 := n4 + n6
	n13 = aspeedMul(n4-n6, 362)
	n2 := aspeedMul(n5+n3, 473)
	n14 = aspeedMul(n3, 277) - n2
	n11 = aspeedMul(n5, -669) + n2
	n8 := n11 - n7
	n9 := n13 - n8
	n10 := n14 + n9
	o[0] = n18 + n7
	o[7] = n18 - n7
	o[1] = n17 + n8
	o[6] = n17 - n8
	o[2] = n16 + n9
	o[5] = n16 - n9
	o[4] = n15 + n10
	o[3] = n15 - n10
}

// ---- decoder ----------------------------------------------------------------

// ASpeedStats are counters of the 0x86 decoder exposed through
// VideoStream.Stats.
type ASpeedStats struct {
	Packets       uint64 // 0x86 packets parsed
	Fragments     uint64 // continuation packets appended to a pending frame
	Dropped       uint64 // continuation packets with no frame start to append to
	Frames        uint64 // frames decoded to the frame-end code
	Blocks        uint64 // blocks decoded (JPEG + VQ)
	JPEGBlocks    uint64
	VQBlocks      uint64
	SkipCodes     uint64 // blocks that carried an explicit position
	Errors        uint64
	Resizes       uint64
	CursorPackets uint64 // mode 2/3 cursor bitmaps
}

// aspeedCursorImage is a decoded hardware cursor bitmap (class b).
type aspeedCursorImage struct {
	w, h int
	pix  []uint32 // 0xAARRGGBB
}

// aspeedCursorState is what the viewer passes to the model
// (k.c(boolean), k.a(Integer), k.a(int,int,int,int)) for every video frame.
type aspeedCursorState struct {
	visible          bool
	id               int
	x, y, hotX, hotY int
}

// aspeedDecoder decodes 0x86 frames into a Framebuffer. One instance holds
// the per-connection codec state that the Java keeps in static fields of d.
type aspeedDecoder struct {
	fb  *Framebuffer
	log Logger

	stats ASpeedStats

	pending *aspeedPacket // first fragment plus appended continuations (n.k)

	// Codec configuration (d.ab, d.W, d.X, d.C, d.bb/db, d.eb, d.cb).
	configured      bool
	mode420         bool
	yTable, uvTable int
	quant           [4][64]float32 // [0] Y, [1] UV, [2] low-quality Y, [3] low-quality UV
	sized           bool
	width, height   int
	padHeight       int // height rounded for the block-row wrap (d.cb)

	// Per-frame state.
	blockX, blockY        int       // d.jb, d.kb
	predY, predCb, predCr int16     // d.P, d.Q, d.R
	vqColours             [4]uint32 // e.a: 0x00YYCbCr
	vqIndex               [4]int    // e.b
	vqBits                int       // e.c: index bits per pixel (0, 1, 2)
	warnedVQ420           bool

	// Scratch buffers.
	coef   [64]int16
	ws     [64]int32
	planes [6][64]uint8 // d.Wb: Y (1 or 4 blocks), Cb, Cr
	pix    [256]uint32

	cursors map[int]*aspeedCursorImage
	cursor  aspeedCursorState
}

func newASpeedDecoder(fb *Framebuffer, log Logger) *aspeedDecoder {
	initASpeedTables()
	if log == nil {
		log = nopLogger{}
	}
	return &aspeedDecoder{fb: fb, log: log, yTable: -1, uvTable: -1, cursors: map[int]*aspeedCursorImage{}}
}

// handle processes one 0x86 payload (n.a(gb)). A first-fragment packet
// triggers the decode of the previously accumulated frame and starts a new
// one; any other packet is appended to the pending frame. frame reports
// whether a frame was decoded (successfully or not).
func (d *aspeedDecoder) handle(payload []byte) (frame bool, err error) {
	d.stats.Packets++
	p, err := parseASpeedPacket(payload)
	if err != nil {
		d.stats.Errors++
		return false, err
	}
	if !p.first {
		if d.pending == nil {
			d.stats.Dropped++
			return false, nil
		}
		d.pending.data = append(d.pending.data, p.data...)
		d.stats.Fragments++
		return false, nil
	}
	prev := d.pending
	p.data = append([]byte(nil), p.data...)
	d.pending = p
	if prev == nil {
		return false, nil
	}
	if err := d.decodeFrame(prev); err != nil {
		d.stats.Errors++
		return true, err
	}
	return true, nil
}

// flush decodes the pending frame immediately instead of waiting for the
// next first fragment. Not used by the Java viewer; offered for callers that
// know no further packets will arrive.
func (d *aspeedDecoder) flush() (frame bool, err error) {
	prev := d.pending
	if prev == nil {
		return false, nil
	}
	d.pending = nil
	if err := d.decodeFrame(prev); err != nil {
		d.stats.Errors++
		return true, err
	}
	return true, nil
}

// decodeFrame is a.a(int,int,int,int,int,byte[],int,int,int,int,int,int,boolean)
// followed by the a.d() loop until the frame-end code.
func (d *aspeedDecoder) decodeFrame(p *aspeedPacket) error {
	if p.mode == aspeedModeCursorXOR || p.mode == aspeedModeCursorARGB {
		return d.cursorImage(p)
	}
	// Cursor placement travels with every video frame.
	if p.cursorID > 0 {
		d.cursor = aspeedCursorState{visible: true, id: p.cursorID, x: p.cursorX, y: p.cursorY, hotX: p.hotX, hotY: p.hotY}
	} else {
		d.cursor.visible = false
	}

	mode420 := p.mode&1 == 1
	if !d.configured || d.mode420 != mode420 || d.yTable != p.yTable || d.uvTable != p.uvTable {
		if p.yTable > 7 || p.uvTable > 7 {
			// The Java switch has no case for 8..15 and would NPE.
			return fmt.Errorf("aspeed: unsupported quantisation table selector Y=%d UV=%d", p.yTable, p.uvTable)
		}
		d.configured = true
		d.mode420 = mode420
		d.yTable, d.uvTable = p.yTable, p.uvTable
		// d.d(): C[0] = luma[W], C[1] = chroma[X] (d.Z is always 0),
		// C[2] = luma[d.Y=0], C[3] = chroma[d.Y=0].
		aspeedBuildQuant(&aspeedQuantLuma[p.yTable], aspeedQuantScale, &d.quant[0])
		aspeedBuildQuant(&aspeedQuantChroma[p.uvTable], aspeedQuantScale, &d.quant[1])
		aspeedBuildQuant(&aspeedQuantLuma[0], aspeedQuantScale, &d.quant[2])
		aspeedBuildQuant(&aspeedQuantChroma[0], aspeedQuantScale, &d.quant[3])
		chroma := "4:4:4"
		if mode420 {
			chroma = "4:2:0"
		}
		d.log.Printf("aspeed: compression mode %d (YUV %s), Y table %d, UV table %d", p.mode&1, chroma, p.yTable, p.uvTable)
	}

	if !d.sized || d.width != p.width || d.height != p.height {
		if p.width <= 0 || p.height <= 0 || p.width > 4096 || p.height > 4096 {
			return fmt.Errorf("aspeed: bad video frame size: %d x %d", p.width, p.height)
		}
		// d.a(int,int): padded height for the block-row wrap.
		d.width, d.height = p.width, p.height
		d.padHeight = p.height
		if p.height == 600 && mode420 {
			d.padHeight = 608
		} else if p.padTo16 && p.height%16 != 0 {
			d.padHeight = p.height/16*16 + 16
		}
		d.sized = true
		d.log.Printf("aspeed: resolution %dx%d (padded height %d)", p.width, p.height, d.padHeight)
	}
	if w, h := d.fb.Size(); w != p.width || h != p.height {
		if p.width > 2000 || p.height > 2000 {
			// c.i.a(int,int) ignores such sizes; blocks outside are clipped.
			d.log.Printf("aspeed: ignoring invalid resolution change: %dx%d", p.width, p.height)
		} else {
			d.log.Printf("aspeed: video size change: new (%dx%d), old (%dx%d)", p.width, p.height, w, h)
			d.fb.Resize(p.width, p.height)
			d.stats.Resizes++
		}
	}

	r, err := newASpeedBitReader(p.data)
	if err != nil {
		return err
	}
	d.blockX, d.blockY = 0, 0
	d.predY, d.predCb, d.predCr = 0, 0, 0
	d.resetVQ()

	var derr error
	d.fb.modify(func(img *image.RGBA) image.Rectangle {
		dirty, err := d.decodeBlocks(img, r)
		derr = err
		return dirty
	})
	if derr != nil {
		return derr
	}
	d.stats.Frames++
	d.fb.endFrame()
	return nil
}

// resetVQ is e.a(): default palette black, white, grey, light grey (YCbCr).
func (d *aspeedDecoder) resetVQ() {
	d.vqIndex = [4]int{0, 1, 2, 3}
	d.vqColours = [4]uint32{0x008080, 0xFF8080, 0x808080, 0xC08080}
	d.vqBits = 0
}

// decodeBlocks runs the a.d() block loop over the frame, blitting each block
// into img, until the frame-end code. It returns the union of touched
// rectangles; on error the blocks decoded so far stay on screen.
func (d *aspeedDecoder) decodeBlocks(img *image.RGBA, r *aspeedBitReader) (image.Rectangle, error) {
	var dirty image.Rectangle
	bs := 8
	if d.mode420 {
		bs = 16
	}
	for {
		code := int(r.window() >> 28)
		draw := true
		var err error
		switch code {
		case aspeedCodeFrameEnd:
			return dirty, nil

		case aspeedCodeJPEG, aspeedCodeJPEGSkip, aspeedCodeLowJPEG, aspeedCodeLowJPEGSkip:
			if err = d.readPosition(r, code&8 != 0); err != nil {
				return dirty, err
			}
			tbl := 0
			if code&4 != 0 {
				tbl = 2
			}
			err = d.jpegBlock(r, tbl)
			d.stats.JPEGBlocks++

		case aspeedCodeVQ1, aspeedCodeVQ2, aspeedCodeVQ4, aspeedCodeVQ1Skip, aspeedCodeVQ2Skip, aspeedCodeVQ4Skip:
			if err = d.readPosition(r, code&8 != 0); err != nil {
				return dirty, err
			}
			d.vqBits = code&3 - 1 // 5/13 -> 0, 6/14 -> 1, 7/15 -> 2
			for i := 0; i < 1<<uint(d.vqBits); i++ {
				if err = d.readVQColour(r, i); err != nil {
					return dirty, err
				}
			}
			draw, err = d.vqBlock(r)
			d.stats.VQBlocks++

		default:
			// a.d() returns -1 -> IOException("Decoder error") in the
			// decoder thread (iDRAC8: "Got in error phase blockHeaderValue").
			return dirty, fmt.Errorf("aspeed: unknown block code %d at block (%d,%d)", code, d.blockX, d.blockY)
		}
		if err != nil {
			return dirty, err
		}
		d.stats.Blocks++
		if draw {
			dirty = dirty.Union(d.blit(img, bs))
		}
		d.advance(bs)
	}
}

// readPosition consumes the block header: for skip codes the 20-bit header
// also sets the block position (a.d(): jb = bits 27..20, kb = bits 19..12).
func (d *aspeedDecoder) readPosition(r *aspeedBitReader, skip bool) error {
	if !skip {
		return r.consume(aspeedCodeBits)
	}
	w := r.window()
	d.blockX = int(w >> 20 & 0xFF)
	d.blockY = int(w >> 12 & 0xFF)
	d.stats.SkipCodes++
	return r.consume(aspeedSkipBits)
}

// readVQColour reads palette entry i of a VQ block header (a.d()):
// bit 31 = load a new colour, bits 30..29 = palette slot, then optionally
// 24 bits of Y, Cb, Cr.
func (d *aspeedDecoder) readVQColour(r *aspeedBitReader, i int) error {
	w := r.window()
	d.vqIndex[i] = int(w >> 29 & 3)
	if w>>31&1 == 0 {
		return r.consume(aspeedVQReuseBits)
	}
	d.vqColours[d.vqIndex[i]] = w >> 5 & 0xFFFFFF
	return r.consume(aspeedVQColourBits)
}

// vqBlock is d.b(int,int,int[],char): 64 palette pixels, each selected by
// vqBits index bits (none for the 1-colour mode). It reports whether the
// block was drawn: in 4:2:0 mode the Java conversion fails (Xb has only 3
// planes for a 6-plane macroblock, ArrayIndexOutOfBounds caught and
// printed) and nothing is drawn, which is reproduced here.
func (d *aspeedDecoder) vqBlock(r *aspeedBitReader) (bool, error) {
	if d.vqBits == 0 {
		c := d.vqColours[d.vqIndex[0]]
		for k := 0; k < 64; k++ {
			d.planes[0][k] = uint8(c >> 16)
			d.planes[1][k] = uint8(c >> 8)
			d.planes[2][k] = uint8(c)
		}
	} else {
		for k := 0; k < 64; k++ {
			c := d.vqColours[d.vqIndex[r.peek(d.vqBits)]]
			d.planes[0][k] = uint8(c >> 16)
			d.planes[1][k] = uint8(c >> 8)
			d.planes[2][k] = uint8(c)
			if err := r.consume(d.vqBits); err != nil {
				return false, err
			}
		}
	}
	if d.mode420 {
		if !d.warnedVQ420 {
			d.warnedVQ420 = true
			d.log.Printf("aspeed: VQ block in 4:2:0 mode; the viewer cannot draw these, skipping (unverified)")
		}
		return false, nil
	}
	d.convert444()
	return true, nil
}

// jpegBlock is d.a(int,int,int[],char): Huffman decode + IDCT of the Y,
// Cb, Cr blocks of one macroblock, tbl selecting quant[tbl] for Y and
// quant[tbl+1] for Cb/Cr.
func (d *aspeedDecoder) jpegBlock(r *aspeedBitReader, tbl int) error {
	ny := 1
	if d.mode420 {
		ny = 4
	}
	for i := 0; i < ny; i++ {
		if err := d.decodeCoefBlock(r, &aspeedDCTables[0], &aspeedACTables[0], &d.predY); err != nil {
			return err
		}
		aspeedIDCT(&d.coef, &d.quant[tbl], &d.ws, &d.planes[i])
	}
	if err := d.decodeCoefBlock(r, &aspeedDCTables[1], &aspeedACTables[1], &d.predCb); err != nil {
		return err
	}
	aspeedIDCT(&d.coef, &d.quant[tbl+1], &d.ws, &d.planes[ny])
	if err := d.decodeCoefBlock(r, &aspeedDCTables[1], &aspeedACTables[1], &d.predCr); err != nil {
		return err
	}
	aspeedIDCT(&d.coef, &d.quant[tbl+1], &d.ws, &d.planes[ny+1])
	if d.mode420 {
		d.convert420()
	} else {
		d.convert444()
	}
	return nil
}

// decodeCoefBlock is d.a(int,int,short[]): one 8x8 block of Huffman coded
// coefficients into d.coef (natural order), updating the DC predictor.
func (d *aspeedDecoder) decodeCoefBlock(r *aspeedBitReader, dc, ac *aspeedHuffTable, pred *int16) error {
	var zz [64]int16 // d.Vb, zig-zag order
	sym, err := dc.decode(r)
	if err != nil {
		return err
	}
	if sym == 0 {
		zz[0] = *pred
	} else {
		diff, err := r.receiveExtend(int(sym))
		if err != nil {
			return err
		}
		zz[0] = *pred + diff
		*pred = zz[0]
	}
	k := 1
	for k <= 63 {
		sym, err := ac.decode(r)
		if err != nil {
			return err
		}
		size := int(sym & 0xF)
		run := int(sym >> 4)
		if size == 0 {
			if run == 0 {
				break // EOB
			}
			if run == 15 {
				k += 16 // ZRL
			}
			// Other size-0 symbols are ignored by the Java (it resumes
			// with the next symbol).
			continue
		}
		k += run
		if k >= 64 {
			// The Java ends the block here without reading the magnitude
			// bits; treated as corrupt data.
			return fmt.Errorf("aspeed: AC coefficient index %d out of range", k)
		}
		v, err := r.receiveExtend(size)
		if err != nil {
			return err
		}
		zz[k] = v
		k++
	}
	for i := 0; i < 64; i++ {
		d.coef[i] = zz[aspeedZigzag[i]]
	}
	return nil
}

// convert444 fills d.pix (8x8) from planes Y, Cb, Cr (d.a(..., ab == 0)).
func (d *aspeedDecoder) convert444() {
	for m := 0; m < 64; m++ {
		d.pix[m] = aspeedYUV(d.planes[0][m], d.planes[1][m], d.planes[2][m])
	}
}

// convert420 fills d.pix (16x16) from four Y blocks (top-left, top-right,
// bottom-left, bottom-right) and one Cb and Cr block (d.a(..., ab == 1)).
func (d *aspeedDecoder) convert420() {
	for row := 0; row < 16; row++ {
		for col := 0; col < 16; col++ {
			y := d.planes[(row/8)*2+col/8][(row%8)*8+col%8]
			ci := ((row >> 1) << 3) + (col >> 1)
			d.pix[row*16+col] = aspeedYUV(y, d.planes[4][ci], d.planes[5][ci])
		}
	}
}

// blit copies the current bs x bs block to its position, clipped to the
// image. (The Java k.b(int,int,int,int,int[]) copies rows out of a linear
// full-frame buffer and only drops rows that would run past its end; blocks
// beyond the right edge would wrap into the next row there.)
func (d *aspeedDecoder) blit(img *image.RGBA, bs int) image.Rectangle {
	x0, y0 := d.blockX*bs, d.blockY*bs
	rect := image.Rect(x0, y0, x0+bs, y0+bs).Intersect(img.Rect)
	if rect.Empty() {
		return image.Rectangle{}
	}
	W := img.Rect.Dx()
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		src := (y - y0) * bs
		for x := rect.Min.X; x < rect.Max.X; x++ {
			putARGB(img.Pix, y*W+x, d.pix[src+x-x0])
		}
	}
	return rect
}

// advance is d.e(): next block in row-major order over width/bs columns and
// padHeight/bs rows.
func (d *aspeedDecoder) advance(bs int) {
	d.blockX++
	if d.blockX >= d.width/bs {
		d.blockY++
		if d.blockY >= d.padHeight/bs {
			d.blockY = 0
		}
		d.blockX = 0
	}
}

// cursorImage decodes a mode 2/3 packet into a cursor bitmap (class b) and
// stores it under the packet's cursor id (a.a(...): m.a(Integer(n8), b2)).
// The Framebuffer has no overlay layer, so the cursor is kept but not drawn.
func (d *aspeedDecoder) cursorImage(p *aspeedPacket) error {
	n := p.width * p.height
	if p.width <= 0 || p.height <= 0 || p.width > 4096 || p.height > 4096 || len(p.data) < n*2 {
		return fmt.Errorf("aspeed: bad cursor image %dx%d (%d bytes, mode %d)", p.width, p.height, len(p.data), p.mode)
	}
	img := &aspeedCursorImage{w: p.width, h: p.height, pix: make([]uint32, n)}
	for i := 0; i < n; i++ {
		v := uint32(p.data[2*i]) | uint32(p.data[2*i+1])<<8
		r := (v >> 8) & 0xF
		g := (v >> 4) & 0xF
		b := v & 0xF
		r |= r << 4
		g |= g << 4
		b |= b << 4
		switch {
		case p.mode == aspeedModeCursorARGB:
			a := (v >> 12) & 0xF
			a |= a << 4
			img.pix[i] = a<<24 | r<<16 | g<<8 | b
		case v&0x8000 == 0:
			img.pix[i] = 0xFF000000 | r<<16 | g<<8 | b
		case v&0x4000 != 0:
			// Java: 0xFF000000 | (~r << 16) + (~g << 8) + ~b on ints.
			inv := int32(^int32(r))<<16 + int32(^int32(g))<<8 + ^int32(b)
			img.pix[i] = 0xFF000000 | uint32(inv)
		default:
			img.pix[i] = 0 // transparent
		}
	}
	d.cursors[p.cursorID] = img
	d.stats.CursorPackets++
	d.log.Printf("aspeed: cursor image id %d, %dx%d, mode %d", p.cursorID, p.width, p.height, p.mode)
	return nil
}
