package kvm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// Control-channel packet catalogue. Every type below names the Java class
// in com.avocent.kvm.b.a it was reverse engineered from. All multi-byte
// integers are big-endian. Bodies shorter than 8 bytes are zero-padded so
// that the total frame length is at least 16 (the reader rejects shorter
// frames).
//
// Packet numbering: client->server types are small (< 0x1000);
// server->client types have bit 15 set (0x81xx keyboard/mouse replies,
// 0x82xx video configuration, 0x83xx session, 0x84xx sharing/servers,
// 0x86xx video data on the control socket). Video data on the dedicated
// video socket uses the low byte only (0x80..0x8A).

// Client -> server packet types.
const (
	TypeVideoAck              uint16 = 0x0000 // cb   "Video Ack"
	TypeVideoChannelAuth      uint16 = 0x0001 // ac   "Video Channel Auth" (video socket header)
	TypeLoginRequest          uint16 = 0x0100 // ab   "Login Request" (protocol < 2, no share byte)
	TypeLoginRequestAlt       uint16 = 0x0101 // bb   variant of ab, never sent by the viewer
	TypeLoginRequestShare     uint16 = 0x0102 // ab(true) "Login Request" with share-mode byte (protocol >= 2, minor < 41)
	TypeLoginRequestLong      uint16 = 0x0104 // iDRAC8 com.avocent.c.d.ib long layout (negotiated >= 2.41)
	TypeKeyboardData          uint16 = 0x0200 // f    "KeyboardDataRequest"
	TypeMouseData             uint16 = 0x0201 // gc   "Mouse Data Request" (absolute)
	TypeMouseOrigin           uint16 = 0x0202 // h    "MouseOrigin"
	TypeKeyboardLEDRequest    uint16 = 0x0204 // g    "Keyboard LED Request"
	TypeFocusControl          uint16 = 0x0208 // d    "FocusControl"
	TypeMouseDelta            uint16 = 0x0209 // hc   "Mouse Delta Request" (relative)
	TypeSetMouseAccel         uint16 = 0x020A // t    "SetMouseAccel"
	TypeScreenRefresh         uint16 = 0x0301 // n    "ScreenRefresh"
	TypeSetDisplayArea        uint16 = 0x0302 // x    "Set Display Area"
	TypeSetScaleMode1to1      uint16 = 0x0304 // o    "SetScaleMode1to1"
	TypeVideoEnable           uint16 = 0x030E // db   "Video Enable Request"
	TypeTextMessage           uint16 = 0x0320 // w    (chat/"We are here." – KVMMessage command 7)
	TypeKeepAlive             uint16 = 0x0400 // p    "KeepAlive"
	TypeSetVideoTransmitLimit uint16 = 0x0402 // u    "SetVideoTransmitLimit"
	TypeSetDVCColorDepth      uint16 = 0x0403 // k    "SetDVCColorDepthMessage"
	TypeSharingResponse       uint16 = 0x0413 // fc   "SharingResponse"
	TypeGetAvailableServers   uint16 = 0x0420 // e    "GetAvailableServers"
	TypeSetPowerState         uint16 = 0x0424 // dc   "SetPowerState"
	TypeRequestCredentials    uint16 = 0x0427 // r    "RequestCredentialsMessage"
	TypeVideoAckAlt           uint16 = 0x0600 // cb(1536) alternative Video Ack id
)

// Server -> client packet types.
const (
	// Video data as seen on the dedicated video socket (type = header byte 5).
	TypeVideoGeneric       uint16 = 0x0080 // fb  "Video Packet" (raw)
	TypeVideoDVC1          uint16 = 0x0081 // hb  "Video Packet" (DVC)
	TypeVideoDVC2          uint16 = 0x0082 // hb
	TypeVideoDVC3          uint16 = 0x0083 // hb
	TypeVideoConnectStatus uint16 = 0x0084 // xb  "Video Connect Status"
	TypeVideoStopped       uint16 = 0x0085 // v   "VideoStopped"
	TypeVideoJPEG          uint16 = 0x0086 // gb  "ASpeed JPEG Video"
	TypeVideoText          uint16 = 0x0087 // ub  "Text Mode Video"
	TypeColorPalette       uint16 = 0x0088 // ib  "Color Palette"
	TypeFontTable          uint16 = 0x0089 // kb  "Font Table"
	TypeVideoDVC4          uint16 = 0x008A // hb

	// The same video packets when they ride the control socket carry 0x86
	// in header byte 4 (com.avocent.kvm.b.a.a.a(int) cases 34304..34314).
	TypeCtlVideoGeneric uint16 = 0x8600
	TypeCtlVideoDVC1    uint16 = 0x8601
	TypeCtlVideoDVC2    uint16 = 0x8602
	TypeCtlVideoDVC3    uint16 = 0x8603
	TypeCtlVideoStopped uint16 = 0x8605
	TypeCtlVideoJPEG    uint16 = 0x8606
	TypeCtlVideoText    uint16 = 0x8607
	TypeCtlColorPalette uint16 = 0x8608
	TypeCtlFontTable    uint16 = 0x8609
	TypeCtlVideoDVC4    uint16 = 0x860A

	TypeKeyboardLED             uint16 = 0x8100 // mb  "Keyboard LED"
	TypeMouseAck                uint16 = 0x8101 // pb  "MouseAcknowledgeResponse"
	TypeMouseAccelResponse      uint16 = 0x8102 // ob  "MouseAccelResponse"
	TypeInputResolution         uint16 = 0x8200 // lb  "Input Resolution Response"
	TypeDisplayResolution       uint16 = 0x8201 // jb  "Display Resolution Response"
	TypeVideoSetupData          uint16 = 0x8202 // yb  "VideoSetupData"
	TypeScaling                 uint16 = 0x8203 // tb  "Scaling"
	TypeDVCColorMode            uint16 = 0x8204 // l   "DVCColorModeResponse"
	TypeInputResolutionAlt      uint16 = 0x8206 // lb  (33286)
	TypeLoginResponseV1         uint16 = 0x8300 // vb  "User Login Response" (old layout)
	TypeUserDisconnectPending   uint16 = 0x8301 // z   "User Disconnect Pending Message"
	TypeProtocolVersion         uint16 = 0x8302 // rb  "Protocol Version"
	TypePendingRequestCancelled uint16 = 0x8304 // q   "PendingRequestCancelledMessage"
	TypeLoginResponse           uint16 = 0x8305 // vb(true) "User Login Response" (protocol >= 2)
	TypeSharingRequest          uint16 = 0x8401 // ec  "SharingRequest"
	TypeSharedUsers             uint16 = 0x8410 // y   "SharedUserResponse"
	TypeLocked                  uint16 = 0x8412 // nb  "Locked"
	TypeReserved                uint16 = 0x8413 // sb  "Reserved"
	TypeUserPrivilege           uint16 = 0x8420 // wb  "User Priviledge Parameters"
	TypeAvailableServers        uint16 = 0x8430 // c   "AvailableServerNames"
	TypeSelectedServerUpdate    uint16 = 0x8431 // bc  "SelectedServerUpdate"
	TypeServerStatusUpdate      uint16 = 0x8432 // cc  "ServerStatusUpdate"
	TypeCredentialsPush         uint16 = 0x8434 // j   "RequestCredentialsMessage" (server->client)

	// iDRAC7/8-only server->client types (com.avocent.d.g.a.a factory). They
	// are tolerated: decoded as RawPacket and trace-logged, never an error.
	TypeAbsoluteMouseSyncResp    uint16 = 0x8103 // 33027 com.avocent.c.d.wb (reply to 523)
	TypeDisplayResolutionInvalid uint16 = 0x8205 // 33285 DisplayResolutionInvalidResponse
	TypeUserDisconnectPendingTO  uint16 = 0x8303 // 33539 UserDisconnectPendingTimeoutMessage
	TypeExclusiveModeResp        uint16 = 0x8411 // 33809 ExclusiveModeResp ([0]=granted)
	TypeApplianceOptionsResponse uint16 = 0x8415 // 33813 ApplianceOptionsResponse
	TypeChatReceive              uint16 = 0x8435 // 33845 chat text + trailing u32 sessionId
	TypeCustomServerMacro        uint16 = 0x9F00 // 40704 CustomServerMacroMessage (may carry "CSCO")
	TypeMgmtSessionResponse      uint16 = 0x5555 // 21845 MgmtSessionResponse ("MGMT" next-boot)
	TypeMgmtBootDeviceStruct     uint16 = 0x9E10 // 40464 MgmtBootDeviceStruct
	TypeMgmtBootDeviceResult     uint16 = 0x9E11 // 40465 MgmtBootDeviceResult
	TypeInventoryResponse        uint16 = 0x2009 // InventoryResponseMsg (JSON; APCP-framed in practice)
)

// IsIDRAC8Only reports whether t is one of the iDRAC7/8-only server packet
// types that this package tolerates without decoding.
func IsIDRAC8Only(t uint16) bool {
	switch t {
	case TypeAbsoluteMouseSyncResp, TypeDisplayResolutionInvalid, TypeUserDisconnectPendingTO,
		TypeExclusiveModeResp, TypeApplianceOptionsResponse, TypeChatReceive, TypeCustomServerMacro,
		TypeMgmtSessionResponse, TypeMgmtBootDeviceStruct, TypeMgmtBootDeviceResult, TypeInventoryResponse:
		return true
	}
	return false
}

// TypeName returns a readable name for a packet type (Java e() strings
// where available).
func TypeName(t uint16) string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("Unknown(0x%04x)", t)
}

