// Command idrac is a native Go client for Dell iDRAC6/7/8/9: inventory,
// power, sensors, logs, racadm, raw Redfish/legacy-web calls and the remote
// console (screenshots, key/mouse injection and a VNC bridge) without Java.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"idrac/pkg/config"
	"idrac/pkg/redfish"
)

// globals holds the flags shared by every subcommand.
type globals struct {
	configPath string
	host       string
	user       string
	password   string
	gen        string
	insecure   bool
	verbose    bool
	trace      bool
	jsonOut    bool
	timeout    time.Duration

	cfg    *config.File
	target *config.Host
	logger *log.Logger
	out    io.Writer
}

type command struct {
	name, usage, help string
	run               func(ctx context.Context, g *globals, args []string) error
	noHost            bool // command does not need --host
}

var commands []*command

func register(c *command) { commands = append(commands, c) }

func main() {
	g := &globals{out: os.Stdout}
	fs := flag.NewFlagSet("idrac", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&g.configPath, "config", "", "hosts file (default: $IDRAC_CONFIG, ./idrac.json, ~/.config/idrac/hosts.json)")
	fs.StringVar(&g.host, "host", os.Getenv("IDRAC_HOST"), "host name from the config file, or an IP/hostname ($IDRAC_HOST)")
	fs.StringVar(&g.host, "H", g.host, "short for -host")
	fs.StringVar(&g.user, "user", "", "override the username ($IDRAC_USER)")
	fs.StringVar(&g.password, "password", "", "override the password (prefer $IDRAC_PASSWORD or the config file)")
	fs.StringVar(&g.gen, "gen", "", "force generation: idrac6|idrac7|idrac8|idrac9 (default: from config or probe)")
	fs.BoolVar(&g.insecure, "insecure", true, "skip TLS certificate verification")
	fs.BoolVar(&g.verbose, "v", false, "log requests to stderr")
	fs.BoolVar(&g.trace, "trace", false, "hex-dump console protocol packets (implies -v)")
	fs.BoolVar(&g.jsonOut, "json", false, "machine-readable JSON output where supported")
	fs.DurationVar(&g.timeout, "timeout", 90*time.Second, "per-operation timeout")
	fs.Usage = func() { usage(fs) }
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if u := os.Getenv("IDRAC_USER"); g.user == "" && u != "" {
		g.user = u
	}
	if g.trace {
		g.verbose = true
	}
	g.logger = log.New(io.Discard, "", log.Ltime|log.Lmicroseconds)
	if g.verbose {
		g.logger.SetOutput(os.Stderr)
	}
	args := fs.Args()
	if len(args) == 0 {
		usage(fs)
		os.Exit(2)
	}
	cmd := lookup(args[0])
	if cmd == nil {
		fmt.Fprintf(os.Stderr, "idrac: unknown command %q\n\n", args[0])
		usage(fs)
		os.Exit(2)
	}
	var err error
	g.cfg, err = config.Load(g.configPath)
	if err != nil {
		fatal(err)
	}
	installPasswordPrompt()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !cmd.noHost {
		if g.host == "" {
			fatal(fmt.Errorf("no host given: use -host <name|address> (configured: %s)", strings.Join(g.cfg.Names(), ", ")))
		}
		g.target = g.cfg.Resolve(g.host)
		if g.user != "" {
			g.target.Username = g.user
		}
		if g.password != "" {
			g.target.Password = g.password
		}
		if g.gen != "" {
			g.target.Generation = config.Generation(g.gen)
		}
		ins := g.insecure
		g.target.Insecure = &ins
	}
	if err := cmd.run(ctx, g, args[1:]); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "idrac:", err)
	os.Exit(1)
}

