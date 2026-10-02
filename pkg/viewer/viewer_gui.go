//go:build gui

package viewer

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Available reports whether this binary was built with the GUI.
const Available = true

type gui struct {
	opts Options
	app  fyne.App
	win  fyne.Window // the window the console is shown in
	// embedded: part of another window's layout (see Panel), with a toolbar
	// in place of the main menu, rather than a window of its own.
	embedded bool
	content  fyne.CanvasObject
	tools    fyne.CanvasObject // embedded only
	popOut   func()            // embedded only: move to a window of its own
	onClose  func()            // own window only: called once as it closes

	screen *screen
	idle   *fyne.Container // stands in for the screen while there is no session
	note   *widget.Label
	act    *widget.Button
	status *widget.Label
	info   *widget.Label
	power  *widget.Label

	ctx  context.Context // ends when the console is closed for good
	done context.CancelFunc

	mu      sync.Mutex
	backend Backend
	unsub   func()
	stop    context.CancelFunc // set while a session is open or being opened
	gen     int                // bumps on every (re)connect

	viewOnly  atomic.Bool
	dirty     atomic.Bool
	painting  atomic.Bool
	connected atomic.Bool
	paused    atomic.Bool // embedded and out of view: nothing to paint
	fps       atomic.Int64

	viewOnlyItem *fyne.MenuItem
	smoothItem   *fyne.MenuItem
	fullItem     *fyne.MenuItem
	menu         *fyne.MainMenu
}

// Run opens a single viewer window as its own application and blocks until
// it is closed. It must be called from the main goroutine.
func Run(o Options) error {
	if o.Connect == nil {
		return errors.New("viewer: Options.Connect is required")
	}
	a := app.NewWithID("io.thecurve.idrac.viewer")
	g := newGUI(o, a, nil)
	g.start()
	if o.ExitAfter > 0 {
		go func() {
			time.Sleep(o.ExitAfter)
			g.ui(g.quit)
		}()
	}
	g.win.Show()
	a.Run()
	return nil
}

// Panel is a console that is part of another window's layout: the manager
// shows one per server as its Console tab. It connects only when asked, and
// can move to a window of its own and back without dropping the session. Its
// methods must be called on the UI goroutine.
type Panel struct {
	g   *gui // the embedded console
	pop *gui // its own window, while popped out
}

// NewPanel builds a console to be shown inside host, which also parents its
// dialogs. Nothing connects until the user asks, or Connect is called.
func NewPanel(a fyne.App, host fyne.Window, o Options) *Panel {
	p := &Panel{g: newGUI(o, a, host)}
	p.g.popOut = p.popOut
	return p
}

// Content is the panel's widget tree, to be placed in the host's layout.
func (p *Panel) Content() fyne.CanvasObject { return p.g.content }

// Connect opens the session unless one is open or on its way.
func (p *Panel) Connect() {
	if p.pop != nil {
		p.pop.start()
		return
	}
	p.g.start()
}

// Connected reports whether a session is open, here or in the panel's window.
func (p *Panel) Connected() bool {
	return p.g.current() != nil || p.pop != nil && p.pop.current() != nil
}

// SetActive tells the panel whether it is in view. Out of view it stops
// painting and gives up the keyboard, so that nothing typed elsewhere reaches
// the server; the session itself stays open.
func (p *Panel) SetActive(on bool) {
	g := p.g
	g.paused.Store(!on)
	if !on {
		g.blur()
		return
	}
	g.dirty.Store(true)
	if g.current() != nil {
		g.focusScreen()
	}
}

// Close ends the session and closes the panel's window, if it has one. The
// panel cannot be used afterwards.
func (p *Panel) Close() {
	if w := p.pop; w != nil {
		p.pop, w.onClose = nil, nil
		w.quit()
	}
	p.g.shut()
}

// popOut moves the console, session included, to a window of its own.
// Closing that window brings it back.
func (p *Panel) popOut() {
	g := p.g
	w := newGUI(g.opts, g.app, nil)
	p.pop = w
	w.onClose = func() {
		p.pop = nil
		g.tools.Show()
		g.offer("Not connected", "Not connected", "Connect")
		handOver(w, g)
	}
	g.tools.Hide()
	handOver(g, w)
	g.status.SetText("In its own window")
	g.info.SetText("")
	g.power.SetText("")
	g.showIdle("The console is open in its own window.", "Bring Back", w.quit)
	w.win.Show()
}