var typeNames = map[uint16]string{
	TypeVideoAck: "Video Ack", TypeVideoChannelAuth: "Video Channel Auth",
	TypeLoginRequest: "Login Request", TypeLoginRequestAlt: "Login Request (alt)", TypeLoginRequestShare: "Login Request",
	TypeKeyboardData: "KeyboardDataRequest", TypeMouseData: "Mouse Data Request", TypeMouseOrigin: "MouseOrigin",
	TypeKeyboardLEDRequest: "Keyboard LED Request", TypeFocusControl: "FocusControl", TypeMouseDelta: "Mouse Delta Request",
	TypeSetMouseAccel: "SetMouseAccel", TypeScreenRefresh: "ScreenRefresh", TypeSetDisplayArea: "Set Display Area",
	TypeSetScaleMode1to1: "SetScaleMode1to1", TypeVideoEnable: "Video Enable Request", TypeTextMessage: "TextMessage",
	TypeKeepAlive: "KeepAlive", TypeSetVideoTransmitLimit: "SetVideoTransmitLimit", TypeSetDVCColorDepth: "SetDVCColorDepthMessage",
	TypeSharingResponse: "SharingResponse", TypeGetAvailableServers: "GetAvailableServers", TypeSetPowerState: "SetPowerState",
	TypeRequestCredentials: "RequestCredentialsMessage", TypeVideoAckAlt: "Video Ack (1536)",
	TypeVideoGeneric: "Video Packet", TypeVideoDVC1: "Video Packet (DVC)", TypeVideoDVC2: "Video Packet (DVC)",
	TypeVideoDVC3: "Video Packet (DVC)", TypeVideoDVC4: "Video Packet (DVC)", TypeVideoConnectStatus: "Video Connect Status",
	TypeVideoStopped: "VideoStopped", TypeVideoJPEG: "ASpeed JPEG Video", TypeVideoText: "Text Mode Video",
	TypeColorPalette: "Color Palette", TypeFontTable: "Font Table",
	TypeCtlVideoGeneric: "Video Packet", TypeCtlVideoDVC1: "Video Packet (DVC)", TypeCtlVideoDVC2: "Video Packet (DVC)",
	TypeCtlVideoDVC3: "Video Packet (DVC)", TypeCtlVideoDVC4: "Video Packet (DVC)", TypeCtlVideoStopped: "VideoStopped",
	TypeCtlVideoJPEG: "ASpeed JPEG Video", TypeCtlVideoText: "Text Mode Video", TypeCtlColorPalette: "Color Palette",
	TypeCtlFontTable: "Font Table",
	TypeKeyboardLED:  "Keyboard LED", TypeMouseAck: "MouseAcknowledgeResponse", TypeMouseAccelResponse: "MouseAccelResponse",
	TypeInputResolution: "Input Resolution Response", TypeInputResolutionAlt: "Input Resolution Response",
	TypeDisplayResolution: "Display Resolution Response", TypeVideoSetupData: "VideoSetupData", TypeScaling: "Scaling",
	TypeDVCColorMode: "DVCColorModeResponse", TypeLoginResponseV1: "User Login Response", TypeLoginResponse: "User Login Response",
	TypeUserDisconnectPending: "User Disconnect Pending Message", TypeProtocolVersion: "Protocol Version",
	TypePendingRequestCancelled: "PendingRequestCancelledMessage", TypeSharingRequest: "SharingRequest",
	TypeSharedUsers: "SharedUserResponse", TypeLocked: "Locked", TypeReserved: "Reserved",
	TypeUserPrivilege: "User Priviledge Parameters", TypeAvailableServers: "AvailableServerNames",
	TypeSelectedServerUpdate: "SelectedServerUpdate", TypeServerStatusUpdate: "ServerStatusUpdate",
	TypeCredentialsPush: "RequestCredentialsMessage", FrameTypeInband: "in-band (non-AVSP magic)",
	TypeLoginRequestLong:      "Login Request (long)",
	TypeAbsoluteMouseSyncResp: "AbsoluteMouseSyncResponse", TypeDisplayResolutionInvalid: "DisplayResolutionInvalidResponse",
	TypeUserDisconnectPendingTO: "UserDisconnectPendingTimeoutMessage", TypeExclusiveModeResp: "ExclusiveModeResp",
	TypeApplianceOptionsResponse: "ApplianceOptionsResponse", TypeChatReceive: "ChatMessage",
	TypeCustomServerMacro: "CustomServerMacroMessage", TypeMgmtSessionResponse: "MgmtSessionResponse",
	TypeMgmtBootDeviceStruct: "MgmtBootDeviceStruct", TypeMgmtBootDeviceResult: "MgmtBootDeviceResult",
	TypeInventoryResponse: "InventoryResponseMsg",
}

// IsVideoDataType reports whether t carries video data (either numbering).
func IsVideoDataType(t uint16) bool {
	switch t {
	case TypeVideoGeneric, TypeVideoDVC1, TypeVideoDVC2, TypeVideoDVC3, TypeVideoDVC4,
		TypeVideoJPEG, TypeVideoText, TypeColorPalette, TypeFontTable,
		TypeCtlVideoGeneric, TypeCtlVideoDVC1, TypeCtlVideoDVC2, TypeCtlVideoDVC3, TypeCtlVideoDVC4,
		TypeCtlVideoJPEG, TypeCtlVideoText, TypeCtlColorPalette, TypeCtlFontTable:
		return true
	}
	return false
}

// Packet is anything that can be put on the wire.
type Packet interface {
	// Type is the 16-bit packet type as it appears on the control channel.
	Type() uint16
	// Name is the human-readable packet name (Java e()).
	Name() string
	// MarshalBody encodes the body (without the 8-byte header).
	MarshalBody() ([]byte, error)
}

// framer is implemented by packets that need a non-standard header.
type framer interface {
	Frame() (*Frame, error)
}

// Marshal produces the complete frame for p. Most packets get the "BEEF"
// control header; VideoChannelAuth supplies its own.
func Marshal(p Packet) (*Frame, error) {
	if fr, ok := p.(framer); ok {
		return fr.Frame()
	}
	body, err := p.MarshalBody()
	if err != nil {
		return nil, err
	}
	return ControlFrame(p.Type(), body), nil
}

// ServerPacket is a decoded server->client packet.
type ServerPacket interface {
	Packet
	// UnmarshalBody decodes the body. hdr is the 8-byte header.
	UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error
}

// ErrShortPacket is returned when a body is shorter than the layout needs.
var ErrShortPacket = errors.New("kvm: packet body too short")

func need(body []byte, n int) error {
	if len(body) < n {
		return fmt.Errorf("%w: have %d, need %d", ErrShortPacket, len(body), n)
	}
	return nil
}

func rd16(b []byte, off int) int   { return int(binary.BigEndian.Uint16(b[off:])) }
func rd32(b []byte, off int) int32 { return int32(binary.BigEndian.Uint32(b[off:])) }

// cString returns b up to the first NUL.
func cString(b []byte) string {
	if i := indexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// putPaddedString writes s into a fixed-width field, NUL padded and
// truncated to width bytes (com.avocent.kvm.c.d.a.a(String,byte[],int,int)).
func putPaddedString(dst []byte, s string, width int) {
	b := []byte(s)
	if len(b) > width {
		b = b[:width]
	}
	copy(dst, b)
	for i := len(b); i < width; i++ {
		dst[i] = 0
	}
}

// ---------------------------------------------------------------------------
// Client -> server packets
// ---------------------------------------------------------------------------

// Login share modes (com.avocent.kvm.b.a.ab.e(int) accepts 0,1,2,5,6,7).
const (
	ShareModeNone   = 0 // normal login
	ShareModeShared = 1 // request a shared session (sent after IN_USE, SHARED_SESSION=true)
	// 2, 5, 6, 7 are accepted by the viewer but their meaning is unknown.
)

// LoginRequest is com.avocent.kvm.b.a.ab, sent immediately after the
// transport is up. Java type 258 (with the trailing share byte) when
// protocol major >= 2, else 256.
//
//	0    uint8  len(username)
//	1    96 B   username, UTF-8, NUL padded
//	97   uint8  len(password)
//	98   96 B   password, UTF-8, NUL padded
//	194  8 B    RIP id (all zero for iDRAC; hex string "xx-xx.." parsed by ab.a(String))
//	202  uint8  port     (launcher PORT param, default 1; 0 when a RIP is given)
//	203  uint8  channel  (launcher CHANNEL param, default 0)
//	204  int32  client random (session id, Math.random()*1e7)
//	208  uint8  share mode (only when WithShareMode)
//
// Total frame length 216 (217 with the share byte).
//
// Long layout (Long=true, type 0x0104, iDRAC8 com.avocent.c.d.ib.o() when
// the negotiated version is >= 2.41; strings are not padded):
//
//	0    uint16 len(username)
//	2    uint16 username offset from frame start (= 31)
//	4    uint16 len(password)
//	6    uint16 password offset from frame start (= 31 + ulen)
//	8    8 B    RIP id
//	16   uint8  port
//	17   uint8  channel
//	18   int32  client random
//	22   uint8  share mode
//	23   username, then password
//
// Total frame length 31 + ulen + plen.
type LoginRequest struct {
	Username      string
	Password      string
	RIP           [8]byte
	Port          uint8
	Channel       uint8
	ClientRandom  int32
	ShareMode     uint8
	WithShareMode bool // true => type 0x0102 with trailing share byte
	Long          bool // true => type 0x0104 long layout (implies the share byte)
}

const loginLongFixed = 23 // body bytes before the strings in the long layout

const loginFieldWidth = 96

func (p *LoginRequest) Type() uint16 {
	if p.Long {
		return TypeLoginRequestLong
	}
	if p.WithShareMode {
		return TypeLoginRequestShare
	}
	return TypeLoginRequest
}
func (p *LoginRequest) Name() string { return "Login Request" }

// SetRIP parses a hexadecimal RIP id ("0011-2233-4455-6677" or plain hex)
// into the 8-byte field, as com.avocent.kvm.b.a.ab.a(String) does. Java
// removes only the first '-'; we remove all of them.
func (p *LoginRequest) SetRIP(s string) error {
	s = strings.ReplaceAll(s, "-", "")
	if s == "" {
		p.RIP = [8]byte{}
		return nil
	}
	if len(s) != 16 {
		return fmt.Errorf("kvm: RIP id must be 16 hex digits, got %d", len(s))
	}
	for i := 0; i < 8; i++ {
		hi, ok1 := hexNibble(s[2*i])
		lo, ok2 := hexNibble(s[2*i+1])
		if !ok1 || !ok2 {
			return errors.New("kvm: RIP ID contains non-hex characters")
		}
		p.RIP[i] = hi<<4 | lo
	}
	p.Port = 0
	return nil
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	}
	return 0, false
}

