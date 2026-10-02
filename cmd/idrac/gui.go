//go:build gui

package main

import (
	"context"
	"errors"
	"fmt"
	"image/color"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/config"
	"github.com/the-curve-consulting/modern-idrac-client/pkg/kvm"
	"github.com/the-curve-consulting/modern-idrac-client/pkg/redfish"
	"github.com/the-curve-consulting/modern-idrac-client/pkg/viewer"
	"github.com/the-curve-consulting/modern-idrac-client/pkg/webapi"
)

// The manager is the graphical front end to everything the CLI does: a list
// of iDRACs on the left, and for the selected one a set of tabs that each run
// the corresponding CLI command in-process (runCaptured) and present its
// output. Nothing here talks to an iDRAC by itself, so GUI and CLI cannot
// drift apart.

type probeResult struct {
	done      bool
	reachable bool
	gen       config.Generation
	detail    string
}

type manager struct {
	g   *globals
	app fyne.App
	win fyne.Window

	names    []string
	list     *widget.List
	selected string
	detail   *fyne.Container
	status   *widget.Label
	busy     *widget.ProgressBarInfinite
	busyN    int
	tabs     *container.AppTabs // tabs of the selected host
	pages    []*page

	mu        sync.Mutex
	passwords map[string]string // entered or resolved this session
	unsaved   map[string]string // entered ones to write to the hosts file once accepted
	probes    map[string]probeResult
	// ask asks the user for a host's password, and whether to save it.
	ask      func(h *config.Host) (pw string, save bool, err error)
	consoles map[string]*viewer.Panel // by host; they outlive host selection
}

// runGUI opens the manager window and blocks until it is closed.
func runGUI(g *globals) error {
	a := app.NewWithID("io.thecurve.idrac")
	m := newManager(g, a)
	m.win.SetMaster()
	// The manager asks for missing passwords itself (hostFor), so that it
	// can offer to save them.
	config.PromptPassword = nil
	if d, err := time.ParseDuration(os.Getenv("IDRAC_GUI_EXIT_AFTER")); err == nil && d > 0 {
		go func() { // smoke tests
			time.Sleep(d)
			fyne.Do(a.Quit)
		}()
	}
	m.win.ShowAndRun()
	return nil
}

