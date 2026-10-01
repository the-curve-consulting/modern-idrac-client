package main

import (
	"context"
	"fmt"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/config"
	"github.com/the-curve-consulting/modern-idrac-client/pkg/redfish"
)

// Device abstracts the management operations that exist on every generation,
// so the CLI commands do not care whether they talk Redfish (iDRAC7+) or the
// legacy /data XML API (iDRAC6).
type Device interface {
	Info(ctx context.Context) (map[string]string, error)
	PowerState(ctx context.Context) (string, error)
	// Power performs one of: on, off, graceful, reset, cycle, nmi, button.
	Power(ctx context.Context, action string) error
	Sensors(ctx context.Context) ([]redfish.Sensor, error)
	SEL(ctx context.Context, limit int) ([]redfish.LogEntry, error)
	ClearSEL(ctx context.Context) error
	// Identify blinks (on=true) or stops blinking the chassis LED.
	Identify(ctx context.Context, on bool) error
	// BootOnce sets a one-time boot device: pxe, hdd, cd, bios, usb, none.
	BootOnce(ctx context.Context, target string) error
	Close(ctx context.Context) error
}

// openDevice picks the implementation for the target's generation.
func openDevice(ctx context.Context, g *globals) (Device, error) {
	gen, err := g.generation(ctx)
	if err != nil {
		return nil, err
	}
	switch gen {
	case config.GenIDRAC6:
		return openIDRAC6(ctx, g)
	case config.GenDemo:
		return demoDevice{}, nil
	}
	c, err := g.redfishClient(ctx)
	if err != nil {
		return nil, err
	}
	return &redfishDevice{c: c}, nil
}

type redfishDevice struct{ c *redfish.Client }

func (d *redfishDevice) Info(ctx context.Context) (map[string]string, error) {
	s, o, err := d.c.System(ctx)
	if err != nil {
		return nil, err
	}
	m := map[string]string{
		"Model": s.Model, "ServiceTag": s.SKU, "SerialNumber": s.SerialNumber, "HostName": s.HostName,
		"PowerState": s.PowerState, "Health": s.Health, "BIOS": s.BiosVersion,
		"CPU": fmt.Sprintf("%d x %s", s.ProcessorCount, s.ProcessorModel), "MemoryGiB": fmt.Sprintf("%.0f", s.MemoryGiB),
		"IndicatorLED": s.IndicatorLED, "BootOverride": s.BootOverrideTarget + "/" + s.BootOverrideEnabled, "UUID": s.UUID,
	}
	if mgr, err := d.c.GetObject(ctx, redfish.ManagerPath); err == nil {
		m["iDRACFirmware"] = mgr.Str("FirmwareVersion")
		m["iDRACModel"] = mgr.Str("Model")
	}
	if len(o) > 0 {
		m["Manufacturer"] = o.Str("Manufacturer")
	}
	return m, nil
}

func (d *redfishDevice) PowerState(ctx context.Context) (string, error) { return d.c.PowerState(ctx) }

var redfishPowerActions = map[string]string{
	"on": redfish.ResetOn, "off": redfish.ResetForceOff, "graceful": redfish.ResetGracefulShutdown,
	"reset": redfish.ResetForceRestart, "cycle": redfish.ResetPowerCycle, "nmi": redfish.ResetNmi,
	"button": redfish.ResetPushPowerButton, "restart": redfish.ResetGracefulRestart,
}

func (d *redfishDevice) Power(ctx context.Context, action string) error {
	rt, ok := redfishPowerActions[action]
	if !ok {
		return fmt.Errorf("unknown power action %q", action)
	}
	err := d.c.Reset(ctx, rt)
	if err != nil && action == "cycle" {
		// Older firmware lacks PowerCycle; emulate with off + on.
		if e := d.c.Reset(ctx, redfish.ResetForceOff); e == nil {
			return d.c.Reset(ctx, redfish.ResetOn)
		}
	}
	return err
}

func (d *redfishDevice) Sensors(ctx context.Context) ([]redfish.Sensor, error) {
	return d.c.Sensors(ctx)
}
func (d *redfishDevice) SEL(ctx context.Context, limit int) ([]redfish.LogEntry, error) {
	return d.c.Log(ctx, redfish.SELPath, limit)
}
func (d *redfishDevice) ClearSEL(ctx context.Context) error {
	return d.c.ClearLog(ctx, redfish.SELPath)
}
func (d *redfishDevice) Identify(ctx context.Context, on bool) error {
	if on {
		return d.c.SetIndicatorLED(ctx, "Blinking")
	}
	return d.c.SetIndicatorLED(ctx, "Off")
}

var redfishBootTargets = map[string]string{"pxe": "Pxe", "hdd": "Hdd", "cd": "Cd", "bios": "BiosSetup", "usb": "Usb", "none": "None", "utilities": "Utilities", "diags": "Diags", "sd": "SDCard", "floppy": "Floppy", "uefi": "UefiTarget"}

func (d *redfishDevice) BootOnce(ctx context.Context, target string) error {
	t, ok := redfishBootTargets[target]
	if !ok {
		return fmt.Errorf("unknown boot target %q", target)
	}
	enabled := "Once"
	if t == "None" {
		enabled = "Disabled"
	}
	return d.c.SetBootOverride(ctx, t, enabled, "")
}
func (d *redfishDevice) Close(ctx context.Context) error { return d.c.Logout(ctx) }
