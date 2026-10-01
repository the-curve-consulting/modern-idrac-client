package kvm

import (
	"fmt"
	"image"
	"sync"
)

// DVC ("Dambrackas Video Compression") decoder, ported from the iDRAC6 viewer
// classes com.avocent.kvm.d.b (command loop), com.avocent.kvm.d.{c,d,e,f}
// (per-colour-depth "make pixel"), com.avocent.kvm.d.i (7-bit palette) and
// the pixel store com.avocent.kvm.c.j / com.avocent.kvm.c.i.
//
// The bitstream is a sequence of byte-oriented commands operating on a
// linear pixel cursor over the current frame (row-major, top-left origin):
//
//	1xxxxxxx            Make Pixel  - 7 payload bits + 0..2 extra bytes (mode dependent)
//	000nnnnn [ext...]   No Change   - advance cursor by run length
//	001nnnnn [ext...]   Copy Left   - fill run with pixel at cursor-1
//	010nnnnn [ext...]   Copy Above  - copy run from the row above
//	011Cbbbb [cont...]  Make Series - two-colour bitmap: 4 pixels from bbbb,
//	                    C=1 means continuation bytes follow (7 pixels each,
//	                    bit7 = more follow)
//
// Run lengths: 5 bits in the command byte, extended by up to four more bytes
// carrying the SAME 3-bit opcode in their top bits and 5 more bits each
// (little-endian, i.e. n |= bits << (5*k)). In the two 1-byte-per-pixel
// modes (DVC7 palette and DVC7 grey) every run length is increased by 2.
//
// Frames: a packet flagged BOF resets the cursor to 0; a packet flagged EOF
// ends the frame (optionally carrying a 16-bit checksum). Commands may span
// non-EOF packet boundaries.

// DVC colour modes = video packet type byte.
const (
	dvcMode15    byte = 0x81 // "DVC15": 15-bit RGB555, 2 bytes per Make Pixel
	dvcMode7     byte = 0x82 // "DVC7": 7-bit palette index, 1 byte per Make Pixel
	dvcMode7Gray byte = 0x83 // "DVC7_GRAY": 7-bit grey, 1 byte per Make Pixel
	dvcMode23    byte = 0x8A // "DVC23": 8/8/7-bit RGB, 3 bytes per Make Pixel
)

func isDVCMode(t byte) bool {
	switch t {
	case dvcMode15, dvcMode7, dvcMode7Gray, dvcMode23:
		return true
	}
	return false
}

func dvcModeName(t byte) string {
	switch t {
	case dvcMode15:
		return "DVC15"
	case dvcMode7:
		return "DVC7"
	case dvcMode7Gray:
		return "DVC7_GRAY"
	case dvcMode23:
		return "DVC23"
	}
	return fmt.Sprintf("0x%02x", t)
}

// dvcExtraBytes is the number of bytes following a Make Pixel command byte.
func dvcExtraBytes(mode byte) int {
	switch mode {
	case dvcMode15:
		return 1
	case dvcMode23:
		return 2
	}
	return 0
}

// dvcRunBias is the constant added to every run length (com.avocent.kvm.d.e
// and d.f override g(): return super.g(n) + 2).
func dvcRunBias(mode byte) int {
	if mode == dvcMode7 || mode == dvcMode7Gray {
		return 2
	}
	return 0
}

// ---- 7-bit palette (com.avocent.kvm.d.i) ---------------------------------

var dvcPaletteLevels = [5]uint32{0, 70, 127, 191, 255}

// dvcPalettePermutation is com.avocent.kvm.d.i.e: the wire index -> base
// palette index mapping.
var dvcPalettePermutation = [128]byte{
	0, 25, 50, 75, 5, 30, 55, 80, 10, 35, 60, 85, 15, 40, 65, 90,
	1, 26, 51, 76, 6, 31, 56, 81, 11, 36, 61, 86, 16, 41, 66, 91,
	2, 27, 52, 77, 7, 32, 57, 82, 12, 37, 62, 87, 17, 42, 67, 92,
	3, 28, 53, 78, 8, 33, 58, 83, 13, 38, 63, 88, 18, 43, 68, 93,
	100, 101, 102, 103, 105, 106, 107, 108, 110, 111, 112, 113, 115, 116, 117, 118,
	20, 45, 70, 95, 21, 46, 71, 96, 22, 47, 72, 97, 23, 48, 73, 98,
	4, 29, 54, 79, 9, 34, 59, 84, 14, 39, 64, 89, 19, 44, 69, 94,
	120, 121, 122, 123, 104, 109, 114, 119, 24, 49, 74, 99, 124, 125, 126, 127,
}

