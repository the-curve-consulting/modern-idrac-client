package kvm

import (
	"fmt"
	"image"
)

// Text-mode video, ported from com.avocent.kvm.b.t (TextModeDecoder),
// com.avocent.kvm.b.k (font table assembly) and the packet classes
// com.avocent.kvm.b.a.ub (0x87 "Text Mode Video"), ib (0x88 "Color Palette")
// and kb (0x89 "Font Table").
//
// When the host is in VGA text mode the iDRAC does not send DVC pixels but
// the VGA character/attribute buffer (2 bytes per cell) plus the VGA font
// (256 glyphs x 32 rows, 8 pixels wide) and the 16-entry palette. The
// client renders cells itself. Cell size is derived from the announced
// pixel resolution / rows / columns (e.g. 720x400, 25x80 -> 9x16 cells).
//
// Differences from the Java: blinking text and the blinking cursor are
// rendered steadily (no timers); everything else follows the Java, including
// its odd choices (see doc for the list).

const (
	textFontGlyphBytes = 32   // rows per glyph in the font table
	textFontTableSize  = 8192 // 256 glyphs * 32 bytes
	textDefaultFg      = 0xFFFFFFFF
	textDefaultBg      = 0xFF000000
)

// textPacket is a parsed 0x87 payload (com.avocent.kvm.b.a.ub).
type textPacket struct {
	height, width int  // payload[4:6], payload[6:8]  (pixels)
	bof, eof      bool // payload[8] bit0, bit1
	blink         bool // payload[8] bit3 : blinking attribute enabled
	lineGraphics  bool // payload[8] bit4 : 9th column replication for 0xC0-0xDF
	underlineRow  int  // payload[9]
	rows, cols    int  // payload[10], payload[11]
	empty         bool // payload length == 12: nothing to do
	cursorOnly    bool // payload length == 16 && bof && eof
	cursorPos     int  // cell index
	cursorStart   int  // first scanline of the cursor
	cursorEnd     int  // last scanline of the cursor
	data          []byte
}

func parseTextPacket(payload []byte) (*textPacket, error) {
	n := len(payload)
	if n < 12 {
		return nil, fmt.Errorf("textmode: payload too short (%d bytes)", n)
	}
	p := &textPacket{
		height:       int(be16(payload[4:])),
		width:        int(be16(payload[6:])),
		bof:          payload[8]&1 != 0,
		eof:          payload[8]&2 != 0,
		blink:        payload[8]&8 != 0,
		lineGraphics: payload[8]&0x10 != 0,
		underlineRow: int(payload[9]),
		rows:         int(payload[10]),
		cols:         int(payload[11]),
	}
	switch {
	case n == 12:
		p.empty = true
	case n == 16 && p.bof && p.eof:
		p.cursorOnly = true
		p.cursorPos = int(be16(payload[12:]))
		p.cursorStart = int(payload[14])
		p.cursorEnd = int(payload[15])
	default:
		p.data = payload[12:]
		if p.eof {
			p.cursorPos = int(be16(payload[n-4:]))
			p.cursorStart = int(payload[n-2])
			p.cursorEnd = int(payload[n-1])
		}
	}
	return p, nil
}

// textFontTable accumulates 0x89 Font Table packets (com.avocent.kvm.b.k).
type textFontTable struct {
	primary, secondary [textFontTableSize]byte
	count              int // number of fonts announced (1 or 2)
	gotPrimary         int // bytes received
	gotSecondary       int
	ready              bool
	readyPrimary       []byte
	readySecondary     []byte // nil when count == 1
}

