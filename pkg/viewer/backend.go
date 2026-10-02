// Package viewer is the integrated graphical console: the remote screen with
// keyboard/mouse capture, a macros menu, power control, next-boot selection,
// screenshots and session statistics — the functionality of Dell's Java
// viewer, without Java. It runs as a native window (Run) or as part of
// another window's layout (Panel).
//
// The GUI itself (viewer_gui.go) needs cgo and OpenGL and is compiled only
// with the "gui" build tag; this file and keys.go are toolkit-independent.
package viewer

import (
	"context"
	"time"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/kvm"
)

// Backend is a live console session as the viewer sees it.
type Backend interface {
	Framebuffer() *kvm.Framebuffer
	KeyDown(usage uint16) error
	KeyUp(usage uint16) error
	Chord(usages ...uint16) error
	// PointerEvent takes framebuffer coordinates and an RFB-style button mask
	// (bit0 left, bit1 middle, bit2 right, bit3 wheel up, bit4 wheel down).
	PointerEvent(x, y int, mask uint8) error
	ReleaseAllKeys() error
	TypeString(s string, delay time.Duration) error
	Refresh() error
	// Stats returns human-readable session statistics.
	Stats() string
	// Done is closed when the session has ended; Err then says why.
	Done() <-chan struct{}
	Err() error
	Close() error
}

// Actions are optional out-of-band management operations (Redfish or the
// legacy web API). Nil functions hide the corresponding menu entries.
type Actions struct {
	PowerState func(ctx context.Context) (string, error)
	// Power takes: on, off, graceful, reset, cycle, nmi.
	Power func(ctx context.Context, action string) error
	// BootOnce takes: none, pxe, bios, cd, hdd.
	BootOnce func(ctx context.Context, target string) error
	Identify func(ctx context.Context, on bool) error
}

// Options configures Run and NewPanel.
type Options struct {
	Title string // window title, e.g. "server01 (192.0.2.10)"
	// Connect opens (or re-opens) the console session.
	Connect  func(ctx context.Context) (Backend, error)
	Actions  Actions
	ViewOnly bool
	Logger   kvm.Logger
	// ScreenshotDir is where File > Save Screenshot writes (default: cwd).
	ScreenshotDir string

	// Do, if set, runs the viewer's callbacks on the UI goroutine in place of
	// fyne.Do; a host that serialises its own UI callbacks passes that here.
	Do func(func())

	// ExitAfter makes Run close the window after this long (smoke tests).
	ExitAfter time.Duration
}