func lookup(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

func usage(fs *flag.FlagSet) {
	fmt.Fprintf(os.Stderr, "usage: idrac [global flags] <command> [args]\n\nGlobal flags:\n")
	fs.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nCommands:\n")
	sort.Slice(commands, func(i, j int) bool { return commands[i].name < commands[j].name })
	tw := tabwriter.NewWriter(os.Stderr, 2, 4, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.help)
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "\nUsage per command:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  idrac -host <name|ip> %s\n", c.usage)
	}
	fmt.Fprintf(os.Stderr, `
Quick start (no credentials needed):
  idrac -host 192.168.11.221 probe        # detect generation / Redfish
  idrac -host 192.168.11.221 kvm probe    # console transport handshake
Then, with IDRAC_PASSWORD set (or a hosts file at %s):
  idrac -host 192.168.11.221 racadm getsysinfo
  idrac -host 192.168.11.221 info
  idrac -host 192.168.11.221 -v -trace kvm screenshot shot.png
`, config.DefaultPath())
}

// password resolves the target's password once.
func (g *globals) passwd(ctx context.Context) (string, error) {
	p, err := g.target.ResolvePassword(ctx)
	if err != nil {
		return "", err
	}
	g.target.Password = p
	return p, nil
}

// generation returns the target's generation, probing Redfish when unknown.
func (g *globals) generation(ctx context.Context) (config.Generation, error) {
	if g.target.Generation != config.GenAuto {
		return g.target.Generation, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	root, err := redfish.Probe(pctx, g.target.BaseURL(), g.target.InsecureTLS())
	if err != nil {
		if redfish.IsNotFound(err) {
			g.logger.Printf("no Redfish service on %s: assuming iDRAC6", g.target.Address)
			g.target.Generation = config.GenIDRAC6
			return g.target.Generation, nil
		}
		return "", fmt.Errorf("probe %s: %w", g.target.Address, err)
	}
	g.target.Generation = classifyRedfish(root)
	g.logger.Printf("probed %s: %s (RedfishVersion %s)", g.target.Address, g.target.Generation, root.Str("RedfishVersion"))
	return g.target.Generation, nil
}

// classifyRedfish guesses the generation from the unauthenticated service root.
// iDRAC9 reports RedfishVersion >= 1.6 and a ServiceRoot v1_5+ type.
func classifyRedfish(root redfish.Object) config.Generation {
	v := root.Str("RedfishVersion")
	t := root.Str("@odata.type")
	switch {
	case strings.HasPrefix(v, "1.0") || strings.HasPrefix(v, "1.1") || strings.HasPrefix(v, "1.2"):
		return config.GenIDRAC7
	case strings.Contains(t, "v1_3_") || strings.Contains(t, "v1_4_") || strings.HasPrefix(v, "1.3") || strings.HasPrefix(v, "1.4") || strings.HasPrefix(v, "1.5"):
		return config.GenIDRAC8
	default:
		return config.GenIDRAC9
	}
}

func (g *globals) redfishClient(ctx context.Context) (*redfish.Client, error) {
	pw, err := g.passwd(ctx)
	if err != nil {
		return nil, err
	}
	opts := []redfish.Option{redfish.WithInsecure(g.target.InsecureTLS()), redfish.WithTimeout(g.timeout)}
	if g.verbose {
		opts = append(opts, redfish.WithLogger(g.logger))
	}
	return redfish.New(g.target.BaseURL(), g.target.Username, pw, opts...), nil
}

// printJSON pretty-prints v.
func (g *globals) printJSON(v any) error {
	enc := json.NewEncoder(g.out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table prints rows with aligned columns.
func (g *globals) table(header []string, rows [][]string) {
	tw := tabwriter.NewWriter(g.out, 2, 4, 2, ' ', 0)
	if header != nil {
		fmt.Fprintln(tw, strings.Join(header, "\t"))
	}
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// subflags builds a FlagSet for a subcommand with a consistent usage line.
func subflags(name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: idrac [global flags] %s\n", usage)
		fs.PrintDefaults()
	}
	return fs
}
