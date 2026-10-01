package kvm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"image/color"
	"io"
	"sync"
	"sync/atomic"
)

// VideoStream reads the Avocent video socket and drives a Framebuffer.
//
// Packet framing on the video socket (com.avocent.kvm.b.a.a.a(DataInputStream,...)):
//
//	0..3  magic  "BEEF" for client->server packets; the Java reader ignores it
//	4     type high byte (ignored by the reader; the iDRAC6 sends 0x86 here for video)
//	5     type low byte  (dispatch key)
//	6..7  total length, big endian, INCLUDING this 8-byte header; 16 <= len <= 15000
//	8..   payload (len-8 bytes)
//
// Packets are dispatched on byte 5 (com.avocent.kvm.b.n.a(c)). See
// docs/kvm-video-channel.md for every type.
type VideoStream struct {
	rw  io.ReadWriter
	br  *bufio.Reader
	fb  *Framebuffer
	log Logger

	// Control, if non-nil, is used for the messages the Java viewer sends
	// on the CONTROL channel in reaction to video traffic:
	// SetDVCColorDepth (0x0403) when the DVC mode changes and ScreenRefresh
	// (0x0301) for RequestRefresh. Without it those are logged and skipped.
	Control VideoControlSender

	// OnConnectStatus is called for a 0x84 "Video Connect Status" packet.
	// The Java session reacts to it by sending Video Enable Request (782),
	// Set Display Area (770, 1024x768) and SetScaleMode1to1 (772) on the
	// control channel and entering RUNNING (com.avocent.kvm.b.r.y()).
	OnConnectStatus func(connected bool)

	// OnVideoStopped is called for a 0x85 "VideoStopped" packet with the
	// server's reason code (payload[0]). The screen has been cleared to black.
	OnVideoStopped func(reason int)

	// RefreshOnChecksumError requests a full-screen refresh through Control
	// when a frame checksum does not match (Java: -DrefreshOnError=true).
	RefreshOnChecksumError bool

	// AckInterval is the number of video packets between "Video Ack"
	// messages (com.avocent.kvm.b.r.W = 20).
	AckInterval int

	trace atomic.Bool

	wmu sync.Mutex // serialises writes to rw

	dvc    *dvcDecoder
	text   *textDecoder
	aspeed *aspeedDecoder

	ackPending  int
	lastDVCType int
	inTextMode  bool

	statsMu sync.Mutex
	stats   VideoStats
}

// VideoControlSender sends one packet on the control (keyboard/mouse)
// channel. typ is the 16-bit packet type; payload excludes the 8-byte
// header. Session is expected to implement it.
type VideoControlSender interface {
	SendControlPacket(typ uint16, payload []byte) error
}

// VideoStats are counters for diagnostics.
type VideoStats struct {
	Packets        uint64
	Bytes          uint64
	AcksSent       uint64
	Unknown        uint64
	DecodeErrors   uint64
	VideoStopped   uint64
	ASpeedPackets  uint64 // iDRAC7+ ASpeed JPEG packets (0x86) received
	ASpeedFrames   uint64 // ASpeed frames handed to the decoder
	ASpeedErrors   uint64 // ASpeed packets/frames that failed to decode
	TextPackets    uint64
	FontPackets    uint64
	PalettePackets uint64
	DVC            DVCStats
	ASpeed         ASpeedStats
}

// ErrNoControlChannel is returned by RequestRefresh when no
// VideoControlSender is configured.
var ErrNoControlChannel = errors.New("kvm: screen refresh must be sent on the control channel; set VideoStream.Control")

