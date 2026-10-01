package main

import (
	"context"
	"errors"
	"fmt"
	"image/png"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"idrac/pkg/kvm"
	"idrac/pkg/webapi"
)

func init() {
	register(&command{
		name:  "kvm",
		usage: "kvm [-via-web] probe | screenshot <file.png> | key <name>... | type <text> | mouse <x> <y> [click|right] | ctrl-alt-del | vnc [-listen :5901] [-vnc-password p]",
		help:  "remote console (Avocent protocol, no Java): screenshots, input, VNC bridge",
		run:   cmdKVM,
	})
}

type kvmOpts struct {
	viaWeb   bool
	direct   bool
	wait     time.Duration
	noAPCP   bool
	shared   bool
	viewOnly bool
}

func cmdKVM(ctx context.Context, g *globals, args []string) error {
	fs := subflags("kvm", "kvm [flags] screenshot|key|type|mouse|ctrl-alt-del|vnc|watch ...")
	o := kvmOpts{}
	fs.BoolVar(&o.viaWeb, "via-web", false, "log in to the web UI and use the one-time console credentials from viewer.jnlp (what the browser does)")
	fs.BoolVar(&o.direct, "direct", false, "always use the configured username/password on port 5900 (default: direct first, web fallback on login failure)")
	fs.DurationVar(&o.wait, "wait", 6*time.Second, "how long to let the video settle before a screenshot")
	fs.BoolVar(&o.noAPCP, "no-apcp", false, "skip the APCP pre-handshake (direct TLS)")
	fs.BoolVar(&o.shared, "shared", false, "request a shared session if the console is in use")
	fs.BoolVar(&o.viewOnly, "view-only", false, "VNC bridge: do not forward keyboard/mouse")
	// allow flags after the verb too
	var verbArgs, flagArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(verbArgs) == 0 {
			flagArgs = append(flagArgs, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && isValueFlag(a) {
				flagArgs = append(flagArgs, args[i+1])
				i++
			}
			continue
		}
		verbArgs = append(verbArgs, a)
	}
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if len(verbArgs) == 0 {
		fs.Usage()
		return errors.New("kvm: missing verb")
	}
	verb, rest := verbArgs[0], verbArgs[1:]
	switch verb {
	case "screenshot", "shot":
		return kvmScreenshot(ctx, g, o, rest)
	case "key", "keys":
		return kvmKeys(ctx, g, o, rest)
	case "type":
		return kvmType(ctx, g, o, strings.Join(rest, " "))
	case "mouse":
		return kvmMouse(ctx, g, o, rest)
	case "ctrl-alt-del", "cad":
		return kvmKeys(ctx, g, o, []string{"ctrl-alt-del"})
	case "vnc":
		return kvmVNC(ctx, g, o, rest)
	case "probe":
		return kvmProbe(ctx, g, o)
	case "creds":
		l, err := kvmWebCredentials(ctx, g)
		if err != nil {
			return err
		}
		return g.printJSON(map[string]any{"host": l.Host, "kmport": l.KMPort, "vport": l.VPort, "user": l.User, "passwd": "<redacted>", "args": redact(l.Args)})
	}
	return fmt.Errorf("kvm: unknown verb %q", verb)
}

// kvmProbe performs only the APCP handshake and TLS upgrade on the control
// and video sockets — no login, no credentials — so the transport can be
// checked without risking an account lockout.
func kvmProbe(ctx context.Context, g *globals, o kvmOpts) error {
	opts := kvm.DialOptions{UseAPCP: !o.noAPCP, Logger: g.logger, Trace: g.trace, Timeout: 15 * time.Second}
	res := map[string]any{"host": g.target.Address, "port": g.target.KVMPort}
	for name, dial := range map[string]func(context.Context, string, int, kvm.DialOptions) (net.Conn, *kvm.APCPInfo, error){"control": kvm.DialControl, "video": kvm.DialVideo} {
		conn, info, err := dial(ctx, g.target.Address, g.target.KVMPort, opts)
		if err != nil {
			res[name] = map[string]any{"error": err.Error()}
			continue
		}
		conn.Close()
		m := map[string]any{"tls": info.TLS, "apcp_version": fmt.Sprintf("%d.%d", info.Major, info.Minor), "capabilities": info.Capabilities, "message_type": fmt.Sprintf("%#x", info.MessageType), "reply_length": info.Length}
		if info.RedirectPort != 0 {
			m["redirect_port"] = info.RedirectPort
		}
		res[name] = m
	}
	return g.printJSON(res)
}

