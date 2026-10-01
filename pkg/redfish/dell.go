package redfish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Well-known iDRAC resource paths. Dell uses fixed IDs, which keeps these
// stable across iDRAC7/8/9.
const (
	SystemPath     = "/redfish/v1/Systems/System.Embedded.1"
	ManagerPath    = "/redfish/v1/Managers/iDRAC.Embedded.1"
	ChassisPath    = "/redfish/v1/Chassis/System.Embedded.1"
	SELPath        = ManagerPath + "/Logs/Sel"
	LCLogPath      = ManagerPath + "/Logs/Lclog"
	JobsPath       = ManagerPath + "/Jobs"
	AccountsPath   = ManagerPath + "/Accounts" // iDRAC7/8 (fixed slots 1..16)
	Accounts9Path  = "/redfish/v1/AccountService/Accounts"
	TasksPath      = "/redfish/v1/TaskService/Tasks"
	VirtualMedia   = ManagerPath + "/VirtualMedia"
	BiosPath       = SystemPath + "/Bios"
	BiosSettings   = SystemPath + "/Bios/Settings"
	AttributesPath = ManagerPath + "/Attributes" // iDRAC9 (and iDRAC8 >= 2.40 partially)
	UpdateService  = "/redfish/v1/UpdateService"
)

// ResetType values for ComputerSystem.Reset.
const (
	ResetOn               = "On"
	ResetForceOff         = "ForceOff"
	ResetGracefulShutdown = "GracefulShutdown"
	ResetGracefulRestart  = "GracefulRestart"
	ResetForceRestart     = "ForceRestart"
	ResetNmi              = "Nmi"
	ResetPushPowerButton  = "PushPowerButton"
	ResetPowerCycle       = "PowerCycle"
)

// SystemSummary is the subset of ComputerSystem most people want.
type SystemSummary struct {
	Model, Manufacturer, SerialNumber, SKU, HostName string
	PowerState, Health, State, BiosVersion           string
	ProcessorCount                                   int
	ProcessorModel                                   string
	MemoryGiB                                        float64
	IndicatorLED                                     string
	BootOverrideTarget, BootOverrideEnabled          string
	UUID                                             string
}

// System fetches the ComputerSystem summary.
func (c *Client) System(ctx context.Context) (*SystemSummary, Object, error) {
	o, err := c.GetObject(ctx, SystemPath)
	if err != nil {
		return nil, nil, err
	}
	s := &SystemSummary{
		Model:               o.Str("Model"),
		Manufacturer:        o.Str("Manufacturer"),
		SerialNumber:        o.Str("SerialNumber"),
		SKU:                 o.Str("SKU"),
		HostName:            o.Str("HostName"),
		PowerState:          o.Str("PowerState"),
		Health:              o.Str("Status.Health"),
		State:               o.Str("Status.State"),
		BiosVersion:         o.Str("BiosVersion"),
		ProcessorModel:      o.Str("ProcessorSummary.Model"),
		IndicatorLED:        o.Str("IndicatorLED"),
		BootOverrideTarget:  o.Str("Boot.BootSourceOverrideTarget"),
		BootOverrideEnabled: o.Str("Boot.BootSourceOverrideEnabled"),
		UUID:                o.Str("UUID"),
	}
	if f, ok := o.Float("ProcessorSummary.Count"); ok {
		s.ProcessorCount = int(f)
	}
	s.MemoryGiB, _ = o.Float("MemorySummary.TotalSystemMemoryGiB")
	return s, o, nil
}

// PowerState returns "On"/"Off".
func (c *Client) PowerState(ctx context.Context) (string, error) {
	var o struct {
		PowerState string
	}
	err := c.Get(ctx, SystemPath, &o)
	return o.PowerState, err
}

// Reset performs ComputerSystem.Reset with the given ResetType. When the
// firmware advertises @Redfish.AllowableValues and the requested type is not
// among them, the nearest supported type is used instead (iDRAC8 2.x offers
// only one of GracefulRestart/ForceRestart and has no PowerCycle).
func (c *Client) Reset(ctx context.Context, resetType string) error {
	allowed, _ := c.AllowableResetTypes(ctx)
	if len(allowed) > 0 && !contains(allowed, resetType) {
		alt := ""
		switch resetType {
		case ResetGracefulRestart:
			alt = ResetForceRestart
		case ResetForceRestart:
			alt = ResetGracefulRestart
		case ResetPowerCycle:
			// Emulate: force off, then on.
			if err := c.Reset(ctx, ResetForceOff); err != nil {
				return err
			}
			time.Sleep(3 * time.Second)
			return c.Reset(ctx, ResetOn)
		}
		if alt == "" || !contains(allowed, alt) {
			return fmt.Errorf("reset type %s not supported by this iDRAC (allowed: %s)", resetType, strings.Join(allowed, ", "))
		}
		resetType = alt
	}
	_, err := c.Post(ctx, SystemPath+"/Actions/ComputerSystem.Reset", map[string]string{"ResetType": resetType})
	return err
}