var (
	dvcPaletteOnce    sync.Once
	dvcPalette        [128]uint32       // wire index -> 0xAARRGGBB
	dvcPaletteReverse map[uint32]uint16 // 0x00RRGGBB -> wire index
)

func initDVCPalette() {
	dvcPaletteOnce.Do(func() {
		var base [128]uint32
		// d.i.a(): a[i2+i3+i4] = 0xFF000000 | c[i2/25]<<0 | c[i3/5]<<8 | c[i4]<<16
		for i2 := 0; i2 < 125; i2 += 25 {
			for i3 := 0; i3 < 25; i3 += 5 {
				for i4 := 0; i4 < 5; i4++ {
					base[i2+i3+i4] = 0xFF000000 | dvcPaletteLevels[i2/25] | dvcPaletteLevels[i3/5]<<8 | dvcPaletteLevels[i4]<<16
				}
			}
		}
		base[125] = 0xFF5F5F5F // -10526881
		base[126] = 0xFF9F9F9F // -6316129
		base[127] = 0xFFDFDFDF // -2105377
		dvcPaletteReverse = make(map[uint32]uint16, 128)
		for i := 0; i < 128; i++ {
			dvcPalette[i] = base[dvcPalettePermutation[i]]
			dvcPaletteReverse[dvcPalette[i]&0xFFFFFF] = uint16(i)
		}
	})
}

// DVCPalette returns the 128-entry 7-bit palette as 0xAARRGGBB values.
func DVCPalette() [128]uint32 {
	initDVCPalette()
	return dvcPalette
}

// dvcMakePixel converts a Make Pixel command (cmd plus up to two following
// bytes) to 0xAARRGGBB for the given mode.
func dvcMakePixel(mode, cmd, b1, b2 byte) uint32 {
	switch mode {
	case dvcMode15: // com.avocent.kvm.d.c
		v := uint32(cmd&0x7F)<<8 | uint32(b1)
		r := (v & 0x7C00) >> 7
		g := (v & 0x3E0) >> 2
		b := (v & 0x1F) << 3
		return 0xFF000000 | r<<16 | g<<8 | b
	case dvcMode23: // com.avocent.kvm.d.d
		v := uint32(cmd&0x7F)<<16 | uint32(b1)<<8 | uint32(b2)
		r := (v & 0x7F8000) >> 15
		g := (v & 0x7F80) >> 7
		b := (v & 0x7F) << 1
		return 0xFF000000 | r<<16 | g<<8 | b
	case dvcMode7Gray: // com.avocent.kvm.d.e
		g := uint32(cmd&0x7F) << 1
		return 0xFF000000 | g<<16 | g<<8 | g
	default: // dvcMode7, com.avocent.kvm.d.f
		initDVCPalette()
		return dvcPalette[cmd&0x7F]
	}
}

// ---- packet ---------------------------------------------------------------

// dvcPacket is a parsed video packet (com.avocent.kvm.b.a.hb).
type dvcPacket struct {
	mode        byte // packet type byte (0x81/0x82/0x83/0x8A)
	width       int  // payload[6:8]
	height      int  // payload[4:6]
	bof, eof    bool // payload[8] bit0 / bit1
	hasChecksum bool // == eof flag in the Java (bit1)
	checksum    uint16
	data        []byte // payload[12:]
}

// parseDVCPacket parses a video packet payload (the bytes after the 8-byte
// header). Mirrors hb.a(byte[],byte[],int).
func parseDVCPacket(mode byte, payload []byte) (*dvcPacket, error) {
	if len(payload) < 12 {
		return nil, fmt.Errorf("dvc: video packet payload too short (%d bytes)", len(payload))
	}
	p := &dvcPacket{
		mode:   mode,
		height: int(be16(payload[4:])),
		width:  int(be16(payload[6:])),
		data:   payload[12:],
	}
	flags := payload[8]
	p.bof = flags&1 != 0
	p.eof = flags&2 != 0
	if p.eof {
		p.hasChecksum = true
		p.checksum = be16(payload[10:])
	}
	return p, nil
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }

