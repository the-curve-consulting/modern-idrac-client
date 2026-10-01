package kvm

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"strings"
)

// Logger is the minimal logging contract used throughout package kvm.
// *log.Logger satisfies it.
type Logger interface {
	Printf(format string, args ...any)
}

// nopLogger discards everything.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// NewLogger returns a Logger writing to w, or a no-op logger when w is nil.
func NewLogger(w io.Writer, prefix string) Logger {
	if w == nil {
		return nopLogger{}
	}
	return log.New(w, prefix, log.Ltime|log.Lmicroseconds)
}

// HexDump formats b as a compact multi-line hex dump for trace output.
// Only the first max bytes are shown (max <= 0 means all).
func HexDump(b []byte, max int) string {
	if max > 0 && len(b) > max {
		return strings.TrimRight(hex.Dump(b[:max]), "\n") + fmt.Sprintf("\n... (%d more bytes)", len(b)-max)
	}
	return strings.TrimRight(hex.Dump(b), "\n")
}
