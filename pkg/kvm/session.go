package kvm

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// State is the session state, mirroring the SESSION_STATE property of
// com.avocent.kvm.b.r.
type State int

const (
	StateInitializing     State = iota
	StateConnecting             // transport being established
	StateAuthenticating         // Login Request sent, waiting for User Login Response
	StateVideoPending           // login accepted, waiting for Video Connect Status
	StateRunning                // RUNNING: video enabled, input accepted
	StateClosing                // server announced disconnect (User Disconnect Pending)
	StateConnectionFailed       // transport failure
	StateLoginFailed            // CONNECTION_LOGIN_FAILED
	StateClosed                 // Close() called / socket gone
)

func (s State) String() string {
	switch s {
	case StateInitializing:
		return "INITIALIZING"
	case StateConnecting:
		return "CONNECTING"
	case StateAuthenticating:
		return "AUTHENTICATING"
	case StateVideoPending:
		return "VIDEO_PENDING"
	case StateRunning:
		return "RUNNING"
	case StateClosing:
		return "CLOSING"
	case StateConnectionFailed:
		return "CONNECTION_FAILED"
	case StateLoginFailed:
		return "CONNECTION_LOGIN_FAILED"
	case StateClosed:
		return "CLOSED"
	}
	return fmt.Sprintf("State(%d)", int(s))
}

// LoginError is returned by Connect when the appliance rejects the login.
type LoginError struct {
	Status  int
	Reason  string // LOGIN_REASON string (BAD_LOGIN, ACCESS_DENIED, IN_USE, ...)
	Message string // free-text message from the appliance, if any
}

func (e *LoginError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("kvm: login failed: %s (status %d): %s", e.Reason, e.Status, e.Message)
	}
	return fmt.Sprintf("kvm: login failed: %s (status %d)", e.Reason, e.Status)
}

// Config describes one console session.
type Config struct {
	Host        string
	ControlPort int // kmport (iDRAC: 5900)
	VideoPort   int // vport  (iDRAC: 5900)
	Username    string
	Password    string
	// UseAPCP selects the APCP pre-handshake (launcher apcp=1). Off means a
	// direct TLS control socket.
	UseAPCP bool
	// Shared requests a shared session (share-mode byte 1). The viewer sets
	// it when the user accepts the "session in use, share?" dialog.
	Shared bool
	// LoginPort is the "port" byte of the Login Request. 0 = automatic:
	// 1 (launcher PORT param, iDRAC6 viewer) when the APCP server version
	// is < 2, else ControlPort&0xFF (iDRAC8 viewer sends KMPORT&0xFF,
	// com.avocent.d.d.b.a(int,int,boolean)). Set to a non-zero value to
	// force it. Channel is the launcher CHANNEL param (0).
	LoginPort int
	Channel   int
	// RIP is an optional hexadecimal RIP id (unused by iDRAC).
	RIP string
	// ProtocolMajor/Minor default to 2.34; they select the Login Request
	// variant and the SharedUsers layout.
	ProtocolMajor, ProtocolMinor int
	// TLSConfig overrides DefaultTLSConfig().
	TLSConfig *tls.Config
	// Timeout bounds dials/handshakes and the wait for the login response
	// (default 30s).
	Timeout time.Duration
	// KeepAliveInterval defaults to 2s (com.avocent.kvm.b.x).
	KeepAliveInterval time.Duration
	// PollServers controls the GetAvailableServers (0x0420) poll the iDRAC6
	// viewer sends with every keepalive while RUNNING. PollAuto (default)
	// enables it only when the negotiated protocol is < 2.34 (iDRAC6); the
	// packet no longer exists on iDRAC7/8.
	PollServers PollMode
	// VideoConnectTimeout: if no Video Connect Status arrives this long
	// after OpenVideo, the session goes RUNNING anyway (0 = 10s, negative =
	// wait forever). UNVERIFIED which socket carries that packet.
	VideoConnectTimeout time.Duration
	// ReadTimeout, when > 0, aborts Run if nothing arrives on the control
	// socket for that long. The appliance does not send periodic traffic
	// unless GetAvailableServers polling is on, so keep this >> 2s.
	ReadTimeout time.Duration
	// Dialer optionally replaces the TCP dialer.
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)
	Logger Logger
	// Trace hex-dumps every control-channel packet and the video handshake.
	Trace bool
	// TraceVideo additionally hex-dumps traffic on the video socket returned
	// by OpenVideo (reads are truncated to 64 bytes).
	TraceVideo bool
}