func (p *LoginRequest) MarshalBody() ([]byte, error) {
	if p.Long {
		ulen, plen := len(p.Username), len(p.Password)
		if ulen > 0xFFFF || plen > 0xFFFF {
			return nil, errors.New("kvm: username/password too long")
		}
		b := make([]byte, loginLongFixed+ulen+plen)
		binary.BigEndian.PutUint16(b[0:], uint16(ulen))
		binary.BigEndian.PutUint16(b[2:], uint16(FrameHeaderLen+loginLongFixed))
		binary.BigEndian.PutUint16(b[4:], uint16(plen))
		binary.BigEndian.PutUint16(b[6:], uint16(FrameHeaderLen+loginLongFixed+ulen))
		copy(b[8:16], p.RIP[:])
		b[16] = p.Port
		b[17] = p.Channel
		binary.BigEndian.PutUint32(b[18:22], uint32(p.ClientRandom))
		b[22] = p.ShareMode
		copy(b[23:], p.Username)
		copy(b[23+ulen:], p.Password)
		return b, nil
	}
	if len(p.Username) > loginFieldWidth || len(p.Password) > loginFieldWidth {
		return nil, errors.New("kvm: username/password longer than 96 bytes")
	}
	n := 1 + loginFieldWidth + 1 + loginFieldWidth + 8 + 1 + 1 + 4
	if p.WithShareMode {
		n++
	}
	b := make([]byte, n)
	// Java writes String.length() (UTF-16 units) as the length byte and the
	// UTF-8 bytes into the field; for ASCII credentials these agree.
	b[0] = byte(len(p.Username))
	putPaddedString(b[1:], p.Username, loginFieldWidth)
	b[97] = byte(len(p.Password))
	putPaddedString(b[98:], p.Password, loginFieldWidth)
	copy(b[194:202], p.RIP[:])
	b[202] = p.Port
	b[203] = p.Channel
	binary.BigEndian.PutUint32(b[204:208], uint32(p.ClientRandom))
	if p.WithShareMode {
		b[208] = p.ShareMode
	}
	return b, nil
}

// UnmarshalBody decodes a LoginRequest (ab.a(byte[],byte[])); useful for
// tests and packet-log analysis.
func (p *LoginRequest) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if binary.BigEndian.Uint16(hdr[4:6]) == TypeLoginRequestLong {
		p.Long, p.WithShareMode = true, true
		if err := need(body, loginLongFixed); err != nil {
			return err
		}
		ulen, uoff := rd16(body, 0), rd16(body, 2)-FrameHeaderLen
		plen, poff := rd16(body, 4), rd16(body, 6)-FrameHeaderLen
		if uoff < loginLongFixed || poff < loginLongFixed || uoff+ulen > len(body) || poff+plen > len(body) {
			return errors.New("kvm: long login request offsets out of range")
		}
		p.Username = string(body[uoff : uoff+ulen])
		p.Password = string(body[poff : poff+plen])
		copy(p.RIP[:], body[8:16])
		p.Port = body[16]
		p.Channel = body[17]
		p.ClientRandom = rd32(body, 18)
		p.ShareMode = body[22]
		return nil
	}
	p.Long = false
	if err := need(body, 208); err != nil {
		return err
	}
	ul, pl := int(body[0]), int(body[97])
	if ul > loginFieldWidth || pl > loginFieldWidth {
		return errors.New("kvm: login request field length out of range")
	}
	p.Username = string(body[1 : 1+ul])
	p.Password = string(body[98 : 98+pl])
	copy(p.RIP[:], body[194:202])
	p.Port = body[202]
	p.Channel = body[203]
	p.ClientRandom = rd32(body, 204)
	if len(body) > 208 {
		p.WithShareMode = true
		p.ShareMode = body[208]
	} else {
		p.WithShareMode = false
	}
	return nil
}

// VideoChannelAuth is com.avocent.kvm.b.a.ac, the first (and only)
// handshake packet on the video socket. It uses the video-channel header
// 00 00 00 00 01 01 00 10.
//
//	0  int32 client random  (the LoginRequest.ClientRandom of this session)
//	4  int32 appliance random (LoginResponse.ApplianceRandom)
type VideoChannelAuth struct {
	ClientRandom    int32
	ApplianceRandom int32
}

func (p *VideoChannelAuth) Type() uint16 { return TypeVideoChannelAuth }
func (p *VideoChannelAuth) Name() string { return "Video Channel Auth" }
func (p *VideoChannelAuth) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:], uint32(p.ClientRandom))
	binary.BigEndian.PutUint32(b[4:], uint32(p.ApplianceRandom))
	return b, nil
}
func (p *VideoChannelAuth) Frame() (*Frame, error) {
	body, _ := p.MarshalBody()
	return VideoFrame(uint8(TypeVideoChannelAuth), body), nil
}
func (p *VideoChannelAuth) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 8); err != nil {
		return err
	}
	p.ClientRandom = rd32(body, 0)
	p.ApplianceRandom = rd32(body, 4)
	return nil
}

// VideoAck is com.avocent.kvm.b.a.cb "Video Ack": flow control sent once
// every 20 received video packets (com.avocent.kvm.b.r.B(), W=20), to the
// video socket if one exists, else to the control socket. Body byte 0 is
// the number of packets acknowledged (mod 256).
type VideoAck struct {
	Count uint8
}

func (p *VideoAck) Type() uint16 { return TypeVideoAck }
func (p *VideoAck) Name() string { return "Video Ack" }
func (p *VideoAck) MarshalBody() ([]byte, error) {
	return []byte{p.Count, 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *VideoAck) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Count = body[0]
	return nil
}

// KeyboardData is com.avocent.kvm.b.a.f "KeyboardDataRequest".
//
//	0  0
//	1  uint8  0 = key pressed, 1 = key released
//	2  uint16 USB HID keyboard usage id (see keymap.go)
//	4  4 B    zero
type KeyboardData struct {
	Usage uint16
	Down  bool
}

func (p *KeyboardData) Type() uint16 { return TypeKeyboardData }
func (p *KeyboardData) Name() string { return "KeyboardDataRequest" }
func (p *KeyboardData) MarshalBody() ([]byte, error) {
	rel := byte(1)
	if p.Down {
		rel = 0
	}
	return []byte{0, rel, byte(p.Usage >> 8), byte(p.Usage), 0, 0, 0, 0}, nil
}
func (p *KeyboardData) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 4); err != nil {
		return err
	}
	p.Down = body[1] == 0
	p.Usage = uint16(rd16(body, 2))
	return nil
}

// Mouse button bits used in MouseData/MouseDelta (com.avocent.kvm.b.h).
const (
	MouseLeft   uint8 = 1
	MouseRight  uint8 = 2
	MouseMiddle uint8 = 4
)

// MouseData is com.avocent.kvm.b.a.gc "Mouse Data Request": an absolute
// mouse report in video pixel coordinates (the viewer runs at 1:1 scale).
//
//	0  0
//	1  uint8  button state (MouseLeft|MouseRight|MouseMiddle), persistent
//	2  uint16 x (clamped at 0)
//	4  uint16 y (clamped at 0)
//	6  int16  wheel: positive = scroll up (Java: -getWheelRotation())
type MouseData struct {
	Buttons uint8
	X, Y    int
	Wheel   int
}

func (p *MouseData) Type() uint16 { return TypeMouseData }
func (p *MouseData) Name() string { return "Mouse Data Request" }
func (p *MouseData) MarshalBody() ([]byte, error) {
	return marshalMouse(p.Buttons, p.X, p.Y, p.Wheel, true), nil
}
func (p *MouseData) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 8); err != nil {
		return err
	}
	p.Buttons = body[1]
	p.X = rd16(body, 2)
	p.Y = rd16(body, 4)
	p.Wheel = int(int16(binary.BigEndian.Uint16(body[6:])))
	return nil
}

func marshalMouse(buttons uint8, x, y, wheel int, clamp bool) []byte {
	if clamp {
		if x < 0 {
			x = 0
		}
		if y < 0 {
			y = 0
		}
	}
	b := make([]byte, 8)
	b[1] = buttons
	binary.BigEndian.PutUint16(b[2:], uint16(x))
	binary.BigEndian.PutUint16(b[4:], uint16(y))
	binary.BigEndian.PutUint16(b[6:], uint16(int16(wheel)))
	return b
}

