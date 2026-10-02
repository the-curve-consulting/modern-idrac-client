//go:build gui

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/config"
)

// page is one tab of the host view: its content and how to (re)load it.
type page struct {
	title  string
	body   fyne.CanvasObject
	load   func()
	loaded bool
	shown  func(bool) // if set, told when the tab comes into and goes out of view
}

// selectHost builds the tabbed view for a host.
func (m *manager) selectHost(name string) {
	if name == m.selected && len(m.detail.Objects) > 0 {
		return
	}
	if p, ok := m.consoles[m.selected]; ok {
		p.SetActive(false) // the host going out of view may be showing its console
	}
	m.selected = name
	h := m.g.cfg.Resolve(name)

	pages := []*page{
		m.overviewPage(name),
		m.consolePage(name),
		m.tablePage(name, "Sensors", "reading sensors", func() []string { return []string{"sensors"} }),
		m.logsPage(name),
		m.jobsPage(name),
		m.vmediaPage(name),
		m.biosPage(name),
		m.tablePage(name, "Accounts", "listing accounts", func() []string { return []string{"accounts"} }),
		m.racadmPage(name),
		m.apiPage(name),
		m.maintenancePage(name),
	}
	var items []*container.TabItem
	byItem := map[*container.TabItem]*page{}
	for _, p := range pages {
		it := container.NewTabItem(p.title, p.body)
		items = append(items, it)
		byItem[it] = p
	}
	tabs := container.NewAppTabs(items...)
	show := func(it *container.TabItem) {
		p := byItem[it]
		if p == nil {
			return
		}
		if !p.loaded && p.load != nil {
			p.loaded = true
			p.load()
		}
		if p.shown != nil {
			p.shown(true)
		}
	}
	tabs.OnSelected = show
	tabs.OnUnselected = func(it *container.TabItem) {
		if p := byItem[it]; p != nil && p.shown != nil {
			p.shown(false)
		}
	}

	title := widget.NewLabelWithStyle(name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	sub := h.Address
	if h.Description != "" {
		sub += "  ·  " + h.Description
	}
	header := row(title, widget.NewLabel(sub))

	m.detail.Objects = []fyne.CanvasObject{container.NewBorder(header, nil, nil, nil, tabs)}
	m.detail.Refresh()
	m.tabs, m.pages = tabs, pages
	show(items[0])
	// Smoke-test aid: start on a named tab.
	if want := os.Getenv("IDRAC_GUI_TAB"); want != "" {
		for _, it := range items {
			if it.Text == want {
				tabs.Select(it)
			}
		}
	}
}

// consolePage is the remote console. It connects only when asked: a session
// takes one of the iDRAC's few console slots.
func (m *manager) consolePage(name string) *page {
	p := m.console(name)
	return &page{title: "Console", body: p.Content(), shown: p.SetActive}
}

// showErr puts a command failure where the user is looking.
func showErr(out *widget.Label, res *cmdResult) bool {
	if res.Err == nil {
		return false
	}
	msg := res.Err.Error()
	if res.Text != "" {
		msg = res.Text + "\n\n" + msg
	}
	out.SetText(msg)
	return true
}

// infoOrder lists the overview fields in the order people look for them;
// anything else follows alphabetically.
var infoOrder = []string{"Model", "ServiceTag", "ExpressServiceCode", "HostName", "PowerState", "Health", "CPU", "MemoryGiB", "BIOS", "iDRACFirmware", "LifecycleController", "BootOverride", "IndicatorLED", "OS"}

// orderInfo parses the `info` command's "Key:  value" lines into ordered pairs.
func orderInfo(text string) [][2]string {
	vals := map[string]string{}
	var rest []string
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, dup := vals[k]; !dup {
			rest = append(rest, k)
		}
		vals[k] = strings.TrimSpace(v)
	}
	var out [][2]string
	seen := map[string]bool{}
	for _, k := range infoOrder {
		if v, ok := vals[k]; ok && v != "" {
			out = append(out, [2]string{k, v})
			seen[k] = true
		}
	}
	for _, k := range rest {
		if !seen[k] && vals[k] != "" {
			out = append(out, [2]string{k, vals[k]})
		}
	}
	return out
}