func isValueFlag(a string) bool {
	switch strings.TrimLeft(a, "-") {
	case "wait", "listen", "vnc-password":
		return true
	}
	return false
}

func redact(m map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if k == "passwd" || k == "password" {
			v = "<redacted>"
		}
		out[k] = v
	}
	return out
}

// kvmWebCredentials performs the browser's launch sequence: web login, then
// fetch viewer.jnlp, whose arguments carry temporary console credentials.
func kvmWebCredentials(ctx context.Context, g *globals) (*webapi.ConsoleLaunch, error) {
	pw, err := g.passwd(ctx)
	if err != nil {
		return nil, err
	}
	w := webapi.New(g.target.BaseURL(), g.target.Username, pw, g.target.InsecureTLS(), g.timeout)
	if g.verbose {
		w.Logger = g.logger
	}
	if err := w.Login(ctx); err != nil {
		return nil, err
	}
	defer w.Logout(ctx)
	return w.ConsoleCredentials(ctx, "idrac-go")
}

func (g *globals) kvmConfig(ctx context.Context, o kvmOpts) (kvm.Config, error) {
	cfg := kvm.Config{
		Host:        g.target.Address,
		ControlPort: g.target.KVMPort,
		VideoPort:   g.target.KVMPort,
		UseAPCP:     !o.noAPCP,
		Shared:      o.shared,
		Logger:      g.logger,
		Trace:       g.trace,
		Timeout:     30 * time.Second,
	}
	if o.viaWeb {
		l, err := kvmWebCredentials(ctx, g)
		if err != nil {
			return cfg, fmt.Errorf("console credentials via web UI: %w", err)
		}
		cfg.Username, cfg.Password = l.User, l.Password
		if l.KMPort != 0 {
			cfg.ControlPort = l.KMPort
		}
		if l.VPort != 0 {
			cfg.VideoPort = l.VPort
		}
		cfg.UseAPCP = l.APCP || cfg.UseAPCP
		return cfg, nil
	}
	pw, err := g.passwd(ctx)
	if err != nil {
		return cfg, err
	}
	cfg.Username, cfg.Password = g.target.Username, pw
	return cfg, nil
}

