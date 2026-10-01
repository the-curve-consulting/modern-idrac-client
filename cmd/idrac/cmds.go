package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"idrac/pkg/racadm"
	"idrac/pkg/redfish"
)

func init() {
	register(&command{name: "hosts", usage: "hosts", help: "list configured iDRACs", noHost: true, run: cmdHosts})
	register(&command{name: "probe", usage: "probe", help: "detect generation / API surfaces (no credentials needed)", run: cmdProbe})
	register(&command{name: "info", usage: "info", help: "system summary (model, service tag, firmware, power, health)", run: cmdInfo})
	register(&command{name: "power", usage: "power [status|on|off|graceful|reset|cycle|nmi|button]", help: "query or change host power", run: cmdPower})
	register(&command{name: "sensors", usage: "sensors", help: "temperatures, fans, voltages, PSUs", run: cmdSensors})
	register(&command{name: "sel", usage: "sel [-n N] [clear]", help: "system event log", run: cmdSEL})
	register(&command{name: "lclog", usage: "lclog [-n N]", help: "lifecycle log (Redfish generations)", run: cmdLCLog})
	register(&command{name: "led", usage: "led on|off", help: "chassis identify LED", run: cmdLED})
	register(&command{name: "boot", usage: "boot pxe|hdd|cd|bios|usb|none", help: "one-time boot device override", run: cmdBoot})
	register(&command{name: "racadm", usage: "racadm <command...>", help: "run a racadm command over SSH (all generations)", run: cmdRacadm})
	register(&command{name: "ssh", usage: "ssh", help: "interactive SSH shell on the iDRAC", run: cmdSSH})
	register(&command{name: "redfish", usage: "redfish get|post|patch|delete <path> [json-body]", help: "raw Redfish call", run: cmdRedfish})
	register(&command{name: "jobs", usage: "jobs [delete <JID|all>]", help: "iDRAC job queue", run: cmdJobs})
	register(&command{name: "vmedia", usage: "vmedia [insert CD|RemovableDisk <url>|eject <slot>]", help: "Redfish virtual media", run: cmdVMedia})
	register(&command{name: "accounts", usage: "accounts", help: "list iDRAC user accounts", run: cmdAccounts})
	register(&command{name: "bios", usage: "bios [set Name=Value... [-reboot]]", help: "BIOS attributes (read, or stage changes)", run: cmdBios})
	register(&command{name: "attrs", usage: "attrs [set Group.N.Name=Value...]", help: "iDRAC attributes (iDRAC9; partial on iDRAC8)", run: cmdAttrs})
	register(&command{name: "scp", usage: "scp export [-target ALL] [-format XML] | import <file> [-shutdown Graceful]", help: "server configuration profile", run: cmdSCP})
	register(&command{name: "update", usage: "update <image-uri>", help: "firmware update via Redfish SimpleUpdate", run: cmdUpdate})
	register(&command{name: "reset-idrac", usage: "reset-idrac", help: "reboot the iDRAC itself", run: cmdResetIDRAC})
}

func cmdHosts(ctx context.Context, g *globals, args []string) error {
	if len(g.cfg.Names()) == 0 {
		fmt.Fprintf(g.out, "no hosts configured (looked in %s)\n", g.cfg.Path())
		return nil
	}
	rows := [][]string{}
	for _, n := range g.cfg.Names() {
		h := g.cfg.Resolve(n)
		rows = append(rows, []string{n, h.Address, string(h.Generation), h.Username, h.Description})
	}
	g.table([]string{"NAME", "ADDRESS", "GEN", "USER", "DESCRIPTION"}, rows)
	return nil
}

func cmdProbe(ctx context.Context, g *globals, args []string) error {
	g.target.Generation = ""
	res := map[string]any{"address": g.target.Address}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	root, err := redfish.Probe(pctx, g.target.BaseURL(), g.target.InsecureTLS())
	if err != nil {
		res["redfish"] = false
		res["redfish_error"] = err.Error()
		if redfish.IsNotFound(err) {
			res["generation"] = "idrac6"
		}
	} else {
		res["redfish"] = true
		res["redfish_version"] = root.Str("RedfishVersion")
		res["service_root_type"] = root.Str("@odata.type")
		res["generation"] = string(classifyRedfish(root))
	}
	if g.jsonOut {
		return g.printJSON(res)
	}
	keys := make([]string, 0, len(res))
	for k := range res {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(g.out, "%-18s %v\n", k+":", res[k])
	}
	return nil
}

