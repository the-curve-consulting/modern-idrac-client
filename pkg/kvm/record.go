package kvm

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Video recordings capture the raw bytes read from the video socket together
// with their arrival times, so a session can be replayed offline through the
// same decoders (for debugging codec problems and for demoing the viewer
// without an iDRAC).
//
// Format: magic "IDRACREC1\n", then records of
//
//	u32be delta-milliseconds since the previous record
//	u32be length
//	<length> bytes exactly as read from the socket

const recordMagic = "IDRACREC1\n"

// Recorder writes a video recording. It is safe for use from one reader
// goroutine; errors are sticky and surfaced by Err.
type Recorder struct {
	mu   sync.Mutex
	w    io.Writer
	last time.Time
	err  error
}

// NewRecorder starts a recording on w.
func NewRecorder(w io.Writer) (*Recorder, error) {
	if _, err := io.WriteString(w, recordMagic); err != nil {
		return nil, err
	}
	return &Recorder{w: w, last: time.Now()}, nil
}

func (r *Recorder) write(b []byte) {
	if len(b) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return
	}
	now := time.Now()
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:], uint32(now.Sub(r.last).Milliseconds()))
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(b)))
	r.last = now
	if _, err := r.w.Write(hdr[:]); err != nil {
		r.err = err
		return
	}
	if _, err := r.w.Write(b); err != nil {
		r.err = err
	}
}

// Err returns the first write error, if any.
func (r *Recorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// recordingRW tees everything read from the video socket into a Recorder.
type recordingRW struct {
	io.ReadWriter
	rec *Recorder
}

func (t *recordingRW) Read(b []byte) (int, error) {
	n, err := t.ReadWriter.Read(b)
	if n > 0 {
		t.rec.write(b[:n])
	}
	return n, err
}

func (t *recordingRW) Close() error {
	if c, ok := t.ReadWriter.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// replayReader plays a recording back as an io.Reader, sleeping between
// records according to the recorded timing scaled by speed (0 = no delays).
type replayReader struct {
	ctx   context.Context
	r     io.Reader
	speed float64
	buf   []byte
}

func (p *replayReader) Read(b []byte) (int, error) {
	for len(p.buf) == 0 {
		var hdr [8]byte
		if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
			return 0, err
		}
		delta := time.Duration(binary.BigEndian.Uint32(hdr[0:])) * time.Millisecond
		n := binary.BigEndian.Uint32(hdr[4:])
		if n > 16<<20 {
			return 0, fmt.Errorf("recording: implausible record length %d", n)
		}
		if p.speed > 0 && delta > 0 {
			if delta > 2*time.Second {
				delta = 2 * time.Second // skip long idle gaps
			}
			select {
			case <-p.ctx.Done():
				return 0, p.ctx.Err()
			case <-time.After(time.Duration(float64(delta) / p.speed)):
			}
		}
		p.buf = make([]byte, n)
		if _, err := io.ReadFull(p.r, p.buf); err != nil {
			return 0, err
		}
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

// Replay decodes a recording into fb. speed 1 plays in real time, 2 twice as
// fast, 0 as fast as possible. It returns nil at the end of the recording.
func Replay(ctx context.Context, rec io.Reader, fb *Framebuffer, log Logger, speed float64) (*VideoStats, error) {
	magic := make([]byte, len(recordMagic))
	if _, err := io.ReadFull(rec, magic); err != nil || string(magic) != recordMagic {
		return nil, errors.New("not an idrac video recording")
	}
	if log == nil {
		log = nopLogger{}
	}
	rw := struct {
		io.Reader
		io.Writer
	}{&replayReader{ctx: ctx, r: rec, speed: speed}, io.Discard}
	v := NewVideoStream(rw, fb, log)
	err := v.Run(ctx)
	st := v.Stats()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return &st, err
}