// addPacket consumes one Font Table payload:
//
//	[0]    table index (0 = primary, else secondary)
//	[1]    number of font tables (1 or 2)
//	[2:4]  (offset field - see below)
//	[4:]   glyph bytes
//
// Java quirk (kb.a): the destination offset is read as a big-endian 16-bit
// value at offset THIS.l which is always 0, i.e. ((payload[0]<<8)|payload[1])
// - 1, so in practice chunks land at offset 0 or 1 and the table is expected
// to arrive in a single packet of 8192 bytes. We reproduce that, but if the
// chunk would not fit we fall back to the u16 at [2:4] (UNVERIFIED).
func (f *textFontTable) addPacket(payload []byte) (published bool, err error) {
	if len(payload) < 4 {
		return false, fmt.Errorf("textmode: font packet too short (%d bytes)", len(payload))
	}
	idx := int(payload[0])
	f.count = int(payload[1])
	glyphs := payload[4:]
	off := int(be16(payload[0:])) - 1
	if off < 0 {
		off = 0
	}
	if off+len(glyphs) > textFontTableSize {
		off = int(be16(payload[2:]))
		if off+len(glyphs) > textFontTableSize {
			return false, fmt.Errorf("textmode: font chunk (%d bytes @%d) does not fit table", len(glyphs), off)
		}
	}
	if idx == 0 {
		copy(f.primary[off:], glyphs)
		f.gotPrimary += len(glyphs)
	} else {
		copy(f.secondary[off:], glyphs)
		f.gotSecondary += len(glyphs)
	}
	// Java publishes when the primary table is complete (the secondary
	// check is a typo there and effectively also tests the primary).
	if (f.count == 2 && f.gotPrimary >= textFontTableSize) || (f.count == 1 && f.gotPrimary >= textFontTableSize) {
		f.gotPrimary, f.gotSecondary = 0, 0
		f.ready = true
		f.readyPrimary = append([]byte(nil), f.primary[:]...)
		if f.count == 2 {
			f.readySecondary = append([]byte(nil), f.secondary[:]...)
		} else {
			f.readySecondary = nil
		}
		return true, nil
	}
	return false, nil
}

// parseTextPalette parses a 0x88 Color Palette payload (com.avocent.kvm.b.a.ib):
//
//	[0:2] entry count, [2] unused, [3] pad, then count x {r, g, b, pad}
func parseTextPalette(payload []byte) ([]uint32, error) {
	if len(payload) < 4 {
		return nil, fmt.Errorf("textmode: palette payload too short (%d bytes)", len(payload))
	}
	n := int(be16(payload))
	if 4+n*4 > len(payload) {
		return nil, fmt.Errorf("textmode: palette declares %d entries but payload is %d bytes", n, len(payload))
	}
	pal := make([]uint32, n)
	o := 4
	for i := 0; i < n; i++ {
		pal[i] = 0xFF000000 | uint32(payload[o])<<16 | uint32(payload[o+1])<<8 | uint32(payload[o+2])
		o += 4
	}
	return pal, nil
}

// textDecoder holds the text-mode state (fields named after b.t where useful).
type textDecoder struct {
	fb  *Framebuffer
	log Logger

	font    textFontTable
	fontA   []byte // primary font (b.t.s)
	fontB   []byte // secondary font (b.t.t), nil if one font
	nFonts  int    // b.t.w
	palette []uint32

	// geometry
	valid       bool
	rows, cols  int      // z, A
	width       int      // x (pixels)
	height      int      // y (pixels)
	cellW       int      // q
	cellH       int      // r
	cell        []uint32 // M: cellW*cellH scratch
	shown       []byte   // B: what is on screen (2 bytes / cell)
	incoming    []byte   // C: buffer being filled
	fill        int      // D: bytes of incoming received so far
	blink       bool     // F
	lineGfx     bool     // G
	underline   int      // K
	cursorPos   int      // H
	cursorStart int      // I
	cursorEnd   int      // J
	cursorOn    bool     // L
	fontMissing bool     // logged once

	// set by the stream when switching graphics -> text so the geometry is
	// re-applied even if unchanged ("modechanged" property in the Java)
	modeChanged bool
}

func newTextDecoder(fb *Framebuffer, log Logger) *textDecoder {
	return &textDecoder{fb: fb, log: log, modeChanged: true}
}

// setFont installs published font tables (b.t.a(i)).
func (t *textDecoder) setFont(primary, secondary []byte, count int) {
	t.fontA, t.fontB, t.nFonts = primary, secondary, count
	t.redrawAll()
}

// setPalette installs the 16-colour palette (b.t.a(int[])).
func (t *textDecoder) setPalette(p []uint32) {
	t.palette = p
	t.redrawAll()
}

// redrawAll = b.t.h(): forget what is shown and repaint every cell.
func (t *textDecoder) redrawAll() {
	if !t.valid {
		return
	}
	for i := range t.shown {
		t.shown[i] = 0
	}
	t.fb.modify(func(img *image.RGBA) image.Rectangle {
		return t.diffRedraw(img)
	})
}