func cmdInfo(ctx context.Context, g *globals, args []string) error {
	d, err := openDevice(ctx, g)
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	info, err := d.Info(ctx)
	if err != nil {
		return err
	}
	if g.jsonOut {
		return g.printJSON(info)
	}
	keys := make([]string, 0, len(info))
	for k := range info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(g.out, "%-16s %s\n", k+":", info[k])
	}
	return nil
}

func cmdPower(ctx context.Context, g *globals, args []string) error {
	action := "status"
	if len(args) > 0 {
		action = args[0]
	}
	d, err := openDevice(ctx, g)
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	if action == "status" {
		st, err := d.PowerState(ctx)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return g.printJSON(map[string]string{"power_state": st})
		}
		fmt.Fprintln(g.out, st)
		return nil
	}
	if err := d.Power(ctx, action); err != nil {
		return err
	}
	fmt.Fprintf(g.out, "power %s: ok\n", action)
	return nil
}

func cmdSensors(ctx context.Context, g *globals, args []string) error {
	d, err := openDevice(ctx, g)
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	ss, err := d.Sensors(ctx)
	if err != nil {
		return err
	}
	if g.jsonOut {
		return g.printJSON(ss)
	}
	rows := [][]string{}
	for _, s := range ss {
		rows = append(rows, []string{s.Kind, s.Name, fmt.Sprintf("%g %s", s.Reading, s.Units), s.Health, s.State})
	}
	g.table([]string{"KIND", "NAME", "READING", "HEALTH", "STATE"}, rows)
	return nil
}

func cmdSEL(ctx context.Context, g *globals, args []string) error {
	fs := subflags("sel", "sel [-n N] [clear]")
	n := fs.Int("n", 50, "max entries (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := openDevice(ctx, g)
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	if fs.NArg() > 0 && fs.Arg(0) == "clear" {
		if err := d.ClearSEL(ctx); err != nil {
			return err
		}
		fmt.Fprintln(g.out, "SEL cleared")
		return nil
	}
	entries, err := d.SEL(ctx, *n)
	if err != nil {
		return err
	}
	return printLog(g, entries)
}

func printLog(g *globals, entries []redfish.LogEntry) error {
	if g.jsonOut {
		return g.printJSON(entries)
	}
	rows := [][]string{}
	for _, e := range entries {
		rows = append(rows, []string{e.ID, e.Created, e.Severity, e.Message})
	}
	g.table([]string{"ID", "CREATED", "SEVERITY", "MESSAGE"}, rows)
	return nil
}

func cmdLCLog(ctx context.Context, g *globals, args []string) error {
	fs := subflags("lclog", "lclog [-n N]")
	n := fs.Int("n", 50, "max entries (0 = all)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	entries, err := c.Log(ctx, redfish.LCLogPath, *n)
	if err != nil {
		return err
	}
	return printLog(g, entries)
}

func cmdLED(ctx context.Context, g *globals, args []string) error {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return errors.New("usage: led on|off")
	}
	d, err := openDevice(ctx, g)
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	return d.Identify(ctx, args[0] == "on")
}

func cmdBoot(ctx context.Context, g *globals, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: boot pxe|hdd|cd|bios|usb|none")
	}
	d, err := openDevice(ctx, g)
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	if err := d.BootOnce(ctx, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(g.out, "next boot: %s\n", args[0])
	return nil
}

func (g *globals) racadmClient(ctx context.Context) (*racadm.Client, error) {
	pw, err := g.passwd(ctx)
	if err != nil {
		return nil, err
	}
	return &racadm.Client{Address: fmt.Sprintf("%s:%d", g.target.Address, g.target.SSHPort), Username: g.target.Username, Password: pw, Timeout: g.timeout}, nil
}

func cmdRacadm(ctx context.Context, g *globals, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: racadm <command...>  e.g. racadm getsysinfo")
	}
	c, err := g.racadmClient(ctx)
	if err != nil {
		return err
	}
	res, err := c.Run(ctx, strings.Join(args, " "))
	if err != nil {
		return err
	}
	fmt.Fprint(g.out, res.Stdout)
	if res.Stderr != "\n" {
		fmt.Fprint(g.errw, res.Stderr)
	}
	if res.ExitCode != 0 {
		return &exitError{res.ExitCode}
	}
	return nil
}

