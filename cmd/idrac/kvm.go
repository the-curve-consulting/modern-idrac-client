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
	"sync"
	"time"

	"idrac/pkg/kvm"
	"idrac/pkg/viewer"
	"idrac/pkg/webapi"
)

func init() {
	register(&command{
		name:   "kvm",
		usage:  "kvm [flags] [view | screenshot <file.png> | key <name>... | type <text> | mouse <x> <y> [click|right|middle|double] | ctrl-alt-del | vnc | probe | replay <f.rec> [out.png] | demo]",
		help:   "remote console, no Java: viewer window (default), screenshots, scripted input, VNC bridge",
		run:    cmdKVM,
		noHost: true, // replay and demo need none; the other verbs resolve it themselves
	})
}

type kvmOpts struct {
	viaWeb      bool
	direct      bool
	noAPCP      bool
	shared      bool
	viewOnly    bool
	record      string
	wait        time.Duration
	listen      string
	vncPassword string
	speed       float64
	exitAfter   time.Duration
}

const kvmVerbs = `verbs:
  view                  open the console window (default; needs the viewer build)
  screenshot <file.png> save the current screen
  key <name>...         press keys: F2, Return, Escape, ctrl+alt+F2, a
  type <text>           type text (\n = Enter; use -- before text starting with -)
  mouse <x> <y> [click|right|middle|double]
  ctrl-alt-del
  vnc                   serve the console to any VNC viewer (-listen, -vnc-password)
  probe                 transport handshake only: no login, no credentials
  replay <f.rec> [out.png]  play a recording in the window, or decode it to a PNG
  demo                  viewer test pattern, no iDRAC needed
`

func cmdKVM(ctx context.Context, g *globals, args []string) error {
	fs := subflags("kvm", "kvm [flags] [verb] [args]   (flags may also follow the verb)")
	o := kvmOpts{}
	fs.BoolVar(&o.viaWeb, "via-web", false, "use the web UI's one-time console credentials (what the browser does)")
	fs.BoolVar(&o.direct, "direct", false, "only try the configured account on port 5900 (default: direct, then web fallback)")
	fs.BoolVar(&o.noAPCP, "no-apcp", false, "skip the APCP pre-handshake (direct TLS)")
	fs.BoolVar(&o.shared, "shared", false, "request a shared session if the console is in use")
	fs.BoolVar(&o.viewOnly, "view-only", false, "view, vnc: do not forward keyboard/mouse")
	fs.StringVar(&o.record, "record", "", "also write the raw video stream to `file` (play it back with: kvm replay file)")
	fs.DurationVar(&o.wait, "wait", 6*time.Second, "screenshot: how long to let the video settle")
	fs.StringVar(&o.listen, "listen", "127.0.0.1:5901", "vnc: address for VNC viewers to connect to")
	fs.StringVar(&o.vncPassword, "vnc-password", os.Getenv("IDRAC_VNC_PASSWORD"), "vnc: require this password from viewers ($IDRAC_VNC_PASSWORD)")
	fs.Float64Var(&o.speed, "speed", 1, "replay: playback speed in the window")
	fs.DurationVar(&o.exitAfter, "exit-after", 0, "view, replay, demo: close the window after this long (smoke tests)")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		fmt.Fprint(os.Stderr, kvmVerbs)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	verb := "view"
	rest := fs.Args()
	if len(rest) > 0 {
		verb = rest[0]
		// Accept flags after the verb as well: `kvm view -view-only`.
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		rest = fs.Args()
	}
	switch verb {
	case "replay":
		return kvmReplay(ctx, g, o, rest)
	case "demo":
		return kvmDemo(g, o)
	}
	if err := g.resolveTarget(); err != nil {
		return err
	}
	switch verb {
	case "view":
		return kvmView(ctx, g, o)
	case "screenshot":
		return kvmScreenshot(ctx, g, o, rest)
	case "key":
		return kvmKeys(ctx, g, o, rest)
	case "type":
		return kvmType(ctx, g, o, strings.Join(rest, " "))
	case "mouse":
		return kvmMouse(ctx, g, o, rest)
	case "ctrl-alt-del":
		return kvmKeys(ctx, g, o, []string{"ctrl-alt-del"})
	case "vnc":
		return kvmVNC(ctx, g, o)
	case "probe":
		return kvmProbe(ctx, g, o)
	}
	fs.Usage()
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
	rec, err := recorderFor(o.record)
	if err != nil {
		return nil, err
	}
	return openConsoleRec(ctx, g, o, rec)
}