// MouseDelta is com.avocent.kvm.b.a.hc "Mouse Delta Request": same layout
// as MouseData but X/Y are signed deltas (relative mode). The viewer never
// sends it on iDRAC (only absolute mode is wired up); UNVERIFIED.
type MouseDelta struct {
	Buttons uint8
	DX, DY  int
	Wheel   int
}

func (p *MouseDelta) Type() uint16 { return TypeMouseDelta }
func (p *MouseDelta) Name() string { return "Mouse Delta Request" }
func (p *MouseDelta) MarshalBody() ([]byte, error) {
	return marshalMouse(p.Buttons, p.DX, p.DY, p.Wheel, false), nil
}
func (p *MouseDelta) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 8); err != nil {
		return err
	}
	p.Buttons = body[1]
	p.DX = int(int16(binary.BigEndian.Uint16(body[2:])))
	p.DY = int(int16(binary.BigEndian.Uint16(body[4:])))
	p.Wheel = int(int16(binary.BigEndian.Uint16(body[6:])))
	return nil
}

// emptyPacket is the shared implementation for the many 16-byte
// zero-body request packets.
type emptyPacket struct {
	typ  uint16
	name string
}

func (p emptyPacket) Type() uint16                                     { return p.typ }
func (p emptyPacket) Name() string                                     { return p.name }
func (p emptyPacket) MarshalBody() ([]byte, error)                     { return make([]byte, 8), nil }
func (p emptyPacket) UnmarshalBody([FrameHeaderLen]byte, []byte) error { return nil }

// MouseOrigin is com.avocent.kvm.b.a.h "MouseOrigin" (zero body). The
// viewer sends it when the pointer enters the video panel (KVMMessage 4).
type MouseOrigin struct{ emptyPacket }

// KeyboardLEDRequest is com.avocent.kvm.b.a.g "Keyboard LED Request"; the
// server answers with KeyboardLED (0x8100).
type KeyboardLEDRequest struct{ emptyPacket }

// ScreenRefresh is com.avocent.kvm.b.a.n "ScreenRefresh": asks for a full
// frame (sent after every Display Resolution Response).
type ScreenRefresh struct{ emptyPacket }

// SetScaleMode1to1 is com.avocent.kvm.b.a.o "SetScaleMode1to1".
type SetScaleMode1to1 struct{ emptyPacket }

// KeepAlive is com.avocent.kvm.b.a.p "KeepAlive", sent every 2 seconds by
// com.avocent.kvm.b.x.
type KeepAlive struct{ emptyPacket }

// GetAvailableServers is com.avocent.kvm.b.a.e "GetAvailableServers", sent
// every 2 seconds while RUNNING (same thread as KeepAlive). The server
// answers with AvailableServers (0x8430).
type GetAvailableServers struct{ emptyPacket }

func NewMouseOrigin() *MouseOrigin {
	return &MouseOrigin{emptyPacket{TypeMouseOrigin, "MouseOrigin"}}
}
func NewKeyboardLEDRequest() *KeyboardLEDRequest {
	return &KeyboardLEDRequest{emptyPacket{TypeKeyboardLEDRequest, "Keyboard LED Request"}}
}
func NewScreenRefresh() *ScreenRefresh {
	return &ScreenRefresh{emptyPacket{TypeScreenRefresh, "ScreenRefresh"}}
}
func NewSetScaleMode1to1() *SetScaleMode1to1 {
	return &SetScaleMode1to1{emptyPacket{TypeSetScaleMode1to1, "SetScaleMode1to1"}}
}
func NewKeepAlive() *KeepAlive { return &KeepAlive{emptyPacket{TypeKeepAlive, "KeepAlive"}} }
func NewGetAvailableServers() *GetAvailableServers {
	return &GetAvailableServers{emptyPacket{TypeGetAvailableServers, "GetAvailableServers"}}
}

// FocusControl is com.avocent.kvm.b.a.d "FocusControl": byte 0 = 1 when
// the viewer window gained keyboard focus, 0 when lost (sent together with
// a KeyboardLEDRequest, com.avocent.kvm.b.r.a(boolean)).
type FocusControl struct {
	Focused bool
}

func (p *FocusControl) Type() uint16 { return TypeFocusControl }
func (p *FocusControl) Name() string { return "FocusControl" }
func (p *FocusControl) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	if p.Focused {
		b[0] = 1
	}
	return b, nil
}
func (p *FocusControl) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Focused = body[0] > 0
	return nil
}

// SetMouseAccel is com.avocent.kvm.b.a.t "SetMouseAccel": byte 0 = value.
type SetMouseAccel struct {
	Value uint8
}

func (p *SetMouseAccel) Type() uint16 { return TypeSetMouseAccel }
func (p *SetMouseAccel) Name() string { return "SetMouseAccel" }
func (p *SetMouseAccel) MarshalBody() ([]byte, error) {
	return []byte{p.Value, 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *SetMouseAccel) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Value = body[0]
	return nil
}

// SetDisplayArea is com.avocent.kvm.b.a.x "Set Display Area": uint16
// width, uint16 height. The viewer sends 1024x768 when the session becomes
// RUNNING (com.avocent.kvm.b.r.y()).
type SetDisplayArea struct {
	Width, Height int
}

func (p *SetDisplayArea) Type() uint16 { return TypeSetDisplayArea }
func (p *SetDisplayArea) Name() string { return "Set Display Area" }
func (p *SetDisplayArea) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:], uint16(p.Width))
	binary.BigEndian.PutUint16(b[2:], uint16(p.Height))
	return b, nil
}
func (p *SetDisplayArea) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 4); err != nil {
		return err
	}
	p.Width = rd16(body, 0)
	p.Height = rd16(body, 2)
	return nil
}

// VideoEnable is com.avocent.kvm.b.a.db "Video Enable Request": byte 0 =
// enable flag, byte 1 = 1 (constant field l, meaning unknown).
type VideoEnable struct {
	Enable bool
}

func (p *VideoEnable) Type() uint16 { return TypeVideoEnable }
func (p *VideoEnable) Name() string { return "Video Enable Request" }
func (p *VideoEnable) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	if p.Enable {
		b[0] = 1
	}
	b[1] = 1
	return b, nil
}
func (p *VideoEnable) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Enable = body[0] > 0
	return nil
}

// TextMessage is com.avocent.kvm.b.a.w (KVMMessage command 7, type 800):
// int16 code, int16 length, then the text. Presumably the chat feature.
type TextMessage struct {
	Code int
	Text string
}

func (p *TextMessage) Type() uint16 { return TypeTextMessage }
func (p *TextMessage) Name() string { return "TextMessage" }
func (p *TextMessage) MarshalBody() ([]byte, error) {
	b := make([]byte, 4+len(p.Text))
	binary.BigEndian.PutUint16(b[0:], uint16(p.Code))
	binary.BigEndian.PutUint16(b[2:], uint16(len(p.Text)))
	copy(b[4:], p.Text)
	return b, nil
}
func (p *TextMessage) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 4); err != nil {
		return err
	}
	p.Code = rd16(body, 0)
	n := rd16(body, 2)
	if err := need(body, 4+n); err != nil {
		return err
	}
	p.Text = string(body[4 : 4+n])
	return nil
}

// Video transmit limits (com.avocent.kvm.b.a.u / c.v constants a..e).
const (
	VideoLimitNone = 0
	VideoLimit1    = 1
	VideoLimit2    = 2
	VideoLimit3    = 3
	VideoLimit4    = 4
)

// SetVideoTransmitLimit is com.avocent.kvm.b.a.u "SetVideoTransmitLimit"
// (bandwidth throttle 0..4, KVMMessage command 2).
type SetVideoTransmitLimit struct {
	Limit uint8
}

func (p *SetVideoTransmitLimit) Type() uint16 { return TypeSetVideoTransmitLimit }
func (p *SetVideoTransmitLimit) Name() string { return "SetVideoTransmitLimit" }
func (p *SetVideoTransmitLimit) MarshalBody() ([]byte, error) {
	if p.Limit > 4 {
		return nil, fmt.Errorf("kvm: invalid video transmit limit %d", p.Limit)
	}
	return []byte{p.Limit, 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *SetVideoTransmitLimit) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Limit = body[0]
	return nil
}

// SetDVCColorDepth is com.avocent.kvm.b.a.k "SetDVCColorDepthMessage".
//
//	0  uint8  0 = apply settings below (Java k=true, default), 1 = "reset"
//	          (sent with k=false when the video packet type changes or on
//	          KVMMessage 8; UNVERIFIED meaning)
//	1  uint8  colour depth
//	2  uint8  1 = grayscale requested (object == c.v.f)
//	3  uint8  1 = flag n (always true when sent via KVMMessage 3)
type SetDVCColorDepth struct {
	Reset     bool
	Depth     uint8
	Grayscale bool
	Flag      bool
}

func (p *SetDVCColorDepth) Type() uint16 { return TypeSetDVCColorDepth }
func (p *SetDVCColorDepth) Name() string { return "SetDVCColorDepthMessage" }
func (p *SetDVCColorDepth) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	if p.Reset {
		b[0] = 1
	}
	b[1] = p.Depth
	if p.Grayscale {
		b[2] = 1
	}
	if p.Flag {
		b[3] = 1
	}
	return b, nil
}
func (p *SetDVCColorDepth) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 4); err != nil {
		return err
	}
	p.Reset = body[0] > 0
	p.Depth = body[1]
	p.Grayscale = body[2] > 0
	p.Flag = body[3] > 0
	return nil
}