// ---- Overview ----

func (m *manager) overviewPage(name string) *page {
	p := &page{title: "Overview"}
	grid := container.New(layout.NewFormLayout())
	note := widget.NewLabel("")
	note.Wrapping = fyne.TextWrapWord

	p.load = func() {
		m.exec(name, "reading system information", func(res *cmdResult) {
			grid.Objects = nil
			if res.Err != nil {
				note.SetText(fmt.Sprintf("Could not read system information:\n%v", res.Err))
				grid.Refresh()
				return
			}
			note.SetText("")
			for _, kv := range orderInfo(res.Text) {
				val := widget.NewLabel(kv[1])
				val.Selectable = true
				grid.Add(widget.NewLabelWithStyle(kv[0], fyne.TextAlignTrailing, fyne.TextStyle{Bold: true}))
				grid.Add(val)
			}
			grid.Refresh()
		}, "info")
	}

	power := func(label, action, question string) *fyne.MenuItem {
		return fyne.NewMenuItem(label, func() {
			m.confirmExec(name, label, question, func(*cmdResult) { p.load() }, "power", action)
		})
	}
	powerMenu := fyne.NewMenu("",
		power("Power On", "on", "Power the server on?"),
		power("Graceful Shutdown", "graceful", "Ask the operating system to shut down?"),
		power("Power Off (forced)", "off", "Force the server off immediately? Unsaved data will be lost."),
		fyne.NewMenuItemSeparator(),
		power("Reset (warm boot)", "reset", "Reset the server immediately? Unsaved data will be lost."),
		power("Power Cycle (cold boot)", "cycle", "Power-cycle the server immediately? Unsaved data will be lost."),
		power("NMI", "nmi", "Send a non-maskable interrupt to the server?"),
	)
	boot := func(label, target string) *fyne.MenuItem {
		return fyne.NewMenuItem(label, func() {
			m.exec(name, "setting next boot to "+label, func(res *cmdResult) {
				if res.Err != nil {
					dialog.ShowError(res.Err, m.win)
				}
				p.load()
			}, "boot", target)
		})
	}
	bootMenu := fyne.NewMenu("", boot("Normal Boot", "none"), boot("PXE", "pxe"), boot("BIOS Setup", "bios"), boot("CD/DVD", "cd"), boot("Hard Disk", "hdd"))

	var powerBtn, bootBtn *widget.Button
	powerBtn = widget.NewButtonWithIcon("Power", theme.MediaPlayIcon(), func() {
		widget.ShowPopUpMenuAtRelativePosition(powerMenu, m.win.Canvas(), fyne.NewPos(0, powerBtn.Size().Height), powerBtn)
	})
	bootBtn = widget.NewButtonWithIcon("Next Boot", theme.MediaSkipNextIcon(), func() {
		widget.ShowPopUpMenuAtRelativePosition(bootMenu, m.win.Canvas(), fyne.NewPos(0, bootBtn.Size().Height), bootBtn)
	})
	led := func(label, arg string) *widget.Button {
		return widget.NewButton(label, func() {
			m.exec(name, "identify LED "+arg, func(res *cmdResult) {
				if res.Err != nil {
					dialog.ShowError(res.Err, m.win)
				}
			}, "led", arg)
		})
	}
	actions := row(powerBtn, bootBtn, led("Identify LED On", "on"), led("Identify LED Off", "off"),
		layout.NewSpacer(), widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() { p.load() }))
	p.body = container.NewBorder(container.NewVBox(actions, widget.NewSeparator()), nil, nil, nil,
		container.NewVScroll(container.NewVBox(grid, note)))
	return p
}

// ---- generic table page ----

// tableView is a table plus a message line, fed by one CLI command.
type tableView struct {
	table *dataTable
	msg   *widget.Label
	body  fyne.CanvasObject
	load  func()
}