// PollMode selects GetAvailableServers polling (Config.PollServers).
type PollMode int

const (
	PollAuto PollMode = iota // on when negotiated protocol < 2.34, else off
	PollOn
	PollOff
)

// Session is one Avocent KVM control-channel session.
type Session struct {
	cfg Config
	log Logger

	// Callbacks (set before Connect/Run; called from Run's goroutine).
	OnState          func(State)
	OnShutdown       func(reason int, text string)
	OnVideoMode      func(width, height int)
	OnPacket         func(Packet)       // every decoded server packet
	OnVideoPacket    func(*VideoPacket) // video data arriving on the control socket
	OnVideoStopped   func(reason int)
	OnKeyboardLED    func(mask uint8)
	OnSharingRequest func(*SharingRequest) // answer with RespondSharing
	OnServers        func([]ServerEntry)

	mu                     sync.Mutex
	state                  State
	ctrl                   net.Conn
	apcp                   *APCPInfo
	wmu                    sync.Mutex // serialises writes to ctrl
	video                  net.Conn
	vwmu                   sync.Mutex
	login                  *LoginResponse
	platform               string
	clientRandom           int32
	protoMajor, protoMinor int
	videoOpened            time.Time
	videoAckCount          int
	mouseButtons           uint8
	mouseX, mouseY         int
	rfbWheel               uint8
	closeOnce              sync.Once
	closed                 chan struct{}
	done                   chan struct{}
	runErr                 error
}

// NewSession creates a session; nothing is connected yet.
func NewSession(cfg Config) *Session {
	if cfg.Logger == nil {
		cfg.Logger = nopLogger{}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.KeepAliveInterval <= 0 {
		cfg.KeepAliveInterval = 2 * time.Second
	}
	if cfg.VideoConnectTimeout == 0 {
		cfg.VideoConnectTimeout = 10 * time.Second
	}
	if cfg.ProtocolMajor == 0 && cfg.ProtocolMinor == 0 {
		cfg.ProtocolMajor, cfg.ProtocolMinor = DefaultProtocolMajor, DefaultProtocolMinor
	}
	if cfg.ControlPort == 0 {
		cfg.ControlPort = 5900
	}
	if cfg.VideoPort == 0 {
		cfg.VideoPort = cfg.ControlPort
	}
	return &Session{
		cfg:        cfg,
		log:        cfg.Logger,
		state:      StateInitializing,
		protoMajor: cfg.ProtocolMajor,
		protoMinor: cfg.ProtocolMinor,
		closed:     make(chan struct{}),
		done:       make(chan struct{}),
		// com.avocent.kvm.b.r.C = (int)(Math.random()*1e7)
		clientRandom: int32(rand.IntN(10_000_000)),
	}
}

// State returns the current session state.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// LoginResponse returns the appliance's login response (nil before
// Connect succeeds).
func (s *Session) LoginResponse() *LoginResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.login
}

// ClientRandom is the session id sent in the Login Request and repeated in
// the Video Channel Auth.
func (s *Session) ClientRandom() int32 { return s.clientRandom }

// ProtocolVersion returns the negotiated protocol version (after APCP).
func (s *Session) ProtocolVersion() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protoMajor, s.protoMinor
}

// Platform returns the platform tag from the Input Resolution Response
// ("AS" on iDRAC7/8, "" on iDRAC6 or before the packet arrived).
func (s *Session) Platform() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.platform
}