// handOver moves the session and view settings from one console to another.
// A session that was still being opened is opened afresh.
func handOver(from, to *gui) {
	to.viewOnly.Store(from.viewOnly.Load())
	to.viewOnlyItem.Checked = from.viewOnlyItem.Checked
	to.smoothItem.Checked = from.smoothItem.Checked
	to.screen.setSmooth(to.smoothItem.Checked)
	opening := from.busy()
	switch b := from.release(); {
	case b != nil:
		to.adopt(b)
	case opening:
		to.start()
	}
}

// newGUI builds the console's widgets and menus without showing anything, so
// tests can drive it with Fyne's software test driver. Given a host window
// the console is embedded in that window's layout (g.content); without one it
// gets a window of its own.
func newGUI(o Options, a fyne.App, host fyne.Window) *gui {
	if o.Title == "" {
		o.Title = "iDRAC console"
	}
	g := &gui{opts: o, app: a, win: host, embedded: host != nil}
	g.ctx, g.done = context.WithCancel(context.Background())
	g.viewOnly.Store(o.ViewOnly)
	g.screen = newScreen(g)
	g.status = widget.NewLabel("")
	g.info = widget.NewLabel("")
	g.power = widget.NewLabel("")
	g.note = widget.NewLabel("")
	g.note.Alignment = fyne.TextAlignCenter
	g.note.Wrapping = fyne.TextWrapWord
	g.act = widget.NewButton("", nil)
	g.act.Importance = widget.HighImportance
	g.idle = container.NewVBox(layout.NewSpacer(), g.note, container.NewCenter(g.act), layout.NewSpacer())
	view := container.NewStack(g.screen, g.idle)
	bar := container.NewBorder(nil, nil, g.status, container.NewHBox(g.info, g.power))
	g.buildMenu()
	if g.embedded {
		g.paused.Store(true)
		g.tools = g.toolbar()
		g.content = container.NewBorder(g.tools, bar, nil, nil, view)
	} else {
		g.win = a.NewWindow(o.Title + " — iDRAC console")
		g.content = container.NewBorder(nil, bar, nil, nil, view)
		g.win.SetContent(g.content)
		g.win.SetMainMenu(g.menu)
		g.win.Resize(fyne.NewSize(1024, 768+80))
		g.win.SetCloseIntercept(g.quit)
		g.win.SetOnClosed(g.shut)
	}
	g.offer("Not connected", "Not connected", "Connect")
	return g
}

// ui runs fn on the UI goroutine.
func (g *gui) ui(fn func()) {
	if g.opts.Do != nil {
		g.opts.Do(fn)
		return
	}
	fyne.Do(fn)
}

func (g *gui) logf(format string, args ...any) {
	if g.opts.Logger != nil {
		g.opts.Logger.Printf("viewer: "+format, args...)
	}
}

func (g *gui) current() Backend {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.backend
}

// busy reports whether a session is open or being opened.
func (g *gui) busy() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stop != nil
}

// release gives up the session without closing it, so that another console
// can adopt it.
func (g *gui) release() Backend {
	g.mu.Lock()
	b, unsub, stop := g.backend, g.unsub, g.stop
	g.backend, g.unsub, g.stop = nil, nil, nil
	if stop != nil {
		stop() // under the lock: connect relies on it to tell whose session it has
	}
	g.mu.Unlock()
	g.connected.Store(false)
	if unsub != nil {
		unsub()
	}
	if b != nil {
		b.ReleaseAllKeys()
	}
	return b
}

func (g *gui) closeBackend() {
	if b := g.release(); b != nil {
		b.Close()
	}
}

// quit closes the console's own window.
func (g *gui) quit() {
	g.shut()
	g.win.Close()
}

// shut ends the console for good.
func (g *gui) shut() {
	if g.ctx.Err() != nil {
		return
	}
	if g.onClose != nil {
		g.onClose() // the panel takes the session back
	}
	g.done()
	g.closeBackend()
}