func (m *manager) newTableView(name, what string, args func() []string, controls ...fyne.CanvasObject) *tableView {
	v := &tableView{table: newDataTable(), msg: widget.NewLabel("")}
	v.msg.Wrapping = fyne.TextWrapWord
	v.load = func() {
		m.exec(name, what, func(res *cmdResult) {
			if showErr(v.msg, res) {
				v.table.set(nil, nil)
				return
			}
			switch {
			case len(res.Tables) > 0:
				t := res.Tables[0]
				v.table.set(t.Header, t.Rows)
				v.msg.SetText(fmt.Sprintf("%d rows", len(t.Rows)))
			default:
				v.table.set(nil, nil)
				v.msg.SetText(res.Text)
			}
		}, args()...)
	}
	bar := append([]fyne.CanvasObject{}, controls...)
	bar = append(bar, layout.NewSpacer(), widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() { v.load() }))
	v.body = container.NewBorder(row(bar...), v.msg, nil, nil, v.table.table)
	return v
}

func (m *manager) tablePage(name, title, what string, args func() []string) *page {
	v := m.newTableView(name, what, args)
	return &page{title: title, body: v.body, load: v.load}
}

// ---- Event logs ----

func (m *manager) logsPage(name string) *page {
	which := widget.NewSelect([]string{"System Event Log", "Lifecycle Log"}, nil)
	which.SetSelectedIndex(0)
	count := widget.NewSelect([]string{"50", "200", "1000", "All"}, nil)
	count.SetSelectedIndex(0)
	var v *tableView
	clear := widget.NewButtonWithIcon("Clear SEL", theme.DeleteIcon(), func() {
		m.confirmExec(name, "Clear event log", "Clear the system event log on "+name+"?", func(*cmdResult) { v.load() }, "sel", "clear")
	})
	v = m.newTableView(name, "reading the log", func() []string {
		n := count.Selected
		if n == "All" {
			n = "0"
		}
		if which.SelectedIndex() == 1 {
			return []string{"lclog", "-n", n}
		}
		return []string{"sel", "-n", n}
	}, which, widget.NewLabel("Entries"), count, clear)
	which.OnChanged = func(string) { v.load() }
	count.OnChanged = func(string) { v.load() }
	return &page{title: "Logs", body: v.body, load: v.load}
}

// ---- Jobs ----

func (m *manager) jobsPage(name string) *page {
	var v *tableView
	del := widget.NewButtonWithIcon("Delete Selected", theme.DeleteIcon(), func() {
		r := v.table.selectedRow()
		if r == nil {
			return
		}
		m.confirmExec(name, "Delete job", "Delete job "+r[0]+"?", func(*cmdResult) { v.load() }, "jobs", "delete", r[0])
	})
	clear := widget.NewButton("Clear Queue", func() {
		m.confirmExec(name, "Clear job queue", "Delete every job in the queue on "+name+"?", func(*cmdResult) { v.load() }, "jobs", "delete", "all")
	})
	v = m.newTableView(name, "listing jobs", func() []string { return []string{"jobs"} }, del, clear)
	return &page{title: "Jobs", body: v.body, load: v.load}
}

// ---- Virtual media ----

func (m *manager) vmediaPage(name string) *page {
	var v *tableView
	insert := widget.NewButtonWithIcon("Insert…", theme.ContentAddIcon(), func() {
		slot := widget.NewSelect([]string{"CD", "RemovableDisk"}, nil)
		slot.SetSelectedIndex(0)
		url := widget.NewEntry()
		url.SetPlaceHolder("http://server/image.iso  (http, https or nfs)")
		d := dialog.NewForm("Insert virtual media", "Insert", "Cancel", []*widget.FormItem{
			widget.NewFormItem("Slot", slot), widget.NewFormItem("Image URL", url),
		}, func(ok bool) {
			if !ok || strings.TrimSpace(url.Text) == "" {
				return
			}
			m.exec(name, "inserting media", func(res *cmdResult) {
				if res.Err != nil {
					dialog.ShowError(res.Err, m.win)
				}
				v.load()
			}, "vmedia", "insert", slot.Selected, strings.TrimSpace(url.Text))
		}, m.win)
		d.Resize(fyne.NewSize(560, 200))
		d.Show()
	})
	eject := widget.NewButton("Eject Selected", func() {
		r := v.table.selectedRow()
		if r == nil {
			return
		}
		m.exec(name, "ejecting media", func(res *cmdResult) {
			if res.Err != nil {
				dialog.ShowError(res.Err, m.win)
			}
			v.load()
		}, "vmedia", "eject", r[0])
	})
	v = m.newTableView(name, "listing virtual media", func() []string { return []string{"vmedia"} }, insert, eject)
	return &page{title: "Virtual Media", body: v.body, load: v.load}
}