func newManager(g *globals, a fyne.App) *manager {
	m := &manager{g: g, app: a, passwords: map[string]string{}, unsaved: map[string]string{}, probes: map[string]probeResult{}, consoles: map[string]*viewer.Panel{}}
	m.ask = m.askPassword
	m.win = a.NewWindow("iDRAC Manager")
	m.win.SetOnClosed(func() { // log out of the consoles: an iDRAC has few slots
		for name := range m.consoles {
			m.dropConsole(name)
		}
	})
	m.status = widget.NewLabel("Ready")
	m.status.Truncation = fyne.TextTruncateEllipsis
	m.busy = widget.NewProgressBarInfinite()
	m.busy.Hide()
	m.detail = container.NewStack()

	m.list = widget.NewList(
		func() int { return len(m.names) },
		func() fyne.CanvasObject {
			dot := canvas.NewText("●", color.Gray{Y: 128})
			dot.TextSize = theme.TextSize()
			name := widget.NewLabelWithStyle("name", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
			sub := canvas.NewText("address", theme.Color(theme.ColorNamePlaceHolder))
			sub.TextSize = theme.CaptionTextSize()
			return container.NewBorder(nil, nil, container.NewCenter(dot), nil,
				container.NewVBox(name, container.NewPadded(sub)))
		},
		m.updateListItem,
	)
	m.list.OnSelected = func(id widget.ListItemID) {
		if id >= 0 && id < len(m.names) {
			m.selectHost(m.names[id])
		}
	}

	tools := widget.NewToolbar(
		widget.NewToolbarAction(theme.ContentAddIcon(), func() { m.editHost("") }),
		widget.NewToolbarAction(theme.DocumentCreateIcon(), func() {
			if m.selected != "" {
				m.editHost(m.selected)
			}
		}),
		widget.NewToolbarAction(theme.DeleteIcon(), m.removeHost),
		widget.NewToolbarSpacer(),
		widget.NewToolbarAction(theme.ViewRefreshIcon(), m.probeAll),
	)
	left := container.NewBorder(tools, nil, nil, nil, m.list)
	split := container.NewHSplit(left, m.detail)
	bar := container.NewBorder(nil, nil, nil, container.NewGridWrap(fyne.NewSize(120, 8), m.busy), m.status)
	m.win.SetContent(container.NewBorder(nil, bar, nil, nil, split))
	m.win.Resize(fyne.NewSize(1180, 760))
	split.SetOffset(0.27)

	m.win.SetMainMenu(fyne.NewMainMenu(
		fyne.NewMenu("iDRAC",
			fyne.NewMenuItem("Add…", func() { m.editHost("") }),
			fyne.NewMenuItem("Edit…", func() {
				if m.selected != "" {
					m.editHost(m.selected)
				}
			}),
			fyne.NewMenuItem("Remove", m.removeHost),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Refresh Status", m.probeAll),
			fyne.NewMenuItem("Forget Entered Passwords", func() {
				m.mu.Lock()
				m.passwords, m.unsaved = map[string]string{}, map[string]string{}
				m.mu.Unlock()
				m.setStatus("Passwords forgotten; you will be asked again")
			}),
		),
		fyne.NewMenu("Help", fyne.NewMenuItem("About", func() {
			dialog.ShowInformation("iDRAC Manager", fmt.Sprintf("idrac %s\n\nHosts file: %s\nEvery tab runs the same code as the command line.", buildVersion(), m.g.cfg.Path()), m.win)
		})),
	))

	// Nothing is selected to begin with: showing a host reads from it, which
	// may ask for its password. IDRAC_GUI_HOST names one to start on (smoke tests).
	m.reloadHosts(os.Getenv("IDRAC_GUI_HOST"))
	m.probeAll()
	return m
}

// uiMu serialises UI callbacks. With the real driver they already run one at
// a time on the main goroutine, so it is never contended; Fyne's test driver
// runs them on the calling goroutine, where this keeps them from interleaving.
var uiMu sync.Mutex

// ui runs fn on the UI goroutine.
func (m *manager) ui(fn func()) {
	fyne.Do(func() {
		uiMu.Lock()
		defer uiMu.Unlock()
		fn()
	})
}

func (m *manager) setStatus(format string, args ...any) {
	m.status.SetText(fmt.Sprintf(format, args...))
}

func (m *manager) startBusy() {
	m.busyN++
	m.busy.Show()
	m.busy.Start()
}

func (m *manager) stopBusy() {
	if m.busyN > 0 {
		m.busyN--
	}
	if m.busyN == 0 {
		m.busy.Stop()
		m.busy.Hide()
	}
}

// ---- host list ----

func (m *manager) reloadHosts(selectName string) {
	m.names = m.g.cfg.Names()
	m.list.Refresh()
	if selectName == "" && m.selected != "" {
		selectName = m.selected
	}
	idx := sort.SearchStrings(m.names, selectName)
	if selectName != "" && idx < len(m.names) && m.names[idx] == selectName {
		m.list.Select(idx)
		return
	}
	m.selected = ""
	m.list.UnselectAll()
	m.showWelcome()
}

func (m *manager) updateListItem(id widget.ListItemID, o fyne.CanvasObject) {
	if id < 0 || id >= len(m.names) {
		return
	}
	name := m.names[id]
	h := m.g.cfg.Resolve(name)
	row := o.(*fyne.Container)
	// Border layout keeps [center, left] order: objects[0] is the VBox, [1] the dot holder.
	box := row.Objects[0].(*fyne.Container)
	dot := row.Objects[1].(*fyne.Container).Objects[0].(*canvas.Text)
	box.Objects[0].(*widget.Label).SetText(name)
	sub := box.Objects[1].(*fyne.Container).Objects[0].(*canvas.Text)

	m.mu.Lock()
	p := m.probes[name]
	m.mu.Unlock()
	text := h.Address
	gen := h.Generation
	if gen == config.GenAuto {
		gen = p.gen
	}
	if gen != config.GenAuto {
		text = genLabel(gen) + "  ·  " + h.Address
	}
	switch {
	case !p.done:
		dot.Color = color.Gray{Y: 128}
	case p.reachable:
		dot.Color = color.NRGBA{R: 0x3c, G: 0xb3, B: 0x71, A: 0xff}
	default:
		dot.Color = color.NRGBA{R: 0xd9, G: 0x4f, B: 0x4f, A: 0xff}
		text += "  ·  unreachable"
	}
	sub.Text = text
	dot.Refresh()
	sub.Refresh()
}

func genLabel(g config.Generation) string {
	switch g {
	case config.GenIDRAC6:
		return "iDRAC6"
	case config.GenIDRAC7:
		return "iDRAC7"
	case config.GenIDRAC8:
		return "iDRAC8"
	case config.GenIDRAC9:
		return "iDRAC9"
	case config.GenDemo:
		return "demo"
	}
	return string(g)
}

// probeAll checks reachability and generation of every host, without
// credentials, in the background.
func (m *manager) probeAll() {
	for _, name := range m.names {
		name := name
		h := m.g.cfg.Resolve(name)
		go func() {
			res := probeHost(h)
			m.mu.Lock()
			m.probes[name] = res
			m.mu.Unlock()
			m.ui(m.list.Refresh)
		}()
	}
}

func probeHost(h *config.Host) probeResult {
	res := probeResult{done: true, gen: h.Generation}
	if h.Generation == config.GenDemo {
		res.reachable = true
		return res
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", h.Address, h.HTTPSPort), 4*time.Second)
	if err != nil {
		res.detail = err.Error()
		return res
	}
	conn.Close()
	res.reachable = true
	if h.Generation == config.GenAuto {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		root, err := redfish.Probe(ctx, h.BaseURL(), h.InsecureTLS())
		switch {
		case err == nil:
			res.gen = classifyRedfish(root)
		case redfish.IsNotFound(err):
			res.gen = config.GenIDRAC6
		}
	}
	return res
}

// showWelcome fills the detail pane while no host is selected.
func (m *manager) showWelcome() {
	if len(m.names) > 0 {
		m.detail.Objects = []fyne.CanvasObject{container.NewCenter(widget.NewLabel("Select an iDRAC from the list"))}
		m.detail.Refresh()
		return
	}
	msg := widget.NewLabelWithStyle("No iDRACs configured yet", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})
	hint := widget.NewLabelWithStyle("Hosts are stored in "+m.g.cfg.Path(), fyne.TextAlignCenter, fyne.TextStyle{})
	add := widget.NewButtonWithIcon("Add an iDRAC", theme.ContentAddIcon(), func() { m.editHost("") })
	add.Importance = widget.HighImportance
	m.detail.Objects = []fyne.CanvasObject{container.NewCenter(container.NewVBox(msg, hint, container.NewCenter(add)))}
	m.detail.Refresh()
}

// ---- add / edit / remove ----

var passwordModes = []string{"Ask when needed", "Save in hosts file", "Environment variable", "1Password reference", "Command"}

func (m *manager) editHost(name string) {
	var h config.Host
	title := "Add iDRAC"
	if name != "" {
		h, _ = m.g.cfg.Get(name)
		title = "Edit " + name
	}
	nameE := widget.NewEntry()
	nameE.SetText(name)
	nameE.SetPlaceHolder("server01")
	addrE := widget.NewEntry()
	addrE.SetText(h.Address)
	addrE.SetPlaceHolder("192.0.2.10 or idrac.example.com")
	genS := widget.NewSelect([]string{"Detect automatically", "iDRAC6", "iDRAC7", "iDRAC8", "iDRAC9", "Demo (no hardware)"}, nil)
	gens := []config.Generation{config.GenAuto, config.GenIDRAC6, config.GenIDRAC7, config.GenIDRAC8, config.GenIDRAC9, config.GenDemo}
	genS.SetSelectedIndex(0)
	for i, gen := range gens {
		if gen == h.Generation {
			genS.SetSelectedIndex(i)
		}
	}
	userE := widget.NewEntry()
	userE.SetText(h.Username)
	userE.SetPlaceHolder("root")
	descE := widget.NewEntry()
	descE.SetText(h.Description)

	secret := widget.NewPasswordEntry()
	plain := widget.NewEntry()
	valueBox := container.NewStack(secret, plain)
	modeS := widget.NewSelect(passwordModes, nil)
	setMode := func(mode string) {
		secret.Hide()
		plain.Hide()
		switch mode {
		case passwordModes[1]:
			secret.SetPlaceHolder("stored in plain text in the hosts file")
			secret.Show()
		case passwordModes[2]:
			plain.SetPlaceHolder("IDRAC_SERVER01_PASSWORD")
			plain.Show()
		case passwordModes[3]:
			plain.SetPlaceHolder("op://Vault/Item/password")
			plain.Show()
		case passwordModes[4]:
			plain.SetPlaceHolder("pass show idrac/server01")
			plain.Show()
		}
	}
	modeS.OnChanged = setMode
	switch {
	case h.Password != "":
		modeS.SetSelected(passwordModes[1])
		secret.SetText(h.Password)
	case h.PasswordEnv != "":
		modeS.SetSelected(passwordModes[2])
		plain.SetText(h.PasswordEnv)
	case h.PasswordRef != "":
		modeS.SetSelected(passwordModes[3])
		plain.SetText(h.PasswordRef)
	case h.PasswordCmd != "":
		modeS.SetSelected(passwordModes[4])
		plain.SetText(h.PasswordCmd)
	default:
		modeS.SetSelected(passwordModes[0])
	}

	items := []*widget.FormItem{
		widget.NewFormItem("Name", nameE),
		widget.NewFormItem("Address", addrE),
		widget.NewFormItem("Generation", genS),
		widget.NewFormItem("Username", userE),
		widget.NewFormItem("Password", modeS),
		widget.NewFormItem("", valueBox),
		widget.NewFormItem("Description", descE),
	}
	d := dialog.NewForm(title, "Save", "Cancel", items, func(ok bool) {
		if !ok {
			return
		}
		newName := strings.TrimSpace(nameE.Text)
		addr := strings.TrimSpace(addrE.Text)
		if addr == "" {
			dialog.ShowError(errors.New("an address is required"), m.win)
			return
		}
		if newName == "" {
			newName = addr
		}
		if newName != name {
			if _, exists := m.g.cfg.Get(newName); exists {
				dialog.ShowError(fmt.Errorf("%q already exists", newName), m.win)
				return
			}
		}
		h.Address = addr
		h.Generation = gens[genS.SelectedIndex()]
		h.Username = strings.TrimSpace(userE.Text)
		h.Description = strings.TrimSpace(descE.Text)
		h.Password, h.PasswordEnv, h.PasswordRef, h.PasswordCmd = "", "", "", ""
		switch modeS.Selected {
		case passwordModes[1]:
			h.Password = secret.Text
		case passwordModes[2]:
			h.PasswordEnv = strings.TrimSpace(plain.Text)
		case passwordModes[3]:
			h.PasswordRef = strings.TrimSpace(plain.Text)
		case passwordModes[4]:
			h.PasswordCmd = strings.TrimSpace(plain.Text)
		}
		if name != "" && newName != name {
			m.g.cfg.Delete(name)
		}
		m.g.cfg.Set(newName, h)
		if err := m.g.cfg.Save(); err != nil {
			dialog.ShowError(err, m.win)
			return
		}
		m.forgetPassword(name)
		m.forgetPassword(newName)
		m.mu.Lock()
		delete(m.probes, name)
		delete(m.probes, newName)
		m.mu.Unlock()
		m.dropConsole(name)
		m.selected = ""
		m.reloadHosts(newName)
		m.probeAll()
		m.setStatus("Saved %s", newName)
	}, m.win)
	d.Resize(fyne.NewSize(520, 420))
	d.Show()
}

func (m *manager) removeHost() {
	name := m.selected
	if name == "" {
		return
	}
	dialog.ShowConfirm("Remove iDRAC", fmt.Sprintf("Remove %q from the hosts file?", name), func(yes bool) {
		if !yes {
			return
		}
		m.g.cfg.Delete(name)
		if err := m.g.cfg.Save(); err != nil {
			dialog.ShowError(err, m.win)
			return
		}
		m.dropConsole(name)
		m.selected = ""
		m.reloadHosts("")
		m.setStatus("Removed %s", name)
	}, m.win)
}

// ---- credentials and command execution ----

// askPassword asks for a host's password and whether to save it. It is called
// off the UI goroutine and blocks until the dialog is answered.
func (m *manager) askPassword(h *config.Host) (pw string, save bool, err error) {
	type answer struct {
		pw       string
		save, ok bool
	}
	ch := make(chan answer, 1)
	m.ui(func() {
		e := widget.NewPasswordEntry()
		keep := widget.NewCheck("Save in the hosts file (plain text)", nil)
		d := dialog.NewForm("Password required", "OK", "Cancel",
			[]*widget.FormItem{
				widget.NewFormItem(fmt.Sprintf("Password for %s@%s", h.Username, h.Address), e),
				widget.NewFormItem("", keep),
			},
			func(ok bool) { ch <- answer{e.Text, keep.Checked, ok} }, m.win)
		d.Resize(fyne.NewSize(460, 200))
		d.Show()
		m.win.Canvas().Focus(e)
	})
	a := <-ch
	if !a.ok || a.pw == "" {
		return "", false, errors.New("password entry cancelled")
	}
	return a.pw, a.save, nil
}

// forgetPassword drops a host's entered password, so that the next action
// asks again.
func (m *manager) forgetPassword(name string) {
	m.mu.Lock()
	delete(m.passwords, name)
	delete(m.unsaved, name)
	m.mu.Unlock()
}

// passwordWorked is called on the UI goroutine once the iDRAC has accepted a
// host's password. If the user asked for it to be saved, this is when it is
// written to the hosts file: a mistyped one never gets there, to be retried
// until the account locks.
func (m *manager) passwordWorked(name string) {
	m.mu.Lock()
	pw, ok := m.unsaved[name]
	delete(m.unsaved, name)
	m.mu.Unlock()
	h, exists := m.g.cfg.Get(name)
	if !ok || !exists {
		return
	}
	h.Password = pw
	m.g.cfg.Set(name, h)
	if err := m.g.cfg.Save(); err != nil {
		dialog.ShowError(fmt.Errorf("saving the password: %w", err), m.win)
		return
	}
	m.setStatus("%s: password saved in %s", name, m.g.cfg.Path())
}

// hostFor returns a resolved copy of the host carrying its generation and
// password, asking for the password once per session if no source is
// configured (and remembering whether to save it once it has worked). Call
// it off the UI goroutine.
func (m *manager) hostFor(ctx context.Context, name string) (*config.Host, error) {
	h := m.g.cfg.Resolve(name)
	m.mu.Lock()
	if h.Generation == config.GenAuto {
		h.Generation = m.probes[name].gen
	}
	pw, cached := m.passwords[name]
	m.mu.Unlock()
	if h.Generation == config.GenDemo {
		h.Password = "demo"
		return h, nil
	}
	if !cached {
		var save bool
		var err error
		if pw, err = h.ResolvePassword(ctx); errors.Is(err, config.ErrNoPassword) {
			pw, save, err = m.ask(h)
		}
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.passwords[name] = pw
		if save {
			m.unsaved[name] = pw
		}
		m.mu.Unlock()
	}
	h.Password = pw
	return h, nil
}

// isAuthError reports whether err means the credentials were rejected, in
// which case the cached password is dropped so the next action asks again.
// Nothing is retried automatically: iDRACs lock accounts.
func isAuthError(err error) bool {
	var le *webapi.LoginError
	var re *redfish.Error
	var ke *kvm.LoginError
	switch {
	case errors.As(err, &le):
		return true
	case errors.As(err, &re):
		return re.Status == 401
	case errors.As(err, &ke):
		return true
	}
	return err != nil && strings.Contains(err.Error(), "unable to authenticate")
}

// exec runs a CLI command against the named host in the background and
// delivers the result on the UI goroutine.
func (m *manager) exec(name, what string, done func(*cmdResult), args ...string) {
	m.startBusy()
	m.setStatus("%s: %s…", name, what)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		var res *cmdResult
		h, err := m.hostFor(ctx, name)
		if err != nil {
			res = &cmdResult{Err: err}
		} else {
			res = runCaptured(ctx, m.g, h, args...)
		}
		if isAuthError(res.Err) {
			m.forgetPassword(name)
		}
		m.ui(func() {
			m.stopBusy()
			if res.Err != nil {
				m.setStatus("%s: %s failed: %v", name, what, res.Err)
			} else {
				m.setStatus("%s: %s done", name, what)
				m.passwordWorked(name)
			}
			if done != nil {
				done(res)
			}
		})
	}()
}