// ---- decoder ---------------------------------------------------------------

// DVCStats are decoder counters exposed through VideoStream.Stats.
type DVCStats struct {
	Packets        uint64
	Commands       uint64
	Pixels         uint64 // pixels covered by commands (incl. No Change)
	Frames         uint64
	ChecksumErrors uint64
	Resizes        uint64
}

type dvcDecoder struct {
	fb  *Framebuffer
	log Logger

	started  bool   // a BOF packet has been seen (s.f(): skip until first BOF)
	cursor   int    // linear pixel index; Java starts at -1 and treats it as 0
	leftover []byte // bytes of an incomplete command awaiting the next packet
	mode     byte

	stats DVCStats

	// per-packet dirty tracking (linear pixel index range written)
	dirtyLo, dirtyHi int

	// trace
	trace  bool
	series [7]uint32
}

func newDVCDecoder(fb *Framebuffer, log Logger) *dvcDecoder {
	initDVCPalette()
	return &dvcDecoder{fb: fb, log: log}
}

// decode applies one video packet to the framebuffer.
func (d *dvcDecoder) decode(p *dvcPacket) error {
	d.stats.Packets++
	d.mode = p.mode

	// Frame size (d.b.a(ib,boolean,boolean) + c.i.a(int,int)).
	w, h := d.fb.Size()
	if p.width != w || p.height != h {
		switch {
		case p.width <= 0 || p.height <= 0 || p.width > 4096 || p.height > 4096:
			d.log.Printf("dvc: bad video frame size: %d x %d", p.width, p.height)
		case p.width > 2000 || p.height > 2000:
			// c.j.a(): "Ignoring invalid resolution change"
			d.log.Printf("dvc: ignoring invalid resolution change: %dx%d", p.width, p.height)
		default:
			d.log.Printf("dvc: video size change: new (%dx%d), old (%dx%d)", p.width, p.height, w, h)
			d.fb.Resize(p.width, p.height)
			d.stats.Resizes++
			d.leftover = nil
		}
	}

	if p.bof {
		// d.b.f(): frame start -> cursor 0. Any half-read command is dropped
		// (in Java the truncated command is applied with length 0).
		d.started = true
		d.cursor = 0
		d.leftover = nil
	}
	if !d.started {
		// s.f(): packets before the first BOF are skipped.
		return nil
	}

	var buf []byte
	if len(d.leftover) > 0 {
		buf = append(d.leftover, p.data...)
		d.leftover = nil
	} else {
		buf = p.data
	}

	var checksumErr error
	d.fb.modify(func(img *image.RGBA) image.Rectangle {
		d.dirtyLo, d.dirtyHi = -1, -1
		d.run(img, buf, p.eof)
		if p.eof {
			d.leftover = nil
			if p.hasChecksum && p.checksum != 0 {
				got := d.checksum(img)
				if got != p.checksum {
					d.stats.ChecksumErrors++
					checksumErr = fmt.Errorf("dvc: checksum failed: expected (%d) and got (%d)", p.checksum, got)
				}
			}
		}
		return d.dirtyRect(img)
	})
	if p.eof {
		d.stats.Frames++
		d.fb.endFrame()
	}
	return checksumErr
}