// decode applies a Text Mode Video packet (b.t.d()).
func (t *textDecoder) decode(p *textPacket) error {
	if p.empty {
		return nil
	}
	if p.cursorOnly {
		if !t.valid {
			return nil
		}
		old := t.cursorPos
		t.fb.modify(func(img *image.RGBA) image.Rectangle {
			r := t.drawCell(img, old)
			if p.cursorPos < t.rows*t.cols {
				t.cursorPos, t.cursorStart, t.cursorEnd = p.cursorPos, p.cursorStart, p.cursorEnd
				t.cursorOn = true
				r = r.Union(t.drawCursor(img))
			} else {
				t.cursorOn = false
			}
			return r
		})
		return nil
	}

	if p.bof {
		if t.blink != p.blink {
			t.blink = p.blink
		}
		t.lineGfx = p.lineGraphics
		t.underline = p.underlineRow
		t.fill = 0
		if !t.valid || t.modeChanged || t.rows != p.rows || t.cols != p.cols || t.width != p.width || t.height != p.height {
			if p.rows <= 0 || p.cols <= 0 || p.width <= 0 || p.height <= 0 || p.width > 4096 || p.height > 4096 {
				t.valid = false
				return fmt.Errorf("textmode: bad geometry %dx%d px, %dx%d cells", p.width, p.height, p.rows, p.cols)
			}
			t.rows, t.cols, t.width, t.height = p.rows, p.cols, p.width, p.height
			t.cellH = t.height / t.rows
			t.cellW = t.width / t.cols
			if t.cellW <= 0 || t.cellH <= 0 || t.cellH > textFontGlyphBytes {
				t.valid = false
				return fmt.Errorf("textmode: unsupported cell size %dx%d", t.cellW, t.cellH)
			}
			t.cell = make([]uint32, t.cellW*t.cellH)
			t.incoming = make([]byte, t.rows*t.cols*2)
			t.shown = make([]byte, t.rows*t.cols*2)
			t.valid = true
			t.modeChanged = false
			t.cursorOn = false
			t.fb.Resize(t.width, t.height)
		}
	}
	if !t.valid {
		return nil
	}

	if !p.eof {
		// Accumulate.
		if t.fill+len(p.data) > len(t.incoming) {
			return fmt.Errorf("textmode: cell data overflow (%d+%d > %d)", t.fill, len(p.data), len(t.incoming))
		}
		copy(t.incoming[t.fill:], p.data)
		t.fill += len(p.data)
		return nil
	}

	// EOF: the last 4 bytes of the data are the cursor when the buffer
	// would overflow (which is the normal case: rows*cols*2 + 4 bytes).
	n5 := len(p.data)
	old := t.cursorPos
	limit := len(t.incoming)
	if t.fill+n5 <= limit {
		copy(t.incoming[t.fill:], p.data)
		t.cursorOn = false
	} else {
		body := n5 - 4
		if body < 0 {
			body = 0
		}
		if t.fill+body > limit {
			return fmt.Errorf("textmode: cell data overflow (%d+%d > %d)", t.fill, body, limit)
		}
		copy(t.incoming[t.fill:], p.data[:body])
		if p.cursorPos < t.rows*t.cols {
			t.cursorOn = true
			t.cursorPos, t.cursorStart, t.cursorEnd = p.cursorPos, p.cursorStart, p.cursorEnd
		} else {
			t.cursorOn = false
		}
	}
	t.fill = 0
	t.fb.modify(func(img *image.RGBA) image.Rectangle {
		r := t.diffRedraw(img)
		r = r.Union(t.drawCell(img, old))
		if t.cursorOn {
			r = r.Union(t.drawCursor(img))
		}
		return r
	})
	t.fb.endFrame()
	return nil
}

// diffRedraw = b.t.g(): repaint cells whose char/attr changed.
func (t *textDecoder) diffRedraw(img *image.RGBA) image.Rectangle {
	var dirty image.Rectangle
	n := 0
	for y := 0; y+t.cellH <= t.height; y += t.cellH {
		for x := 0; x+t.cellW <= t.width; x += t.cellW {
			if n+1 >= len(t.incoming) {
				return dirty
			}
			if t.shown[n] != t.incoming[n] || t.shown[n+1] != t.incoming[n+1] {
				t.shown[n] = t.incoming[n]
				t.shown[n+1] = t.incoming[n+1]
				dirty = dirty.Union(t.renderCell(img, x, y, int(t.incoming[n]), int(t.incoming[n+1])))
			}
			n += 2
		}
	}
	return dirty
}