// loginPort returns the Login Request port byte (Config.LoginPort rule).
func (s *Session) loginPort() uint8 {
	if s.cfg.LoginPort != 0 {
		return uint8(s.cfg.LoginPort)
	}
	s.mu.Lock()
	info := s.apcp
	s.mu.Unlock()
	if info != nil && info.Major >= 2 {
		return uint8(s.cfg.ControlPort)
	}
	return 1
}

// pollServers applies Config.PollServers.
func (s *Session) pollServers() bool {
	switch s.cfg.PollServers {
	case PollOn:
		return true
	case PollOff:
		return false
	}
	maj, min := s.ProtocolVersion()
	return maj < 2 || (maj == 2 && min < 34)
}

// NeedsVideoChannel reports whether a separate video socket must be opened
// (login response flag bit 0 clear). Valid after Connect.
func (s *Session) NeedsVideoChannel() bool {
	lr := s.LoginResponse()
	return lr != nil && !lr.VideoOnControlChannel()
}

func (s *Session) setState(st State) {
	s.mu.Lock()
	if s.state == st || s.state == StateClosed {
		s.mu.Unlock()
		return
	}
	old := s.state
	s.state = st
	s.mu.Unlock()
	s.log.Printf("kvm: state %s -> %s", old, st)
	if s.OnState != nil {
		s.OnState(st)
	}
}

func (s *Session) dialOptions() DialOptions {
	return DialOptions{
		Timeout:       s.cfg.Timeout,
		TLSConfig:     s.cfg.TLSConfig,
		UseAPCP:       s.cfg.UseAPCP,
		ProtocolMajor: s.cfg.ProtocolMajor,
		ProtocolMinor: s.cfg.ProtocolMinor,
		Dialer:        s.cfg.Dialer,
		Logger:        s.log,
		Trace:         s.cfg.Trace,
	}
}

// negotiateVersion applies the APCP version rule of com.avocent.kvm.b.r:
// if the server reports at least our version keep the server's, otherwise
// fall back to 2.33. iDRAC8 (com.avocent.d.a.a) simply adopts what the
// server echoes; since iDRAC8 echoes the advertised version (2.34 by
// default) both rules agree.
func (s *Session) negotiateVersion(info *APCPInfo) {
	if info == nil || !s.cfg.UseAPCP {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if info.Major >= s.cfg.ProtocolMajor && info.Minor >= s.cfg.ProtocolMinor {
		s.protoMajor, s.protoMinor = info.Major, info.Minor
	} else if info.Major >= 2 {
		s.protoMajor, s.protoMinor = info.Major, info.Minor
	} else {
		s.protoMajor, s.protoMinor = 2, 33
	}
}

// Connect opens the control socket, sends the Login Request and waits for
// the User Login Response. Any other packets that arrive first are
// dispatched normally. On rejection a *LoginError is returned and the
// socket is closed.
func (s *Session) Connect(ctx context.Context) error {
	s.setState(StateConnecting)
	conn, info, err := DialControl(ctx, s.cfg.Host, s.cfg.ControlPort, s.dialOptions())
	if err != nil {
		s.setState(StateConnectionFailed)
		return err
	}
	s.mu.Lock()
	s.ctrl = conn
	s.apcp = info
	s.mu.Unlock()
	s.negotiateVersion(info)
	maj, min := s.ProtocolVersion()
	s.log.Printf("kvm: control socket up (tls=%v, protocol %d.%d)", info != nil && info.TLS, maj, min)

	// Layout selection mirrors com.avocent.c.d.ib.b(): major==1 -> 0x0100,
	// (major==0 || major>=2) && minor<41 -> 0x0102, major>=2 && minor>=41 -> 0x0104.
	req := &LoginRequest{
		Username:      s.cfg.Username,
		Password:      s.cfg.Password,
		Port:          s.loginPort(),
		Channel:       uint8(s.cfg.Channel),
		ClientRandom:  s.clientRandom,
		WithShareMode: maj == 0 || maj >= 2,
		Long:          maj >= 2 && min >= 41,
	}
	if s.cfg.Shared {
		req.ShareMode = ShareModeShared
	}
	if s.cfg.RIP != "" {
		if err := req.SetRIP(s.cfg.RIP); err != nil {
			s.closeConn()
			return err
		}
		req.Channel = uint8(s.cfg.Channel)
	}
	s.setState(StateAuthenticating)
	if err := s.Send(req); err != nil {
		s.setState(StateConnectionFailed)
		s.closeConn()
		return fmt.Errorf("kvm: send login: %w", err)
	}

	deadline := time.Now().Add(s.cfg.Timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		_ = conn.SetReadDeadline(deadline)
		f, err := ReadFrame(conn, false)
		if err != nil {
			s.setState(StateConnectionFailed)
			s.closeConn()
			return fmt.Errorf("kvm: waiting for login response: %w", err)
		}
		p := s.handleFrame(f)
		if lr, ok := p.(*LoginResponse); ok {
			_ = conn.SetReadDeadline(time.Time{})
			if !lr.Accepted() {
				s.setState(StateLoginFailed)
				s.closeConn()
				return &LoginError{Status: lr.Status, Reason: lr.Reason(), Message: lr.Message}
			}
			return nil
		}
	}
}

// Run is the control loop: it reads server packets until the socket
// closes or ctx is cancelled, sends keepalives, and fires callbacks. It
// returns nil after a clean server-initiated shutdown or Close(), the
// read error otherwise.
func (s *Session) Run(ctx context.Context) error {
	s.mu.Lock()
	conn := s.ctrl
	s.mu.Unlock()
	if conn == nil {
		return errors.New("kvm: Run called before Connect")
	}
	defer close(s.done)

	stop := make(chan struct{})
	defer close(stop)
	go s.keepAliveLoop(stop)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-stop:
		case <-s.closed:
		}
	}()

	for {
		if s.cfg.ReadTimeout > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		}
		f, err := ReadFrame(conn, false)
		if err != nil {
			st := s.State()
			select {
			case <-s.closed:
				return nil
			default:
			}
			if st == StateClosing {
				s.Close()
				return nil
			}
			if ctx.Err() != nil {
				s.Close()
				return ctx.Err()
			}
			s.setState(StateConnectionFailed)
			s.Close()
			return fmt.Errorf("kvm: control socket: %w", err)
		}
		s.handleFrame(f)
		s.checkVideoTimeout()
	}
}