// run decodes commands from buf. Incomplete trailing commands are stashed in
// d.leftover unless eof is set, in which case they are applied with whatever
// bytes are present (Make Pixel: dropped) - see doc for the Java behaviour.
func (d *dvcDecoder) run(img *image.RGBA, buf []byte, eof bool) {
	mode := d.mode
	extra := dvcExtraBytes(mode)
	bias := dvcRunBias(mode)
	pos := 0
	for pos < len(buf) {
		cmd := buf[pos]
		if cmd&0x80 != 0 {
			// Make Pixel
			if pos+1+extra > len(buf) {
				if !eof {
					d.leftover = append([]byte(nil), buf[pos:]...)
					return
				}
				// UNVERIFIED: Java would pull the next packet to finish the
				// pixel; if that packet is a BOF the pixel is dropped. We
				// always drop it at EOF.
				return
			}
			var b1, b2 byte
			if extra >= 1 {
				b1 = buf[pos+1]
			}
			if extra >= 2 {
				b2 = buf[pos+2]
			}
			d.setPixel(img, dvcMakePixel(mode, cmd, b1, b2))
			d.stats.Commands++
			d.stats.Pixels++
			pos += 1 + extra
			continue
		}
		op := cmd & 0xE0
		if op == 0x60 {
			// Make Series
			used, complete := dvcSeriesExtent(buf[pos:])
			if !complete && !eof {
				d.leftover = append([]byte(nil), buf[pos:]...)
				return
			}
			d.makeSeries(img, buf[pos:pos+used])
			d.stats.Commands++
			pos += used
			continue
		}
		n, used, complete := dvcRunLength(buf[pos:])
		if !complete && !eof {
			d.leftover = append([]byte(nil), buf[pos:]...)
			return
		}
		n += bias
		switch op {
		case 0x00: // No Change (d.b.c)
			d.setCursor(img, d.cursor+n)
		case 0x40: // Copy Above (d.b.d)
			src := d.cursor - img.Rect.Dx()
			if src >= 0 {
				d.copyPixels(img, src, n)
			}
			// else: "Copy Above on first line ignored" - cursor NOT advanced.
		case 0x20: // Copy Left (d.b.e)
			var c uint32
			if d.cursor-1 >= 0 {
				c = d.pixelAt(img, d.cursor-1)
			}
			d.fill(img, c, n)
		}
		d.stats.Commands++
		d.stats.Pixels += uint64(n)
		pos += used
	}
}

// dvcRunLength parses a run-length command at b[0] (d.b.g). It returns the
// length, the bytes consumed, and whether the parse is definitely complete
// (false when more extension bytes might follow in the next packet).
func dvcRunLength(b []byte) (n, used int, complete bool) {
	op := b[0] & 0xE0
	n = int(b[0] & 0x1F)
	used = 1
	for k := 1; k < 5; k++ {
		if used >= len(b) {
			return n, used, false
		}
		if b[used]&0xE0 != op {
			return n, used, true
		}
		n |= int(b[used]&0x1F) << (5 * k)
		used++
	}
	return n, used, true
}

// dvcSeriesExtent returns the byte extent of a Make Series command at b[0].
func dvcSeriesExtent(b []byte) (used int, complete bool) {
	used = 1
	if b[0]&0x10 == 0 {
		return used, true
	}
	for {
		if used >= len(b) {
			return used, false
		}
		c := b[used]
		used++
		if c&0x80 == 0 {
			return used, true
		}
	}
}

// makeSeries applies a Make Series command (d.b.f). cmd[0] is the command
// byte, cmd[1:] the continuation bytes.
func (d *dvcDecoder) makeSeries(img *image.RGBA, cmd []byte) {
	// Colour A = pixel before the cursor; colour B = most recent earlier
	// pixel that differs from A (A if none).
	var a uint32
	if d.cursor-1 >= 0 {
		a = d.pixelAt(img, d.cursor-1)
	}
	b := a
	total := len(img.Pix) / 4
	start := d.cursor - 1
	if start >= total {
		start = total - 1
	}
	for k := start; k >= 0; k-- {
		if c := d.pixelAt(img, k); c != a {
			b = c
			break
		}
	}
	pick := func(set bool) uint32 {
		if set {
			return b
		}
		return a
	}
	c0 := cmd[0]
	four := d.series[:4]
	four[0] = pick(c0&8 != 0)
	four[1] = pick(c0&4 != 0)
	four[2] = pick(c0&2 != 0)
	four[3] = pick(c0&1 != 0)
	d.writePixels(img, four)
	d.stats.Pixels += 4
	for _, c := range cmd[1:] {
		s := d.series[:7]
		s[0] = pick(c&0x40 != 0)
		s[1] = pick(c&0x20 != 0)
		s[2] = pick(c&0x10 != 0)
		s[3] = pick(c&8 != 0)
		s[4] = pick(c&4 != 0)
		s[5] = pick(c&2 != 0)
		s[6] = pick(c&1 != 0)
		d.writePixels(img, s)
		d.stats.Pixels += 7
	}
}

// ---- pixel store primitives (com.avocent.kvm.c.j) ------------------------

func (d *dvcDecoder) pixelAt(img *image.RGBA, i int) uint32 {
	if i < 0 || i >= len(img.Pix)/4 {
		return 0
	}
	return 0xFF000000 | getRGB(img.Pix, i)
}

func (d *dvcDecoder) markDirty(lo, hi int) {
	if hi < lo {
		return
	}
	if d.dirtyLo < 0 || lo < d.dirtyLo {
		d.dirtyLo = lo
	}
	if hi > d.dirtyHi {
		d.dirtyHi = hi
	}
}

