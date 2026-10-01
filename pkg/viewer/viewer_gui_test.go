//go:build gui

package viewer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/kvm"
)

type fakeBackend struct {
	mu     sync.Mutex
	fb     *kvm.Framebuffer
	events []string
	done   chan struct{}
}

func newFake() *fakeBackend {
	return &fakeBackend{fb: kvm.NewFramebuffer(), done: make(chan struct{})}
}
func (f *fakeBackend) add(format string, a ...any) error {
	f.mu.Lock()
	f.events = append(f.events, fmt.Sprintf(format, a...))
	f.mu.Unlock()
	return nil
}
func (f *fakeBackend) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.events
	f.events = nil
	return e
}
func (f *fakeBackend) Framebuffer() *kvm.Framebuffer              { return f.fb }
func (f *fakeBackend) KeyDown(u uint16) error                     { return f.add("down %#x", u) }
func (f *fakeBackend) KeyUp(u uint16) error                       { return f.add("up %#x", u) }
func (f *fakeBackend) Chord(u ...uint16) error                    { return f.add("chord %v", u) }
func (f *fakeBackend) PointerEvent(x, y int, m uint8) error       { return f.add("ptr %d,%d,%d", x, y, m) }
func (f *fakeBackend) ReleaseAllKeys() error                      { return f.add("release") }
func (f *fakeBackend) TypeString(s string, _ time.Duration) error { return f.add("type %s", s) }
func (f *fakeBackend) Refresh() error                             { return f.add("refresh") }
func (f *fakeBackend) Stats() string                              { return "stats" }
func (f *fakeBackend) Done() <-chan struct{}                      { return f.done }
func (f *fakeBackend) Err() error                                 { return nil }
func (f *fakeBackend) Close() error                               { return nil }

func setup(t *testing.T, o Options) (*gui, *fakeBackend) {
	t.Helper()
	a := test.NewApp()
	t.Cleanup(a.Quit)
	g := newGUI(o, a)
	f := newFake()
	g.mu.Lock()
	g.backend = f
	g.mu.Unlock()
	return g, f
}

func eq(t *testing.T, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
}

func TestPointerMappingLetterboxed(t *testing.T) {
	g, f := setup(t, Options{})
	s := g.screen
	// 1024x768 remote shown in a 1024x384 widget: scale 0.5, image 512 wide, 256 px bars either side.
	s.Resize(fyne.NewSize(1024, 384))
	for _, c := range []struct {
		px, py float32
		x, y   int
	}{{256, 0, 0, 0}, {512, 192, 512, 384}, {0, 0, 0, 0}, {1024, 384, 1023, 767}, {767.5, 383.5, 1023, 767}} {
		if x, y := s.toRemote(fyne.NewPos(c.px, c.py)); x != c.x || y != c.y {
			t.Fatalf("toRemote(%v,%v) = %d,%d want %d,%d", c.px, c.py, x, y, c.x, c.y)
		}
	}
	ev := func(x, y float32, b desktop.MouseButton) *desktop.MouseEvent {
		return &desktop.MouseEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(x, y)}, Button: b}
	}
	s.MouseDown(ev(512, 192, desktop.MouseButtonPrimary))
	s.MouseUp(ev(512, 192, desktop.MouseButtonPrimary))
	s.MouseDown(ev(512, 192, desktop.MouseButtonSecondary))
	s.MouseUp(ev(512, 192, desktop.MouseButtonSecondary))
	eq(t, f.take(), "ptr 512,384,1", "ptr 512,384,0", "ptr 512,384,4", "ptr 512,384,0")
	s.Scrolled(&fyne.ScrollEvent{Scrolled: fyne.Delta{DY: 10}})
	s.Scrolled(&fyne.ScrollEvent{Scrolled: fyne.Delta{DY: -10}})
	eq(t, f.take(), "ptr 512,384,8", "ptr 512,384,0", "ptr 512,384,16", "ptr 512,384,0")

	// Motion is rate limited: many moves, one event with the last position.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.motionLoop(ctx)
	for i := 0; i < 50; i++ {
		s.MouseMoved(ev(256+float32(i), 10, 0))
	}
	time.Sleep(120 * time.Millisecond)
	got := f.take()
	if len(got) == 0 || len(got) > 6 || got[len(got)-1] != "ptr 98,20,0" {
		t.Fatalf("throttled motion: %v", got)
	}
}

func TestKeyboardForwarding(t *testing.T) {
	g, f := setup(t, Options{})
	s := g.screen
	key := func(name fyne.KeyName, sc int) *fyne.KeyEvent {
		return &fyne.KeyEvent{Name: name, Physical: fyne.HardwareKey{ScanCode: sc}}
	}
	s.KeyDown(key(fyne.KeyA, 38))
	s.KeyDown(key(fyne.KeyA, 38)) // auto-repeat is swallowed
	s.KeyUp(key(fyne.KeyA, 38))
	s.KeyUp(key(fyne.KeyA, 38)) // stray release ignored
	s.KeyDown(key(desktop.KeyControlLeft, 37))
	s.KeyDown(key(fyne.KeyTab, 23))
	eq(t, f.take(), "down 0x4", "up 0x4", "down 0xe0", "down 0x2b")
	if !s.AcceptsTab() {
		t.Fatal("Tab must go to the remote side")
	}
	s.FocusLost() // held keys must not stay stuck
	time.Sleep(50 * time.Millisecond)
	eq(t, f.take(), "release")

	g.viewOnly.Store(true)
	s.KeyDown(key(fyne.KeyB, 56))
	s.MouseDown(&desktop.MouseEvent{Button: desktop.MouseButtonPrimary})
	eq(t, f.take())
}

func TestMenusFollowActions(t *testing.T) {
	names := func(g *gui) (out []string) {
		for _, m := range g.menu.Items {
			out = append(out, m.Label)
		}
		return
	}
	g, f := setup(t, Options{})
	eq(t, names(g), "File", "View", "Macros", "Tools", "Help")
	// A macro sends its chord.
	g.menu.Items[2].Items[0].Action()
	time.Sleep(50 * time.Millisecond)
	eq(t, f.take(), "chord [224 226 76]")

	nop := func(context.Context, string) error { return nil }
	g2, _ := setup(t, Options{Actions: Actions{Power: nop, BootOnce: nop, Identify: func(context.Context, bool) error { return nil }}})
	eq(t, names(g2), "File", "View", "Macros", "Power", "Next Boot", "Tools", "Help")
}