// ---- BIOS ----

func (m *manager) biosPage(name string) *page {
	filter := widget.NewEntry()
	filter.SetPlaceHolder("filter, e.g. boot")
	table := newDataTable()
	msg := widget.NewLabel("")
	msg.Wrapping = fyne.TextWrapWord
	var load func()
	load = func() {
		args := []string{"bios"}
		if f := strings.TrimSpace(filter.Text); f != "" {
			args = append(args, f)
		}
		m.exec(name, "reading BIOS attributes", func(res *cmdResult) {
			if showErr(msg, res) {
				table.set(nil, nil)
				return
			}
			var rows [][]string
			for _, line := range strings.Split(res.Text, "\n") {
				if k, v, ok := strings.Cut(line, "="); ok {
					rows = append(rows, []string{k, v})
				}
			}
			table.set([]string{"ATTRIBUTE", "VALUE"}, rows)
			msg.SetText(fmt.Sprintf("%d attributes", len(rows)))
		}, args...)
	}
	filter.OnSubmitted = func(string) { load() }
	set := widget.NewButtonWithIcon("Change…", theme.DocumentCreateIcon(), func() {
		attr := widget.NewEntry()
		val := widget.NewEntry()
		if r := table.selectedRow(); r != nil {
			attr.SetText(r[0])
			val.SetText(r[1])
		}
		reboot := widget.NewCheck("Power-cycle now to apply", nil)
		d := dialog.NewForm("Change BIOS attribute", "Stage Change", "Cancel", []*widget.FormItem{
			widget.NewFormItem("Attribute", attr), widget.NewFormItem("New value", val), widget.NewFormItem("", reboot),
		}, func(ok bool) {
			if !ok || strings.TrimSpace(attr.Text) == "" {
				return
			}
			args := []string{"bios", "set", strings.TrimSpace(attr.Text) + "=" + val.Text}
			if reboot.Checked {
				args = append(args, "-reboot")
			}
			m.exec(name, "staging BIOS change", func(res *cmdResult) {
				if res.Err != nil {
					dialog.ShowError(res.Err, m.win)
					return
				}
				dialog.ShowInformation("BIOS change staged", res.Text, m.win)
			}, args...)
		}, m.win)
		d.Resize(fyne.NewSize(520, 240))
		d.Show()
	})
	bar := container.NewBorder(nil, nil, nil, row(set, widget.NewButtonWithIcon("Refresh", theme.ViewRefreshIcon(), func() { load() })), filter)
	return &page{title: "BIOS", body: container.NewBorder(bar, msg, nil, nil, table.table), load: load}
}

// ---- racadm ----

func (m *manager) racadmPage(name string) *page {
	entry := widget.NewEntry()
	entry.SetPlaceHolder("getsysinfo")
	out := monoLabel()
	scroll := container.NewScroll(out)
	run := func() {
		cmd := strings.TrimSpace(entry.Text)
		if cmd == "" {
			return
		}
		m.exec(name, "racadm "+cmd, func(res *cmdResult) {
			text := "$ racadm " + strings.TrimPrefix(cmd, "racadm ") + "\n" + res.Text
			if res.Err != nil {
				text += "\n" + res.Err.Error()
			}
			out.SetText(text)
			scroll.ScrollToTop()
		}, append([]string{"racadm"}, strings.Fields(cmd)...)...)
	}
	entry.OnSubmitted = func(string) { run() }
	quick := widget.NewSelect([]string{"getsysinfo", "getsel", "getraclog", "getniccfg", "getversion", "getsensorinfo", "get iDRAC.Info", "serveraction powerstatus"}, func(s string) {
		entry.SetText(s)
		run()
	})
	quick.PlaceHolder = "Common commands"
	bar := container.NewBorder(nil, nil, widget.NewLabel("racadm"), row(widget.NewButtonWithIcon("Run", theme.MediaPlayIcon(), run), quick), entry)
	return &page{title: "racadm", body: container.NewBorder(bar, nil, nil, nil, scroll)}
}