func cmdRedfish(ctx context.Context, g *globals, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: redfish get|post|patch|delete <path> [json-body]")
	}
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	method := strings.ToUpper(args[0])
	var body any
	if len(args) > 2 {
		b := args[2]
		if strings.HasPrefix(b, "@") {
			data, err := os.ReadFile(b[1:])
			if err != nil {
				return err
			}
			b = string(data)
		}
		body = []byte(b)
	}
	resp, err := c.Do(ctx, method, args[1], body)
	if err != nil {
		if resp != nil && len(resp.Body) > 0 && g.verbose {
			fmt.Fprintln(g.errw, string(resp.Body))
		}
		return err
	}
	if resp.Location != "" {
		fmt.Fprintf(g.errw, "HTTP %d Location: %s\n", resp.Status, resp.Location)
	}
	if len(strings.TrimSpace(string(resp.Body))) == 0 {
		fmt.Fprintf(g.out, "HTTP %d\n", resp.Status)
		return nil
	}
	var v any
	if err := resp.JSON(&v); err != nil {
		fmt.Fprintln(g.out, string(resp.Body))
		return nil
	}
	return g.printJSON(v)
}

func cmdJobs(ctx context.Context, g *globals, args []string) error {
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	if len(args) == 2 && args[0] == "delete" {
		id := args[1]
		if id == "all" {
			id = "JID_CLEARALL"
		}
		return c.DeleteJob(ctx, id)
	}
	jobs, err := c.Jobs(ctx)
	if err != nil {
		return err
	}
	if g.jsonOut {
		return g.printJSON(jobs)
	}
	rows := [][]string{}
	for _, j := range jobs {
		rows = append(rows, []string{j.Str("Id"), j.Str("JobState"), j.Str("PercentComplete") + "%", j.Str("Name"), j.Str("Message")})
	}
	g.table([]string{"ID", "STATE", "DONE", "NAME", "MESSAGE"}, rows)
	return nil
}

func cmdVMedia(ctx context.Context, g *globals, args []string) error {
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	switch {
	case len(args) == 0:
		devs, err := c.VirtualMediaDevices(ctx)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return g.printJSON(devs)
		}
		rows := [][]string{}
		for _, d := range devs {
			rows = append(rows, []string{d.Str("Id"), d.Str("Inserted"), d.Str("Image"), d.Str("ConnectedVia")})
		}
		g.table([]string{"SLOT", "INSERTED", "IMAGE", "VIA"}, rows)
		return nil
	case len(args) >= 3 && args[0] == "insert":
		user, pass := "", ""
		if len(args) >= 5 {
			user, pass = args[3], args[4]
		}
		return c.InsertMedia(ctx, args[1], args[2], user, pass)
	case len(args) == 2 && args[0] == "eject":
		return c.EjectMedia(ctx, args[1])
	}
	return errors.New("usage: vmedia | vmedia insert CD|RemovableDisk <url> [user pass] | vmedia eject <slot>")
}

func cmdAccounts(ctx context.Context, g *globals, args []string) error {
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	accs, err := c.Accounts(ctx)
	if err != nil {
		return err
	}
	if g.jsonOut {
		return g.printJSON(accs)
	}
	rows := [][]string{}
	for _, a := range accs {
		if a.Str("UserName") == "" && a.Str("Enabled") != "true" {
			continue
		}
		rows = append(rows, []string{a.Str("Id"), a.Str("UserName"), a.Str("RoleId"), a.Str("Enabled"), a.Str("Locked")})
	}
	g.table([]string{"ID", "USER", "ROLE", "ENABLED", "LOCKED"}, rows)
	return nil
}

func parseAssignments(args []string) (map[string]any, error) {
	m := map[string]any{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("expected Name=Value, got %q", a)
		}
		if i, err := strconv.Atoi(v); err == nil {
			m[k] = i
		} else if v == "true" || v == "false" {
			m[k] = v == "true"
		} else {
			m[k] = v
		}
	}
	return m, nil
}

