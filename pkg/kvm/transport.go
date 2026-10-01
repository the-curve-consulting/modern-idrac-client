package kvm

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// This file implements the two transport layers of the Avocent virtual
// console protocol as used by Dell iDRAC6/7/8:
//
//   - APCP ("Avocent Proxy Connection Protocol"?) – a 53-byte clear-text
//     pre-handshake spoken before anything else on a fresh TCP connection.
//     It negotiates the protocol version and whether the same TCP connection
//     is upgraded to SSL/TLS (Java: com.avocent.kvm.b.l).
//   - AVSP framing – the 8-byte header used by every packet on the control
//     and video sockets afterwards (Java: com.avocent.kvm.b.a.a for the
//     reader, com.avocent.kvm.b.a.b / com.avocent.kvm.b.a.zb for the
//     header builders, com.avocent.kvm.c.d.g for the writer).

// Protocol version advertised by the fw 2.92 iDRAC6 viewer
// (com.avocent.kvm.b.r fields bb/cb, com.avocent.kvm.b.l fields f/g).
const (
	DefaultProtocolMajor = 2
	DefaultProtocolMinor = 34
)

// APCP constants (com.avocent.kvm.b.l).
const (
	APCPMagic         = "APCP"
	apcpMessageLen    = 53
	apcpMsgRequest    = 0x0100 // int16 written after the length
	apcpMsgResponse   = 0x8100 // server reply type (Java: -32512)
	apcpMsgResponse2  = 0x8101 // reply type for the 68-byte reconnect-capable request (iDRAC8)
	apcpMaxReplyLen   = 512
	apcpRandomLen     = 32
	APCPTypeControl   = 3 // request type for the KVM control (keyboard/mouse) socket
	APCPTypeVideo     = 4 // request type for the video socket
	APCPCapPlain      = 1 // capability bit: clear-text connection
	APCPCapSSL        = 4 // capability bit: upgrade this connection to SSL/TLS
	DefaultAPCPCaps   = APCPCapPlain | APCPCapSSL
	apcpHeaderTimeout = 30 * time.Second
)

// DialOptions controls how the control and video sockets are opened.
type DialOptions struct {
	// Timeout bounds the TCP connect and the APCP/TLS handshakes. Zero
	// means 30s (the Java viewer uses a 30s socket timeout for APCP).
	Timeout time.Duration
	// TLSConfig is used for the SSL upgrade. nil means DefaultTLSConfig().
	TLSConfig *tls.Config
	// UseAPCP selects the APCP pre-handshake (iDRAC launches the viewer
	// with apcp=1). When false the socket is a direct TLS connection
	// (com.avocent.kvm.b.r.b(String,int,boolean) path).
	UseAPCP bool
	// ProtocolMajor/ProtocolMinor are advertised in the APCP request
	// (defaults 2.34). The server's answer is returned in APCPInfo.
	ProtocolMajor, ProtocolMinor int
	// Capabilities is the APCP capability mask requested (default 5).
	Capabilities uint32
	// Dialer optionally replaces net.Dialer.DialContext (proxies, tests).
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)
	// Logger receives trace output when Trace is set. nil disables.
	Logger Logger
	// Trace hex-dumps the APCP exchange.
	Trace bool
}

func (o DialOptions) timeout() time.Duration {
	if o.Timeout <= 0 {
		return apcpHeaderTimeout
	}
	return o.Timeout
}

func (o DialOptions) logger() Logger {
	if o.Logger == nil {
		return nopLogger{}
	}
	return o.Logger
}

func (o DialOptions) version() (int, int) {
	maj, min := o.ProtocolMajor, o.ProtocolMinor
	if maj == 0 && min == 0 {
		maj, min = DefaultProtocolMajor, DefaultProtocolMinor
	}
	return maj, min
}

