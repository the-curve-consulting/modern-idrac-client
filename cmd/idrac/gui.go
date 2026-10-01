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

	"idrac/pkg/config"
	"idrac/pkg/kvm"
	"idrac/pkg/redfish"
	"idrac/pkg/viewer"
	"idrac/pkg/webapi"
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
	passwords map[string]string
	probes    map[string]probeResult
	consoles  map[string]fyne.Window
}

// runGUI opens the manager window and blocks until it is closed.
func runGUI(g *globals) error {
	a := app.NewWithID("io.thecurve.idrac")
	m := newManager(g, a)
	m.win.SetMaster()
	config.PromptPassword = m.promptPassword
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
	m := &manager{g: g, app: a, passwords: map[string]string{}, probes: map[string]probeResult{}, consoles: map[string]fyne.Window{}}
	m.win = a.NewWindow("iDRAC Manager")
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
				m.passwords = map[string]string{}
				m.mu.Unlock()
				m.setStatus("Passwords forgotten; you will be asked again")
			}),
		),
		fyne.NewMenu("Help", fyne.NewMenuItem("About", func() {
			dialog.ShowInformation("iDRAC Manager", fmt.Sprintf("idrac %s\n\nHosts file: %s\nEvery tab runs the same code as the command line.", version, m.g.cfg.Path()), m.win)
		})),
	))

	m.reloadHosts("")
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
	if len(m.names) > 0 {
		m.list.Select(0)
		return
	}
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

func (m *manager) showWelcome() {
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
		m.mu.Lock()
		delete(m.passwords, name)
		delete(m.passwords, newName)
		delete(m.probes, name)
		delete(m.probes, newName)
		m.mu.Unlock()
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
		m.selected = ""
		m.reloadHosts("")
		m.setStatus("Removed %s", name)
	}, m.win)
}

// ---- credentials and command execution ----

// promptPassword is installed as config.PromptPassword: it is called off the
// UI goroutine and blocks until the dialog is answered.
func (m *manager) promptPassword(prompt string) (string, error) {
	type answer struct {
		pw string
		ok bool
	}
	ch := make(chan answer, 1)
	m.ui(func() {
		e := widget.NewPasswordEntry()
		d := dialog.NewForm("Password required", "OK", "Cancel",
			[]*widget.FormItem{widget.NewFormItem(strings.TrimSuffix(strings.TrimSpace(prompt), ":"), e)},
			func(ok bool) { ch <- answer{e.Text, ok} }, m.win)
		d.Resize(fyne.NewSize(460, 160))
		d.Show()
		m.win.Canvas().Focus(e)
	})
	a := <-ch
	if !a.ok || a.pw == "" {
		return "", errors.New("password entry cancelled")
	}
	return a.pw, nil
}

// hostFor returns a resolved copy of the host carrying its generation and
// password, asking for the password once per session if no source is
// configured. Call it off the UI goroutine.
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
		var err error
		if pw, err = h.ResolvePassword(ctx); err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.passwords[name] = pw
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
			m.mu.Lock()
			delete(m.passwords, name)
			m.mu.Unlock()
		}
		m.ui(func() {
			m.stopBusy()
			if res.Err != nil {
				m.setStatus("%s: %s failed: %v", name, what, res.Err)
			} else {
				m.setStatus("%s: %s done", name, what)
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

// openConsole opens (or raises) the console window for a host.
func (m *manager) openConsole(name string) {
	if w, ok := m.consoles[name]; ok {
		w.RequestFocus()
		return
	}
	m.startBusy()
	m.setStatus("%s: opening console…", name)
	go func() {
		h, err := m.hostFor(context.Background(), name)
		m.ui(func() {
			m.stopBusy()
			if err != nil {
				m.setStatus("%s: %v", name, err)
				dialog.ShowError(err, m.win)
				return
			}
			hg := *m.g
			hg.target, hg.host = h, h.Name
			title := name
			if name != h.Address {
				title = fmt.Sprintf("%s (%s)", name, h.Address)
			}
			o := viewer.Options{Title: title, Logger: m.g.logger, Actions: viewerActions(&hg)}
			if h.Generation == config.GenDemo {
				o.Connect = func(ctx context.Context) (viewer.Backend, error) { return newDemoBackend(), nil }
			} else {
				o.Connect = func(ctx context.Context) (viewer.Backend, error) {
					b, err := connectBackend(ctx, &hg, kvmOpts{})
					if isAuthError(err) {
						m.mu.Lock()
						delete(m.passwords, name)
						m.mu.Unlock()
					}
					return b, err
				}
			}
			o.OnClosed = func() { delete(m.consoles, name) }
			m.consoles[name] = viewer.Open(m.app, o)
			m.setStatus("%s: console open", name)
		})
	}()
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