func (g *gui) focusScreen() {
	if !g.paused.Load() {
		g.win.Canvas().Focus(g.screen)
	}
}

// blur gives up the keyboard, which also releases any held keys.
func (g *gui) blur() {
	if c := g.win.Canvas(); c.Focused() == fyne.Focusable(g.screen) {
		c.Unfocus()
	}
}

// tapGuard is how long the idle pane's button ignores taps after changing.
const tapGuard = 500 * time.Millisecond

// showIdle replaces the screen with a note and one button. The button often
// takes the place of the one just tapped (Connect becomes Cancel, and back),
// so it ignores taps for a moment: the second of a double tap is not for it.
func (g *gui) showIdle(note, button string, tapped func()) {
	shown := time.Now()
	g.note.SetText(note)
	g.act.OnTapped = func() {
		if time.Since(shown) >= tapGuard {
			tapped()
		}
	}
	g.act.SetText(button)
	g.screen.Hide()
	g.idle.Show()
	g.idle.Refresh() // lay out again around the new texts
}

// offer says why there is no session and offers to open one.
func (g *gui) offer(status, why, button string) {
	if g.busy() {
		return
	}
	g.status.SetText(status)
	g.info.SetText("")
	g.power.SetText("")
	g.showIdle(why, button, g.start)
}

// begin claims the session and starts the loops that last as long as it does.
// It returns nil if there is a session already.
func (g *gui) begin() context.Context {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stop != nil || g.ctx.Err() != nil {
		return nil
	}
	ctx, stop := context.WithCancel(g.ctx)
	g.stop = stop
	go g.paintLoop(ctx)
	go g.statusLoop(ctx)
	go g.screen.motionLoop(ctx)
	return ctx
}

// start opens a session unless one is open or on its way.
func (g *gui) start() {
	ctx := g.begin()
	if ctx == nil {
		return
	}
	g.status.SetText("Connecting…")
	g.showIdle("Connecting…", "Cancel", g.disconnect)
	go g.connect(ctx)
}

// adopt takes over a session that another console released.
func (g *gui) adopt(b Backend) {
	ctx := g.begin()
	if ctx == nil {
		b.Close()
		return
	}
	g.mu.Lock()
	g.attach(b)
	g.mu.Unlock()
	go g.watch(ctx, b)
}

func (g *gui) disconnect() {
	g.closeBackend()
	g.offer("Not connected", "Not connected", "Connect")
}

// attach makes b the session; g.mu must be held.
func (g *gui) attach(b Backend) {
	g.backend = b
	g.gen++
	g.unsub = b.Framebuffer().Subscribe(func(image.Rectangle) { g.dirty.Store(true) })
}

// connect opens the session and watches for its end.
func (g *gui) connect(ctx context.Context) {
	b, err := g.opts.Connect(ctx)
	g.mu.Lock()
	if ctx.Err() != nil { // cancelled, handed over or closed meanwhile
		g.mu.Unlock()
		if err == nil {
			b.Close()
		}
		return
	}
	if err != nil {
		stop := g.stop
		g.stop = nil
		g.mu.Unlock()
		stop()
		g.logf("connect: %v", err)
		g.ui(func() {
			g.offer("Not connected", fmt.Sprintf("Could not open the console:\n\n%v", err), "Reconnect")
		})
		return
	}
	g.attach(b)
	g.mu.Unlock()
	g.watch(ctx, b)
}

// watch shows the session's screen and waits for the session to end.
func (g *gui) watch(ctx context.Context, b Backend) {
	g.connected.Store(true)
	g.dirty.Store(true)
	g.ui(func() {
		if g.current() != b {
			return
		}
		g.status.SetText("Connected")
		g.idle.Hide()
		g.screen.Show()
		g.focusScreen()
	})
	select {
	case <-ctx.Done():
		return
	case <-b.Done():
	}
	if g.current() != b {
		return // replaced, handed over or closed deliberately
	}
	reason := "The console session ended."
	if err := b.Err(); err != nil {
		reason = fmt.Sprintf("The console session ended:\n\n%v", err)
	}
	g.closeBackend()
	g.ui(func() { g.offer("Disconnected", reason, "Reconnect") })
}