func (o DialOptions) dial(ctx context.Context, addr string) (net.Conn, error) {
	if o.Dialer != nil {
		return o.Dialer(ctx, "tcp", addr)
	}
	d := net.Dialer{Timeout: o.timeout()}
	return d.DialContext(ctx, "tcp", addr)
}

// APCPInfo is what the server told us during the APCP exchange.
type APCPInfo struct {
	Major, Minor int    // protocol version reported by the server (iDRAC6: 1.0, iDRAC8: 2.34/2.41)
	Capabilities uint32 // capability mask (bit 0 plain, bit 2 SSL, bit 3 SSL+cert check, bit 9 reconnect support)
	RedirectPort int    // non-zero: server asked us to reconnect to this port
	ServerRandom []byte // the server's random bytes (unused by the viewer; iDRAC8 echoes length 0)
	TLS          bool   // whether the returned connection is TLS
	// iDRAC8 (com.avocent.d.a.a.a(Socket,int)) additions:
	MessageType uint16 // 0x8100 or 0x8101
	APCPVersion uint16 // second u16 of the reply ("APCP version"; 0, or 0x0104 for the variant-C request)
	Length      int    // declared reply length (53, or 68 with trailer)
	Trailer     []byte // raw bytes after the 53-byte body, if any
	// Decoded from Trailer when Length == 68:
	EchoedRequestType byte
	ConnectionID      uint32 // 0x8101 replies: u32 at trailer[3..7]
	HeartbeatTimeout  int    // seconds; variant C (APCPVersion 0x0104) replies only
	UpdateInterval    int    // variant C replies only
}

// DefaultTLSConfig returns a tls.Config able to talk to the ancient TLS
// stacks in iDRAC6/7/8: certificate verification is disabled (the devices
// use self-signed certs), TLS 1.0 is allowed, and every cipher suite Go
// knows (including RSA key exchange, 3DES and RC4) is offered explicitly
// because Go 1.27 no longer enables them via GODEBUG.
func DefaultTLSConfig() *tls.Config {
	var suites []uint16
	for _, s := range tls.CipherSuites() {
		suites = append(suites, s.ID)
	}
	for _, s := range tls.InsecureCipherSuites() {
		suites = append(suites, s.ID)
	}
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // iDRAC certificates are self-signed
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS12,
		CipherSuites:       suites,
	}
}

// DialControl opens the KVM control socket (kmport, default 5900) using
// APCP request type 3 when o.UseAPCP is set, or a direct TLS connection
// otherwise.
func DialControl(ctx context.Context, host string, port int, o DialOptions) (net.Conn, *APCPInfo, error) {
	if o.UseAPCP {
		return DialAPCP(ctx, host, port, APCPTypeControl, o)
	}
	c, err := DialTLS(ctx, host, port, o)
	if err != nil {
		return nil, nil, err
	}
	return c, &APCPInfo{TLS: true}, nil
}

// DialVideo opens the video socket (vport) using APCP request type 4.
// Callers that need the non-APCP variants (direct TLS or clear TCP as
// selected by the login response) use DialTLS / DialPlain.
func DialVideo(ctx context.Context, host string, port int, o DialOptions) (net.Conn, *APCPInfo, error) {
	if o.UseAPCP {
		return DialAPCP(ctx, host, port, APCPTypeVideo, o)
	}
	c, err := DialTLS(ctx, host, port, o)
	if err != nil {
		return nil, nil, err
	}
	return c, &APCPInfo{TLS: true}, nil
}

