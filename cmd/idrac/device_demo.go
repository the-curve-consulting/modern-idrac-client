package main

import (
	"context"
	"fmt"
	"sync"

	"idrac/pkg/redfish"
)

// demoDevice is a canned, in-memory iDRAC used by tests and by hosts with
// "generation": "demo", so the GUI and CLI can be exercised with no hardware.
type demoDevice struct{}

var demoState = struct {
	sync.Mutex
	power string
	boot  string
	led   bool
	sel   []redfish.LogEntry
}{
	power: "On", boot: "none",
	sel: []redfish.LogEntry{
		{ID: "3", Created: "2026-09-30T07:54:11", Severity: "OK", Message: "The system was powered on."},
		{ID: "2", Created: "2026-09-26T01:02:03", Severity: "Critical", Message: "Persistent correctable memory error rate has increased for a memory device at location DIMM_B8."},
		{ID: "1", Created: "2026-01-15T10:20:30", Severity: "Warning", Message: "Correctable memory error rate exceeded for DIMM_B8."},
	},
}

func (demoDevice) Info(ctx context.Context) (map[string]string, error) {
	demoState.Lock()
	defer demoState.Unlock()
	return map[string]string{
		"Model": "PowerEdge R730 (demo)", "ServiceTag": "DEMO123", "HostName": "demo01", "PowerState": demoState.power,
		"Health": "OK", "BIOS": "2.19.0", "iDRACFirmware": "2.86.86.86", "CPU": "2 x Intel(R) Xeon(R) CPU E5-2680 v4", "MemoryGiB": "128",
		"BootOverride": demoState.boot,
	}, nil
}
func (demoDevice) PowerState(ctx context.Context) (string, error) {
	demoState.Lock()
	defer demoState.Unlock()
	return demoState.power, nil
}
func (demoDevice) Power(ctx context.Context, action string) error {
	demoState.Lock()
	defer demoState.Unlock()
	switch action {
	case "on", "reset", "cycle", "restart", "nmi", "button":
		demoState.power = "On"
	case "off", "graceful":
		demoState.power = "Off"
	default:
		return fmt.Errorf("unknown power action %q", action)
	}
	return nil
}
func (demoDevice) Sensors(ctx context.Context) ([]redfish.Sensor, error) {
	return []redfish.Sensor{
		{Kind: "temperature", Name: "System Board Inlet Temp", Health: "OK", State: "Enabled", Reading: 21, Units: "C"},
		{Kind: "temperature", Name: "CPU1 Temp", Health: "OK", State: "Enabled", Reading: 44, Units: "C"},
		{Kind: "fan", Name: "System Board Fan1A", Health: "OK", State: "Enabled", Reading: 5880, Units: "RPM"},
		{Kind: "fan", Name: "System Board Fan2A", Health: "Warning", State: "Enabled", Reading: 1200, Units: "RPM"},
		{Kind: "voltage", Name: "PS1 Voltage 1", Health: "OK", State: "Enabled", Reading: 238, Units: "V"},
		{Kind: "psu", Name: "PS1 Status", Health: "OK", State: "Enabled", Reading: 184, Units: "W"},
	}, nil
}
func (demoDevice) SEL(ctx context.Context, limit int) ([]redfish.LogEntry, error) {
	demoState.Lock()
	defer demoState.Unlock()
	out := append([]redfish.LogEntry(nil), demoState.sel...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (demoDevice) ClearSEL(ctx context.Context) error {
	demoState.Lock()
	defer demoState.Unlock()
	demoState.sel = nil
	return nil
}
func (demoDevice) Identify(ctx context.Context, on bool) error {
	demoState.Lock()
	defer demoState.Unlock()
	demoState.led = on
	return nil
}
func (demoDevice) BootOnce(ctx context.Context, target string) error {
	demoState.Lock()
	defer demoState.Unlock()
	demoState.boot = target
	return nil
}
func (demoDevice) Close(ctx context.Context) error { return nil }