// paintLoop copies the framebuffer to the screen at most ~30 times a second.
func (g *gui) paintLoop(ctx context.Context) {
	t := time.NewTicker(33 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if g.paused.Load() || !g.dirty.Load() || g.painting.Load() {
			continue
		}
		b := g.current()
		if b == nil {
			continue
		}
		g.dirty.Store(false)
		g.painting.Store(true)
		fb := b.Framebuffer()
		g.ui(func() {
			defer g.painting.Store(false)
			g.screen.update(fb)
		})
	}
}

// statusLoop refreshes the status bar: resolution, frame rate, power state.
func (g *gui) statusLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastFrames uint64
	var lastGen int
	tick := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tick++
		b := g.current()
		text := ""
		if b != nil {
			fb := b.Framebuffer()
			w, h := fb.Size()
			frames := fb.Frames()
			g.mu.Lock()
			gen := g.gen
			g.mu.Unlock()
			if gen != lastGen {
				lastGen, lastFrames = gen, frames
			}
			fps := frames - lastFrames
			lastFrames = frames
			text = fmt.Sprintf("%d×%d   %d fps", w, h, fps)
			if g.viewOnly.Load() {
				text += "   view only"
			}
		}
		g.ui(func() {
			if ctx.Err() == nil {
				g.info.SetText(text)
			}
		})
		// Only once connected: asking any earlier could prompt for the
		// password a second time while Connect is still asking.
		if b != nil && g.opts.Actions.PowerState != nil && tick%20 == 1 {
			go g.refreshPower(ctx)
		}
	}
}

func (g *gui) refreshPower(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	st, err := g.opts.Actions.PowerState(cctx)
	if err != nil {
		g.logf("power state: %v", err)
		return
	}
	g.ui(func() {
		if ctx.Err() == nil {
			g.power.SetText("Power: " + st)
		}
	})
}

// ---- menus ----