// DialPlain opens a clear TCP connection (com.avocent.kvm.b.r.z() fallback
// when neither APCP nor direct SSL is selected for video).
func DialPlain(ctx context.Context, host string, port int, o DialOptions) (net.Conn, error) {
	c, err := o.dial(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	setNoDelay(c)
	return c, nil
}

// DialTLS opens a direct TLS connection without APCP
// (com.avocent.kvm.b.r.b(String,int,boolean)). The Java viewer offers
// SSLv3 here when the SSLV3 flag is set; Go cannot speak SSLv3, so TLS 1.0+
// is the best we can do.
func DialTLS(ctx context.Context, host string, port int, o DialOptions) (net.Conn, error) {
	raw, err := o.dial(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	setNoDelay(raw)
	tc, err := upgradeTLS(ctx, raw, host, o)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return tc, nil
}

// DialAPCP performs the APCP exchange (com.avocent.kvm.b.l) for the given
// request type and returns the connection ready for AVSP framing.
//
// Wire format of the 53-byte request (all big-endian):
//
//	0  "APCP"
//	4  int32  53               total length
//	8  int16  0x0100           message type: request
//	10 int16  0
//	12 byte   requestType      3 = control, 4 = video
//	13 byte   versionMajor     2
//	14 byte   versionMinor     34
//	15 byte   0
//	16 int32  capabilities     5 (plain|ssl)
//	20 byte   randomLen        32
//	21 32 bytes random
//
// The 53-byte response:
//
//	0  "APCP"
//	4  int32  length
//	8  int16  0x8100           message type: response
//	10 int16  (ignored)
//	12 byte   versionMajor
//	13 byte   versionMinor
//	14 int32  capabilities     0 => connection refused, &1 plain, &4 SSL
//	18 int16  redirectPort     0 => keep this socket
//	20 byte   randomLen
//	21 32 bytes random (padded)
//
// If capabilities&1 the socket stays clear (checked first, as in Java);
// else if capabilities&4 the same TCP connection is upgraded to TLS.
func DialAPCP(ctx context.Context, host string, port int, reqType byte, o DialOptions) (net.Conn, *APCPInfo, error) {
	lg := o.logger()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := o.dial(ctx, addr)
	if err != nil {
		return nil, nil, fmt.Errorf("apcp: dial %s: %w", addr, err)
	}
	setNoDelay(conn)
	deadline := time.Now().Add(o.timeout())
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)

	maj, min := o.version()
	caps := o.Capabilities
	if caps == 0 {
		caps = DefaultAPCPCaps
	}
	req := buildAPCPRequest(reqType, maj, min, caps, nil)
	if o.Trace {
		lg.Printf("apcp >>> %s type=%d ver=%d.%d caps=%d\n%s", addr, reqType, maj, min, caps, HexDump(req, 0))
	}
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("apcp: write request: %w", err)
	}
	info, resp, err := readAPCPReply(conn)
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if o.Trace {
		lg.Printf("apcp <<< %s msg=0x%04x apcpver=0x%04x ver=%d.%d caps=%d redirect=%d len=%d connid=%d\n%s", addr, info.MessageType, info.APCPVersion, info.Major, info.Minor, info.Capabilities, info.RedirectPort, info.Length, info.ConnectionID, HexDump(resp, 0))
	}
	if info.Capabilities == 0 {
		conn.Close()
		return nil, nil, errors.New("apcp: server returned capabilities=0 (connection refused)")
	}
	var mode uint32
	switch {
	case info.Capabilities&APCPCapPlain != 0:
		mode = APCPCapPlain
	case info.Capabilities&APCPCapSSL != 0:
		mode = APCPCapSSL
	default:
		conn.Close()
		return nil, nil, fmt.Errorf("apcp: unsupported connection type requested (capabilities=%d)", info.Capabilities)
	}
	if info.RedirectPort != 0 {
		// Java: new Socket(inetAddress, port) – a fresh TCP connection to the
		// redirect port, with no second APCP exchange. UNVERIFIED: never seen
		// on iDRAC (both devices answer redirectPort=0).
		conn.Close()
		raddr := net.JoinHostPort(host, strconv.Itoa(info.RedirectPort))
		lg.Printf("apcp: server redirected to %s", raddr)
		conn, err = o.dial(ctx, raddr)
		if err != nil {
			return nil, nil, fmt.Errorf("apcp: redirect dial %s: %w", raddr, err)
		}
		setNoDelay(conn)
		_ = conn.SetDeadline(deadline)
	}
	if mode == APCPCapSSL {
		tc, err := upgradeTLS(ctx, conn, host, o)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		conn = tc
		info.TLS = true
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, info, nil
}

// buildAPCPRequest encodes the 53-byte APCP request. rnd may be nil (a
// fresh random is generated) or up to 32 bytes.
func buildAPCPRequest(reqType byte, maj, min int, caps uint32, rnd []byte) []byte {
	if rnd == nil {
		rnd = make([]byte, apcpRandomLen)
		_, _ = rand.Read(rnd)
	}
	if len(rnd) > apcpRandomLen {
		rnd = rnd[:apcpRandomLen]
	}
	b := make([]byte, 0, apcpMessageLen)
	b = append(b, APCPMagic...)
	b = binary.BigEndian.AppendUint32(b, apcpMessageLen)
	b = binary.BigEndian.AppendUint16(b, apcpMsgRequest)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = append(b, reqType, byte(maj), byte(min), 0)
	b = binary.BigEndian.AppendUint32(b, caps)
	b = append(b, byte(len(rnd)))
	b = append(b, rnd...)
	for i := len(rnd); i < apcpRandomLen; i++ {
		b = append(b, 0)
	}
	return b
}

// readAPCPReply reads and decodes the server's APCP reply. The fixed 53-byte
// body is read first; if the declared length is larger (iDRAC8 answers the
// 68-byte reconnect-capable requests with 68 bytes) the trailer is read and
// decoded as described in docs/kvm-idrac8-notes.md §2.2. The raw bytes are
// returned for tracing.
func readAPCPReply(r io.Reader) (*APCPInfo, []byte, error) {
	resp := make([]byte, apcpMessageLen)
	if _, err := io.ReadFull(r, resp[:21]); err != nil {
		return nil, nil, fmt.Errorf("apcp: read response header: %w", err)
	}
	if string(resp[:4]) != APCPMagic {
		return nil, resp[:21], fmt.Errorf("apcp: bad magic %q", resp[:4])
	}
	info := &APCPInfo{
		Length:       int(binary.BigEndian.Uint32(resp[4:8])),
		MessageType:  binary.BigEndian.Uint16(resp[8:10]),
		APCPVersion:  binary.BigEndian.Uint16(resp[10:12]),
		Major:        int(resp[12]),
		Minor:        int(resp[13]),
		Capabilities: binary.BigEndian.Uint32(resp[14:18]),
		RedirectPort: int(binary.BigEndian.Uint16(resp[18:20])),
	}
	if info.MessageType != apcpMsgResponse && info.MessageType != apcpMsgResponse2 {
		return nil, resp[:21], fmt.Errorf("apcp: unexpected message type 0x%04x", info.MessageType)
	}
	rndLen := int(resp[20])
	if rndLen > apcpRandomLen {
		return nil, resp[:21], fmt.Errorf("apcp: random length %d too long", rndLen)
	}
	if _, err := io.ReadFull(r, resp[21:21+apcpRandomLen]); err != nil {
		return nil, resp, fmt.Errorf("apcp: read response random: %w", err)
	}
	info.ServerRandom = append([]byte(nil), resp[21:21+rndLen]...)
	if info.Length > apcpMessageLen {
		if info.Length > apcpMaxReplyLen {
			return nil, resp, fmt.Errorf("apcp: reply length %d too long", info.Length)
		}
		tr := make([]byte, info.Length-apcpMessageLen)
		if _, err := io.ReadFull(r, tr); err != nil {
			return nil, resp, fmt.Errorf("apcp: read response trailer: %w", err)
		}
		info.Trailer = tr
		resp = append(resp, tr...)
		if len(tr) >= 15 {
			info.EchoedRequestType = tr[0]
			if info.MessageType == apcpMsgResponse2 || info.APCPVersion == 0 {
				// variant B: u8 reqType | u16 0 | u32 connectionId | u32 | u32
				info.ConnectionID = binary.BigEndian.Uint32(tr[3:7])
			} else {
				// variant C: u8 reqType | u16 hbTimeout | u16 180 | u8 updateInterval | u8 connIdNonZero | u32 | u32
				info.HeartbeatTimeout = int(binary.BigEndian.Uint16(tr[1:3]))
				info.UpdateInterval = int(tr[5])
				info.ConnectionID = uint32(tr[6])
			}
		}
	}
	return info, resp, nil
}

func upgradeTLS(ctx context.Context, raw net.Conn, host string, o DialOptions) (net.Conn, error) {
	cfg := o.TLSConfig
	if cfg == nil {
		cfg = DefaultTLSConfig()
	} else {
		cfg = cfg.Clone()
	}
	if cfg.ServerName == "" && !cfg.InsecureSkipVerify {
		cfg.ServerName = host
	}
	tc := tls.Client(raw, cfg)
	hctx, cancel := context.WithTimeout(ctx, o.timeout())
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	if o.Trace {
		st := tc.ConnectionState()
		o.logger().Printf("tls: established version=0x%04x suite=%s", st.Version, tls.CipherSuiteName(st.CipherSuite))
	}
	return tc, nil
}

func setNoDelay(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
		_ = tc.SetReadBuffer(32768)
	}
}