// Video socket packet types (low byte). Names from the decompiled classes.
const (
	vidTypeAck           byte = 0x00 // "Video Ack"            com.avocent.kvm.b.a.cb (client->server)
	vidTypeAuth          byte = 0x01 // "Video Channel Auth"   com.avocent.kvm.b.a.ac (client->server)
	vidTypeNop           byte = 0x80 // "Video Packet"         com.avocent.kvm.b.a.fb (payload unused; ack only)
	vidTypeDVC15         byte = 0x81 // DVC 15-bit             com.avocent.kvm.b.a.hb -> d.c
	vidTypeDVC7          byte = 0x82 // DVC 7-bit palette      hb -> d.f
	vidTypeDVC7Gray      byte = 0x83 // DVC 7-bit grey         hb -> d.e
	vidTypeConnectStatus byte = 0x84 // "Video Connect Status" com.avocent.kvm.b.a.xb
	vidTypeVideoStopped  byte = 0x85 // "VideoStopped"         com.avocent.kvm.b.a.v
	vidTypeASpeedJPEG    byte = 0x86 // "ASpeed JPEG Video"    com.avocent.kvm.b.a.gb (iDRAC7+) -> aspeed.go
	vidTypeTextMode      byte = 0x87 // "Text Mode Video"      com.avocent.kvm.b.a.ub
	vidTypeColorPalette  byte = 0x88 // "Color Palette"        com.avocent.kvm.b.a.ib
	vidTypeFontTable     byte = 0x89 // "Font Table"           com.avocent.kvm.b.a.kb
	vidTypeDVC23         byte = 0x8A // DVC 23-bit             hb -> d.d
)

// Control-channel packet types the video layer needs.
const (
	vidCtlScreenRefresh    uint16 = 0x0301 // 769  "ScreenRefresh"          com.avocent.kvm.b.a.n
	vidCtlSetDVCColorDepth uint16 = 0x0403 // 1027 "SetDVCColorDepthMessage" com.avocent.kvm.b.a.k
)

const (
	vidHeaderLen = 8
	vidMinLen    = 16
	vidMaxLen    = 15000
	vidAckEvery  = 20
)

// NewVideoStream wraps the video socket. rw must be positioned right after
// the session has sent the "Video Channel Auth" (ac) packet; everything the
// server sends from then on is consumed by Run. If rw implements io.Closer
// it is closed when the context given to Run is cancelled.
func NewVideoStream(rw io.ReadWriter, fb *Framebuffer, log Logger) *VideoStream {
	if log == nil {
		log = nopLogger{}
	}
	if fb == nil {
		fb = NewFramebuffer()
	}
	return &VideoStream{
		rw:          rw,
		br:          bufio.NewReaderSize(rw, 64*1024),
		fb:          fb,
		log:         log,
		AckInterval: vidAckEvery,
		dvc:         newDVCDecoder(fb, log),
		text:        newTextDecoder(fb, log),
		aspeed:      newASpeedDecoder(fb, log),
		lastDVCType: -1,
	}
}

// Framebuffer returns the framebuffer this stream draws into.
func (v *VideoStream) Framebuffer() *Framebuffer { return v.fb }

// SetTrace enables hex dumps of every packet header through the Logger.
func (v *VideoStream) SetTrace(on bool) { v.trace.Store(on) }

// Stats returns a copy of the counters.
func (v *VideoStream) Stats() VideoStats {
	v.statsMu.Lock()
	defer v.statsMu.Unlock()
	s := v.stats
	s.DVC = v.dvc.stats
	s.ASpeed = v.aspeed.stats
	return s
}

// RequestRefresh asks the server for a full frame. In the Avocent protocol
// this is the control-channel "ScreenRefresh" message (type 0x0301, empty
// 8-byte payload), so it is forwarded to Control; ErrNoControlChannel is
// returned when none is configured.
func (v *VideoStream) RequestRefresh() error {
	if v.Control == nil {
		return ErrNoControlChannel
	}
	return v.Control.SendControlPacket(vidCtlScreenRefresh, make([]byte, 8))
}

// Run reads and decodes packets until an error occurs or ctx is cancelled.
// The returned error is ctx.Err() on cancellation, otherwise the I/O or
// framing error that stopped the loop (io.EOF when the server closed).
func (v *VideoStream) Run(ctx context.Context) error {
	done := make(chan struct{})
	defer close(done)
	if c, ok := v.rw.(io.Closer); ok {
		go func() {
			select {
			case <-ctx.Done():
				_ = c.Close()
			case <-done:
			}
		}()
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, payload, err := v.readPacket()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if err := v.handle(hdr, payload); err != nil {
			// Decode errors are per-packet in the Java (caught and logged);
			// only I/O errors are fatal.
			var ioErr *videoIOError
			if errors.As(err, &ioErr) {
				return ioErr.err
			}
			v.statsMu.Lock()
			v.stats.DecodeErrors++
			v.statsMu.Unlock()
			v.log.Printf("video: %v", err)
		}
	}
}

type videoIOError struct{ err error }