// Done is closed when Run returns.
func (s *Session) Done() <-chan struct{} { return s.done }

// handleFrame decodes and dispatches one control-channel frame.
func (s *Session) handleFrame(f *Frame) Packet {
	if s.cfg.Trace {
		s.log.Printf("kvm <<< type 0x%04x %s len %d\n%s", f.Type, TypeName(f.Type), f.Len(), HexDump(f.Bytes(), 512))
	}
	maj, min := s.ProtocolVersion()
	p, err := Decode(f, maj, min)
	if err != nil {
		s.log.Printf("kvm: %v", err)
		return p
	}
	if s.OnPacket != nil {
		s.OnPacket(p)
	}
	switch pk := p.(type) {
	case *LoginResponse:
		s.onLoginResponse(pk)
	case *VideoConnectStatus:
		s.log.Printf("kvm: video connect status: connected=%v", pk.Connected)
		if pk.Connected {
			s.VideoConnected()
		}
	case *DisplayResolution:
		s.log.Printf("kvm: display resolution %dx%d", pk.Width, pk.Height)
		if s.OnVideoMode != nil {
			s.OnVideoMode(pk.Width, pk.Height)
		}
		// com.avocent.kvm.b.n.a(jb) -> r.b(): request a full refresh.
		if s.State() == StateRunning {
			_ = s.Send(NewScreenRefresh())
		}
	case *InputResolution:
		s.log.Printf("kvm: input resolution %dx%d platform %q", pk.Width, pk.Height, pk.Platform)
		if pk.Platform != "" {
			s.mu.Lock()
			s.platform = pk.Platform
			s.mu.Unlock()
		}
	case *UserDisconnectPending:
		s.log.Printf("kvm: server disconnecting: %s", ShutdownText(pk.Reason))
		s.setState(StateClosing)
		if s.OnShutdown != nil {
			s.OnShutdown(pk.Reason, ShutdownText(pk.Reason))
		}
	case *VideoStopped:
		s.log.Printf("kvm: video stopped: %s", VideoStoppedText(pk.Reason))
		if s.OnVideoStopped != nil {
			s.OnVideoStopped(pk.Reason)
		}
		s.countVideoPacket()
	case *VideoPacket:
		if s.OnVideoPacket != nil {
			s.OnVideoPacket(pk)
		}
		s.countVideoPacket()
	case *KeyboardLED:
		if s.OnKeyboardLED != nil {
			s.OnKeyboardLED(pk.Mask)
		}
	case *SharingRequest:
		s.log.Printf("kvm: sharing request %d from %q (mode %d)", pk.RequestID, pk.User, pk.Mode)
		if s.OnSharingRequest != nil {
			s.OnSharingRequest(pk)
		}
	case *PendingRequestCancelled:
		s.log.Printf("kvm: pending request %d cancelled (kind %d)", pk.RequestID, pk.Kind)
	case *AvailableServers:
		if s.OnServers != nil {
			s.OnServers(pk.Servers)
		}
	case *ServerStatusUpdate:
		if s.OnServers != nil {
			s.OnServers([]ServerEntry{pk.Server})
		}
	case *UserPrivilege:
		s.log.Printf("kvm: user privilege mask 0x%04x (power menu %v)", pk.Mask, pk.PowerMenuEnabled())
	case *DVCColorMode:
		s.log.Printf("kvm: colour mode depth=%d colour=%v", pk.Depth, pk.Color)
	case *ProtocolVersion:
		s.log.Printf("kvm: server protocol version %d.%d", pk.Major, pk.Minor)
	case *RawPacket:
		switch {
		case pk.Magic != "":
			s.log.Printf("kvm: in-band %s message (%d bytes) ignored", pk.Magic, len(pk.Raw))
		case IsIDRAC8Only(pk.PacketType):
			if s.cfg.Trace {
				s.log.Printf("kvm: %s (0x%04x, %d bytes) not handled", pk.Name(), pk.PacketType, len(pk.Raw))
			}
		default:
			s.log.Printf("kvm: unexpected packet type 0x%04x (%d bytes)", pk.PacketType, len(pk.Raw))
		}
	}
	return p
}

