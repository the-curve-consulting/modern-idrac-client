package kvm

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

func TestAPCPRequestEncoding(t *testing.T) {
	rnd := bytes.Repeat([]byte{0xAB}, 32)
	b := buildAPCPRequest(APCPTypeControl, 2, 34, 5, rnd)
	if len(b) != 53 {
		t.Fatalf("len = %d, want 53", len(b))
	}
	want := "41504350" + "00000035" + "0100" + "0000" + "03" + "02" + "22" + "00" + "00000005" + "20" + strings.Repeat("ab", 32)
	if got := hex.EncodeToString(b); got != want {
		t.Fatalf("request =\n%s\nwant\n%s", got, want)
	}
	// short random is zero padded
	b = buildAPCPRequest(APCPTypeVideo, 2, 34, 5, []byte{1, 2, 3})
	if len(b) != 53 || b[12] != 4 || b[20] != 3 || b[21] != 1 || b[24] != 0 {
		t.Fatalf("bad padded request % x", b)
	}
}

func TestControlFrameHeader(t *testing.T) {
	f := ControlFrame(TypeKeepAlive, nil)
	if got := hex.EncodeToString(f.Bytes()); got != "42454546"+"0400"+"0010"+"0000000000000000" {
		t.Fatalf("keepalive frame = %s", got)
	}
	f = ControlFrame(TypeLoginRequestShare, make([]byte, 209))
	if f.Len() != 217 || binary.BigEndian.Uint16(f.Header[6:]) != 217 {
		t.Fatalf("login frame len = %d hdr=% x", f.Len(), f.Header)
	}
}

func TestVideoFrameHeader(t *testing.T) {
	fr, err := Marshal(&VideoChannelAuth{ClientRandom: 0x01020304, ApplianceRandom: -2})
	if err != nil {
		t.Fatal(err)
	}
	want := "0000000001010010" + "01020304" + "fffffffe"
	if got := hex.EncodeToString(fr.Bytes()); got != want {
		t.Fatalf("video auth = %s want %s", got, want)
	}
}

