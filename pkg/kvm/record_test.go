package kvm

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestRecorderReplayRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	rec, err := NewRecorder(&buf)
	if err != nil {
		t.Fatal(err)
	}
	src := struct {
		io.Reader
		io.Writer
	}{bytes.NewReader([]byte("hello world, this is video")), io.Discard}
	rw := &recordingRW{ReadWriter: src, rec: rec}
	chunk := make([]byte, 7)
	var got []byte
	for {
		n, err := rw.Read(chunk)
		got = append(got, chunk[:n]...)
		if err != nil {
			break
		}
	}
	if rec.Err() != nil {
		t.Fatal(rec.Err())
	}
	data := buf.Bytes()
	if string(data[:len(recordMagic)]) != recordMagic {
		t.Fatal("magic missing")
	}
	rr := &replayReader{ctx: context.Background(), r: bytes.NewReader(data[len(recordMagic):])}
	back, err := io.ReadAll(rr)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(got) || string(back) != "hello world, this is video" {
		t.Fatalf("round trip: %q", back)
	}
	if _, err := Replay(context.Background(), bytes.NewReader([]byte("nope")), NewFramebuffer(), nil, 0); err == nil {
		t.Fatal("expected error for bad magic")
	}
}

func TestReplayDecodesRecordedFrame(t *testing.T) {
	// One DVC15 frame, 4x2: make a red pixel, then copy it left-to-right.
	stream := dvcPkt(0x81, 4, 2, true, true, 0, []byte{0xFC, 0x00, 0x20 | 6})
	var file bytes.Buffer
	rec, err := NewRecorder(&file)
	if err != nil {
		t.Fatal(err)
	}
	rw := &recordingRW{ReadWriter: struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(stream), io.Discard}, rec: rec}
	if _, err := io.Copy(io.Discard, rw); err != nil {
		t.Fatal(err)
	}
	fb := NewFramebuffer()
	st, err := Replay(context.Background(), bytes.NewReader(file.Bytes()), fb, nil, 0)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if w, h := fb.Size(); w != 4 || h != 2 {
		t.Fatalf("size %dx%d", w, h)
	}
	if st.Packets != 1 || fb.Frames() != 1 {
		t.Fatalf("packets %d frames %d", st.Packets, fb.Frames())
	}
	expectPx(t, fb, 0, 0, red15)
	expectPx(t, fb, 3, 0, red15)
}