// SharingResponse is com.avocent.kvm.b.a.fc "SharingResponse", the
// answer to a SharingRequest.
//
//	0  uint8  answer (dialog result; UNVERIFIED which value grants access)
//	1  0
//	2  uint16 request id (SharingRequest.RequestID)
type SharingResponse struct {
	RequestID int
	Answer    uint8
}

func (p *SharingResponse) Type() uint16 { return TypeSharingResponse }
func (p *SharingResponse) Name() string { return "SharingResponse" }
func (p *SharingResponse) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	b[0] = p.Answer
	binary.BigEndian.PutUint16(b[2:], uint16(p.RequestID))
	return b, nil
}
func (p *SharingResponse) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 4); err != nil {
		return err
	}
	p.Answer = body[0]
	p.RequestID = rd16(body, 2)
	return nil
}

// Power operations (com.avocent.kvm.b.a.dc.a(Object)).
const (
	PowerOn               = 1
	PowerOff              = 2
	PowerCycle            = 3
	PowerReboot           = 4
	PowerGracefulShutdown = 5
)

// SetPowerState is com.avocent.kvm.b.a.dc "SetPowerState": uint16 name
// length, server name (from AvailableServers/SelectedServerUpdate), uint8
// operation.
type SetPowerState struct {
	ServerName string
	Operation  uint8
}

func (p *SetPowerState) Type() uint16 { return TypeSetPowerState }
func (p *SetPowerState) Name() string { return "SetPowerState" }
func (p *SetPowerState) MarshalBody() ([]byte, error) {
	if p.Operation < PowerOn || p.Operation > PowerGracefulShutdown {
		return nil, fmt.Errorf("kvm: unsupported power operation %d", p.Operation)
	}
	b := make([]byte, 2+len(p.ServerName)+1)
	binary.BigEndian.PutUint16(b[0:], uint16(len(p.ServerName)))
	copy(b[2:], p.ServerName)
	b[2+len(p.ServerName)] = p.Operation
	return b, nil
}
func (p *SetPowerState) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 2); err != nil {
		return err
	}
	n := rd16(body, 0)
	if err := need(body, 2+n+1); err != nil {
		return err
	}
	p.ServerName = string(body[2 : 2+n])
	p.Operation = body[2+n]
	return nil
}

// RequestCredentials is com.avocent.kvm.b.a.r (type 1063), sent by
// com.avocent.kvm.b.r.I(): an int32 with the constant 30. Purpose unknown
// (virtual media single sign-on?); UNVERIFIED.
type RequestCredentials struct {
	Value int32 // Java default 30
}

func (p *RequestCredentials) Type() uint16 { return TypeRequestCredentials }
func (p *RequestCredentials) Name() string { return "RequestCredentialsMessage" }
func (p *RequestCredentials) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b, uint32(p.Value))
	return b, nil
}

// ---------------------------------------------------------------------------
// Server -> client packets
// ---------------------------------------------------------------------------

// Login status codes (com.avocent.kvm.b.n.a(vb)).
const (
	LoginOK                = 0
	LoginBadLogin1         = 1
	LoginBadLogin2         = 2
	LoginAccessDenied      = 3
	LoginInUse             = 4
	LoginInUsePreemptable  = 6 // iDRAC8: in use but pre-emptable (com.avocent.a.b.u)
	LoginAllChannelsInUse  = 8
	LoginSharingDenied2    = 9 // iDRAC8 also maps 9 to sharing denied
	LoginInUse2            = 12
	LoginSharingDenied     = 15
	LoginSharingTimeout    = 16
	LoginLicenceExpired    = 24
	loginFlagVideoOnCtl    = 0x01 // vb.s: video arrives on the control socket, skip the video channel
	loginFlagDirectSSL     = 0x02 // vb.t: video socket is direct SSL (no APCP)
	loginFlagViewOnlyAvail = 0x20 // vb.v
	loginFlagViewOnly      = 0x40 // vb.u
)

// LoginReason converts a login status code to the viewer's LOGIN_REASON
// string.
func LoginReason(status int) string {
	switch status {
	case LoginOK:
		return "OK"
	case LoginBadLogin1, LoginBadLogin2:
		return "BAD_LOGIN"
	case LoginAccessDenied:
		return "ACCESS_DENIED"
	case LoginInUse, LoginInUse2:
		return "IN_USE"
	case LoginInUsePreemptable:
		return "IN_USE_PREEMPTABLE"
	case LoginAllChannelsInUse:
		return "ALL_CHANNELS_IN_USE"
	case LoginSharingDenied, LoginSharingDenied2:
		return "SHARING_DENIED"
	case LoginSharingTimeout:
		return "SHARING_TIMEOUT"
	case LoginLicenceExpired:
		return "LICENCE_EXPIRED"
	}
	return "BAD_LOGIN"
}

// LoginResponse is com.avocent.kvm.b.a.vb "User Login Response".
//
// New layout (type 0x8305, protocol >= 2, frame length 113):
//
//	0   uint8  status (0 = accepted; see Login* constants)
//	1   uint8  field k (unknown)
//	2   uint8  field l (default 30; unknown, possibly a timeout in seconds)
//	3   -
//	4   int32  appliance random -> VideoChannelAuth.ApplianceRandom
//	8   uint16 flags: 0x01 video on control socket, 0x02 direct-SSL video,
//	           0x20 view-only available, 0x40 view-only
//	10  uint8  message length (also reported as "n" when status == 0)
//	11  ...    message (UTF-8)
//
// Old layout (type 0x8300, frame length 115):
//
//	0   uint8  status
//	3   uint8  flags
//	4   int32  appliance random
//	8   uint8  message length
//	9   96 B   message
type LoginResponse struct {
	Status          int
	FieldK          uint8
	FieldL          uint8
	ApplianceRandom int32
	Flags           uint16
	Message         string
	Legacy          bool // decoded from the 0x8300 layout
}

func (p *LoginResponse) Type() uint16 {
	if p.Legacy {
		return TypeLoginResponseV1
	}
	return TypeLoginResponse
}
func (p *LoginResponse) Name() string { return "User Login Response" }

// Accepted reports whether the login succeeded.
func (p *LoginResponse) Accepted() bool { return p.Status == LoginOK }

// Reason returns the LOGIN_REASON string.
func (p *LoginResponse) Reason() string { return LoginReason(p.Status) }

// VideoOnControlChannel is flag bit 0: no separate video socket is opened
// and the session goes RUNNING right away (com.avocent.kvm.b.n.a(vb)).
func (p *LoginResponse) VideoOnControlChannel() bool { return p.Flags&loginFlagVideoOnCtl != 0 }

// DirectSSLVideo is flag bit 1: when not using APCP, open the video socket
// as direct SSL (com.avocent.kvm.b.r field T).
func (p *LoginResponse) DirectSSLVideo() bool { return p.Flags&loginFlagDirectSSL != 0 }

// ViewOnly reports flags 0x20 and 0x40 both set.
func (p *LoginResponse) ViewOnly() bool {
	return p.Flags&loginFlagViewOnlyAvail != 0 && p.Flags&loginFlagViewOnly != 0
}

func (p *LoginResponse) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	typ := binary.BigEndian.Uint16(hdr[4:6])
	if typ == TypeLoginResponseV1 {
		p.Legacy = true
		if err := need(body, 9); err != nil {
			return err
		}
		p.Status = int(body[0])
		p.Flags = uint16(body[3])
		p.ApplianceRandom = rd32(body, 4)
		n := int(body[8])
		if 9+n > len(body) {
			n = len(body) - 9
		}
		p.Message = cString(body[9 : 9+n])
		return nil
	}
	p.Legacy = false
	if err := need(body, 11); err != nil {
		return err
	}
	p.Status = int(body[0])
	p.FieldK = body[1]
	p.FieldL = body[2]
	p.ApplianceRandom = rd32(body, 4)
	p.Flags = binary.BigEndian.Uint16(body[8:10])
	n := int(body[10])
	if 11+n > len(body) {
		n = len(body) - 11
	}
	p.Message = cString(body[11 : 11+n])
	return nil
}

// MarshalBody encodes the new layout (for tests / simulators).
func (p *LoginResponse) MarshalBody() ([]byte, error) {
	if p.Legacy {
		b := make([]byte, 105)
		b[0] = byte(p.Status)
		b[3] = byte(p.Flags)
		binary.BigEndian.PutUint32(b[4:], uint32(p.ApplianceRandom))
		b[8] = byte(len(p.Message))
		putPaddedString(b[9:], p.Message, 96)
		return b, nil
	}
	b := make([]byte, 105)
	b[0] = byte(p.Status)
	b[1] = p.FieldK
	b[2] = p.FieldL
	binary.BigEndian.PutUint32(b[4:], uint32(p.ApplianceRandom))
	binary.BigEndian.PutUint16(b[8:], p.Flags)
	b[10] = byte(len(p.Message))
	putPaddedString(b[11:], p.Message, 94)
	return b, nil
}

// VideoConnectStatus is com.avocent.kvm.b.a.xb "Video Connect Status":
// byte 0 == 0 means the video channel is connected. Receiving it makes the
// session RUNNING (com.avocent.kvm.b.n.a(xb) -> r.y()).
type VideoConnectStatus struct {
	Connected bool
}

func (p *VideoConnectStatus) Type() uint16 { return TypeVideoConnectStatus }
func (p *VideoConnectStatus) Name() string { return "Video Connect Status" }
func (p *VideoConnectStatus) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	if !p.Connected {
		b[0] = 1
	}
	return b, nil
}
func (p *VideoConnectStatus) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Connected = body[0] == 0
	return nil
}