func TestReadFrame(t *testing.T) {
	// control: type from bytes 4-5
	raw, _ := hex.DecodeString("42454546" + "8305" + "0010" + "0001020304050607")
	f, err := ReadFrame(bytes.NewReader(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != TypeLoginResponse || len(f.Body) != 8 || f.Body[7] != 7 {
		t.Fatalf("bad frame %+v", f)
	}
	// video: type from byte 5 only
	raw2, _ := hex.DecodeString("00000000" + "0084" + "0010" + "0000000000000000")
	f, err = ReadFrame(bytes.NewReader(raw2), true)
	if err != nil || f.Type != TypeVideoConnectStatus {
		t.Fatalf("video frame type=%#x err=%v", f.Type, err)
	}
	// too short
	raw3, _ := hex.DecodeString("42454546" + "0400" + "0007")
	if _, err = ReadFrame(bytes.NewReader(raw3), false); err == nil {
		t.Fatal("expected error for length 7")
	}
	// too long
	raw4, _ := hex.DecodeString("42454546" + "0400" + "8001")
	if _, err = ReadFrame(bytes.NewReader(raw4), false); err == nil {
		t.Fatal("expected error for length 32769")
	}
	// in-band APCP
	raw5 := append([]byte("APCP"), 0, 0, 0, 12, 1, 2, 3, 4)
	f, err = ReadFrame(bytes.NewReader(raw5), false)
	if err != nil || f.Type != FrameTypeAPCPInband || len(f.Body) != 4 {
		t.Fatalf("apcp inband: %+v %v", f, err)
	}
	// round trip through WriteFrame
	var buf bytes.Buffer
	if err := WriteFrame(&buf, ControlFrame(TypeMouseData, []byte{0, 1, 0, 2, 0, 3, 0, 4})); err != nil {
		t.Fatal(err)
	}
	f, err = ReadFrame(&buf, false)
	if err != nil || f.Type != TypeMouseData || !bytes.Equal(f.Body, []byte{0, 1, 0, 2, 0, 3, 0, 4}) {
		t.Fatalf("round trip: %+v %v", f, err)
	}
}

func TestLoginRequestLayout(t *testing.T) {
	p := &LoginRequest{Username: "root", Password: "calvin", Port: 1, Channel: 0, ClientRandom: 1234567, WithShareMode: true}
	body, err := p.MarshalBody()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 209 {
		t.Fatalf("body len %d", len(body))
	}
	if body[0] != 4 || string(body[1:5]) != "root" || body[5] != 0 {
		t.Fatalf("username field wrong: % x", body[:8])
	}
	if body[97] != 6 || string(body[98:104]) != "calvin" {
		t.Fatalf("password field wrong")
	}
	if !bytes.Equal(body[194:202], make([]byte, 8)) || body[202] != 1 || body[203] != 0 {
		t.Fatalf("rip/port/channel wrong: % x", body[194:204])
	}
	if binary.BigEndian.Uint32(body[204:]) != 1234567 || body[208] != 0 {
		t.Fatalf("random/share wrong: % x", body[204:])
	}
	fr, _ := Marshal(p)
	if fr.Len() != 217 || fr.Type != TypeLoginRequestShare {
		t.Fatalf("frame len %d type %#x", fr.Len(), fr.Type)
	}
	p2 := &LoginRequest{WithShareMode: false, Port: 1}
	body2, _ := p2.MarshalBody()
	if len(body2) != 208 {
		t.Fatalf("no-share body len %d", len(body2))
	}
	fr2, _ := Marshal(p2)
	if fr2.Len() != 216 || fr2.Type != TypeLoginRequest {
		t.Fatalf("frame len %d type %#x", fr2.Len(), fr2.Type)
	}
	// RIP parsing
	p3 := &LoginRequest{Port: 1}
	if err := p3.SetRIP("0011-2233-4455-66ff"); err != nil {
		t.Fatal(err)
	}
	if p3.RIP != [8]byte{0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0xff} || p3.Port != 0 {
		t.Fatalf("rip = % x port %d", p3.RIP, p3.Port)
	}
	if err := p3.SetRIP("zz"); err == nil {
		t.Fatal("expected RIP error")
	}
}

func TestLoginResponseDecode(t *testing.T) {
	// New layout (0x8305)
	body := make([]byte, 105)
	body[0] = LoginInUse
	body[1] = 7
	body[2] = 30
	binary.BigEndian.PutUint32(body[4:], 0xDEADBEEF)
	binary.BigEndian.PutUint16(body[8:], 0x62) // direct-ssl | view-only avail | view-only
	body[10] = 5
	copy(body[11:], "hello")
	f := ControlFrame(TypeLoginResponse, body)
	p, err := Decode(f, 2, 34)
	if err != nil {
		t.Fatal(err)
	}
	lr, ok := p.(*LoginResponse)
	if !ok {
		t.Fatalf("got %T", p)
	}
	if lr.Status != LoginInUse || lr.Reason() != "IN_USE" || lr.Accepted() {
		t.Fatalf("status %d reason %s", lr.Status, lr.Reason())
	}
	if lr.ApplianceRandom != int32(-559038737) || lr.Message != "hello" || lr.FieldK != 7 || lr.FieldL != 30 {
		t.Fatalf("fields: %+v", lr)
	}
	if lr.VideoOnControlChannel() || !lr.DirectSSLVideo() || !lr.ViewOnly() {
		t.Fatalf("flags: %+v", lr)
	}
	// Legacy layout (0x8300)
	body = make([]byte, 107)
	body[0] = 0
	body[3] = 1
	binary.BigEndian.PutUint32(body[4:], 42)
	body[8] = 3
	copy(body[9:], "abc")
	p, err = Decode(ControlFrame(TypeLoginResponseV1, body), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	lr = p.(*LoginResponse)
	if !lr.Legacy || !lr.Accepted() || lr.ApplianceRandom != 42 || lr.Message != "abc" || !lr.VideoOnControlChannel() {
		t.Fatalf("legacy: %+v", lr)
	}
	// Marshal/Unmarshal round trip
	orig := &LoginResponse{Status: 0, FieldK: 1, FieldL: 30, ApplianceRandom: 99, Flags: 0x20, Message: "x"}
	b, _ := orig.MarshalBody()
	var back LoginResponse
	if err := back.UnmarshalBody(ControlFrame(TypeLoginResponse, b).Header, b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*orig, back) {
		t.Fatalf("round trip %+v != %+v", *orig, back)
	}
}

func TestKeyboardAndMouseLayout(t *testing.T) {
	kb := &KeyboardData{Usage: HIDKeyA, Down: true}
	b, _ := kb.MarshalBody()
	if !bytes.Equal(b, []byte{0, 0, 0, 4, 0, 0, 0, 0}) {
		t.Fatalf("key down = % x", b)
	}
	kb.Down = false
	kb.Usage = HIDKeyRightAlt
	b, _ = kb.MarshalBody()
	if !bytes.Equal(b, []byte{0, 1, 0, 0xE6, 0, 0, 0, 0}) {
		t.Fatalf("key up = % x", b)
	}
	m := &MouseData{Buttons: MouseLeft | MouseRight, X: 1023, Y: -5, Wheel: -1}
	b, _ = m.MarshalBody()
	if !bytes.Equal(b, []byte{0, 3, 0x03, 0xFF, 0, 0, 0xFF, 0xFF}) {
		t.Fatalf("mouse = % x", b)
	}
	d := &MouseDelta{Buttons: MouseMiddle, DX: -1, DY: 2, Wheel: 1}
	b, _ = d.MarshalBody()
	if !bytes.Equal(b, []byte{0, 4, 0xFF, 0xFF, 0, 2, 0, 1}) {
		t.Fatalf("delta = % x", b)
	}
}

// roundTrip marshals p, decodes through the type table and compares.
func roundTrip(t *testing.T, p Packet) Packet {
	t.Helper()
	fr, err := Marshal(p)
	if err != nil {
		t.Fatalf("%s marshal: %v", p.Name(), err)
	}
	if fr.Len() < MinFrameLen {
		t.Fatalf("%s frame too short: %d", p.Name(), fr.Len())
	}
	back, err := Decode(fr, 2, 34)
	if err != nil {
		t.Fatalf("%s decode: %v", p.Name(), err)
	}
	if back.Type() != p.Type() {
		t.Fatalf("%s type %#x != %#x", p.Name(), back.Type(), p.Type())
	}
	return back
}

func TestRoundTrips(t *testing.T) {
	cases := []Packet{
		&LoginRequest{Username: "u", Password: "p", Port: 1, ClientRandom: 5, WithShareMode: true, ShareMode: 1},
		&LoginRequest{Username: "u", Password: "p", Port: 1, ClientRandom: 5},
		&VideoChannelAuth{ClientRandom: 1, ApplianceRandom: 2},
		&VideoAck{Count: 20},
		&KeyboardData{Usage: HIDKeyEnter, Down: true},
		&KeyboardData{Usage: HIDKeyF12, Down: false},
		&MouseData{Buttons: MouseLeft, X: 10, Y: 20, Wheel: 3},
		&MouseDelta{Buttons: 0, DX: -3, DY: 4, Wheel: -2},
		&FocusControl{Focused: true},
		&SetDisplayArea{Width: 1024, Height: 768},
		&VideoEnable{Enable: true},
		&VideoConnectStatus{Connected: true},
		&VideoConnectStatus{Connected: false},
		&VideoStopped{Reason: VideoStoppedNoSignal, typ: TypeVideoStopped},
		&KeyboardLED{Mask: 5},
		&MouseAck{Count: 7},
		&MouseAccelResponse{Value: 9},
		NewDisplayResolution(1280, 1024),
		NewInputResolution(800, 600, ""),
		NewInputResolution(1024, 768, "AS"),
		&VideoSetupData{Values: [10]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		&DVCColorMode{Depth: 15, Color: true, Flag: false},
		&UserDisconnectPending{Reason: ShutdownSessionIdleTimeout},
		&ProtocolVersion{Major: 2, Minor: 34},
		&PendingRequestCancelled{Kind: 3, RequestID: 0x1234},
		&SharingRequest{RequestID: 77, Mode: 1, User: "alice"},
		&SharedUsers{Users: []SharedUser{{ID: 1, SessionID: 99, Username: "bob", Address: "10.0.0.1", CurrentUser: "y", SharingOrder: 2}, {ID: 2, Username: "carol"}}},
		&UserPrivilege{Mask: 0x10},
		&AvailableServers{Servers: []ServerEntry{{Name: "srv1", Status: 0x101, PowerState: 1, FieldC: 3, Description: "d1"}, {Name: "srv2", Status: 2, PowerState: 2}}},
		&SelectedServerUpdate{ServerName: "srv1"},
		&ServerStatusUpdate{Server: ServerEntry{Name: "s", Status: 1, PowerState: 1, Description: "x"}},
		&CredentialsPush{User: "root", Password: "pw"},
		&SetDVCColorDepth{Reset: false, Depth: 7, Grayscale: true, Flag: true},
	}
	for _, c := range cases {
		back := roundTrip(t, c)
		if !reflect.DeepEqual(c, back) {
			t.Errorf("%s round trip mismatch:\n got %#v\nwant %#v", c.Name(), back, c)
		}
	}
}

func TestClientOnlyPacketsMarshal(t *testing.T) {
	// These have no server-side decoder in Java; check size and bytes only.
	type tc struct {
		p    Packet
		typ  uint16
		size int
		body []byte // optional exact body
	}
	cases := []tc{
		{NewKeepAlive(), 0x0400, 16, make([]byte, 8)},
		{NewGetAvailableServers(), 0x0420, 16, make([]byte, 8)},
		{NewScreenRefresh(), 0x0301, 16, make([]byte, 8)},
		{NewSetScaleMode1to1(), 0x0304, 16, make([]byte, 8)},
		{NewMouseOrigin(), 0x0202, 16, make([]byte, 8)},
		{NewKeyboardLEDRequest(), 0x0204, 16, make([]byte, 8)},
		{&SetMouseAccel{Value: 3}, 0x020A, 16, []byte{3, 0, 0, 0, 0, 0, 0, 0}},
		{&SetVideoTransmitLimit{Limit: 2}, 0x0402, 16, []byte{2, 0, 0, 0, 0, 0, 0, 0}},
		{&SharingResponse{RequestID: 0x0102, Answer: 1}, 0x0413, 16, []byte{1, 0, 1, 2, 0, 0, 0, 0}},
		{&SetPowerState{ServerName: "abc", Operation: PowerCycle}, 0x0424, 16, []byte{0, 3, 'a', 'b', 'c', 3, 0, 0}},
		{&RequestCredentials{Value: 30}, 0x0427, 16, []byte{0, 0, 0, 30, 0, 0, 0, 0}},
		{&TextMessage{Code: 1, Text: "hi"}, 0x0320, 16, []byte{0, 1, 0, 2, 'h', 'i', 0, 0}},
		{&VideoEnable{Enable: true}, 0x030E, 16, []byte{1, 1, 0, 0, 0, 0, 0, 0}},
		{&VideoAck{Count: 40}, 0x0000, 16, []byte{40, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, c := range cases {
		fr, err := Marshal(c.p)
		if err != nil {
			t.Fatalf("%s: %v", c.p.Name(), err)
		}
		if fr.Type != c.typ || fr.Len() != c.size {
			t.Errorf("%s: type %#x len %d, want %#x/%d", c.p.Name(), fr.Type, fr.Len(), c.typ, c.size)
		}
		if c.body != nil && !bytes.Equal(fr.Body, c.body) {
			t.Errorf("%s: body % x want % x", c.p.Name(), fr.Body, c.body)
		}
		if string(fr.Header[:4]) != "BEEF" {
			t.Errorf("%s: header magic % x", c.p.Name(), fr.Header[:4])
		}
	}
	if _, err := (&SetVideoTransmitLimit{Limit: 5}).MarshalBody(); err == nil {
		t.Error("limit 5 should be rejected")
	}
	if _, err := (&SetPowerState{ServerName: "x", Operation: 9}).MarshalBody(); err == nil {
		t.Error("power op 9 should be rejected")
	}
}

func TestVideoAckMatchesVideoLayer(t *testing.T) {
	fr, _ := Marshal(&VideoAck{Count: 20})
	if !bytes.Equal(fr.Bytes(), EncodeVideoAck(20)) {
		t.Fatalf("VideoAck % x != EncodeVideoAck % x", fr.Bytes(), EncodeVideoAck(20))
	}
}

func TestDecodeDispatch(t *testing.T) {
	cases := map[uint16]string{
		TypeLoginResponse: "*kvm.LoginResponse", TypeLoginResponseV1: "*kvm.LoginResponse",
		TypeVideoConnectStatus: "*kvm.VideoConnectStatus", TypeVideoStopped: "*kvm.VideoStopped",
		TypeCtlVideoStopped: "*kvm.VideoStopped", TypeCtlVideoDVC1: "*kvm.VideoPacket",
		TypeVideoJPEG: "*kvm.VideoPacket", TypeKeyboardLED: "*kvm.KeyboardLED", TypeMouseAck: "*kvm.MouseAck",
		TypeDisplayResolution: "*kvm.DisplayResolution", TypeInputResolution: "*kvm.InputResolution",
		TypeInputResolutionAlt: "*kvm.InputResolution", TypeUserDisconnectPending: "*kvm.UserDisconnectPending",
		TypeSharingRequest: "*kvm.SharingRequest", TypeSharedUsers: "*kvm.SharedUsers",
		TypeAvailableServers: "*kvm.AvailableServers", TypeUserPrivilege: "*kvm.UserPrivilege",
		TypeLocked: "*kvm.Locked", TypeReserved: "*kvm.Reserved", TypeScaling: "*kvm.Scaling",
		0x7777: "*kvm.RawPacket",
	}
	for typ, want := range cases {
		body := make([]byte, 24)
		body[0] = 1
		binary.BigEndian.PutUint16(body[0:], 1) // count=1 for AvailableServers etc.
		if typ == TypeAvailableServers {
			body = []byte{0, 1, 0, 1, 'a', 0, 1, 0, 0, 0, 0}
		}
		p, err := Decode(ControlFrame(typ, body), 2, 34)
		if err != nil {
			t.Errorf("type %#x: %v", typ, err)
			continue
		}
		if got := reflect.TypeOf(p).String(); got != want {
			t.Errorf("type %#x -> %s, want %s", typ, got, want)
		}
	}
}

func TestSharedUsersLegacy(t *testing.T) {
	su := &SharedUsers{Legacy: true, Users: []SharedUser{{ID: -1, Username: "aa"}, {ID: -1, Username: "b"}}}
	body, _ := su.MarshalBody()
	f := ControlFrame(TypeSharedUsers, body)
	p, err := Decode(f, 2, 33)
	if err != nil {
		t.Fatal(err)
	}
	got := p.(*SharedUsers)
	if !got.Legacy || len(got.Users) != 2 || got.Users[0].Username != "aa" || got.Users[1].Username != "b" {
		t.Fatalf("legacy shared users: %+v", got)
	}
}

func TestVideoPacketDimensions(t *testing.T) {
	body := make([]byte, 20)
	binary.BigEndian.PutUint16(body[4:], 1024)
	binary.BigEndian.PutUint16(body[6:], 768)
	p, err := Decode(ControlFrame(TypeCtlVideoDVC1, body), 2, 34)
	if err != nil {
		t.Fatal(err)
	}
	w, h := p.(*VideoPacket).Dimensions()
	if w != 1024 || h != 768 {
		t.Fatalf("dims %dx%d", w, h)
	}
	if !IsVideoDataType(TypeCtlVideoDVC1) || IsVideoDataType(TypeKeepAlive) {
		t.Fatal("IsVideoDataType wrong")
	}
}

func TestShortBodies(t *testing.T) {
	for _, typ := range []uint16{TypeLoginResponse, TypeSharingRequest, TypeVideoSetupData, TypeAvailableServers, TypeCredentialsPush, TypeUserPrivilege} {
		f := &Frame{Type: typ, Body: []byte{1}}
		binary.BigEndian.PutUint16(f.Header[4:], typ)
		if _, err := Decode(f, 2, 34); err == nil {
			t.Errorf("type %#x: expected short-packet error", typ)
		}
	}
}

func TestKeymap(t *testing.T) {
	cases := map[uint32]uint16{
		'a': HIDKeyA, 'A': HIDKeyA, 'z': HIDKeyZ, '1': HIDKey1, '!': HIDKey1, '0': HIDKey0, ')': HIDKey0,
		' ': HIDKeySpace, '-': HIDKeyMinus, '_': HIDKeyMinus, '=': HIDKeyEqual, '+': HIDKeyEqual,
		'[': HIDKeyLeftBrace, ']': HIDKeyRightBrace, '\\': HIDKeyBackslash, '|': HIDKeyBackslash,
		';': HIDKeySemicolon, '\'': HIDKeyApostrophe, '"': HIDKeyApostrophe, '`': HIDKeyGrave, '~': HIDKeyGrave,
		',': HIDKeyComma, '.': HIDKeyPeriod, '/': HIDKeySlash, '?': HIDKeySlash,
		XKReturn: HIDKeyEnter, XKEscape: HIDKeyEscape, XKBackSpace: HIDKeyBackspace, XKTab: HIDKeyTab,
		XKDelete: HIDKeyDelete, XKInsert: HIDKeyInsert, XKHome: HIDKeyHome, XKEnd: HIDKeyEnd,
		XKPageUp: HIDKeyPageUp, XKPageDown: HIDKeyPageDown, XKLeft: HIDKeyLeft, XKRight: HIDKeyRight,
		XKUp: HIDKeyUp, XKDown: HIDKeyDown, XKF1: HIDKeyF1, XKF12: HIDKeyF12, XKF13: HIDKeyF13, XKF24: HIDKeyF24,
		XKShiftL: HIDKeyLeftShift, XKShiftR: HIDKeyRightShift, XKControlL: HIDKeyLeftCtrl, XKControlR: HIDKeyRightCtrl,
		XKAltL: HIDKeyLeftAlt, XKAltR: HIDKeyRightAlt, XKSuperL: HIDKeyLeftGUI, XKSuperR: HIDKeyRightGUI,
		XKMetaL: HIDKeyLeftGUI, XKMenu: HIDKeyApplication, XKCapsLock: HIDKeyCapsLock, XKNumLock: HIDKeyNumLock,
		XKScrollLock: HIDKeyScrollLock, XKPause: HIDKeyPause, XKPrint: HIDKeyPrintScreen,
		XKKP0: HIDKeyKP0, XKKP0 + 1: HIDKeyKP1, XKKP9: HIDKeyKP1 + 8, XKKPEnter: HIDKeyKPEnter,
		XKKPAdd: HIDKeyKPAdd, XKKPSubtract: HIDKeyKPSubtract, XKKPMultiply: HIDKeyKPMultiply,
		XKKPDivide: HIDKeyKPDivide, XKKPDecimal: HIDKeyKPDecimal, XKKPHome: HIDKeyKP1 + 6, XKKPEnd: HIDKeyKP1,
		XKKPInsert: HIDKeyKP0, XKKPDelete: HIDKeyKPDecimal, XKISOLevel3Shift: HIDKeyRightAlt,
		XF86AudioMute: HIDKeyMute,
	}
	for ks, want := range cases {
		got, ok := KeysymToUsage(ks)
		if !ok || got != want {
			t.Errorf("keysym %#x -> %#x ok=%v, want %#x", ks, got, ok, want)
		}
	}
	if _, ok := KeysymToUsage(0x00e9); ok { // é has no US key
		t.Error("é should be unmapped")
	}
	// Java table sanity: AWT VK_A(65)->4, VK_ENTER(10)->40, VK_ESCAPE(27)->41, F1(112)->58, DELETE(127)->76
	if u, _, _ := CharToKey('A'); u != 4 {
		t.Error("A != 4")
	}
	if u, s, _ := CharToKey('Q'); u != 20 || !s {
		t.Error("Q wrong")
	}
	if u, s, ok := CharToKey('%'); !ok || u != HIDKey1+4 || !s {
		t.Error("% wrong")
	}
}

func TestStateString(t *testing.T) {
	if StateRunning.String() != "RUNNING" || StateLoginFailed.String() != "CONNECTION_LOGIN_FAILED" {
		t.Fatal("state names")
	}
	if LoginReason(3) != "ACCESS_DENIED" || LoginReason(12) != "IN_USE" || LoginReason(99) != "BAD_LOGIN" {
		t.Fatal("login reasons")
	}
	if ShutdownText(4) != "channel pre-empted by local user" {
		t.Fatal("shutdown text")
	}
}

// ---------------------------------------------------------------------------
// iDRAC7/8 additions
// ---------------------------------------------------------------------------

func TestLoginRequestLongForm(t *testing.T) {
	p := &LoginRequest{Username: "root", Password: "calvin", Port: 0x0C /* 5900&0xFF */, Channel: 0, ClientRandom: 7654321, ShareMode: 1, Long: true}
	fr, err := Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if fr.Type != TypeLoginRequestLong || fr.Len() != 31+4+6 {
		t.Fatalf("type %#x len %d", fr.Type, fr.Len())
	}
	b := fr.Body
	if binary.BigEndian.Uint16(b[0:]) != 4 || binary.BigEndian.Uint16(b[2:]) != 31 ||
		binary.BigEndian.Uint16(b[4:]) != 6 || binary.BigEndian.Uint16(b[6:]) != 35 {
		t.Fatalf("length/offset fields % x", b[:8])
	}
	if !bytes.Equal(b[8:16], make([]byte, 8)) || b[16] != 0x0C || b[17] != 0 ||
		binary.BigEndian.Uint32(b[18:]) != 7654321 || b[22] != 1 {
		t.Fatalf("fixed fields % x", b[8:23])
	}
	if string(b[23:27]) != "root" || string(b[27:33]) != "calvin" {
		t.Fatalf("strings %q", b[23:])
	}
	// username at frame offset 31, password at 35
	raw := fr.Bytes()
	if string(raw[31:35]) != "root" || string(raw[35:41]) != "calvin" {
		t.Fatalf("absolute offsets wrong: %q", raw[31:])
	}
	back, err := Decode(fr, 2, 41)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, &LoginRequest{Username: "root", Password: "calvin", Port: 0x0C, ClientRandom: 7654321, ShareMode: 1, WithShareMode: true, Long: true}) {
		t.Fatalf("round trip %#v", back)
	}
	// bad offsets rejected
	bad := append([]byte(nil), b...)
	binary.BigEndian.PutUint16(bad[2:], 500)
	if err := (&LoginRequest{}).UnmarshalBody(fr.Header, bad); err == nil {
		t.Fatal("expected offset error")
	}
}

func TestLoginStatusIDRAC8(t *testing.T) {
	if LoginReason(LoginInUsePreemptable) != "IN_USE_PREEMPTABLE" || LoginReason(9) != "SHARING_DENIED" {
		t.Fatal("iDRAC8 login reasons")
	}
}

func TestInputResolutionPlatformTag(t *testing.T) {
	body := []byte{0x04, 0x00, 0x03, 0x00, 'A', 'S', 0, 0}
	p, err := Decode(ControlFrame(TypeInputResolution, body), 2, 34)
	if err != nil {
		t.Fatal(err)
	}
	ir := p.(*InputResolution)
	if ir.Width != 1024 || ir.Height != 768 || ir.Platform != PlatformASpeed {
		t.Fatalf("%+v", ir)
	}
	// iDRAC6: zero padding => no platform
	p, _ = Decode(ControlFrame(TypeInputResolutionAlt, []byte{0x04, 0x00, 0x03, 0x00, 0, 0, 0, 0}), 2, 33)
	if p.(*InputResolution).Platform != "" {
		t.Fatal("expected empty platform")
	}
	// Display resolution never carries a tag
	p, _ = Decode(ControlFrame(TypeDisplayResolution, body), 2, 34)
	if p.(*DisplayResolution).Platform != "" {
		t.Fatal("display resolution should not decode a platform")
	}
}

func TestAPCPReplyVariants(t *testing.T) {
	// 53-byte iDRAC6 reply: ver 1.0 caps 4
	r6, _ := hex.DecodeString("41504350" + "00000035" + "8100" + "0000" + "01" + "00" + "00000004" + "0000" + "20" + strings.Repeat("11", 32))
	info, raw, err := readAPCPReply(bytes.NewReader(r6))
	if err != nil {
		t.Fatal(err)
	}
	if info.Major != 1 || info.Minor != 0 || info.Capabilities != 4 || info.RedirectPort != 0 || len(info.ServerRandom) != 32 || info.Length != 53 || len(raw) != 53 || info.Trailer != nil {
		t.Fatalf("idrac6: %+v", info)
	}
	// 53-byte iDRAC8 reply to the plain request: 0x8100, apcp ver 0, 2.34, caps 4, rndlen 0 (random echoed as length 0)
	r8, _ := hex.DecodeString("41504350" + "00000035" + "8100" + "0000" + "02" + "22" + "00000004" + "0000" + "00" + strings.Repeat("00", 32))
	info, _, err = readAPCPReply(bytes.NewReader(r8))
	if err != nil {
		t.Fatal(err)
	}
	if info.Major != 2 || info.Minor != 34 || info.Capabilities != 4 || len(info.ServerRandom) != 0 || info.MessageType != 0x8100 {
		t.Fatalf("idrac8 plain: %+v", info)
	}
	// 68-byte variant B reply: 0x8101, apcp ver 0, 2.41, trailer 03 0000 00000001 00000000 00000000
	rB, _ := hex.DecodeString("41504350" + "00000044" + "8101" + "0000" + "02" + "29" + "00000004" + "0000" + "00" + strings.Repeat("00", 32) +
		"03" + "0000" + "00000001" + "00000000" + "00000000")
	info, raw, err = readAPCPReply(bytes.NewReader(rB))
	if err != nil {
		t.Fatal(err)
	}
	if info.MessageType != 0x8101 || info.Length != 68 || len(raw) != 68 || info.EchoedRequestType != 3 || info.ConnectionID != 1 || info.Minor != 41 {
		t.Fatalf("variant B: %+v", info)
	}
	// 68-byte variant C reply: 0x8100, apcp ver 0x0104, trailer 03 001e 00b4 0a 01 00000000 00000000
	rC, _ := hex.DecodeString("41504350" + "00000044" + "8100" + "0104" + "02" + "29" + "00000004" + "0000" + "00" + strings.Repeat("00", 32) +
		"03" + "001e" + "00b4" + "0a" + "01" + "00000000" + "00000000")
	info, _, err = readAPCPReply(bytes.NewReader(rC))
	if err != nil {
		t.Fatal(err)
	}
	if info.APCPVersion != 0x0104 || info.HeartbeatTimeout != 30 || info.UpdateInterval != 10 || info.ConnectionID != 1 {
		t.Fatalf("variant C: %+v", info)
	}
	// wrong type rejected
	bad, _ := hex.DecodeString("41504350" + "00000035" + "8200" + "0000" + "02" + "22" + "00000004" + "0000" + "00" + strings.Repeat("00", 32))
	if _, _, err = readAPCPReply(bytes.NewReader(bad)); err == nil {
		t.Fatal("expected message type error")
	}
	// truncated trailer
	if _, _, err = readAPCPReply(bytes.NewReader(rB[:60])); err == nil {
		t.Fatal("expected truncated trailer error")
	}
}

func TestInbandMagicFrames(t *testing.T) {
	// MGMT: BEEF-style u16 length at 6..7 (total 12)
	mgmt := []byte("MGMT")
	mgmt = append(mgmt, 0, 0, 0, 12, 0xAA, 0xBB, 0xCC, 0xDD)
	// ASCP: u32 length at 4 (total 10)
	ascp := []byte("ASCP")
	ascp = append(ascp, 0, 0, 0, 10, 0x01, 0x02)
	// CSCO: treated like MGMT (total 8, empty body)
	csco := []byte("CSCO")
	csco = append(csco, 0, 0, 0, 8)
	// followed by a normal 8-byte frame (min length 8 accepted now)
	normal := []byte{'B', 'E', 'E', 'F', 0x04, 0x00, 0x00, 0x08}
	r := bytes.NewReader(bytes.Join([][]byte{mgmt, ascp, csco, normal}, nil))
	for _, want := range []struct {
		magic string
		body  int
	}{{"MGMT", 4}, {"ASCP", 2}, {"CSCO", 0}} {
		f, err := ReadFrame(r, false)
		if err != nil {
			t.Fatalf("%s: %v", want.magic, err)
		}
		if f.Type != FrameTypeInband || f.Magic != want.magic || len(f.Body) != want.body {
			t.Fatalf("%s: got %+v", want.magic, f)
		}
		p, err := Decode(f, 2, 34)
		if err != nil {
			t.Fatal(err)
		}
		if rp, ok := p.(*RawPacket); !ok || rp.Magic != want.magic {
			t.Fatalf("decode %T %+v", p, p)
		}
	}
	f, err := ReadFrame(r, false)
	if err != nil || f.Type != TypeKeepAlive || len(f.Body) != 0 {
		t.Fatalf("8-byte frame: %+v %v", f, err)
	}
	// maximum 32768 accepted
	big := []byte{'B', 'E', 'E', 'F', 0x86, 0x01, 0x80, 0x00}
	big = append(big, make([]byte, 32760)...)
	f, err = ReadFrame(bytes.NewReader(big), false)
	if err != nil || f.Len() != 32768 {
		t.Fatalf("32768 frame: %v", err)
	}
}

func TestIDRAC8OnlyTypesDecodeAsRaw(t *testing.T) {
	for _, typ := range []uint16{33027, 33285, 33539, 33809, 33813, 33845, 40704, 21845, 40464, 40465, 0x2009} {
		f := ControlFrame(typ, []byte{1, 2, 3})
		p, err := Decode(f, 2, 34)
		if err != nil {
			t.Errorf("type %d: %v", typ, err)
			continue
		}
		if _, ok := p.(*RawPacket); !ok || !IsIDRAC8Only(typ) || strings.HasPrefix(TypeName(typ), "Unknown") {
			t.Errorf("type %d -> %T name %q", typ, p, TypeName(typ))
		}
	}
	if IsIDRAC8Only(TypeKeepAlive) {
		t.Fatal("keepalive is not iDRAC8-only")
	}
}

func TestSessionDefaults(t *testing.T) {
	s := NewSession(Config{Host: "h", ControlPort: 5900, UseAPCP: true})
	if s.cfg.VideoPort != 5900 {
		t.Fatalf("video port %d", s.cfg.VideoPort)
	}
	if s.cfg.ProtocolMajor != 2 || s.cfg.ProtocolMinor != 34 {
		t.Fatal("default protocol")
	}
	// login port rule
	if s.loginPort() != 1 {
		t.Fatal("no APCP info => port byte 1")
	}
	s.apcp = &APCPInfo{Major: 1, Minor: 0}
	if s.loginPort() != 1 {
		t.Fatal("iDRAC6 => 1")
	}
	s.apcp = &APCPInfo{Major: 2, Minor: 34}
	if s.loginPort() != byte(5900&0xFF) {
		t.Fatalf("iDRAC8 => KMPORT&0xFF, got %d", s.loginPort())
	}
	s.cfg.LoginPort = 7
	if s.loginPort() != 7 {
		t.Fatal("explicit LoginPort")
	}
	// version negotiation
	s.negotiateVersion(&APCPInfo{Major: 1, Minor: 0})
	if s.protoMajor != 2 || s.protoMinor != 33 || !s.pollServers() {
		t.Fatalf("idrac6 negotiation %d.%d poll=%v", s.protoMajor, s.protoMinor, s.pollServers())
	}
	s.negotiateVersion(&APCPInfo{Major: 2, Minor: 34})
	if s.protoMajor != 2 || s.protoMinor != 34 || s.pollServers() {
		t.Fatalf("idrac8 negotiation %d.%d poll=%v", s.protoMajor, s.protoMinor, s.pollServers())
	}
	s.negotiateVersion(&APCPInfo{Major: 2, Minor: 33})
	if s.protoMinor != 33 || !s.pollServers() {
		t.Fatal("server 2.33 should be adopted")
	}
	s.cfg.PollServers = PollOff
	if s.pollServers() {
		t.Fatal("PollOff")
	}
}