// drawCell = b.t.a(int): repaint cell idx from the shown buffer.
func (t *textDecoder) drawCell(img *image.RGBA, idx int) image.Rectangle {
	if idx < 0 || idx*2+1 >= len(t.shown) || t.cols == 0 {
		return image.Rectangle{}
	}
	x := idx % t.cols * t.cellW
	y := idx / t.cols * t.cellH
	return t.renderCell(img, x, y, int(t.shown[idx*2]), int(t.shown[idx*2+1]))
}

// colours resolves foreground/background for an attribute byte, following
// b.t.a(int,int,int,int,int).
func (t *textDecoder) colours(attr int) (fg, bg uint32, font []byte) {
	font = t.fontA
	var fgIdx, bgIdx int
	if t.nFonts == 2 {
		fgIdx = attr & 7
		if attr&8 != 0 {
			font = t.fontA
		} else {
			font = t.fontB
		}
	} else {
		fgIdx = attr & 0xF
	}
	hi := attr >> 4
	if t.blink {
		bgIdx = hi & 7 // bit 7 = blink (rendered steadily)
	} else {
		bgIdx = hi & 0xF
	}
	switch {
	case t.palette != nil && fgIdx < len(t.palette) && bgIdx < len(t.palette):
		fg, bg = t.palette[fgIdx], t.palette[bgIdx]
	case fgIdx == 0:
		fg, bg = textDefaultBg, textDefaultFg
	default:
		fg, bg = textDefaultFg, textDefaultBg
	}
	return fg, bg, font
}

// renderCell paints glyph with attr at pixel (x, y).
func (t *textDecoder) renderCell(img *image.RGBA, x, y, glyph, attr int) image.Rectangle {
	fg, bg, font := t.colours(attr)
	if font == nil {
		// Java throws a NullPointerException per step until the font table
		// arrives; nothing is drawn.
		if !t.fontMissing {
			t.fontMissing = true
			t.log.Printf("textmode: text video before font table; cells not drawn")
		}
		return image.Rectangle{}
	}
	q, r := t.cellW, t.cellH
	ulRow := t.underline - 1
	underline := attr&0x77 == 1 && ulRow < r
	base := glyph * textFontGlyphBytes
	for row := 0; row < r; row++ {
		bits := int(font[base+row])
		mask := 0x80
		for col := 0; col < 8; col++ {
			var c uint32
			if bits&mask != 0 {
				c = fg
			} else {
				c = bg
			}
			if row == ulRow && underline {
				c = fg
			}
			if col < q {
				t.cell[row*q+col] = c
			}
			if col == 7 && q == 9 {
				// 9th column: replicate for line-graphics glyphs, else background.
				var c9 uint32
				if t.lineGfx && glyph >= 192 && glyph <= 223 {
					c9 = c
					if bits&mask != 0 {
						c9 = fg
					} else {
						c9 = bg
					}
					if row == ulRow && underline {
						c9 = fg
					}
				} else {
					c9 = bg
				}
				t.cell[row*q+8] = c9
			}
			mask >>= 1
		}
		for col := 9; col < q; col++ {
			t.cell[row*q+col] = bg
		}
	}
	return blitARGB(img, x, y, q, r, t.cell)
}

// drawCursor = b.t.c(int)/a(int,int,int): paint the cursor block (the
// bottom cursorEnd-cursorStart+1 scanlines of the cell) in the cell's
// foreground colour.
func (t *textDecoder) drawCursor(img *image.RGBA) image.Rectangle {
	idx := t.cursorPos
	if idx < 0 || idx*2+1 >= len(t.shown) || t.cols == 0 {
		return image.Rectangle{}
	}
	attr := int(t.shown[idx*2+1])
	x := idx % t.cols * t.cellW
	y := idx / t.cols * t.cellH
	n := (t.cursorEnd & 0xFF) - (t.cursorStart & 0xFF) + 1
	if n <= 0 || n > t.cellH {
		return image.Rectangle{}
	}
	var fgIdx int
	if t.nFonts == 2 {
		fgIdx = attr & 7
	} else {
		fgIdx = attr & 0xF
	}
	var c uint32
	switch {
	case t.palette != nil && fgIdx < len(t.palette):
		c = t.palette[fgIdx]
	case fgIdx == 0:
		c = textDefaultBg
	default:
		c = textDefaultFg
	}
	block := make([]uint32, n*t.cellW)
	for i := range block {
		block[i] = c
	}
	return blitARGB(img, x, y+t.cellH-n, t.cellW, n, block)
}