func (s *Session) onLoginResponse(lr *LoginResponse) {
	s.mu.Lock()
	s.login = lr
	s.mu.Unlock()
	s.log.Printf("kvm: login response status=%d (%s) flags=0x%04x appliance-random=%d msg=%q",
		lr.Status, lr.Reason(), lr.Flags, lr.ApplianceRandom, lr.Message)
	if !lr.Accepted() {
		s.setState(StateLoginFailed)
		return
	}
	if lr.ViewOnly() {
		s.log.Printf("kvm: session is view-only")
	}
	if lr.VideoOnControlChannel() {
		// com.avocent.kvm.b.n.a(vb): vb.p() true -> r.y() immediately.
		s.becomeRunning()
	} else {
		s.setState(StateVideoPending)
	}
}

// VideoConnected tells the session that the video channel is up (a Video
// Connect Status packet with byte 0 == 0 was received). Run calls it for
// the control socket; the video decoder must call it when the packet
// arrives on the video socket (VideoStream.OnConnectStatus). It performs
// com.avocent.kvm.b.r.y(): state RUNNING, then Video Enable Request,
// Set Display Area 1024x768 and SetScaleMode1to1.
func (s *Session) VideoConnected() {
	if s.State() != StateVideoPending && s.State() != StateAuthenticating {
		return
	}
	s.becomeRunning()
}

func (s *Session) becomeRunning() {
	s.setState(StateRunning)
	for _, p := range []Packet{&VideoEnable{Enable: true}, &SetDisplayArea{Width: 1024, Height: 768}, NewSetScaleMode1to1()} {
		if err := s.Send(p); err != nil {
			s.log.Printf("kvm: sending %s: %v", p.Name(), err)
			return
		}
	}
}

