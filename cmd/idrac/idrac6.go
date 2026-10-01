package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"idrac/pkg/redfish"
	"idrac/pkg/webapi"
)

// idrac6Device implements Device on top of the legacy /data XML API, the
// only HTTP API an iDRAC6 has. Key names come from the firmware's own UI
// pages (see docs/idrac6-web-api.md).
type idrac6Device struct{ w *webapi.Client }

func init() {
	newIDRAC6Device = func(ctx context.Context, g *globals) (Device, error) {
		w, err := g.webClient(ctx)
		if err != nil {
			return nil, err
		}
		return &idrac6Device{w: w}, nil
	}
}

func (d *idrac6Device) Close(ctx context.Context) error { return d.w.Logout(ctx) }

var idrac6PowerStates = map[string]string{"0": "Off", "1": "On"}

func (d *idrac6Device) Info(ctx context.Context) (map[string]string, error) {
	m, _, err := d.w.Get(ctx, "sysDesc", "svcTag", "expSvcCode", "hostName", "osName", "osVersion", "biosVer", "fwVersion", "LCCfwVersion", "sysRev", "macAddr", "pwState")
	if err != nil {
		return nil, err
	}
	out := map[string]string{
		"Model": m["sysDesc"], "ServiceTag": m["svcTag"], "ExpressServiceCode": m["expSvcCode"], "HostName": m["hostName"],
		"OS": strings.TrimSpace(m["osName"] + " " + m["osVersion"]), "BIOS": m["biosVer"], "iDRACFirmware": m["fwVersion"],
		"LifecycleController": m["LCCfwVersion"], "SystemRevision": m["sysRev"], "MAC": m["macAddr"],
	}
	if ps, ok := idrac6PowerStates[strings.TrimSpace(m["pwState"])]; ok {
		out["PowerState"] = ps
	} else {
		out["PowerState"] = m["pwState"]
	}
	return out, nil
}

func (d *idrac6Device) PowerState(ctx context.Context) (string, error) {
	m, _, err := d.w.Get(ctx, "pwState")
	if err != nil {
		return "", err
	}
	if ps, ok := idrac6PowerStates[strings.TrimSpace(m["pwState"])]; ok {
		return ps, nil
	}
	return m["pwState"], nil
}

// pwState codes from powercontrol.html: OFF:0 ON:1 POWERCYCLE:2 REBOOT:3 NMI:4 SHUTDOWN:5.
var idrac6PowerActions = map[string]string{"off": "0", "on": "1", "cycle": "2", "reset": "3", "nmi": "4", "graceful": "5", "restart": "3", "button": "1"}

func (d *idrac6Device) Power(ctx context.Context, action string) error {
	code, ok := idrac6PowerActions[action]
	if !ok {
		return fmt.Errorf("unknown power action %q", action)
	}
	_, err := d.w.Set(ctx, map[string]string{"pwState": code})
	return err
}

func idrac6Health(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "2", "ok", "normal":
		return "OK"
	case "3", "warning", "noncritical":
		return "Warning"
	case "4", "5", "critical", "failed":
		return "Critical"
	case "":
		return ""
	}
	return status
}

func (d *idrac6Device) Sensors(ctx context.Context) ([]redfish.Sensor, error) {
	m, _, err := d.w.Get(ctx, "temperatures", "fans", "voltages", "powerSupplies", "intrusion")
	if err != nil {
		return nil, err
	}
	var out []redfish.Sensor
	add := func(kind, key, units string) {
		for _, it := range webapi.ParseList(m[key]) {
			r, _ := strconv.ParseFloat(strings.Fields(it["reading"] + " ")[0], 64)
			u := units
			if it["units"] != "" {
				u = it["units"]
			}
			name := it["name"]
			if name == "" {
				name = it["location"]
			}
			out = append(out, redfish.Sensor{Kind: kind, Name: name, Health: idrac6Health(it["sensorStatus"]), State: it["status"], Reading: r, Units: u})
		}
	}
	add("temperature", "temperatures", "C")
	add("fan", "fans", "RPM")
	add("voltage", "voltages", "V")
	for _, it := range webapi.ParseList(m["powerSupplies"]) {
		r, _ := strconv.ParseFloat(strings.Fields(it["inputWattage"] + " ")[0], 64)
		out = append(out, redfish.Sensor{Kind: "psu", Name: strings.TrimSpace(it["location"] + " " + it["type"]), Health: idrac6Health(it["sensorStatus"]), Reading: r, Units: "W"})
	}
	for _, it := range webapi.ParseList(m["intrusion"]) {
		out = append(out, redfish.Sensor{Kind: "intrusion", Name: it["name"], Health: idrac6Health(it["sensorStatus"]), State: it["reading"]})
	}
	return out, nil
}

func (d *idrac6Device) SEL(ctx context.Context, limit int) ([]redfish.LogEntry, error) {
	m, _, err := d.w.Get(ctx, "eventLogEntries")
	if err != nil {
		return nil, err
	}
	items := webapi.ParseList(m["eventLogEntries"])
	var out []redfish.LogEntry
	for i, it := range items {
		out = append(out, redfish.LogEntry{ID: strconv.Itoa(i + 1), Created: it["dateTime"], Severity: it["severity"], Message: it["description"]})
	}
	// The iDRAC returns oldest first; show newest first like the UI.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (d *idrac6Device) ClearSEL(ctx context.Context) error {
	_, err := d.w.Set(ctx, map[string]string{"clearSEL": "1"})
	return err
}

func (d *idrac6Device) Identify(ctx context.Context, on bool) error {
	if on {
		_, err := d.w.Set(ctx, map[string]string{"IdentifyEnable": "1", "IdentifyTimeout": "0"})
		return err
	}
	_, err := d.w.Set(ctx, map[string]string{"IdentifyEnable": "0", "IdentifyTimeout": "0"})
	return err
}

// firstBootDevice codes from firstboot.html.
var idrac6BootDevices = map[string]string{"none": "0", "pxe": "1", "hdd": "2", "cd": "5", "bios": "6", "vfloppy": "7", "vcd": "8", "iscsi": "9", "vflash": "11", "floppy": "15", "sd": "16", "usb": "15"}

func (d *idrac6Device) BootOnce(ctx context.Context, target string) error {
	code, ok := idrac6BootDevices[target]
	if !ok {
		return fmt.Errorf("unknown boot target %q (iDRAC6 supports: none pxe hdd cd bios floppy vfloppy vcd iscsi vflash sd)", target)
	}
	once := "1"
	if code == "0" {
		once = "0"
	}
	_, err := d.w.Set(ctx, map[string]string{"vmBootOnce": once, "firstBootDevice": code})
	return err
}