// Video stopped reasons (com.avocent.kvm.b.e constants, com.avocent.a.a.m).
const (
	VideoStoppedCalibrating = 0
	VideoStoppedNoSignal    = 1
	VideoStoppedOutOfRange  = 2
	VideoStoppedBlocked     = 3
	VideoStoppedBlocked2    = 4
	VideoStoppedUnknown     = 10
	VideoRunning            = 100 // property value once video packets resume
)

// VideoStopped is com.avocent.kvm.b.a.v "VideoStopped": byte 0 = reason.
type VideoStopped struct {
	Reason int
	typ    uint16
}

func (p *VideoStopped) Type() uint16 {
	if p.typ == 0 {
		return TypeVideoStopped
	}
	return p.typ
}
func (p *VideoStopped) Name() string { return "VideoStopped" }
func (p *VideoStopped) MarshalBody() ([]byte, error) {
	return []byte{byte(p.Reason), 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *VideoStopped) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Reason = int(body[0])
	return nil
}

// VideoStoppedText names a VideoStopped reason.
func VideoStoppedText(reason int) string {
	switch reason {
	case VideoStoppedCalibrating:
		return "calibrating"
	case VideoStoppedNoSignal:
		return "no signal"
	case VideoStoppedOutOfRange:
		return "out of range"
	case VideoStoppedBlocked, VideoStoppedBlocked2:
		return "blocked"
	}
	return "unknown reason"
}

// VideoPacket is any video-data frame (fb/hb/gb/ub/ib/kb) as seen on the
// control channel. The body is left intact for the video decoder; only
// the common DVC header fields are pulled out here (com.avocent.kvm.b.a.hb):
//
//	4  uint16 width
//	6  uint16 height
//	8  uint8  flags (bit0, bit1 => uint16 at 10 valid)
//	12 ...    payload
type VideoPacket struct {
	PacketType uint16
	Raw        []byte
}

func (p *VideoPacket) Type() uint16                 { return p.PacketType }
func (p *VideoPacket) Name() string                 { return TypeName(p.PacketType) }
func (p *VideoPacket) MarshalBody() ([]byte, error) { return p.Raw, nil }
func (p *VideoPacket) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	p.PacketType = binary.BigEndian.Uint16(hdr[4:6])
	p.Raw = body
	return nil
}

// Dimensions returns width/height for DVC/JPEG/text video packets (0,0
// when the body is too short).
func (p *VideoPacket) Dimensions() (int, int) {
	if len(p.Raw) < 8 {
		return 0, 0
	}
	return rd16(p.Raw, 4), rd16(p.Raw, 6)
}

// KeyboardLED is com.avocent.kvm.b.a.mb "Keyboard LED": byte 0 bit mask
// (bit0 NumLock?, bit1 CapsLock?, bit2 ScrollLock?; ordering UNVERIFIED –
// the Java only stores five booleans j..n for bits 0..4).
type KeyboardLED struct {
	Mask uint8
}

func (p *KeyboardLED) Type() uint16 { return TypeKeyboardLED }
func (p *KeyboardLED) Name() string { return "Keyboard LED" }
func (p *KeyboardLED) MarshalBody() ([]byte, error) {
	return []byte{p.Mask, 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *KeyboardLED) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Mask = body[0]
	return nil
}

// MouseAck is com.avocent.kvm.b.a.pb "MouseAcknowledgeResponse": byte 0 =
// number of mouse packets the appliance consumed (flow control for
// com.avocent.kvm.c.d.e when a max-unacknowledged limit is configured).
type MouseAck struct {
	Count uint8
}

func (p *MouseAck) Type() uint16 { return TypeMouseAck }
func (p *MouseAck) Name() string { return "MouseAcknowledgeResponse" }
func (p *MouseAck) MarshalBody() ([]byte, error) {
	return []byte{p.Count, 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *MouseAck) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Count = body[0]
	return nil
}

// MouseAccelResponse is com.avocent.kvm.b.a.ob: byte 0 = acceleration.
type MouseAccelResponse struct {
	Value uint8
}

func (p *MouseAccelResponse) Type() uint16 { return TypeMouseAccelResponse }
func (p *MouseAccelResponse) Name() string { return "MouseAccelResponse" }
func (p *MouseAccelResponse) MarshalBody() ([]byte, error) {
	return []byte{p.Value, 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *MouseAccelResponse) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Value = body[0]
	return nil
}

// Platform tags carried in Input Resolution Response body[4..5] by
// iDRAC7/8 (com.avocent.c.d.tb). iDRAC6 sends only 4 body bytes of data.
const (
	PlatformUnknown = ""
	PlatformASpeed  = "AS" // iDRAC7/8 (PLATFORM_TYPE 2)
	PlatformD5      = "D5" // PLATFORM_TYPE 1
	PlatformP3      = "P3" // PLATFORM_TYPE 3 (RLE video, SetVideoParameters)
	PlatformP4      = "P4" // PLATFORM_TYPE 5
)

// Resolution is shared by DisplayResolution (jb, 0x8201: the video output
// resolution – what the host is displaying) and InputResolution (lb,
// 0x8200/0x8206). uint16 width, uint16 height. Input Resolution on
// iDRAC7/8 adds a 2-character platform tag at offset 4 ("AS" = ASpeed).
type Resolution struct {
	Width, Height int
	Platform      string // Input Resolution only; "" when absent or not printable
	typ           uint16
}

func (p *Resolution) Type() uint16 { return p.typ }
func (p *Resolution) Name() string { return TypeName(p.typ) }
func (p *Resolution) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:], uint16(p.Width))
	binary.BigEndian.PutUint16(b[2:], uint16(p.Height))
	if len(p.Platform) == 2 {
		copy(b[4:6], p.Platform)
	}
	return b, nil
}
func (p *Resolution) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 4); err != nil {
		return err
	}
	p.typ = binary.BigEndian.Uint16(hdr[4:6])
	p.Width = rd16(body, 0)
	p.Height = rd16(body, 2)
	p.Platform = ""
	if (p.typ == TypeInputResolution || p.typ == TypeInputResolutionAlt) && len(body) >= 6 &&
		isTagChar(body[4]) && isTagChar(body[5]) {
		p.Platform = string(body[4:6])
	}
	return nil
}

func isTagChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// DisplayResolution is com.avocent.kvm.b.a.jb "Display Resolution
// Response". On receipt the viewer requests a ScreenRefresh.
type DisplayResolution struct{ Resolution }

// NewDisplayResolution builds a DisplayResolution packet.
func NewDisplayResolution(w, h int) *DisplayResolution {
	return &DisplayResolution{Resolution{Width: w, Height: h, typ: TypeDisplayResolution}}
}

// InputResolution is com.avocent.kvm.b.a.lb "Input Resolution Response".
type InputResolution struct{ Resolution }

// NewInputResolution builds an InputResolution packet; platform may be "".
func NewInputResolution(w, h int, platform string) *InputResolution {
	return &InputResolution{Resolution{Width: w, Height: h, Platform: platform, typ: TypeInputResolution}}
}

// VideoSetupData is com.avocent.kvm.b.a.yb "VideoSetupData": ten uint16
// values; the viewer exposes brightness(0), contrast(1), horz(2), vert(3),
// fine_adjust(5) and priority(9).
type VideoSetupData struct {
	Values [10]int
}

func (p *VideoSetupData) Type() uint16    { return TypeVideoSetupData }
func (p *VideoSetupData) Name() string    { return "VideoSetupData" }
func (p *VideoSetupData) Brightness() int { return p.Values[0] }
func (p *VideoSetupData) Contrast() int   { return p.Values[1] }
func (p *VideoSetupData) HorzPos() int    { return p.Values[2] }
func (p *VideoSetupData) VertPos() int    { return p.Values[3] }
func (p *VideoSetupData) FineAdjust() int { return p.Values[5] }
func (p *VideoSetupData) Priority() int   { return p.Values[9] }
func (p *VideoSetupData) MarshalBody() ([]byte, error) {
	b := make([]byte, 24)
	for i, v := range p.Values {
		binary.BigEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b, nil
}
func (p *VideoSetupData) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 20); err != nil {
		return err
	}
	for i := range p.Values {
		p.Values[i] = rd16(body, 2*i)
	}
	return nil
}

// DVCColorMode is com.avocent.kvm.b.a.l "DVCColorModeResponse": byte 0
// depth, byte 1 colour (else grayscale), byte 2 unknown flag.
type DVCColorMode struct {
	Depth uint8
	Color bool
	Flag  bool
}

func (p *DVCColorMode) Type() uint16 { return TypeDVCColorMode }
func (p *DVCColorMode) Name() string { return "DVCColorModeResponse" }
func (p *DVCColorMode) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	b[0] = p.Depth
	if p.Color {
		b[1] = 1
	}
	if p.Flag {
		b[2] = 1
	}
	return b, nil
}
func (p *DVCColorMode) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 3); err != nil {
		return err
	}
	p.Depth = body[0]
	p.Color = body[1] > 0
	p.Flag = body[2] > 0
	return nil
}

// Session shutdown reasons carried by UserDisconnectPending
// (com.avocent.idrac.kvm.d.b(int)).
const (
	ShutdownAdministratorDisconnect     = 0
	ShutdownSessionIdleTimeout          = 1
	ShutdownApplianceRebootPending      = 2
	ShutdownDSRIQUpgradePending         = 3
	ShutdownChannelPreemptedByLocalUser = 4
	ShutdownLastActiveUserDisconnected  = 5
	ShutdownPrimaryUserExclusiveMode    = 6
)