func (s *Session) checkVideoTimeout() {
	if s.cfg.VideoConnectTimeout < 0 || s.State() != StateVideoPending {
		return
	}
	s.mu.Lock()
	opened := s.videoOpened
	s.mu.Unlock()
	if opened.IsZero() || time.Since(opened) < s.cfg.VideoConnectTimeout {
		return
	}
	s.log.Printf("kvm: no Video Connect Status within %s; assuming RUNNING", s.cfg.VideoConnectTimeout)
	s.becomeRunning()
}

// countVideoPacket implements com.avocent.kvm.b.r.B() for video data that
// arrives on the control socket: every 20 packets a Video Ack is sent on
// the video socket if there is one, else on the control socket.
func (s *Session) countVideoPacket() {
	s.mu.Lock()
	s.videoAckCount++
	n := s.videoAckCount
	if n < 20 {
		s.mu.Unlock()
		return
	}
	s.videoAckCount = 0
	video := s.video
	s.mu.Unlock()
	ack := &VideoAck{Count: uint8(n)}
	var err error
	if video != nil {
		err = s.SendVideo(ack)
	} else {
		err = s.Send(ack)
	}
	if err != nil {
		s.log.Printf("kvm: video ack: %v", err)
	}
}

func (s *Session) keepAliveLoop(stop <-chan struct{}) {
	t := time.NewTicker(s.cfg.KeepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-s.closed:
			return
		case <-t.C:
		}
		if err := s.Send(NewKeepAlive()); err != nil {
			s.log.Printf("kvm: keepalive: %v", err)
			return
		}
		if s.pollServers() && s.State() == StateRunning {
			if err := s.Send(NewGetAvailableServers()); err != nil {
				s.log.Printf("kvm: keepalive: %v", err)
				return
			}
		}
	}
}

// Send marshals p and writes it to the control socket.
func (s *Session) Send(p Packet) error {
	f, err := Marshal(p)
	if err != nil {
		return err
	}
	return s.sendFrame(f, p.Name())
}

// SendControlPacket writes a pre-encoded body with the given type on the
// control socket (VideoControlSender for the video decoder).
func (s *Session) SendControlPacket(typ uint16, payload []byte) error {
	return s.sendFrame(ControlFrame(typ, payload), TypeName(typ))
}

func (s *Session) sendFrame(f *Frame, name string) error {
	s.mu.Lock()
	conn := s.ctrl
	s.mu.Unlock()
	if conn == nil {
		return errors.New("kvm: not connected")
	}
	if s.cfg.Trace {
		s.log.Printf("kvm >>> type 0x%04x %s len %d\n%s", f.Type, name, f.Len(), HexDump(f.Bytes(), 512))
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Timeout))
	return WriteFrame(conn, f)
}

// SendVideo writes p to the video socket opened by OpenVideo.
func (s *Session) SendVideo(p Packet) error {
	f, err := Marshal(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	conn := s.video
	s.mu.Unlock()
	if conn == nil {
		return errors.New("kvm: video socket not open")
	}
	if s.cfg.Trace {
		s.log.Printf("kvm video >>> type 0x%04x %s len %d\n%s", f.Type, p.Name(), f.Len(), HexDump(f.Bytes(), 512))
	}
	s.vwmu.Lock()
	defer s.vwmu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(s.cfg.Timeout))
	return WriteFrame(conn, f)
}