func (g *gui) buildMenu() {
	file := fyne.NewMenu("File",
		fyne.NewMenuItem("Save Screenshot", g.saveScreenshot),
		fyne.NewMenuItem("Save Screenshot As…", g.saveScreenshotAs),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Paste Clipboard as Keystrokes", g.pasteText),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Reconnect", func() {
			g.closeBackend()
			g.start()
		}),
		fyne.NewMenuItem("Disconnect", g.disconnect),
	)

	g.viewOnlyItem = fyne.NewMenuItem("View Only", func() {
		v := !g.viewOnly.Load()
		g.viewOnly.Store(v)
		if v {
			if b := g.current(); b != nil {
				b.ReleaseAllKeys()
			}
		}
		g.viewOnlyItem.Checked = v
		g.menu.Refresh()
	})
	g.viewOnlyItem.Checked = g.viewOnly.Load()
	g.smoothItem = fyne.NewMenuItem("Smooth Scaling", func() {
		g.smoothItem.Checked = !g.smoothItem.Checked
		g.screen.setSmooth(g.smoothItem.Checked)
		g.menu.Refresh()
	})
	g.smoothItem.Checked = true
	view := fyne.NewMenu("View",
		fyne.NewMenuItem("Refresh Screen", func() {
			if b := g.current(); b != nil {
				go b.Refresh()
			}
		}),
	)
	if !g.embedded { // these two act on the window
		g.fullItem = fyne.NewMenuItem("Full Screen", func() {
			full := !g.win.FullScreen()
			g.win.SetFullScreen(full)
			g.fullItem.Checked = full
			g.menu.Refresh()
		})
		view.Items = append(view.Items, fyne.NewMenuItem("Actual Size (1:1)", g.actualSize), g.fullItem)
	}
	view.Items = append(view.Items, g.smoothItem, fyne.NewMenuItemSeparator(), g.viewOnlyItem)

	var macroItems []*fyne.MenuItem
	for _, m := range Macros {
		macroItems = append(macroItems, g.macroItem(m))
	}
	alt, ctrlAlt := FunctionKeyMacros()
	altItem := fyne.NewMenuItem("Alt+F?", nil)
	altItem.ChildMenu = fyne.NewMenu("")
	for _, m := range alt {
		altItem.ChildMenu.Items = append(altItem.ChildMenu.Items, g.macroItem(m))
	}
	caItem := fyne.NewMenuItem("Ctrl+Alt+F?", nil)
	caItem.ChildMenu = fyne.NewMenu("")
	for _, m := range ctrlAlt {
		caItem.ChildMenu.Items = append(caItem.ChildMenu.Items, g.macroItem(m))
	}
	macroItems = append(macroItems, fyne.NewMenuItemSeparator(), altItem, caItem,
		fyne.NewMenuItemSeparator(), fyne.NewMenuItem("Release All Keys", func() {
			if b := g.current(); b != nil {
				go b.ReleaseAllKeys()
			}
		}))
	macros := fyne.NewMenu("Macros", macroItems...)

	menus := []*fyne.Menu{file, view, macros}

	a := g.opts.Actions
	if a.Power != nil {
		p := func(label, action, warn string) *fyne.MenuItem {
			return fyne.NewMenuItem(label, func() { g.confirmAction(label, warn, func(ctx context.Context) error { return a.Power(ctx, action) }) })
		}
		menus = append(menus, fyne.NewMenu("Power",
			p("Power On", "on", "Power the server on?"),
			p("Graceful Shutdown", "graceful", "Ask the operating system to shut down?"),
			p("Power Off (forced)", "off", "Force the server off immediately? Unsaved data will be lost."),
			fyne.NewMenuItemSeparator(),
			p("Reset (warm boot)", "reset", "Reset the server immediately? Unsaved data will be lost."),
			p("Power Cycle (cold boot)", "cycle", "Power-cycle the server immediately? Unsaved data will be lost."),
			p("NMI", "nmi", "Send a non-maskable interrupt to the server?"),
		))
	}
	if a.BootOnce != nil {
		bt := func(label, target string) *fyne.MenuItem {
			return fyne.NewMenuItem(label, func() {
				g.runAction("Next boot: "+label, func(ctx context.Context) error { return a.BootOnce(ctx, target) })
			})
		}
		menus = append(menus, fyne.NewMenu("Next Boot",
			bt("Normal Boot", "none"), bt("PXE", "pxe"), bt("BIOS Setup", "bios"), bt("CD/DVD", "cd"), bt("Hard Disk", "hdd"),
		))
	}

	tools := []*fyne.MenuItem{fyne.NewMenuItem("Session Statistics", g.showStats)}
	if a.Identify != nil {
		tools = append(tools, fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Identify LED On", func() {
				g.runAction("Identify LED on", func(ctx context.Context) error { return a.Identify(ctx, true) })
			}),
			fyne.NewMenuItem("Identify LED Off", func() {
				g.runAction("Identify LED off", func(ctx context.Context) error { return a.Identify(ctx, false) })
			}))
	}
	menus = append(menus, fyne.NewMenu("Tools", tools...))
	if !g.embedded { // the host window has its own Help
		menus = append(menus, fyne.NewMenu("Help", fyne.NewMenuItem("About", func() {
			dialog.ShowInformation("About", "idrac viewer\n\nNative Go console for Dell iDRAC6/7/8.\nKeys the window manager intercepts (Super, Alt+Tab,\nCtrl+Alt+Del, …) are in the Macros menu.", g.win)
		})))
	}

	g.menu = fyne.NewMainMenu(menus...)
}

// toolbar stands in for the main menu of an embedded console, which has no
// window to hang one on: a button per menu, and the way out to a window.
func (g *gui) toolbar() fyne.CanvasObject {
	left := container.NewHBox()
	for _, m := range g.menu.Items {
		var b *widget.Button
		b = widget.NewButton(m.Label, func() {
			widget.ShowPopUpMenuAtRelativePosition(m, g.win.Canvas(), fyne.NewPos(0, b.Size().Height), b)
		})
		b.Importance = widget.LowImportance
		left.Add(b)
	}
	pop := widget.NewButtonWithIcon("Pop Out", theme.ViewFullScreenIcon(), func() { g.popOut() })
	return container.NewBorder(nil, nil, left, pop)
}