// confirmExec asks first, then runs; used for anything that changes state.
func (m *manager) confirmExec(name, title, question string, done func(*cmdResult), args ...string) {
	dialog.ShowConfirm(title, question, func(yes bool) {
		if yes {
			m.exec(name, strings.ToLower(title), func(res *cmdResult) {
				if res.Err != nil {
					dialog.ShowError(res.Err, m.win)
				}
				if done != nil {
					done(res)
				}
			}, args...)
		}
	}, m.win)
}

// console returns the host's console panel, creating it on first use. A panel
// is kept until its host is edited or removed, so a session carries on while
// another host is shown.
func (m *manager) console(name string) *viewer.Panel {
	if p, ok := m.consoles[name]; ok {
		return p
	}
	h := m.g.cfg.Resolve(name)
	title := name
	if name != h.Address {
		title = fmt.Sprintf("%s (%s)", name, h.Address)
	}
	// Resolved on use rather than here: that is what asks for the password.
	target := func(ctx context.Context) (*globals, error) {
		h, err := m.hostFor(ctx, name)
		if err != nil {
			return nil, err
		}
		hg := *m.g
		hg.target, hg.host = h, h.Name
		return &hg, nil
	}
	p := viewer.NewPanel(m.app, m.win, viewer.Options{
		Title: title, Logger: m.g.logger, Actions: viewerActions(target), Do: m.ui,
		Connect: func(ctx context.Context) (viewer.Backend, error) {
			hg, err := target(ctx)
			if err != nil {
				return nil, err
			}
			if hg.target.Generation == config.GenDemo {
				return newDemoBackend(), nil
			}
			b, err := connectBackend(ctx, hg, kvmOpts{})
			switch {
			case isAuthError(err):
				m.forgetPassword(name)
			case err == nil:
				m.ui(func() { m.passwordWorked(name) })
			}
			return b, err
		},
	})
	m.consoles[name] = p
	return p
}

// dropConsole ends a host's console session, if it has one.
func (m *manager) dropConsole(name string) {
	if p, ok := m.consoles[name]; ok {
		p.Close()
		delete(m.consoles, name)
	}
}

// ---- small layout helpers ----

func monoLabel() *widget.Label {
	l := widget.NewLabel("")
	l.TextStyle = fyne.TextStyle{Monospace: true}
	l.Selectable = true
	return l
}

func row(objs ...fyne.CanvasObject) *fyne.Container {
	return container.New(layout.NewHBoxLayout(), objs...)
}