// ShutdownText returns the viewer's description of a shutdown reason.
func ShutdownText(reason int) string {
	switch reason {
	case ShutdownAdministratorDisconnect:
		return "disconnected by administrator"
	case ShutdownSessionIdleTimeout:
		return "session idle timeout exceeded"
	case ShutdownApplianceRebootPending:
		return "appliance reboot pending"
	case ShutdownDSRIQUpgradePending:
		return "DSRIQ upgrade pending"
	case ShutdownChannelPreemptedByLocalUser:
		return "channel pre-empted by local user"
	case ShutdownLastActiveUserDisconnected:
		return "last active user has disconnected"
	case ShutdownPrimaryUserExclusiveMode:
		return "primary user switched to exclusive mode"
	}
	return fmt.Sprintf("unknown reason %d", reason)
}

// UserDisconnectPending is com.avocent.kvm.b.a.z "User Disconnect Pending
// Message": byte 0 = shutdown reason. The viewer switches to CLOSING and
// shows the message, then exits.
type UserDisconnectPending struct {
	Reason int
}

func (p *UserDisconnectPending) Type() uint16 { return TypeUserDisconnectPending }
func (p *UserDisconnectPending) Name() string { return "User Disconnect Pending Message" }
func (p *UserDisconnectPending) MarshalBody() ([]byte, error) {
	return []byte{byte(p.Reason), 0, 0, 0, 0, 0, 0, 0}, nil
}
func (p *UserDisconnectPending) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 1); err != nil {
		return err
	}
	p.Reason = int(int8(body[0]))
	return nil
}

// ProtocolVersion is com.avocent.kvm.b.a.rb "Protocol Version": byte 0
// major, byte 1 minor. Not acted upon by the viewer.
type ProtocolVersion struct {
	Major, Minor int
}

func (p *ProtocolVersion) Type() uint16 { return TypeProtocolVersion }
func (p *ProtocolVersion) Name() string { return "Protocol Version" }
func (p *ProtocolVersion) MarshalBody() ([]byte, error) {
	return []byte{byte(p.Major), byte(p.Minor), 0, 0, 0, 0, 0, 0}, nil
}
func (p *ProtocolVersion) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 2); err != nil {
		return err
	}
	p.Major = int(body[0])
	p.Minor = int(body[1])
	return nil
}

// PendingRequestCancelled is com.avocent.kvm.b.a.q: byte 0 = kind (3 =
// sharing request withdrawn), bytes 1-2 = request id.
type PendingRequestCancelled struct {
	Kind      int
	RequestID int
}

func (p *PendingRequestCancelled) Type() uint16 { return TypePendingRequestCancelled }
func (p *PendingRequestCancelled) Name() string { return "PendingRequestCancelledMessage" }
func (p *PendingRequestCancelled) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	b[0] = byte(p.Kind)
	b[1] = byte(p.RequestID >> 8)
	b[2] = byte(p.RequestID)
	return b, nil
}
func (p *PendingRequestCancelled) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 3); err != nil {
		return err
	}
	p.Kind = int(body[0])
	// Java: byArray2[1] << 8 | byArray2[2] (sign-extending); use unsigned.
	p.RequestID = rd16(body, 1)
	return nil
}

// SharingRequest is com.avocent.kvm.b.a.ec "SharingRequest": another user
// wants to share this console. uint16 request id, uint8 mode, then the
// requesting user's name (NULs stripped) up to length-3 bytes of the body
// (Java stops at n2-8-3 where n2 is the frame length).
type SharingRequest struct {
	RequestID int
	Mode      int
	User      string
}

func (p *SharingRequest) Type() uint16 { return TypeSharingRequest }
func (p *SharingRequest) Name() string { return "SharingRequest" }
func (p *SharingRequest) MarshalBody() ([]byte, error) {
	b := make([]byte, 3+len(p.User)+3)
	binary.BigEndian.PutUint16(b[0:], uint16(p.RequestID))
	b[2] = byte(p.Mode)
	copy(b[3:], p.User)
	return b, nil
}
func (p *SharingRequest) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 3); err != nil {
		return err
	}
	p.RequestID = rd16(body, 0)
	p.Mode = int(body[2])
	end := len(body) - 3
	if end < 3 {
		end = 3
	}
	var sb strings.Builder
	for _, c := range body[3:end] {
		if c != 0 {
			sb.WriteByte(c)
		}
	}
	p.User = sb.String()
	return nil
}

// SharedUser is one entry of SharedUsers (com.avocent.kvm.b.b).
type SharedUser struct {
	ID           int32  // tag 1
	SessionID    int32  // tag 2
	Username     string // tag 3
	Address      string // tag 4
	CurrentUser  string // tag 5
	SharingOrder int32  // tag 6
}

// SharedUsers is com.avocent.kvm.b.a.y "SharedUserResponse". Protocol
// >= 2.34 layout is a TLV list: uint8 tag, uint8 len, value; tag 0 ends a
// record. Tags: 1 id(int32), 2 sessionID(int32), 3 username (len-1 chars),
// 4 address, 5 currentUser (len-1 chars), 6 sharingOrder(int32).
// Older protocols: byte 0 skipped, then {uint8 len, name} entries.
type SharedUsers struct {
	Users []SharedUser
	// Legacy selects the pre-2.34 layout for MarshalBody/UnmarshalBody.
	Legacy bool
}

func (p *SharedUsers) Type() uint16 { return TypeSharedUsers }
func (p *SharedUsers) Name() string { return "SharedUserResponse" }

func (p *SharedUsers) MarshalBody() ([]byte, error) {
	var b []byte
	if p.Legacy {
		b = append(b, 0)
		for _, u := range p.Users {
			b = append(b, byte(len(u.Username)))
			b = append(b, u.Username...)
		}
		return b, nil
	}
	put32 := func(tag byte, v int32) {
		b = append(b, tag, 4)
		b = binary.BigEndian.AppendUint32(b, uint32(v))
	}
	putStrZ := func(tag byte, s string) {
		b = append(b, tag, byte(len(s)+1))
		b = append(b, s...)
		b = append(b, 0)
	}
	for _, u := range p.Users {
		put32(1, u.ID)
		put32(2, u.SessionID)
		putStrZ(3, u.Username)
		b = append(b, 4, byte(len(u.Address)))
		b = append(b, u.Address...)
		putStrZ(5, u.CurrentUser)
		put32(6, u.SharingOrder)
		b = append(b, 0)
	}
	return b, nil
}

func (p *SharedUsers) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	p.Users = p.Users[:0]
	if p.Legacy {
		i := 1
		for i < len(body) {
			n := int(body[i])
			i++
			if n == 0 {
				continue
			}
			if i+n > len(body) {
				return ErrShortPacket
			}
			p.Users = append(p.Users, SharedUser{ID: -1, Username: string(body[i : i+n])})
			i += n
		}
		return nil
	}
	cur := SharedUser{ID: -1}
	i := 0
	for i < len(body) {
		tag := body[i]
		i++
		if tag == 0 {
			if cur.ID != -1 {
				p.Users = append(p.Users, cur)
				cur = SharedUser{ID: -1}
			}
			continue
		}
		if i >= len(body) {
			return ErrShortPacket
		}
		n := int(body[i])
		i++
		if i+n > len(body) {
			return ErrShortPacket
		}
		v := body[i : i+n]
		switch tag {
		case 1:
			if n >= 4 {
				cur.ID = rd32(v, 0)
			}
		case 2:
			if n >= 4 {
				cur.SessionID = rd32(v, 0)
			}
		case 3:
			cur.Username = cString(v)
		case 4:
			cur.Address = cString(v)
		case 5:
			cur.CurrentUser = cString(v)
		case 6:
			if n >= 4 {
				cur.SharingOrder = rd32(v, 0)
			}
		}
		i += n
	}
	// Java only appends a record when a 0 tag follows; a trailing record
	// without terminator is kept here for robustness.
	if cur.ID != -1 {
		p.Users = append(p.Users, cur)
	}
	return nil
}

// Locked is com.avocent.kvm.b.a.nb "Locked" (no body decoding).
type Locked struct{ emptyPacket }

// Reserved is com.avocent.kvm.b.a.sb "Reserved".
type Reserved struct{ emptyPacket }

// Scaling is com.avocent.kvm.b.a.tb "Scaling".
type Scaling struct{ emptyPacket }

// UserPrivilege is com.avocent.kvm.b.a.wb "User Priviledge Parameters":
// uint16 at offset 3; bit 0x10 enables the power menu.
type UserPrivilege struct {
	Mask int
}

func (p *UserPrivilege) Type() uint16           { return TypeUserPrivilege }
func (p *UserPrivilege) Name() string           { return "User Priviledge Parameters" }
func (p *UserPrivilege) PowerMenuEnabled() bool { return p.Mask&0x10 != 0 }
func (p *UserPrivilege) MarshalBody() ([]byte, error) {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[3:], uint16(p.Mask))
	return b, nil
}
func (p *UserPrivilege) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 5); err != nil {
		return err
	}
	p.Mask = rd16(body, 3)
	return nil
}

// ServerEntry is one managed server as reported by AvailableServers /
// ServerStatusUpdate (com.avocent.kvm.b.o).
//
//	uint16 nameLen, name, uint16 status (low 2 bits = power state),
//	uint16 field c, uint16 descLen, description
type ServerEntry struct {
	Name        string
	PowerState  int // status & 3
	Status      int // full status word
	FieldC      int
	Description string
}

func marshalServerEntry(b []byte, e ServerEntry) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(e.Name)))
	b = append(b, e.Name...)
	b = binary.BigEndian.AppendUint16(b, uint16(e.Status))
	b = binary.BigEndian.AppendUint16(b, uint16(e.FieldC))
	b = binary.BigEndian.AppendUint16(b, uint16(len(e.Description)))
	b = append(b, e.Description...)
	return b
}