// setPixel = c.j.b(int)
func (d *dvcDecoder) setPixel(img *image.RGBA, c uint32) {
	if d.cursor < 0 {
		d.cursor = 0
	}
	total := len(img.Pix) / 4
	if d.cursor < total {
		putARGB(img.Pix, d.cursor, c)
		d.markDirty(d.cursor, d.cursor)
	}
	d.cursor++
}

// setCursor = c.i.c(int): clamp to the pixel count.
func (d *dvcDecoder) setCursor(img *image.RGBA, n int) {
	total := len(img.Pix) / 4
	if n > total {
		n = total
	}
	d.cursor = n
}

// copyPixels = c.j.b(int src, int n)
func (d *dvcDecoder) copyPixels(img *image.RGBA, src, n int) {
	total := len(img.Pix) / 4
	if d.cursor > total {
		return
	}
	if src+n >= total {
		n = total - src
	}
	if d.cursor+n >= total {
		n = total - d.cursor
	}
	if n <= 0 || d.cursor < 0 {
		return
	}
	// src < cursor always here (src = cursor - width), so a forward copy is
	// correct even for overlapping ranges.
	copy(img.Pix[d.cursor*4:(d.cursor+n)*4], img.Pix[src*4:(src+n)*4])
	d.markDirty(d.cursor, d.cursor+n-1)
	d.cursor += n
}

// fill = c.j.c(int colour, int n)
func (d *dvcDecoder) fill(img *image.RGBA, c uint32, n int) {
	total := len(img.Pix) / 4
	if d.cursor >= total {
		return
	}
	if d.cursor+n >= total {
		n = total - d.cursor
	}
	if n <= 0 || d.cursor < 0 || n > total {
		return
	}
	for i := d.cursor; i < d.cursor+n; i++ {
		putARGB(img.Pix, i, c)
	}
	d.markDirty(d.cursor, d.cursor+n-1)
	d.cursor += n
}

// writePixels = c.j.a(int[])
func (d *dvcDecoder) writePixels(img *image.RGBA, px []uint32) {
	if d.cursor < 0 {
		d.cursor = 0
	}
	total := len(img.Pix) / 4
	if d.cursor >= total || d.cursor+len(px) >= total {
		// Java: silently dropped, cursor not advanced.
		return
	}
	for i, c := range px {
		putARGB(img.Pix, d.cursor+i, c)
	}
	d.markDirty(d.cursor, d.cursor+len(px)-1)
	d.cursor += len(px)
}

// dirtyRect converts the touched linear range to a rectangle (rows spanned
// get the full width, like c.i.d(int)).
func (d *dvcDecoder) dirtyRect(img *image.RGBA) image.Rectangle {
	if d.dirtyLo < 0 {
		return image.Rectangle{}
	}
	w := img.Rect.Dx()
	if w == 0 {
		return image.Rectangle{}
	}
	y0, y1 := d.dirtyLo/w, d.dirtyHi/w
	if y0 == y1 {
		return image.Rect(d.dirtyLo%w, y0, d.dirtyHi%w+1, y0+1)
	}
	return image.Rect(0, y0, w, y1+1)
}

// checksum reproduces com.avocent.kvm.b.s.r(): for DVC15 a sum of the
// RGB555 values of all non-black pixels; for every other mode the sum of the
// 7-bit palette indices (0 for colours not in the palette), both mod 2^16.
func (d *dvcDecoder) checksum(img *image.RGBA) uint16 {
	total := len(img.Pix) / 4
	var sum uint32
	if d.mode == dvcMode15 {
		for i := 0; i < total; i++ {
			rgb := getRGB(img.Pix, i)
			if rgb == 0 {
				continue
			}
			sum += (rgb&0xFF0000)>>9 | (rgb&0xFF00)>>6 | (rgb&0xFF)>>3
			sum &= 0xFFFF
		}
		return uint16(sum)
	}
	// UNVERIFIED: only meaningful for DVC7; the Java computes it for
	// DVC7_GRAY / DVC23 too (and mostly gets 0).
	for i := 0; i < total; i++ {
		sum += uint32(dvcPaletteReverse[getRGB(img.Pix, i)])
		sum &= 0xFFFF
	}
	return uint16(sum)
}