// ---------------------------------------------------------------------------
// AVSP framing
// ---------------------------------------------------------------------------

// Frame layout constants. Every packet on the control and video sockets
// starts with an 8-byte header followed by (Length-8) body bytes.
//
// Control-channel header (com.avocent.kvm.b.a.b.a(int,int)):
//
//	0  "BEEF"
//	4  uint16 type      big-endian
//	6  uint16 length    total length including the header (>= 16, <= 15000)
//
// Video-channel client header (com.avocent.kvm.b.a.zb.a()):
//
//	0  00 00 00 00
//	4  0x01
//	5  uint8  type
//	6  uint16 length
//
// When parsing, the control-channel reader takes the type from bytes 4-5
// and the video-channel reader from byte 5 only (com.avocent.kvm.b.a.a.a
// with the boolean argument). Bytes 0-3 are never checked by the viewer.
const (
	FrameHeaderLen = 8
	// MinSendFrameLen is the minimum frame the iDRAC6 reader accepts (16);
	// outgoing bodies are padded to reach it.
	MinSendFrameLen = 16
	// MinFrameLen / MaxFrameLen are the receiver limits of the iDRAC8 reader
	// (com.avocent.d.g.a.a: 8..32768); iDRAC6 used 16..15000.
	MinFrameLen = 8
	MaxFrameLen = 32768
	// FrameTypeInband is a synthetic type returned by ReadFrame for frames
	// that carry one of the non-AVSP magics ("APCP", "ASCP", "MGMT", "CSCO")
	// inside the stream; Frame.Magic holds the magic. The iDRAC6 reader
	// tolerated "APCP"; iDRAC8 adds in-band APCP control (session id, OTP,
	// inventory), "MGMT" next-boot structures and "CSCO" server macros.
	FrameTypeInband uint16 = 0xFFFE
	// FrameTypeAPCPInband is the former name of FrameTypeInband.
	FrameTypeAPCPInband = FrameTypeInband
)

