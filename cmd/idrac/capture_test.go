package main

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/config"
)

func demoGlobals(t *testing.T) (*globals, *config.Host) {
	t.Helper()
	cfg, err := config.Load(t.TempDir() + "/hosts.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("demo", config.Host{Address: "demo", Generation: config.GenDemo})
	g := &globals{cfg: cfg, logger: log.New(io.Discard, "", 0), out: io.Discard, errw: io.Discard}
	return g, cfg.Resolve("demo")
}

func TestRunCapturedTablesAndText(t *testing.T) {
	g, h := demoGlobals(t)
	ctx := context.Background()

	res := runCaptured(ctx, g, h, "sensors")
	if res.Err != nil || len(res.Tables) != 1 {
		t.Fatalf("sensors: %+v", res)
	}
	tb := res.Tables[0]
	if tb.Header[0] != "KIND" || len(tb.Rows) != 6 || tb.Rows[1][1] != "CPU1 Temp" {
		t.Fatalf("table: %+v", tb)
	}

	res = runCaptured(ctx, g, h, "sel", "-n", "2")
	if res.Err != nil || len(res.Tables[0].Rows) != 2 {
		t.Fatalf("sel: %+v", res)
	}

	res = runCaptured(ctx, g, h, "power", "off")
	if res.Err != nil || !strings.Contains(res.Text, "power off: ok") {
		t.Fatalf("power off: %+v", res)
	}
	if res = runCaptured(ctx, g, h, "power"); res.Text != "Off" {
		t.Fatalf("power status: %q", res.Text)
	}
	runCaptured(ctx, g, h, "power", "on")

	if res = runCaptured(ctx, g, h, "info"); !strings.Contains(res.Text, "DEMO123") {
		t.Fatalf("info: %q", res.Text)
	}
	if res = runCaptured(ctx, g, h, "nope"); res.Err == nil {
		t.Fatal("unknown command must fail")
	}
}