// OpenVideo performs com.avocent.kvm.b.r.z(): open the video socket
// (APCP type 4 + TLS when UseAPCP; direct TLS when the login response has
// the direct-SSL flag; clear TCP otherwise), send the Video Channel Auth
// packet with the client random and the appliance random, and return the
// stream positioned right after it. Everything the server sends on this
// socket is for the video decoder (NewVideoStream). Closing the returned
// stream closes the socket; Close() closes it too.
func (s *Session) OpenVideo(ctx context.Context) (io.ReadWriteCloser, error) {
	lr := s.LoginResponse()
	if lr == nil || !lr.Accepted() {
		return nil, errors.New("kvm: OpenVideo requires an accepted login")
	}
	if lr.VideoOnControlChannel() {
		return nil, errors.New("kvm: appliance delivers video on the control socket (login flag 0x01)")
	}
	var (
		conn net.Conn
		err  error
	)
	opts := s.dialOptions()
	switch {
	case s.cfg.UseAPCP:
		var info *APCPInfo
		conn, info, err = DialVideo(ctx, s.cfg.Host, s.cfg.VideoPort, opts)
		if err == nil {
			s.log.Printf("kvm: video socket up (tls=%v, server protocol %d.%d)", info.TLS, info.Major, info.Minor)
		}
	case lr.DirectSSLVideo():
		conn, err = DialTLS(ctx, s.cfg.Host, s.cfg.VideoPort, opts)
	default:
		conn, err = DialPlain(ctx, s.cfg.Host, s.cfg.VideoPort, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("kvm: open video socket: %w", err)
	}
	// Java sets a 60s read timeout on the video socket ("video_socket_timeout").
	s.mu.Lock()
	s.video = conn
	s.videoOpened = time.Now()
	s.mu.Unlock()

	auth := &VideoChannelAuth{ClientRandom: s.clientRandom, ApplianceRandom: lr.ApplianceRandom}
	if err := s.SendVideo(auth); err != nil {
		conn.Close()
		s.mu.Lock()
		s.video = nil
		s.mu.Unlock()
		return nil, fmt.Errorf("kvm: video channel auth: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	var rw io.ReadWriteCloser = &videoConn{Conn: conn, s: s}
	if s.cfg.TraceVideo {
		rw = &traceRW{rw: rw, log: s.log, prefix: "kvm video"}
	}
	return rw, nil
}

// videoConn serialises writes with SendVideo and detaches from the session
// on Close.
type videoConn struct {
	net.Conn
	s *Session
}

func (v *videoConn) Write(b []byte) (int, error) {
	v.s.vwmu.Lock()
	defer v.s.vwmu.Unlock()
	return v.Conn.Write(b)
}

func (v *videoConn) Close() error {
	v.s.mu.Lock()
	if v.s.video == v.Conn {
		v.s.video = nil
	}
	v.s.mu.Unlock()
	return v.Conn.Close()
}

type traceRW struct {
	rw     io.ReadWriteCloser
	log    Logger
	prefix string
}

func (t *traceRW) Read(b []byte) (int, error) {
	n, err := t.rw.Read(b)
	if n > 0 {
		t.log.Printf("%s <<< %d bytes\n%s", t.prefix, n, HexDump(b[:n], 64))
	}
	return n, err
}

func (t *traceRW) Write(b []byte) (int, error) {
	t.log.Printf("%s >>> %d bytes\n%s", t.prefix, len(b), HexDump(b, 256))
	return t.rw.Write(b)
}

func (t *traceRW) Close() error { return t.rw.Close() }

// RespondSharing answers a SharingRequest.
func (s *Session) RespondSharing(requestID int, answer uint8) error {
	return s.Send(&SharingResponse{RequestID: requestID, Answer: answer})
}

// SetPower sends a power operation (PowerOn, PowerOff, PowerCycle,
// PowerReboot, PowerGracefulShutdown) for the named server (see OnServers /
// AvailableServers). The appliance must have granted the power privilege.
func (s *Session) SetPower(serverName string, op uint8) error {
	return s.Send(&SetPowerState{ServerName: serverName, Operation: op})
}

func (s *Session) closeConn() {
	s.mu.Lock()
	c := s.ctrl
	s.ctrl = nil
	s.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// Close shuts both sockets. It is safe to call more than once.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.mu.Lock()
		c, v := s.ctrl, s.video
		s.ctrl, s.video = nil, nil
		s.state = StateClosed
		s.mu.Unlock()
		if v != nil {
			v.Close()
		}
		if c != nil {
			c.Close()
		}
		if s.OnState != nil {
			s.OnState(StateClosed)
		}
	})
	return nil
}