// ---- raw API ----

func (m *manager) apiPage(name string) *page {
	kind := widget.NewSelect([]string{"Redfish", "Legacy web: get keys", "Legacy web: set", "Legacy web: raw request"}, nil)
	method := widget.NewSelect([]string{"GET", "POST", "PATCH", "DELETE"}, nil)
	method.SetSelectedIndex(0)
	path := widget.NewEntry()
	body := widget.NewMultiLineEntry()
	body.SetPlaceHolder(`JSON body for POST/PATCH, e.g. {"ResetType":"On"}`)
	body.SetMinRowsVisible(3)
	out := monoLabel()
	scroll := container.NewScroll(out)
	kind.OnChanged = func(s string) {
		switch kind.SelectedIndex() {
		case 0:
			method.Enable()
			path.SetPlaceHolder("/redfish/v1/Systems/System.Embedded.1")
			body.SetPlaceHolder(`JSON body for POST/PATCH, e.g. {"ResetType":"On"}`)
		case 1:
			method.Disable()
			path.SetPlaceHolder("sysDesc,svcTag,pwState")
		case 2:
			method.Disable()
			path.SetPlaceHolder("key=value key=value")
		case 3:
			method.Enable()
			path.SetPlaceHolder("/data?get=pwState")
			body.SetPlaceHolder("form body")
		}
	}
	kind.SetSelectedIndex(0)
	send := func() {
		p := strings.TrimSpace(path.Text)
		if p == "" {
			return
		}
		var args []string
		switch kind.SelectedIndex() {
		case 0:
			args = []string{"redfish", strings.ToLower(method.Selected), p}
			if b := strings.TrimSpace(body.Text); b != "" {
				args = append(args, b)
			}
		case 1:
			args = []string{"web", "get", p}
		case 2:
			args = append([]string{"web", "set"}, strings.Fields(p)...)
		case 3:
			args = []string{"web", "raw", method.Selected, p}
			if b := strings.TrimSpace(body.Text); b != "" {
				args = append(args, b)
			}
		}
		m.exec(name, "API call", func(res *cmdResult) {
			text := res.Text
			if res.Err != nil {
				text += "\n\n" + res.Err.Error()
			}
			out.SetText(strings.TrimSpace(text))
			scroll.ScrollToTop()
		}, args...)
	}
	path.OnSubmitted = func(string) { send() }
	top := container.NewVBox(
		container.NewBorder(nil, nil, row(kind, method), widget.NewButtonWithIcon("Send", theme.MailSendIcon(), send), path),
		body,
	)
	return &page{title: "API", body: container.NewBorder(top, nil, nil, nil, scroll)}
}

// ---- Maintenance ----