func cmdBios(ctx context.Context, g *globals, args []string) error {
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "set" {
		fs := subflags("bios set", "bios set Name=Value... [-reboot]")
		reboot := fs.Bool("reboot", false, "power-cycle now to apply")
		// allow flags after the assignments
		var assigns []string
		var flags []string
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				flags = append(flags, a)
			} else {
				assigns = append(assigns, a)
			}
		}
		if err := fs.Parse(flags); err != nil {
			return err
		}
		attrs, err := parseAssignments(assigns)
		if err != nil {
			return err
		}
		jid, err := c.SetBiosAttributes(ctx, attrs, *reboot)
		if err != nil {
			return err
		}
		fmt.Fprintf(g.out, "staged; job %s (applies at next boot)\n", jid)
		return nil
	}
	attrs, err := c.BiosAttributes(ctx)
	if err != nil {
		return err
	}
	return printAttrs(g, attrs, args)
}

func printAttrs(g *globals, attrs redfish.Object, filter []string) error {
	if g.jsonOut {
		return g.printJSON(attrs)
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		if len(filter) > 0 && !strings.Contains(strings.ToLower(k), strings.ToLower(filter[0])) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(g.out, "%s=%s\n", k, attrs.Str(k))
	}
	return nil
}

func cmdAttrs(ctx context.Context, g *globals, args []string) error {
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "set" {
		attrs, err := parseAssignments(args[1:])
		if err != nil {
			return err
		}
		return c.SetAttributes(ctx, attrs)
	}
	attrs, err := c.Attributes(ctx)
	if err != nil {
		return err
	}
	return printAttrs(g, attrs, args)
}

func cmdSCP(ctx context.Context, g *globals, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: scp export [-target ALL] [-format XML] | scp import <file> [-target ALL] [-shutdown Graceful]")
	}
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	switch args[0] {
	case "export":
		fs := subflags("scp export", "scp export [-target ALL|IDRAC|BIOS|NIC|RAID] [-format XML|JSON]")
		target := fs.String("target", "ALL", "component")
		format := fs.String("format", "XML", "output format")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		loc, err := c.ExportSystemConfiguration(ctx, *target, *format)
		if err != nil {
			return err
		}
		res, err := c.WaitTask(ctx, loc, 3*time.Second, func(o redfish.Object) { g.logger.Printf("task %s %s%%", o.Str("TaskState"), o.Str("PercentComplete")) })
		if err != nil {
			return err
		}
		// When finished the task monitor returns the profile itself.
		if res.Str("TaskState") == "" {
			return g.printJSON(res)
		}
		fmt.Fprintf(g.out, "task %s: %s\n", res.Str("TaskState"), res.Str("Messages"))
		return nil
	case "import":
		fs := subflags("scp import", "scp import <file> [-target ALL] [-shutdown Graceful|Forced|NoReboot]")
		target := fs.String("target", "ALL", "component")
		shutdown := fs.String("shutdown", "Graceful", "host shutdown type")
		rest := args[1:]
		if len(rest) == 0 {
			return errors.New("scp import needs a file")
		}
		file := rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		loc, err := c.ImportSystemConfiguration(ctx, *target, string(data), *shutdown)
		if err != nil {
			return err
		}
		fmt.Fprintf(g.out, "import started: %s\n", loc)
		return nil
	}
	return errors.New("scp: expected export or import")
}

func cmdUpdate(ctx context.Context, g *globals, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: update <http|nfs|cifs image uri>")
	}
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	loc, err := c.SimpleUpdate(ctx, args[0], "")
	if err != nil {
		return err
	}
	fmt.Fprintf(g.out, "update job: %s\n", loc)
	return nil
}

func cmdResetIDRAC(ctx context.Context, g *globals, args []string) error {
	gen, err := g.generation(ctx)
	if err != nil {
		return err
	}
	if !gen.HasRedfish() {
		c, err := g.racadmClient(ctx)
		if err != nil {
			return err
		}
		res, err := c.Run(ctx, "racreset soft")
		if err != nil {
			return err
		}
		fmt.Fprint(g.out, res.Stdout)
		return res.Err()
	}
	c, err := g.redfishClient(ctx)
	if err != nil {
		return err
	}
	return c.ResetManager(ctx)
}