// openConsole connects with the chosen credential strategy. With neither
// -direct nor -via-web it tries the real account first (works on iDRAC6) and,
// if the console rejects the login, falls back to the web-UI launch tokens.
func openConsole(ctx context.Context, g *globals, o kvmOpts) (*kvm.Console, error) {
	cfg, err := g.kvmConfig(ctx, o)
	if err != nil {
		return nil, err
	}
	c, err := kvm.OpenConsole(ctx, cfg)
	var le *kvm.LoginError
	if err != nil && !o.viaWeb && !o.direct && errors.As(err, &le) {
		g.logger.Printf("console refused direct login (%s); retrying with web-UI one-time credentials", le.Reason)
		o.viaWeb = true
		cfg, err2 := g.kvmConfig(ctx, o)
		if err2 != nil {
			return nil, fmt.Errorf("%v; web fallback: %w", err, err2)
		}
		c, err = kvm.OpenConsole(ctx, cfg)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func kvmScreenshot(ctx context.Context, g *globals, o kvmOpts, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: kvm screenshot <file.png>")
	}
	c, err := openConsole(ctx, g, o)
	if err != nil {
		return err
	}
	defer c.Close()
	wctx, cancel := context.WithTimeout(ctx, o.wait+30*time.Second)
	defer cancel()
	img, err := c.WaitFrames(wctx, 2, o.wait)
	if err != nil {
		return err
	}
	f, err := os.Create(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	b := img.Bounds()
	fmt.Fprintf(g.out, "wrote %s (%dx%d, %d frames)\n", args[0], b.Dx(), b.Dy(), c.FB.Frames())
	return nil
}

func kvmKeys(ctx context.Context, g *globals, o kvmOpts, names []string) error {
	if len(names) == 0 {
		return errors.New("usage: kvm key <name>...  (e.g. F1, Return, ctrl-alt-del, ctrl+alt+F2)")
	}
	c, err := openConsole(ctx, g, o)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.WaitRunning(ctx); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	for _, n := range names {
		if err := pressNamed(c.Session, n); err != nil {
			return err
		}
		time.Sleep(150 * time.Millisecond)
	}
	fmt.Fprintf(g.out, "sent: %s\n", strings.Join(names, " "))
	return nil
}

// pressNamed handles "F1", "Return", "ctrl+alt+F2", "ctrl-alt-del", single chars.
func pressNamed(s *kvm.Session, name string) error {
	if strings.EqualFold(name, "ctrl-alt-del") {
		return s.CtrlAltDel()
	}
	parts := strings.Split(name, "+")
	var usages []uint16
	for _, p := range parts {
		u, ok := kvm.KeyNameToUsage(p)
		if !ok {
			return fmt.Errorf("unknown key %q", p)
		}
		usages = append(usages, u)
	}
	if len(usages) == 1 {
		return s.Press(usages[0])
	}
	return s.Chord(usages...)
}

func kvmType(ctx context.Context, g *globals, o kvmOpts, text string) error {
	if text == "" {
		return errors.New("usage: kvm type <text>  (\\n for Enter)")
	}
	text = strings.ReplaceAll(text, `\n`, "\n")
	c, err := openConsole(ctx, g, o)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.WaitRunning(ctx); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	return c.Session.TypeString(text, 40*time.Millisecond)
}

func kvmMouse(ctx context.Context, g *globals, o kvmOpts, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: kvm mouse <x> <y> [click|right|middle|double]")
	}
	x, err1 := strconv.Atoi(args[0])
	y, err2 := strconv.Atoi(args[1])
	if err1 != nil || err2 != nil {
		return errors.New("mouse: x and y must be integers")
	}
	c, err := openConsole(ctx, g, o)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.WaitRunning(ctx); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	c.Session.MouseOrigin()
	if err := c.Session.MouseMove(x, y); err != nil {
		return err
	}
	if len(args) > 2 {
		mask := uint8(1)
		switch args[2] {
		case "right":
			mask = 2
		case "middle":
			mask = 4
		}
		clicks := 1
		if args[2] == "double" {
			clicks = 2
		}
		for i := 0; i < clicks; i++ {
			c.Session.MouseButton(mask, true)
			time.Sleep(60 * time.Millisecond)
			c.Session.MouseButton(mask, false)
			time.Sleep(80 * time.Millisecond)
		}
	}
	return nil
}

func kvmVNC(ctx context.Context, g *globals, o kvmOpts, args []string) error {
	fs := subflags("kvm vnc", "kvm vnc [-listen :5901] [-vnc-password <pw>]")
	listen := fs.String("listen", "127.0.0.1:5901", "address for VNC viewers to connect to")
	vncPass := fs.String("vnc-password", os.Getenv("IDRAC_VNC_PASSWORD"), "require VNC authentication with this password ($IDRAC_VNC_PASSWORD)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := openConsole(ctx, g, o)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.WaitRunning(ctx); err != nil {
		return err
	}
	srv := &kvm.VNCServer{FB: c.FB, Input: c.Session, Password: *vncPass, Name: "iDRAC " + g.target.Name, Logger: g.logger, ViewOnly: o.viewOnly}
	srv.OnClientChange = func(n int) {
		if n > 0 {
			c.Session.MouseOrigin()
			c.Video.RequestRefresh()
		} else {
			c.Session.ReleaseAllKeys()
		}
	}
	fmt.Fprintf(os.Stderr, "console up; connect a VNC viewer to %s (Ctrl-C to stop)\n", *listen)
	vctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-c.Done()
		cancel()
	}()
	err = srv.ListenAndServe(vctx, *listen)
	if cerr := c.Err(); cerr != nil {
		return cerr
	}
	return err
}