// AllowableResetTypes reads ResetType@Redfish.AllowableValues from the system.
func (c *Client) AllowableResetTypes(ctx context.Context) ([]string, error) {
	var o struct {
		Actions struct {
			Reset struct {
				Allowed []string `json:"ResetType@Redfish.AllowableValues"`
			} `json:"#ComputerSystem.Reset"`
		} `json:"Actions"`
	}
	if err := c.Get(ctx, SystemPath, &o); err != nil {
		return nil, err
	}
	return o.Actions.Reset.Allowed, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ResetManager reboots the iDRAC itself (Manager.Reset GracefulRestart).
func (c *Client) ResetManager(ctx context.Context) error {
	_, err := c.Post(ctx, ManagerPath+"/Actions/Manager.Reset", map[string]string{"ResetType": "GracefulRestart"})
	return err
}

// SetIndicatorLED sets the identify LED: "Lit", "Blinking" or "Off".
func (c *Client) SetIndicatorLED(ctx context.Context, state string) error {
	_, err := c.Patch(ctx, SystemPath, map[string]string{"IndicatorLED": state})
	return err
}

// SetBootOverride sets a one-time (or continuous) boot device override.
// target: None, Pxe, Cd, Hdd, BiosSetup, Usb, Utilities, UefiTarget, SDCard, ...
// enabled: Once, Continuous, Disabled. mode may be "" to leave it, or "UEFI"/"Legacy".
func (c *Client) SetBootOverride(ctx context.Context, target, enabled, mode string) error {
	boot := map[string]any{"BootSourceOverrideTarget": target, "BootSourceOverrideEnabled": enabled}
	if mode != "" {
		boot["BootSourceOverrideMode"] = mode
	}
	_, err := c.Patch(ctx, SystemPath, map[string]any{"Boot": boot})
	return err
}

// LogEntry is one SEL / Lifecycle-log record.
type LogEntry struct {
	ID        string
	Created   string
	Severity  string
	Message   string
	MessageID string
	Sensor    string
	EntryType string
}

// Log fetches a log collection (SELPath or LCLogPath); limit <= 0 means all.
func (c *Client) Log(ctx context.Context, path string, limit int) ([]LogEntry, error) {
	var out []LogEntry
	next := path + "/Entries"
	for next != "" {
		var col struct {
			Members []Object `json:"Members"`
			Next    string   `json:"Members@odata.nextLink"`
		}
		if err := c.Get(ctx, next, &col); err != nil {
			// iDRAC8 serves entries inline on the log resource itself.
			if IsNotFound(err) && strings.HasSuffix(next, "/Entries") {
				if err2 := c.Get(ctx, path, &col); err2 != nil {
					return out, err2
				}
			} else {
				return out, err
			}
		}
		for _, m := range col.Members {
			out = append(out, LogEntry{
				ID: m.Str("Id"), Created: m.Str("Created"), Severity: m.Str("Severity"),
				Message: m.Str("Message"), MessageID: m.Str("MessageId"), Sensor: m.Str("SensorType"),
				EntryType: m.Str("EntryType"),
			})
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
		next = col.Next
	}
	return out, nil
}

// ClearLog clears a log. path is SELPath or LCLogPath; the action lives under
// LogServices/<name> on every generation (iDRAC8 only exposes the entries
// under Logs/<name>).
func (c *Client) ClearLog(ctx context.Context, path string) error {
	name := path[strings.LastIndex(path, "/")+1:]
	_, err := c.Post(ctx, ManagerPath+"/LogServices/"+name+"/Actions/LogService.ClearLog", map[string]any{})
	if err != nil && IsNotFound(err) {
		_, err = c.Post(ctx, path+"/Actions/LogService.ClearLog", map[string]any{})
	}
	return err
}

// Sensor is a flattened thermal/power reading.
type Sensor struct {
	Kind, Name, Health, State string
	Reading                   float64
	Units                     string
}

// Sensors returns temperatures, fans, voltages and PSU readings from the
// Chassis Thermal and Power resources.
func (c *Client) Sensors(ctx context.Context) ([]Sensor, error) {
	var out []Sensor
	th, err := c.GetObject(ctx, ChassisPath+"/Thermal")
	if err != nil {
		return nil, err
	}
	for _, t := range th.List("Temperatures") {
		r, _ := t.Float("ReadingCelsius")
		out = append(out, Sensor{Kind: "temperature", Name: t.Str("Name"), Health: t.Str("Status.Health"), State: t.Str("Status.State"), Reading: r, Units: "C"})
	}
	for _, f := range th.List("Fans") {
		r, _ := f.Float("Reading")
		if r == 0 {
			r, _ = f.Float("ReadingRPM")
		}
		u := f.Str("ReadingUnits")
		if u == "" {
			u = "RPM"
		}
		out = append(out, Sensor{Kind: "fan", Name: f.Str("Name"), Health: f.Str("Status.Health"), State: f.Str("Status.State"), Reading: r, Units: u})
	}
	pw, err := c.GetObject(ctx, ChassisPath+"/Power")
	if err != nil {
		return out, err
	}
	for _, v := range pw.List("Voltages") {
		r, _ := v.Float("ReadingVolts")
		out = append(out, Sensor{Kind: "voltage", Name: v.Str("Name"), Health: v.Str("Status.Health"), State: v.Str("Status.State"), Reading: r, Units: "V"})
	}
	for _, p := range pw.List("PowerSupplies") {
		r, _ := p.Float("LastPowerOutputWatts")
		if r == 0 {
			r, _ = p.Float("PowerOutputWatts")
		}
		out = append(out, Sensor{Kind: "psu", Name: p.Str("Name"), Health: p.Str("Status.Health"), State: p.Str("Status.State"), Reading: r, Units: "W"})
	}
	for _, p := range pw.List("PowerControl") {
		r, _ := p.Float("PowerConsumedWatts")
		out = append(out, Sensor{Kind: "power", Name: p.Str("Name"), Health: p.Str("Status.Health"), State: p.Str("Status.State"), Reading: r, Units: "W"})
	}
	return out, nil
}

// Accounts lists iDRAC user accounts (tries the iDRAC9 then iDRAC8 path).
func (c *Client) Accounts(ctx context.Context) ([]Object, error) {
	objs, err := c.MemberObjects(ctx, Accounts9Path)
	if err != nil && IsNotFound(err) {
		objs, err = c.MemberObjects(ctx, AccountsPath)
	}
	return objs, err
}

// VirtualMediaDevices lists the virtual media slots (CD, RemovableDisk).
func (c *Client) VirtualMediaDevices(ctx context.Context) ([]Object, error) {
	return c.MemberObjects(ctx, VirtualMedia)
}

// InsertMedia mounts an image (http/https/nfs/cifs URL) into slot "CD" or "RemovableDisk".
// Older iDRAC8 firmware lacks the action; the returned error says so.
func (c *Client) InsertMedia(ctx context.Context, slot, image string, user, pass string) error {
	body := map[string]any{"Image": image, "Inserted": true, "WriteProtected": true}
	if user != "" {
		body["UserName"], body["Password"] = user, pass
	}
	_, err := c.Post(ctx, VirtualMedia+"/"+slot+"/Actions/VirtualMedia.InsertMedia", body)
	return err
}

// EjectMedia unmounts a slot.
func (c *Client) EjectMedia(ctx context.Context, slot string) error {
	_, err := c.Post(ctx, VirtualMedia+"/"+slot+"/Actions/VirtualMedia.EjectMedia", map[string]any{})
	return err
}

// Jobs lists Dell job queue entries.
func (c *Client) Jobs(ctx context.Context) ([]Object, error) { return c.MemberObjects(ctx, JobsPath) }

// DeleteJob removes a job (JID_...), or "JID_CLEARALL" for the whole queue.
func (c *Client) DeleteJob(ctx context.Context, id string) error {
	_, err := c.Delete(ctx, JobsPath+"/"+id)
	return err
}

// BiosAttributes returns the current BIOS attribute map.
func (c *Client) BiosAttributes(ctx context.Context) (Object, error) {
	o, err := c.GetObject(ctx, BiosPath)
	if err != nil {
		return nil, err
	}
	return o.Obj("Attributes"), nil
}

// SetBiosAttributes stages BIOS attribute changes and creates the config job
// that applies them at next reboot. Returns the job id.
func (c *Client) SetBiosAttributes(ctx context.Context, attrs map[string]any, rebootNow bool) (string, error) {
	if _, err := c.Patch(ctx, BiosSettings, map[string]any{"Attributes": attrs}); err != nil {
		return "", err
	}
	job := map[string]any{"TargetSettingsURI": BiosSettings, "StartTime": "TIME_NOW", "EndTime": "TIME_NA"}
	if rebootNow {
		job["RebootJobType"] = "PowerCycle"
	}
	resp, err := c.Post(ctx, JobsPath, job)
	if err != nil {
		return "", err
	}
	return jobIDFromLocation(resp.Location), nil
}

// Attributes returns the iDRAC attribute registry. iDRAC9 only: iDRAC8
// answers 404 even on its last firmware (2.86); use SCP export/import or
// racadm there.
func (c *Client) Attributes(ctx context.Context) (Object, error) {
	o, err := c.GetObject(ctx, AttributesPath)
	if err != nil {
		return nil, err
	}
	return o.Obj("Attributes"), nil
}

// SetAttributes patches iDRAC attributes ("Users.2.Password", "IPv4.1.Address", ...).
func (c *Client) SetAttributes(ctx context.Context, attrs map[string]any) error {
	_, err := c.Patch(ctx, AttributesPath, map[string]any{"Attributes": attrs})
	return err
}

// ExportSystemConfiguration runs the Dell SCP export and returns the job URI.
// target: ALL, IDRAC, BIOS, NIC, RAID. format: XML or JSON.
func (c *Client) ExportSystemConfiguration(ctx context.Context, target, format string) (string, error) {
	body := map[string]any{
		"ExportFormat": format, "ExportUse": "Default",
		"ShareParameters": map[string]any{"Target": target},
	}
	resp, err := c.Post(ctx, ManagerPath+"/Actions/Oem/EID_674_Manager.ExportSystemConfiguration", body)
	if err != nil {
		return "", err
	}
	return resp.Location, nil
}

// ImportSystemConfiguration applies an SCP buffer. shutdown: Graceful, Forced, NoReboot.
func (c *Client) ImportSystemConfiguration(ctx context.Context, target, buffer, shutdown string) (string, error) {
	body := map[string]any{
		"ImportBuffer": buffer, "ShutdownType": shutdown, "HostPowerState": "On",
		"ShareParameters": map[string]any{"Target": target},
	}
	resp, err := c.Post(ctx, ManagerPath+"/Actions/Oem/EID_674_Manager.ImportSystemConfiguration", body)
	if err != nil {
		return "", err
	}
	return resp.Location, nil
}

// SimpleUpdate starts a firmware update from an HTTP/NFS/CIFS URI.
func (c *Client) SimpleUpdate(ctx context.Context, imageURI, protocol string) (string, error) {
	body := map[string]any{"ImageURI": imageURI}
	if protocol != "" {
		body["TransferProtocol"] = protocol
	}
	resp, err := c.Post(ctx, UpdateService+"/Actions/UpdateService.SimpleUpdate", body)
	if err != nil {
		return "", err
	}
	return resp.Location, nil
}

// Task fetches a task/job monitor by its Location URI.
func (c *Client) Task(ctx context.Context, location string) (Object, error) {
	return c.GetObject(ctx, location)
}

// WaitTask polls a task/job until it leaves Running/New/Scheduled state.
func (c *Client) WaitTask(ctx context.Context, location string, interval time.Duration, progress func(Object)) (Object, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		o, err := c.GetObject(ctx, location)
		if err != nil {
			var e *Error
			// Job monitors return 202 while running and 200 when done; some
			// firmware returns the task with a 202 status which Do treats as OK.
			if !errors.As(err, &e) {
				return nil, err
			}
			return nil, err
		}
		if progress != nil {
			progress(o)
		}
		state := o.Str("TaskState")
		if state == "" {
			state = o.Str("JobState")
		}
		switch state {
		case "", "Running", "New", "Starting", "Scheduled", "Pending", "ReadyForExecution", "Downloading", "Downloaded":
		default:
			return o, nil
		}
		select {
		case <-ctx.Done():
			return o, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func jobIDFromLocation(loc string) string {
	if i := strings.LastIndex(loc, "/"); i >= 0 {
		return loc[i+1:]
	}
	return loc
}

// Probe reports whether the device answers on /redfish/v1 (no auth needed) and
// returns the service root when it does.
func Probe(ctx context.Context, baseURL string, insecure bool) (Object, error) {
	c := New(baseURL, "", "", WithInsecure(insecure), WithTimeout(10*time.Second))
	resp, err := c.Do(ctx, http.MethodGet, "/redfish/v1/", nil)
	if err != nil {
		return nil, err
	}
	var o Object
	if err := resp.JSON(&o); err != nil {
		return nil, fmt.Errorf("service root is not JSON: %w", err)
	}
	return o, nil
}