var controlMagic = [4]byte{'B', 'E', 'E', 'F'}

// inbandMagics maps the non-AVSP magics to how their length is encoded:
// true = u32 total length at offset 4 (APCP/ASCP), false = BEEF-style u16
// total length at offset 6 (MGMT; CSCO assumed the same, UNVERIFIED).
var inbandMagics = map[string]bool{"APCP": true, "ASCP": true, "MGMT": false, "CSCO": false}

// Frame is one raw AVSP packet: 8-byte header plus body.
type Frame struct {
	Header [FrameHeaderLen]byte
	Type   uint16
	Body   []byte
	Magic  string // "" for AVSP frames; "APCP"/"ASCP"/"MGMT"/"CSCO" for in-band frames (Type == FrameTypeInband)
}

// Len returns the total frame length (header + body).
func (f *Frame) Len() int { return FrameHeaderLen + len(f.Body) }

// Bytes returns the frame ready to be written to the wire.
func (f *Frame) Bytes() []byte {
	b := make([]byte, 0, f.Len())
	b = append(b, f.Header[:]...)
	return append(b, f.Body...)
}

// ControlFrame builds a "BEEF"-headed frame for the control channel. The
// body is padded with zeros to at least 8 bytes because the reader
// rejects frames shorter than 16 bytes.
func ControlFrame(typ uint16, body []byte) *Frame {
	body = padBody(body)
	f := &Frame{Type: typ, Body: body}
	copy(f.Header[:4], controlMagic[:])
	binary.BigEndian.PutUint16(f.Header[4:6], typ)
	binary.BigEndian.PutUint16(f.Header[6:8], uint16(FrameHeaderLen+len(body)))
	return f
}