var (
	recorderMu sync.Mutex
	recorders  = map[string]*kvm.Recorder{}
)

// recorderFor returns the process-wide recorder for path ("" = none). It is
// shared across reconnects so a dropped session does not truncate the file.
func recorderFor(path string) (*kvm.Recorder, error) {
	if path == "" {
		return nil, nil
	}
	recorderMu.Lock()
	defer recorderMu.Unlock()
	if r, ok := recorders[path]; ok {
		return r, nil
	}
	f, err := os.Create(path) // stays open for the life of the process
	if err != nil {
		return nil, err
	}
	r, err := kvm.NewRecorder(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	recorders[path] = r
	return r, nil
}

// kvmReplay plays a recording: in the viewer window when no output file is
// given and the viewer is compiled in, otherwise headless, writing the final
// frame and printing decoder statistics (for chasing video artefacts without
// an iDRAC).
func kvmReplay(ctx context.Context, g *globals, o kvmOpts, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: kvm replay <file.rec> [out.png]")
	}
	if len(args) == 1 && viewer.Available {
		return kvmReplayWindow(g, o, args[0])
	}
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	fb := kvm.NewFramebuffer()
	st, err := kvm.Replay(ctx, f, fb, g.logger, 0)
	if st != nil {
		w, h := fb.Size()
		fmt.Fprintf(g.out, "%dx%d, %d frames, %d packets, %d decode errors, %d unknown, aspeed %d frames/%d errors, dvc %d frames/%d checksum errors\n",
			w, h, fb.Frames(), st.Packets, st.DecodeErrors, st.Unknown, st.ASpeedFrames, st.ASpeedErrors, st.DVC.Frames, st.DVC.ChecksumErrors)
	}
	if err != nil {
		return err
	}
	if len(args) > 1 {
		out, err := os.Create(args[1])
		if err != nil {
			return err
		}
		defer out.Close()
		if err := fb.WritePNG(out); err != nil {
			return err
		}
		fmt.Fprintf(g.out, "wrote %s\n", args[1])
	}
	return nil
}

// openConsoleRec is openConsole with an optional video recorder attached.
func openConsoleRec(ctx context.Context, g *globals, o kvmOpts, rec *kvm.Recorder) (*kvm.Console, error) {
	cfg, err := g.kvmConfig(ctx, o)
	if err != nil {
		return nil, err
	}
	cfg.VideoRecorder = rec
	c, err := kvm.OpenConsole(ctx, cfg)
	var le *kvm.LoginError
	if err != nil && !o.viaWeb && !o.direct && errors.As(err, &le) {
		g.logger.Printf("console refused direct login (%s); retrying with web-UI one-time credentials", le.Reason)
		o.viaWeb = true
		cfg, err2 := g.kvmConfig(ctx, o)
		if err2 != nil {
			return nil, fmt.Errorf("%v; web fallback: %w", err, err2)
		}
		cfg.VideoRecorder = rec
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

func kvmVNC(ctx context.Context, g *globals, o kvmOpts) error {
	c, err := openConsole(ctx, g, o)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.WaitRunning(ctx); err != nil {
		return err
	}
	srv := &kvm.VNCServer{FB: c.FB, Input: c.Session, Password: o.vncPassword, Name: "iDRAC " + g.target.Name, Logger: g.logger, ViewOnly: o.viewOnly}
	srv.OnClientChange = func(n int) {
		if n > 0 {
			c.Session.MouseOrigin()
			c.Video.RequestRefresh()
		} else {
			c.Session.ReleaseAllKeys()
		}
	}
	fmt.Fprintf(os.Stderr, "console up; connect a VNC viewer to %s (Ctrl-C to stop)\n", o.listen)
	vctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-c.Done()
		cancel()
	}()
	err = srv.ListenAndServe(vctx, o.listen)
	if cerr := c.Err(); cerr != nil {
		return cerr
	}
	return err
}