func (g *gui) macroItem(m Macro) *fyne.MenuItem {
	return fyne.NewMenuItem(m.Name, func() {
		b := g.current()
		if b == nil || g.viewOnly.Load() {
			return
		}
		go func() {
			if err := b.Chord(m.Usages...); err != nil {
				g.logf("macro %s: %v", m.Name, err)
			}
		}()
		g.focusScreen()
	})
}

func (g *gui) confirmAction(title, question string, fn func(context.Context) error) {
	dialog.ShowConfirm(title, question, func(yes bool) {
		if yes {
			g.runAction(title, fn)
		}
		g.focusScreen()
	}, g.win)
}

// runAction runs a management call off the UI thread and reports failures.
func (g *gui) runAction(title string, fn func(context.Context) error) {
	g.status.SetText(title + "…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		err := fn(ctx)
		g.ui(func() {
			if err != nil {
				g.status.SetText(title + ": failed")
				dialog.ShowError(fmt.Errorf("%s: %w", title, err), g.win)
				return
			}
			g.status.SetText(title + ": done")
		})
		if g.opts.Actions.PowerState != nil {
			time.Sleep(3 * time.Second)
			g.refreshPower(context.Background())
		}
	}()
}

func (g *gui) snapshot() *image.RGBA {
	b := g.current()
	if b == nil {
		return nil
	}
	return b.Framebuffer().Snapshot()
}

func (g *gui) saveScreenshot() {
	img := g.snapshot()
	if img == nil {
		return
	}
	dir := g.opts.ScreenshotDir
	if dir == "" {
		dir = "."
	}
	name := strings.Map(func(r rune) rune {
		if r == ' ' || r == '(' || r == ')' || r == '/' {
			return '-'
		}
		return r
	}, g.opts.Title)
	path := filepath.Join(dir, fmt.Sprintf("idrac-%s-%s.png", strings.Trim(name, "-"), time.Now().Format("20060102-150405")))
	f, err := os.Create(path)
	if err == nil {
		err = png.Encode(f, img)
		f.Close()
	}
	if err != nil {
		dialog.ShowError(err, g.win)
		return
	}
	abs, _ := filepath.Abs(path)
	g.status.SetText("Saved " + abs)
}

func (g *gui) saveScreenshotAs() {
	img := g.snapshot()
	if img == nil {
		return
	}
	d := dialog.NewFileSave(func(w fyne.URIWriteCloser, err error) {
		if err != nil || w == nil {
			return
		}
		defer w.Close()
		if err := png.Encode(w, img); err != nil {
			dialog.ShowError(err, g.win)
			return
		}
		g.status.SetText("Saved " + w.URI().Path())
	}, g.win)
	d.SetFileName("idrac-screenshot.png")
	d.Show()
}

func (g *gui) pasteText() {
	b := g.current()
	if b == nil || g.viewOnly.Load() {
		return
	}
	text := g.app.Clipboard().Content()
	if text == "" {
		g.status.SetText("Clipboard is empty")
		return
	}
	send := func() {
		g.status.SetText(fmt.Sprintf("Typing %d characters…", len([]rune(text))))
		go func() {
			err := b.TypeString(text, 25*time.Millisecond)
			g.ui(func() {
				if err != nil {
					dialog.ShowError(err, g.win)
					return
				}
				g.status.SetText("Clipboard typed")
			})
		}()
	}
	if len(text) > 200 || strings.ContainsAny(text, "\n\r") {
		preview := text
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
		dialog.ShowConfirm("Paste as keystrokes", fmt.Sprintf("Type %d characters into the remote console?\n\n%s", len([]rune(text)), preview), func(yes bool) {
			if yes {
				send()
			}
			g.focusScreen()
		}, g.win)
		return
	}
	send()
}

func (g *gui) actualSize() {
	b := g.current()
	if b == nil {
		return
	}
	w, h := b.Framebuffer().Size()
	if g.win.FullScreen() {
		g.win.SetFullScreen(false)
		g.fullItem.Checked = false
		g.menu.Refresh()
	}
	scale := g.win.Canvas().Scale()
	chrome := g.win.Canvas().Size().Height - g.screen.Size().Height
	g.win.Resize(fyne.NewSize(float32(w)/scale, float32(h)/scale+chrome))
}