func (e *videoIOError) Error() string { return e.err.Error() }
func (e *videoIOError) Unwrap() error { return e.err }

// readPacket reads one framed packet. APCP frames (the transport-level
// handshake, "APCP" + u32 length) that may still be in flight are skipped.
func (v *VideoStream) readPacket() (hdr [vidHeaderLen]byte, payload []byte, err error) {
	for {
		if _, err = io.ReadFull(v.br, hdr[:]); err != nil {
			return hdr, nil, err
		}
		if string(hdr[:4]) == "APCP" {
			// UNVERIFIED: the Java reader reads the APCP body and then
			// (buggily) keeps going with the same header; we skip the frame.
			n := int(uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7]))
			if n < vidHeaderLen || n > 1<<20 {
				return hdr, nil, fmt.Errorf("video: bad APCP frame length %d", n)
			}
			if _, err = io.CopyN(io.Discard, v.br, int64(n-vidHeaderLen)); err != nil {
				return hdr, nil, err
			}
			v.log.Printf("video: skipped APCP frame of %d bytes", n)
			continue
		}
		length := int(be16(hdr[6:]))
		if length < vidMinLen {
			return hdr, nil, fmt.Errorf("video: bad message length (%d)", length)
		}
		if length > vidMaxLen {
			return hdr, nil, fmt.Errorf("video: packet length %d exceeds %d", length, vidMaxLen)
		}
		payload = make([]byte, length-vidHeaderLen)
		if _, err = io.ReadFull(v.br, payload); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return hdr, nil, err
		}
		return hdr, payload, nil
	}
}

// handle dispatches one packet (com.avocent.kvm.b.n.a(c) for the video types).
func (v *VideoStream) handle(hdr [vidHeaderLen]byte, payload []byte) error {
	typ := hdr[5]
	v.statsMu.Lock()
	v.stats.Packets++
	v.stats.Bytes += uint64(len(payload) + vidHeaderLen)
	v.statsMu.Unlock()

	if v.trace.Load() {
		n := len(payload)
		if n > 16 {
			n = 16
		}
		v.log.Printf("video <- type 0x%02x%02x len %d\n%s", hdr[4], hdr[5], len(payload)+vidHeaderLen,
			HexDump(append(append([]byte(nil), hdr[:]...), payload[:n]...), 0))
	}

	switch typ {
	case vidTypeDVC15, vidTypeDVC7, vidTypeDVC7Gray, vidTypeDVC23:
		if int(typ) != v.lastDVCType {
			// n.a(hb): the viewer tells the appliance which colour depth it
			// just saw (SetDVCColorDepthMessage with "not default" = 1).
			v.lastDVCType = int(typ)
			v.log.Printf("video: DVC mode %s (%dx%d)", dvcModeName(typ), int(be16(payload[6:])), int(be16(payload[4:])))
			if v.Control != nil {
				if err := v.Control.SendControlPacket(vidCtlSetDVCColorDepth, []byte{1, 0, 0, 0, 0, 0, 0, 0}); err != nil {
					v.log.Printf("video: SetDVCColorDepth failed: %v", err)
				}
			}
		}
		if v.inTextMode {
			v.inTextMode = false
			v.text.modeChanged = true
		}
		p, err := parseDVCPacket(typ, payload)
		if err != nil {
			return errors.Join(err, v.countAck())
		}
		derr := v.dvc.decode(p)
		if derr != nil && v.RefreshOnChecksumError && v.Control != nil {
			if rerr := v.RequestRefresh(); rerr != nil {
				v.log.Printf("video: reference screen request failed: %v", rerr)
			}
		}
		return errors.Join(derr, v.countAck())

	case vidTypeNop:
		return v.countAck()

	case vidTypeConnectStatus:
		connected := len(payload) > 0 && payload[0] == 0
		v.log.Printf("video: connect status: connected=%v", connected)
		if v.OnConnectStatus != nil {
			v.OnConnectStatus(connected)
		}
		return nil

	case vidTypeVideoStopped:
		reason := -1
		if len(payload) > 0 {
			reason = int(payload[0])
		}
		v.statsMu.Lock()
		v.stats.VideoStopped++
		v.statsMu.Unlock()
		v.log.Printf("video: video stopped (reason %d)", reason)
		// n.a(v): screen.i() (clear to black) + screen.b() (frame end).
		v.fb.clear(color.RGBA{0, 0, 0, 0xFF})
		v.fb.endFrame()
		if v.OnVideoStopped != nil {
			v.OnVideoStopped(reason)
		}
		return v.countAck()

	case vidTypeASpeedJPEG:
		if v.inTextMode {
			// s.q(): a gb packet puts the viewer back into "graphicsmode".
			v.inTextMode = false
			v.text.modeChanged = true
		}
		// n.a(gb): fragments accumulate; the frame is decoded when the
		// next first-fragment arrives. Decode errors are per frame.
		frame, err := v.aspeed.handle(payload)
		v.statsMu.Lock()
		v.stats.ASpeedPackets++
		if frame {
			v.stats.ASpeedFrames++
		}
		if err != nil {
			v.stats.ASpeedErrors++
		}
		v.statsMu.Unlock()
		return errors.Join(err, v.countAck())

	case vidTypeTextMode:
		v.statsMu.Lock()
		v.stats.TextPackets++
		v.statsMu.Unlock()
		if !v.inTextMode {
			v.inTextMode = true
			v.text.modeChanged = true
			v.log.Printf("video: text mode")
		}
		p, err := parseTextPacket(payload)
		if err != nil {
			return errors.Join(err, v.countAck())
		}
		return errors.Join(v.text.decode(p), v.countAck())

	case vidTypeColorPalette:
		v.statsMu.Lock()
		v.stats.PalettePackets++
		v.statsMu.Unlock()
		pal, err := parseTextPalette(payload)
		if err != nil {
			return errors.Join(err, v.countAck())
		}
		v.text.setPalette(pal)
		return v.countAck()

	case vidTypeFontTable:
		v.statsMu.Lock()
		v.stats.FontPackets++
		v.statsMu.Unlock()
		published, err := v.text.font.addPacket(payload)
		if err != nil {
			return err
		}
		if published {
			v.log.Printf("video: font table received (%d font(s))", v.text.font.count)
			v.text.setFont(v.text.font.readyPrimary, v.text.font.readySecondary, v.text.font.count)
		}
		return nil // no ack for font packets (n.a(kb))

	case vidTypeAck, vidTypeAuth:
		v.log.Printf("video: ignoring client-direction packet type 0x%02x from server", typ)
		return nil

	default:
		v.statsMu.Lock()
		v.stats.Unknown++
		v.statsMu.Unlock()
		v.log.Printf("video: unknown packet type 0x%02x%02x (%d bytes)", hdr[4], hdr[5], len(payload))
		return nil
	}
}