func unmarshalServerEntry(body []byte, off int) (ServerEntry, int, error) {
	var e ServerEntry
	if err := need(body, off+2); err != nil {
		return e, off, err
	}
	n := rd16(body, off)
	off += 2
	if err := need(body, off+n+6); err != nil {
		return e, off, err
	}
	e.Name = string(body[off : off+n])
	off += n
	e.Status = rd16(body, off)
	e.PowerState = e.Status & 3
	off += 2
	e.FieldC = rd16(body, off)
	off += 2
	m := rd16(body, off)
	off += 2
	if err := need(body, off+m); err != nil {
		return e, off, err
	}
	e.Description = string(body[off : off+m])
	off += m
	return e, off, nil
}

// AvailableServers is com.avocent.kvm.b.a.c "AvailableServerNames": uint16
// count followed by ServerEntry records. The viewer stores the first
// entry's name as the target for SetPowerState and its power state.
type AvailableServers struct {
	Servers []ServerEntry
}

func (p *AvailableServers) Type() uint16 { return TypeAvailableServers }
func (p *AvailableServers) Name() string { return "AvailableServerNames" }
func (p *AvailableServers) MarshalBody() ([]byte, error) {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(p.Servers)))
	for _, e := range p.Servers {
		b = marshalServerEntry(b, e)
	}
	return b, nil
}
func (p *AvailableServers) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 2); err != nil {
		return err
	}
	n := rd16(body, 0)
	off := 2
	p.Servers = p.Servers[:0]
	for i := 0; i < n; i++ {
		e, next, err := unmarshalServerEntry(body, off)
		if err != nil {
			return err
		}
		p.Servers = append(p.Servers, e)
		off = next
	}
	return nil
}

// SelectedServerUpdate is com.avocent.kvm.b.a.bc "SelectedServerUpdate":
// uint16 length, server name.
type SelectedServerUpdate struct {
	ServerName string
}

func (p *SelectedServerUpdate) Type() uint16 { return TypeSelectedServerUpdate }
func (p *SelectedServerUpdate) Name() string { return "SelectedServerUpdate" }
func (p *SelectedServerUpdate) MarshalBody() ([]byte, error) {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(p.ServerName)))
	return append(b, p.ServerName...), nil
}
func (p *SelectedServerUpdate) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 2); err != nil {
		return err
	}
	n := rd16(body, 0)
	if err := need(body, 2+n); err != nil {
		return err
	}
	p.ServerName = string(body[2 : 2+n])
	return nil
}

// ServerStatusUpdate is com.avocent.kvm.b.a.cc "ServerStatusUpdate": a
// single ServerEntry.
type ServerStatusUpdate struct {
	Server ServerEntry
}

func (p *ServerStatusUpdate) Type() uint16 { return TypeServerStatusUpdate }
func (p *ServerStatusUpdate) Name() string { return "ServerStatusUpdate" }
func (p *ServerStatusUpdate) MarshalBody() ([]byte, error) {
	return marshalServerEntry(nil, p.Server), nil
}
func (p *ServerStatusUpdate) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	e, _, err := unmarshalServerEntry(body, 0)
	if err != nil {
		return err
	}
	p.Server = e
	return nil
}

// CredentialsPush is com.avocent.kvm.b.a.j (server->client type 0x8434,
// "RequestCredentialsMessage"): two 32-byte NUL-terminated strings
// (user, password) forwarded to session listeners – used by the virtual
// media single sign-on. UNVERIFIED.
type CredentialsPush struct {
	User, Password string
}

func (p *CredentialsPush) Type() uint16 { return TypeCredentialsPush }
func (p *CredentialsPush) Name() string { return "RequestCredentialsMessage" }
func (p *CredentialsPush) MarshalBody() ([]byte, error) {
	b := make([]byte, 64)
	putPaddedString(b[0:], p.User, 32)
	putPaddedString(b[32:], p.Password, 32)
	return b, nil
}
func (p *CredentialsPush) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	if err := need(body, 64); err != nil {
		return err
	}
	p.User = cString(body[0:32])
	p.Password = cString(body[32:64])
	return nil
}

// RawPacket is returned for unknown packet types (Java: qb "NoOpResponse").
type RawPacket struct {
	PacketType uint16
	Header     [FrameHeaderLen]byte
	Raw        []byte
	Magic      string // non-empty for in-band (non-AVSP) frames
}

func (p *RawPacket) Type() uint16                 { return p.PacketType }
func (p *RawPacket) Name() string                 { return TypeName(p.PacketType) }
func (p *RawPacket) MarshalBody() ([]byte, error) { return p.Raw, nil }
func (p *RawPacket) UnmarshalBody(hdr [FrameHeaderLen]byte, body []byte) error {
	p.PacketType = binary.BigEndian.Uint16(hdr[4:6])
	p.Header = hdr
	p.Raw = body
	return nil
}

// newServerPacket mirrors com.avocent.kvm.b.a.a.a(int): the type ->
// packet-class table.
func newServerPacket(t uint16) ServerPacket {
	switch t {
	case TypeLoginResponse, TypeLoginResponseV1:
		return &LoginResponse{}
	case TypeVideoConnectStatus:
		return &VideoConnectStatus{}
	case TypeVideoStopped, TypeCtlVideoStopped:
		return &VideoStopped{typ: t}
	case TypeVideoGeneric, TypeVideoDVC1, TypeVideoDVC2, TypeVideoDVC3, TypeVideoDVC4,
		TypeVideoJPEG, TypeVideoText, TypeColorPalette, TypeFontTable,
		TypeCtlVideoGeneric, TypeCtlVideoDVC1, TypeCtlVideoDVC2, TypeCtlVideoDVC3, TypeCtlVideoDVC4,
		TypeCtlVideoJPEG, TypeCtlVideoText, TypeCtlColorPalette, TypeCtlFontTable:
		return &VideoPacket{PacketType: t}
	case TypeKeyboardLED:
		return &KeyboardLED{}
	case TypeMouseAck:
		return &MouseAck{}
	case TypeMouseAccelResponse:
		return &MouseAccelResponse{}
	case TypeInputResolution, TypeInputResolutionAlt:
		return &InputResolution{Resolution{typ: t}}
	case TypeDisplayResolution:
		return &DisplayResolution{Resolution{typ: t}}
	case TypeVideoSetupData:
		return &VideoSetupData{}
	case TypeScaling:
		return &Scaling{emptyPacket{t, "Scaling"}}
	case TypeDVCColorMode:
		return &DVCColorMode{}
	case TypeUserDisconnectPending:
		return &UserDisconnectPending{}
	case TypeProtocolVersion:
		return &ProtocolVersion{}
	case TypePendingRequestCancelled:
		return &PendingRequestCancelled{}
	case TypeSharingRequest:
		return &SharingRequest{}
	case TypeSharedUsers:
		return &SharedUsers{}
	case TypeLocked:
		return &Locked{emptyPacket{t, "Locked"}}
	case TypeReserved:
		return &Reserved{emptyPacket{t, "Reserved"}}
	case TypeUserPrivilege:
		return &UserPrivilege{}
	case TypeAvailableServers:
		return &AvailableServers{}
	case TypeSelectedServerUpdate:
		return &SelectedServerUpdate{}
	case TypeServerStatusUpdate:
		return &ServerStatusUpdate{}
	case TypeCredentialsPush:
		return &CredentialsPush{}
	case TypeVideoAck, TypeVideoAckAlt:
		return &VideoAck{}
	case TypeVideoChannelAuth:
		return &VideoChannelAuth{}
	case TypeKeyboardData:
		return &KeyboardData{}
	case TypeMouseData:
		return &MouseData{}
	case TypeMouseDelta:
		return &MouseDelta{}
	case TypeSetDisplayArea:
		return &SetDisplayArea{}
	case TypeVideoEnable:
		return &VideoEnable{}
	case TypeFocusControl:
		return &FocusControl{}
	case TypeSetDVCColorDepth:
		return &SetDVCColorDepth{}
	case TypeSetMouseAccel:
		return &SetMouseAccel{}
	case TypeSetVideoTransmitLimit:
		return &SetVideoTransmitLimit{}
	case TypeSharingResponse:
		return &SharingResponse{}
	case TypeSetPowerState:
		return &SetPowerState{}
	case TypeTextMessage:
		return &TextMessage{}
	case TypeLoginRequest, TypeLoginRequestShare, TypeLoginRequestLong:
		return &LoginRequest{}
	}
	return &RawPacket{PacketType: t}
}

// Decode turns a frame read from the control channel into a typed packet.
// Unknown types decode to *RawPacket. protocol (major, minor) selects the
// SharedUsers layout; pass 0,0 for the current default (2.34).
func Decode(f *Frame, protoMajor, protoMinor int) (Packet, error) {
	if f.Type == FrameTypeInband {
		return &RawPacket{PacketType: f.Type, Header: f.Header, Raw: f.Body, Magic: f.Magic}, nil
	}
	p := newServerPacket(f.Type)
	if su, ok := p.(*SharedUsers); ok {
		if protoMajor != 0 && (protoMajor < 2 || (protoMajor == 2 && protoMinor < 34)) {
			su.Legacy = true
		}
	}
	if err := p.UnmarshalBody(f.Header, f.Body); err != nil {
		return p, fmt.Errorf("kvm: decode %s (0x%04x): %w", p.Name(), f.Type, err)
	}
	return p, nil
}