func (m *manager) maintenancePage(name string) *page {
	out := monoLabel()
	scroll := container.NewScroll(out)
	show := func(res *cmdResult) {
		text := res.Text
		if res.Err != nil {
			text = strings.TrimSpace(text + "\n\n" + res.Err.Error())
		}
		out.SetText(text)
		scroll.ScrollToTop()
	}

	target := widget.NewSelect([]string{"ALL", "IDRAC", "BIOS", "NIC", "RAID"}, nil)
	target.SetSelectedIndex(0)
	format := widget.NewSelect([]string{"XML", "JSON"}, nil)
	format.SetSelectedIndex(0)
	export := widget.NewButton("Export Profile", func() {
		m.exec(name, "exporting configuration profile", show, "scp", "export", "-target", target.Selected, "-format", format.Selected)
	})
	save := widget.NewButtonWithIcon("Save Output…", theme.DocumentSaveIcon(), func() {
		if out.Text == "" {
			return
		}
		d := dialog.NewFileSave(func(w fyne.URIWriteCloser, err error) {
			if err != nil || w == nil {
				return
			}
			defer w.Close()
			w.Write([]byte(out.Text + "\n"))
			m.setStatus("Saved %s", w.URI().Path())
		}, m.win)
		d.SetFileName(name + "-output.txt")
		d.Show()
	})
	shutdown := widget.NewSelect([]string{"Graceful", "Forced", "NoReboot"}, nil)
	shutdown.SetSelectedIndex(2)
	imp := widget.NewButton("Import Profile…", func() {
		d := dialog.NewFileOpen(func(r fyne.URIReadCloser, err error) {
			if err != nil || r == nil {
				return
			}
			file := r.URI().Path()
			r.Close()
			m.confirmExec(name, "Import configuration profile",
				fmt.Sprintf("Apply %s to %s (target %s, shutdown %s)?", file, name, target.Selected, shutdown.Selected),
				show, "scp", "import", file, "-target", target.Selected, "-shutdown", shutdown.Selected)
		}, m.win)
		d.Show()
	})

	fw := widget.NewEntry()
	fw.SetPlaceHolder("http://server/firmware.exe  (http, nfs or cifs URI)")
	update := widget.NewButton("Start Update", func() {
		uri := strings.TrimSpace(fw.Text)
		if uri == "" {
			return
		}
		m.confirmExec(name, "Firmware update", "Start a firmware update on "+name+" from\n"+uri+"?", show, "update", uri)
	})

	attrFilter := widget.NewEntry()
	attrFilter.SetPlaceHolder("filter, e.g. Users")
	attrs := widget.NewButton("Show", func() {
		args := []string{"attrs"}
		if f := strings.TrimSpace(attrFilter.Text); f != "" {
			args = append(args, f)
		}
		m.exec(name, "reading iDRAC attributes", show, args...)
	})
	attrSet := widget.NewEntry()
	attrSet.SetPlaceHolder("Group.1.Name=Value")
	attrApply := widget.NewButton("Set", func() {
		kv := strings.TrimSpace(attrSet.Text)
		if !strings.Contains(kv, "=") {
			return
		}
		m.confirmExec(name, "Set iDRAC attribute", "Set "+kv+" on "+name+"?", show, "attrs", "set", kv)
	})

	reset := widget.NewButtonWithIcon("Reboot iDRAC", theme.WarningIcon(), func() {
		m.confirmExec(name, "Reboot iDRAC", "Reboot the iDRAC on "+name+"? The server keeps running, but management is unavailable for a few minutes.", show, "reset-idrac")
	})
	probe := widget.NewButton("Probe", func() {
		// Probing needs no credentials, so bypass the password step.
		h := m.g.cfg.Resolve(name)
		if h.Generation == config.GenDemo {
			out.SetText("demo host")
			return
		}
		m.startBusy()
		go func() {
			res := runCaptured(context.Background(), m.g, h, "probe")
			kv := runCaptured(context.Background(), m.g, h, "kvm", "probe")
			m.ui(func() {
				m.stopBusy()
				out.SetText(strings.TrimSpace(res.Text + "\n\nConsole transport:\n" + kv.Text))
			})
		}()
	})

	form := container.New(layout.NewFormLayout(),
		widget.NewLabel("Export profile"), row(widget.NewLabel("Target"), target, widget.NewLabel("Format"), format, export),
		widget.NewLabel("Import profile"), row(widget.NewLabel("Host shutdown"), shutdown, imp),
		widget.NewLabel("Firmware"), container.NewBorder(nil, nil, nil, update, fw),
		widget.NewLabel("Show attributes"), container.NewBorder(nil, nil, nil, attrs, attrFilter),
		widget.NewLabel("Set attribute"), container.NewBorder(nil, nil, nil, attrApply, attrSet),
		widget.NewLabel("Controller"), row(probe, reset, layout.NewSpacer(), save),
	)
	return &page{title: "Maintenance", body: container.NewBorder(container.NewVBox(form, widget.NewSeparator()), nil, nil, nil, scroll)}
}