// countAck implements com.avocent.kvm.b.r.B(): every AckInterval video
// packets send a "Video Ack" carrying the count.
func (v *VideoStream) countAck() error {
	v.ackPending++
	interval := v.AckInterval
	if interval <= 0 {
		interval = vidAckEvery
	}
	if v.ackPending < interval {
		return nil
	}
	count := v.ackPending
	v.ackPending = 0
	if err := v.writePacket(uint16(vidTypeAck), []byte{byte(count), 0, 0, 0, 0, 0, 0, 0}); err != nil {
		return &videoIOError{fmt.Errorf("video: sending ack: %w", err)}
	}
	v.statsMu.Lock()
	v.stats.AcksSent++
	v.statsMu.Unlock()
	return nil
}

// writePacket frames and writes one packet ("BEEF" header).
func (v *VideoStream) writePacket(typ uint16, payload []byte) error {
	buf := make([]byte, vidHeaderLen+len(payload))
	copy(buf, "BEEF")
	buf[4] = byte(typ >> 8)
	buf[5] = byte(typ)
	total := len(buf)
	buf[6] = byte(total >> 8)
	buf[7] = byte(total)
	copy(buf[vidHeaderLen:], payload)
	if v.trace.Load() {
		v.log.Printf("video -> type 0x%04x len %d\n%s", typ, total, HexDump(buf, 0))
	}
	v.wmu.Lock()
	defer v.wmu.Unlock()
	_, err := v.rw.Write(buf)
	return err
}

// EncodeVideoAck returns the wire bytes of a Video Ack for count packets
// (exported for the session/tests).
func EncodeVideoAck(count int) []byte {
	return []byte{'B', 'E', 'E', 'F', 0, vidTypeAck, 0, 16, byte(count), 0, 0, 0, 0, 0, 0, 0}
}
