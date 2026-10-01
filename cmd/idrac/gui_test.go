//go:build gui

package main

import (
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/config"
)

func testManager(t *testing.T) *manager {
	t.Helper()
	cfg, err := config.Load(filepath.Join(t.TempDir(), "hosts.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("demo01", config.Host{Address: "demo", Generation: config.GenDemo})
	a := test.NewApp()
	t.Cleanup(a.Quit)
	g := &globals{cfg: cfg, logger: log.New(io.Discard, "", 0), out: io.Discard, errw: io.Discard, timeout: 10 * time.Second}
	return onUI(func() *manager { return newManager(g, a) })
}

// onUI runs fn the way the UI goroutine would: serialised with callbacks.
func onUI[T any](fn func() T) T {
	uiMu.Lock()
	defer uiMu.Unlock()
	return fn()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if onUI(cond) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func pageByTitle(m *manager, title string) *page {
	for _, p := range m.pages {
		if p.title == title {
			return p
		}
	}
	return nil
}

func TestManagerSelectsHostAndLoadsOverview(t *testing.T) {
	m := testManager(t)
	uiMu.Lock()
	sel, np := m.selected, len(m.pages)
	uiMu.Unlock()
	if sel != "demo01" || np != 10 {
		t.Fatalf("selected %q, %d pages", sel, np)
	}
	waitFor(t, "overview", func() bool { return m.status.Text == "demo01: reading system information done" })
	titles := []string{}
	for _, it := range m.tabs.Items {
		titles = append(titles, it.Text)
	}
	want := []string{"Overview", "Sensors", "Logs", "Jobs", "Virtual Media", "BIOS", "Accounts", "racadm", "API", "Maintenance"}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("tabs %v", titles)
		}
	}
}

func TestManagerRunsCommandsThroughCLI(t *testing.T) {
	m := testManager(t)
	var got *cmdResult
	onUI(func() bool {
		m.exec("demo01", "reading sensors", func(r *cmdResult) { got = r }, "sensors")
		return true
	})
	waitFor(t, "sensors", func() bool { return got != nil })
	if got.Err != nil || len(got.Tables) != 1 || len(got.Tables[0].Rows) != 6 {
		t.Fatalf("sensors result %+v", got)
	}
	// Tabs load lazily, once.
	p := pageByTitle(m, "Sensors")
	if p.loaded {
		t.Fatal("sensors tab loaded before being shown")
	}
	onUI(func() bool { m.tabs.SelectIndex(1); return true })
	waitFor(t, "sensors tab", func() bool { return p.loaded && m.busyN == 0 })
}

func TestOrderInfo(t *testing.T) {
	got := orderInfo("BIOS:        2.19\nZeta:   z\nModel:    PowerEdge R730\nPowerState:  On\nEmpty:\n")
	want := [][2]string{{"Model", "PowerEdge R730"}, {"PowerState", "On"}, {"BIOS", "2.19"}, {"Zeta", "z"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v", got)
		}
	}
}

func TestDataTable(t *testing.T) {
	test.NewApp()
	d := newDataTable()
	d.set([]string{"A", "B"}, [][]string{{"1", "one"}, {"2", "two"}})
	if d.selectedRow() != nil {
		t.Fatal("nothing should be selected")
	}
	d.table.Select(widget.TableCellID{Row: 1, Col: 0})
	if r := d.selectedRow(); r == nil || r[1] != "two" {
		t.Fatalf("selected %v", r)
	}
}

func TestHostEditPersists(t *testing.T) {
	m := testManager(t)
	m.g.cfg.Set("server01", config.Host{Address: "192.0.2.10", PasswordEnv: "X"})
	if err := m.g.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	onUI(func() bool { m.reloadHosts("server01"); return true })
	if m.selected != "server01" || len(m.names) != 2 {
		t.Fatalf("selected %q names %v", m.selected, m.names)
	}
	back, err := config.Load(m.g.cfg.Path())
	if err != nil || back.Hosts["server01"].PasswordEnv != "X" {
		t.Fatalf("reload: %v %+v", err, back.Hosts)
	}
}