// VideoFrame builds a frame with the video-channel client header
// (00 00 00 00 01 type len16), used for the video channel authentication
// packet.
func VideoFrame(typ uint8, body []byte) *Frame {
	body = padBody(body)
	f := &Frame{Type: uint16(typ), Body: body}
	f.Header[4] = 1
	f.Header[5] = typ
	binary.BigEndian.PutUint16(f.Header[6:8], uint16(FrameHeaderLen+len(body)))
	return f
}

func padBody(body []byte) []byte {
	if len(body) >= MinSendFrameLen-FrameHeaderLen {
		return body
	}
	p := make([]byte, MinSendFrameLen-FrameHeaderLen)
	copy(p, body)
	return p
}

// ReadFrame reads one AVSP frame. With video=false the packet type is the
// 16-bit value at header bytes 4-5 (control channel); with video=true it
// is byte 5 only (video channel). It mirrors com.avocent.kvm.b.a.a.a()
// with the iDRAC8 receiver's limits (com.avocent.d.g.a.a). Frames with an
// in-band magic are consumed using their own length field and returned
// with Type == FrameTypeInband.
func ReadFrame(r io.Reader, video bool) (*Frame, error) {
	f := &Frame{}
	if _, err := io.ReadFull(r, f.Header[:]); err != nil {
		return nil, err
	}
	if u32len, ok := inbandMagics[string(f.Header[:4])]; ok {
		f.Magic = string(f.Header[:4])
		var n int
		if u32len {
			n = int(binary.BigEndian.Uint32(f.Header[4:8]))
		} else {
			n = int(binary.BigEndian.Uint16(f.Header[6:8]))
		}
		if n < FrameHeaderLen || n > MaxFrameLen {
			return nil, fmt.Errorf("avsp: bad in-band %s length %d", f.Magic, n)
		}
		f.Type = FrameTypeInband
		f.Body = make([]byte, n-FrameHeaderLen)
		if _, err := io.ReadFull(r, f.Body); err != nil {
			return nil, err
		}
		return f, nil
	}
	if video {
		f.Type = uint16(f.Header[5])
	} else {
		f.Type = binary.BigEndian.Uint16(f.Header[4:6])
	}
	n := int(binary.BigEndian.Uint16(f.Header[6:8]))
	if n < MinFrameLen {
		return nil, fmt.Errorf("avsp: bad message length (%d)", n)
	}
	if n > MaxFrameLen {
		return nil, fmt.Errorf("avsp: packet length %d exceeds maximum", n)
	}
	f.Body = make([]byte, n-FrameHeaderLen)
	if _, err := io.ReadFull(r, f.Body); err != nil {
		return nil, err
	}
	return f, nil
}

// WriteFrame writes header and body in a single Write call
// (com.avocent.kvm.c.d.g.b writes header, body, flush).
func WriteFrame(w io.Writer, f *Frame) error {
	_, err := w.Write(f.Bytes())
	return err
}