func (g *gui) showStats() {
	b := g.current()
	text := "Not connected."
	if b != nil {
		text = b.Stats()
	}
	lbl := widget.NewLabel(text)
	lbl.TextStyle = fyne.TextStyle{Monospace: true}
	d := dialog.NewCustom("Session Statistics", "Close", container.NewVScroll(lbl), g.win)
	d.Resize(fyne.NewSize(520, 420))
	d.Show()
}

// ---- the screen widget ----

// screen shows the remote framebuffer and captures keyboard and mouse.
type screen struct {
	widget.BaseWidget
	g    *gui
	img  *canvas.Image
	bg   *canvas.Rectangle
	rgba *image.RGBA
	keys *keyMapper

	fbW, fbH int

	pmu      sync.Mutex
	px, py   int
	buttons  uint8
	moved    bool
	lastSent time.Time
	held     map[uint16]bool
}

func newScreen(g *gui) *screen {
	s := &screen{g: g, keys: newKeyMapper(runtime.GOOS == "linux"), held: map[uint16]bool{}}
	s.rgba = image.NewRGBA(image.Rect(0, 0, 1024, 768))
	s.img = canvas.NewImageFromImage(s.rgba)
	s.img.FillMode = canvas.ImageFillContain
	s.img.ScaleMode = canvas.ImageScaleSmooth
	s.bg = canvas.NewRectangle(color.Black) // letterbox bars
	s.fbW, s.fbH = 1024, 768
	s.ExtendBaseWidget(s)
	return s
}

func (s *screen) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(container.NewStack(s.bg, s.img))
}

func (s *screen) MinSize() fyne.Size { return fyne.NewSize(320, 200) }

func (s *screen) setSmooth(on bool) {
	if on {
		s.img.ScaleMode = canvas.ImageScaleSmooth
	} else {
		s.img.ScaleMode = canvas.ImageScalePixels
	}
	s.img.Refresh()
}

// update copies the framebuffer into the displayed image (UI thread only).
func (s *screen) update(fb interface {
	Size() (int, int)
	CopyRect(*image.RGBA, image.Rectangle)
}) {
	w, h := fb.Size()
	if w <= 0 || h <= 0 {
		return
	}
	if s.rgba.Rect.Dx() != w || s.rgba.Rect.Dy() != h {
		s.rgba = image.NewRGBA(image.Rect(0, 0, w, h))
		s.img.Image = s.rgba
		s.fbW, s.fbH = w, h
	}
	fb.CopyRect(s.rgba, s.rgba.Rect)
	s.img.Refresh()
}

// toRemote converts a widget position to framebuffer pixels, accounting for
// the letterboxed, aspect-preserving image placement.
func (s *screen) toRemote(p fyne.Position) (int, int) {
	size := s.Size()
	fw, fh := float32(s.fbW), float32(s.fbH)
	if size.Width <= 0 || size.Height <= 0 || fw <= 0 || fh <= 0 {
		return 0, 0
	}
	scale := size.Width / fw
	if hs := size.Height / fh; hs < scale {
		scale = hs
	}
	ox := (size.Width - fw*scale) / 2
	oy := (size.Height - fh*scale) / 2
	x := int((p.X - ox) / scale)
	y := int((p.Y - oy) / scale)
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x >= s.fbW {
		x = s.fbW - 1
	}
	if y >= s.fbH {
		y = s.fbH - 1
	}
	return x, y
}

func (s *screen) active() Backend {
	if s.g.viewOnly.Load() {
		return nil
	}
	return s.g.current()
}

// motionLoop sends pointer motion at a bounded rate so a fast mouse does not
// flood the console connection.
func (s *screen) motionLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.pmu.Lock()
		if !s.moved {
			s.pmu.Unlock()
			continue
		}
		s.moved = false
		x, y, m := s.px, s.py, s.buttons
		s.pmu.Unlock()
		if b := s.active(); b != nil {
			b.PointerEvent(x, y, m)
		}
	}
}

func (s *screen) pointerNow(extra uint8) {
	s.pmu.Lock()
	s.moved = false
	x, y, m := s.px, s.py, s.buttons|extra
	s.pmu.Unlock()
	if b := s.active(); b != nil {
		if err := b.PointerEvent(x, y, m); err != nil {
			s.g.logf("pointer: %v", err)
		}
	}
}

// desktop.Hoverable
func (s *screen) MouseIn(e *desktop.MouseEvent) { s.MouseMoved(e) }
func (s *screen) MouseOut()                     {}
func (s *screen) MouseMoved(e *desktop.MouseEvent) {
	x, y := s.toRemote(e.Position)
	s.pmu.Lock()
	if x != s.px || y != s.py {
		s.px, s.py, s.moved = x, y, true
	}
	s.pmu.Unlock()
}

func buttonBit(b desktop.MouseButton) uint8 {
	switch b {
	case desktop.MouseButtonPrimary:
		return 1
	case desktop.MouseButtonTertiary:
		return 2
	case desktop.MouseButtonSecondary:
		return 4
	}
	return 0
}

// desktop.Mouseable
func (s *screen) MouseDown(e *desktop.MouseEvent) {
	s.g.win.Canvas().Focus(s)
	x, y := s.toRemote(e.Position)
	s.pmu.Lock()
	s.px, s.py = x, y
	s.buttons |= buttonBit(e.Button)
	s.pmu.Unlock()
	s.pointerNow(0)
}

func (s *screen) MouseUp(e *desktop.MouseEvent) {
	x, y := s.toRemote(e.Position)
	s.pmu.Lock()
	s.px, s.py = x, y
	s.buttons &^= buttonBit(e.Button)
	s.pmu.Unlock()
	s.pointerNow(0)
}

// fyne.Scrollable
func (s *screen) Scrolled(e *fyne.ScrollEvent) {
	var bit uint8
	switch {
	case e.Scrolled.DY > 0:
		bit = 8
	case e.Scrolled.DY < 0:
		bit = 16
	default:
		return
	}
	s.pointerNow(bit)
	s.pointerNow(0)
}

// fyne.Focusable
func (s *screen) FocusGained() {}
func (s *screen) FocusLost() {
	s.pmu.Lock()
	had := len(s.held) > 0
	s.held = map[uint16]bool{}
	s.pmu.Unlock()
	if had {
		if b := s.g.current(); b != nil {
			go b.ReleaseAllKeys()
		}
	}
}
func (s *screen) TypedRune(rune)          {}
func (s *screen) TypedKey(*fyne.KeyEvent) {}

// fyne.Tabbable: keep Tab for the remote machine instead of focus traversal.
func (s *screen) AcceptsTab() bool { return true }

// desktop.Keyable
func (s *screen) KeyDown(e *fyne.KeyEvent) {
	u, ok := s.keys.usage(string(e.Name), e.Physical.ScanCode)
	if !ok {
		s.g.logf("unmapped key %q scancode %d", e.Name, e.Physical.ScanCode)
		return
	}
	b := s.active()
	if b == nil {
		return
	}
	s.pmu.Lock()
	already := s.held[u]
	s.held[u] = true
	s.pmu.Unlock()
	if already {
		return // auto-repeat: the remote side repeats on its own
	}
	if err := b.KeyDown(u); err != nil {
		s.g.logf("key down %#x: %v", u, err)
	}
}

func (s *screen) KeyUp(e *fyne.KeyEvent) {
	u, ok := s.keys.usage(string(e.Name), e.Physical.ScanCode)
	if !ok {
		return
	}
	s.pmu.Lock()
	was := s.held[u]
	delete(s.held, u)
	s.pmu.Unlock()
	if !was {
		return
	}
	if b := s.g.current(); b != nil {
		if err := b.KeyUp(u); err != nil {
			s.g.logf("key up %#x: %v", u, err)
		}
	}
}

// desktop.Cursorable: a crosshair makes the local pointer easy to tell apart
// from the remote one.
func (s *screen) Cursor() desktop.Cursor { return desktop.CrosshairCursor }
