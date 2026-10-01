# Avocent KVM control channel (iDRAC6 avctKVM.jar fw 2.92)

Byte-level description of what `pkg/kvm` (`transport.go`, `packets.go`,
`session.go`, `input.go`, `keymap.go`) implements. Java class names refer
to the CFR-decompiled `avctKVM.jar`. All integers are big-endian.

## 1. Transports

### 1.1 APCP pre-handshake (`com.avocent.kvm.b.l`)

Spoken in clear text on every fresh TCP connection when the launcher passes
`apcp=1` (`USE_APCP`). Same port for control and video on iDRAC (5900).

Client request, 53 bytes:

| off | size | value |
|----:|-----:|-------|
| 0 | 4 | `"APCP"` |
| 4 | int32 | 53 (total length) |
| 8 | int16 | 0x0100 (request) |
| 10 | int16 | 0 |
| 12 | u8 | request type: **3** = control/KVM socket, **4** = video socket |
| 13 | u8 | protocol major (2) |
| 14 | u8 | protocol minor (34) |
| 15 | u8 | 0 |
| 16 | int32 | capabilities requested: 5 (= 1 plain \| 4 SSL) |
| 20 | u8 | random length (32) |
| 21 | 32 | random bytes |

Server response, 53 bytes:

| off | size | value |
|----:|-----:|-------|
| 0 | 4 | `"APCP"` |
| 4 | int32 | length |
| 8 | int16 | 0x8100 (response) |
| 10 | int16 | ignored |
| 12 | u8 | server protocol major |
| 13 | u8 | server protocol minor |
| 14 | int32 | capabilities: 0 = refused; bit0 = stay clear; bit2 = upgrade this TCP connection to SSL/TLS |
| 18 | int16 | redirect port (0 = keep this socket; else open a new TCP connection to that port, no second APCP) |
| 20 | u8 | random length |
| 21 | 32 | random (padded) |

Java checks bit0 **before** bit2, so `caps=5` would mean clear text.
Version rule (`r.a(String,int,int,boolean,boolean)`): if the server's
version >= ours, adopt the server's; otherwise use 2.33. Both branches
still produce a Login Request of type 0x0102 (major >= 2).

Live (unauthenticated, no login sent):

| device | control (type 3) | video (type 4) |
|--------|------------------|----------------|
| iDRAC6 192.168.10.162 | ver 1.0, caps=4 -> TLS 1.2 `TLS_RSA_WITH_AES_128_GCM_SHA256` | ver 1.0, **caps=1 -> clear text** |
| iDRAC8 192.168.11.221 | ver 2.34, caps=4 -> TLS 1.2 ECDHE | ver 2.34, caps=4 -> TLS |

The server sends nothing after the TLS handshake until the client's Login
Request. Go needs `InsecureSkipVerify`, `MinVersion=TLS1.0` and an explicit
`CipherSuites` list containing `tls.InsecureCipherSuites()` (see
`DefaultTLSConfig`).

### 1.2 Direct TLS (`r.b(String,int,boolean)`)

Without APCP the control socket is a plain SSL socket (SSLv3 when the
`SSLV3` flag is set – not possible from Go). `DialTLS` implements the TLS
1.0+ subset.

## 2. AVSP framing

Every packet on both sockets: 8-byte header + body; total length in the
header **includes** the header. Reader (`com.avocent.kvm.b.a.a.a`) rejects
length < 16 or > 15000, so bodies are padded to >= 8 bytes.

Control socket header (`com.avocent.kvm.b.a.b.a(int,int)`):

```
0  'B' 'E' 'E' 'F'
4  u16 type
6  u16 total length
```

Video socket client header (`com.avocent.kvm.b.a.zb.a()`, used for Video
Channel Auth):

```
0  00 00 00 00
4  01
5  u8 type
6  u16 total length
```

The control reader takes the type from bytes 4-5, the video reader from
byte 5 only; bytes 0-3 are never checked. A header starting with `"APCP"`
inside the stream is tolerated (int32 length at 4); never observed.

Writes are header+body+flush from a single queue thread
(`com.avocent.kvm.c.d.g`); mouse packets go through a separate
rate-limited queue (`com.avocent.kvm.c.d.e`, unlimited once RUNNING) that
coalesces consecutive moves.

## 3. Connection sequence and state machine

```
INITIALIZING
  -> CONNECTING        DialControl (APCP type 3 [+TLS])
  -> AUTHENTICATING    send Login Request (0x0102)
  <- User Login Response (0x8305)
       status != 0     -> CONNECTION_LOGIN_FAILED (LOGIN_REASON)
       status == 0, flags&1  -> RUNNING (video arrives on control socket)
       status == 0, !(flags&1) -> VIDEO_PENDING: OpenVideo():
             DialVideo (APCP type 4 [+TLS]) | DialTLS if flags&2 | plain TCP
             send Video Channel Auth (type 1, video header) {clientRandom, applianceRandom}
  <- Video Connect Status (0x84, byte0==0)  (Java dispatches it from either socket)
  -> RUNNING           send Video Enable Request(1,1), Set Display Area(1024,768), SetScaleMode1to1
  keepalive thread: every 2 s KeepAlive (0x0400); while RUNNING also GetAvailableServers (0x0420)
  <- User Disconnect Pending (0x8301) -> CLOSING, reason 0..6, then socket closes
```

Login failure codes (`com.avocent.kvm.b.n.a(vb)`): 1,2 BAD_LOGIN; 3
ACCESS_DENIED; 4,12 IN_USE; 8 ALL_CHANNELS_IN_USE; 15 SHARING_DENIED; 16
SHARING_TIMEOUT; 24 LICENCE_EXPIRED; else BAD_LOGIN. After IN_USE the
viewer offers to share and reconnects with share mode 1.

Shutdown reasons (`com.avocent.idrac.kvm.d.b(int)`): 0 administrator
disconnect, 1 idle timeout, 2 appliance reboot pending, 3 DSRIQ upgrade
pending, 4 channel pre-empted by local user, 5 last active user
disconnected, 6 primary user went exclusive.

Video flow control (`r.B()`): every 20 video packets (types 0x80-0x8A /
0x86xx, including VideoStopped) send Video Ack (type 0, body[0]=count) on
the video socket if one exists, else on the control socket. The video
layer (`video.go`) does this for the video socket; `session.go` does it for
video on the control socket.

## 4. Packets

Bodies below exclude the 8-byte header. "16" means total frame length 16
(8-byte zero-padded body).

### 4.1 Client -> server

| type | Java | name | body |
|-----:|------|------|------|
| 0x0000 | cb | Video Ack | `u8 count`, pad |
| 0x0001 | ac | Video Channel Auth | `int32 clientRandom, int32 applianceRandom` (video header) |
| 0x0100/0x0102 | ab | Login Request | `u8 ulen, 96 user, u8 plen, 96 pass, 8 RIP, u8 port(1), u8 channel(0), int32 clientRandom [, u8 shareMode]` -> 216/217 bytes |
| 0x0101 | bb | Login Request alt | same as ab without share byte; unused |
| 0x0200 | f | KeyboardDataRequest | `0, u8 release(0=down,1=up), u16 HID usage, 4x0` |
| 0x0201 | gc | Mouse Data Request | `0, u8 buttons(1 L,2 R,4 M), u16 x, u16 y, i16 wheel(+ = up)` absolute video pixels |
| 0x0202 | h | MouseOrigin | zero (sent on pointer enter) |
| 0x0204 | g | Keyboard LED Request | zero |
| 0x0208 | d | FocusControl | `u8 focused` |
| 0x0209 | hc | Mouse Delta Request | as gc with signed deltas (never used on iDRAC) |
| 0x020A | t | SetMouseAccel | `u8 value` |
| 0x0301 | n | ScreenRefresh | zero |
| 0x0302 | x | Set Display Area | `u16 w, u16 h` (viewer: 1024x768) |
| 0x0304 | o | SetScaleMode1to1 | zero |
| 0x030E | db | Video Enable Request | `u8 enable, u8 1` |
| 0x0320 | w | text/chat message | `u16 code, u16 len, text` |
| 0x0400 | p | KeepAlive | zero, every 2 s |
| 0x0402 | u | SetVideoTransmitLimit | `u8 0..4` |
| 0x0403 | k | SetDVCColorDepthMessage | `u8 reset(0 apply/1 reset), u8 depth, u8 grayscale, u8 flag` |
| 0x0413 | fc | SharingResponse | `u8 answer, 0, u16 requestId` |
| 0x0420 | e | GetAvailableServers | zero, every 2 s while RUNNING |
| 0x0424 | dc | SetPowerState | `u16 len, serverName, u8 op` (1 on,2 off,3 cycle,4 reboot,5 graceful) |
| 0x0427 | r | RequestCredentialsMessage | `int32 30` |
| 0x0600 | cb | Video Ack alt id | as 0x0000 |

### 4.2 Server -> client

| type | Java | name | body |
|-----:|------|------|------|
| 0x8305 / 0x8300 | vb | User Login Response | new: `u8 status, u8 k, u8 l(30), 0, int32 applianceRandom, u16 flags, u8 msgLen, msg`; flags: 0x01 video on control socket, 0x02 direct-SSL video, 0x20 view-only available, 0x40 view-only. old (0x8300): `u8 status, 0,0, u8 flags, int32 applianceRandom, u8 msgLen, 96 msg` |
| 0x0084 / (video) | xb | Video Connect Status | `u8 status` (0 = connected) -> RUNNING |
| 0x0085 / 0x8605 | v | VideoStopped | `u8 reason` 0 calibrating,1 no signal,2 out of range,3/4 blocked |
| 0x0080..0x8A / 0x8600..0x860A | fb hb gb ub ib kb | video data | see `docs/kvm-video-channel.md`; DVC header has `u16 w @4, u16 h @6` |
| 0x8100 | mb | Keyboard LED | `u8 mask` (5 bits) |
| 0x8101 | pb | MouseAcknowledgeResponse | `u8 count` |
| 0x8102 | ob | MouseAccelResponse | `u8 value` |
| 0x8200 / 0x8206 | lb | Input Resolution Response | `u16 w, u16 h` |
| 0x8201 | jb | Display Resolution Response | `u16 w, u16 h` -> viewer sends ScreenRefresh |
| 0x8202 | yb | VideoSetupData | 10 x u16 (brightness, contrast, horz, vert, ?, fine, ?, ?, ?, priority) |
| 0x8203 | tb | Scaling | not decoded |
| 0x8204 | l | DVCColorModeResponse | `u8 depth, u8 colour, u8 flag` |
| 0x8301 | z | User Disconnect Pending | `u8 reason` |
| 0x8302 | rb | Protocol Version | `u8 major, u8 minor` |
| 0x8304 | q | PendingRequestCancelled | `u8 kind(3 = sharing), u16 requestId` |
| 0x8401 | ec | SharingRequest | `u16 requestId, u8 mode, user (NULs stripped, up to len-3)` |
| 0x8410 | y | SharedUserResponse | TLV records `u8 tag, u8 len, value`; tag 0 ends a record; 1 id int32, 2 sessionID int32, 3 username (len-1), 4 address, 5 currentUser (len-1), 6 sharingOrder int32. Pre-2.34: `0, {u8 len, name}...` |
| 0x8412 | nb | Locked | - |
| 0x8413 | sb | Reserved | - |
| 0x8420 | wb | User Priviledge Parameters | `u16 @3`; bit 0x10 = power menu |
| 0x8430 | c | AvailableServerNames | `u16 n, n x {u16 len, name, u16 status(&3 = power), u16 c, u16 len, desc}` |
| 0x8431 | bc | SelectedServerUpdate | `u16 len, name` |
| 0x8432 | cc | ServerStatusUpdate | one server entry |
| 0x8434 | j | RequestCredentialsMessage | `32 user, 32 password` (NUL padded) |

Unknown types decode to `RawPacket` (Java: `qb` "NoOpResponse").

## 5. Keyboard and mouse

* Key codes are **USB HID Keyboard/Keypad usage ids** (page 0x07):
  `com.avocent.kvm.c.c.d.i` maps AWT `VK_A`->4 ... `VK_ENTER`->40,
  `VK_ESCAPE`->41, `VK_F1`->58, Ctrl/Shift/Alt/GUI -> 224..231; the
  fallback switch in `c.c.c.a` adds Power=102, Mute=127 etc. Press = body
  byte 1 == 0, release == 1. `keymap.go` converts X11 keysyms (RFB) and
  US-layout characters to usages; shifted characters map to the physical
  key and Shift is a separate key event.
* Mouse is absolute (`gc`), coordinates in video pixels at 1:1 scale, full
  button state in every packet (L=1, R=2, M=4), wheel as i16 with
  positive = up. The viewer sends `MouseOrigin` when the pointer enters the
  canvas. Relative mode (`hc`) exists but is never selected on iDRAC.
* Input is only sent when the session is RUNNING and not view-only
  (`com.avocent.kvm.c.h.q()`).
* On non-Windows hosts the Java viewer sends the *press* of modifier keys
  (224..229/231) twice (`com.avocent.kvm.c.u`, `nativekeyboard.i`). Not
  replicated.

## 6. Go API summary

* `DialControl/DialVideo/DialAPCP/DialTLS/DialPlain`, `DefaultTLSConfig`,
  `ReadFrame/WriteFrame/ControlFrame/VideoFrame`.
* `Marshal(Packet)`, `Decode(*Frame, major, minor)`, packet structs named
  after their function with the Java class in the comment.
* `NewSession(Config)`, `Connect(ctx)`, `OpenVideo(ctx)`, `Run(ctx)`,
  `Close()`, `VideoConnected()`, `Send`, `SendControlPacket` (for
  `VideoStream.Control`), `SendVideo`; callbacks `OnState`, `OnShutdown`,
  `OnVideoMode`, `OnVideoPacket`, `OnVideoStopped`, `OnKeyboardLED`,
  `OnSharingRequest`, `OnServers`, `OnPacket`.
* Input: `KeyDown/KeyUp/Press/Chord/CtrlAltDel/TypeString/KeyEvent`,
  `MouseMove/MouseButton/MouseWheel/MouseOrigin/MouseMoveRelative/
  PointerEvent`, `RefreshScreen`, `SetDisplayArea`, `SetVideoEnabled`,
  `SetFocus`, `SetPower`, `RespondSharing`.

## 7. Unverified assumptions

1. **Which socket carries Video Connect Status (0x84)** after Video Channel
   Auth. Java dispatches it identically from both reader threads. `Session`
   handles it on the control socket; `VideoStream.OnConnectStatus` must
   call `Session.VideoConnected()` if it shows up on the video socket. As a
   fallback the session goes RUNNING `VideoConnectTimeout` (10 s) after
   `OpenVideo`.
2. The login-response field layout (0x8305) beyond status/flags/appliance
   random: bytes 1 (`k`) and 2 (`l`, default 30) are stored but unused by
   the viewer; the message string is assumed to start at 11.
3. `LoginRequest` length bytes: Java writes `String.length()` (UTF-16
   units); we write the UTF-8 byte length. Identical for ASCII.
4. `SharingResponse.Answer` values (which value grants access) – the value
   comes from a Swing dialog result that was not traced.
5. `SetDVCColorDepth` byte 0 semantics ("reset" when 1).
6. `KeyboardLED` bit assignment (five booleans in Java, unnamed).
7. Video Ack on the control socket when the appliance announces video on
   the control socket (flag 0x01) – implemented as in Java but never seen
   on iDRAC (both devices open a separate video socket).
8. APCP redirect-port handling (never seen; both devices answer 0).
9. Mouse coordinates are assumed to be raw video pixels (Java scale factor
   `e` = 1.0 with SetScaleMode1to1); the appliance may clamp/scale.
10. Relative mouse mode (`hc`) is encoded but never exercised by the viewer.
11. The double modifier press quirk (item 5 above) is not replicated;
    if modifiers are ignored by a host, try sending the press twice.
12. `GetAvailableServers` polling every 2 s mirrors Java; the appliance's
    reaction if it is omitted (`DisablePollServers`) is unknown.
13. The 15000-byte maximum frame length and 16-byte minimum come from the
    Java reader; the appliance was not probed for its own limits.

## 8. iDRAC7/8 differences (fw 2.86, APCP 2.34, ASpeed) — what the Go code does

Source: `docs/kvm-idrac8-notes.md` §8 "Minimal" items. Everything below is
implemented in `transport.go` / `packets.go` / `session.go`.

1. **Ports**: control and video both go to `ControlPort` (KMPORT) when
   `VideoPort` is 0 — the Dell iDRAC8 launcher passes KMPORT for both.
2. **APCP**: the 53-byte request is kept and advertises **2.34** by default
   (iDRAC8 echoes it; advertising 2.41 would switch the login to the long
   form). `readAPCPReply` accepts reply type 0x8100 **or 0x8101**, decodes
   the second u16 as the APCP version, and when the declared length is 68
   reads the 15-byte trailer: `0x8101`/version 0 → `u8 reqType, u16 0,
   u32 connectionId, u32, u32`; version 0x0104 → `u8 reqType, u16
   hbTimeout, u16 180, u8 updateInterval, u8 connIdNonZero, u32, u32`
   (`APCPInfo.ConnectionID/HeartbeatTimeout/UpdateInterval`). The
   reconnect-capable 68-byte *requests* and the 10-s APCP heartbeat are
   not sent (optional feature).
3. **Login**: layout chosen like `com.avocent.c.d.ib.b()`: negotiated
   major==1 → 0x0100; (major==0 or ≥2) and minor<41 → 0x0102 (iDRAC6
   layout + share byte); major≥2 and minor≥41 → **0x0104 long form**
   (`u16 ulen, u16 uoff=31, u16 plen, u16 poff=31+ulen, rip[8], u8 port,
   u8 channel, i32 clientRandom, u8 shareMode, user, pass`; total 31+ulen+plen,
   `LoginRequest.Long`). The port byte is `Config.LoginPort`; 0 = automatic:
   1 when the APCP server version is < 2 (iDRAC6), else `ControlPort&0xFF`
   (iDRAC8 viewer sends KMPORT&0xFF).
4. **Login response**: status **6 = IN_USE_PREEMPTABLE** and 9 =
   SHARING_DENIED added. **Input Resolution Response (0x8200/0x8206)**
   body[4..5] is parsed as the platform tag (`"AS"` ASpeed, `"D5"`, `"P3"`,
   `"P4"`) → `Session.Platform()`; iDRAC6 sends zeros there → `""`.
5. **Receiver limits**: frames of 8..32768 bytes are accepted (iDRAC6 reader
   was 16..15000); outgoing bodies are still padded to a 16-byte frame.
   Frames whose first four bytes are `"APCP"`/`"ASCP"` (u32 total length at
   4) or `"MGMT"`/`"CSCO"` (u16 total length at 6, CSCO assumed) are
   consumed by their own length and surfaced as `Frame.Magic` /
   `RawPacket.Magic` with `Type == FrameTypeInband`; the session logs and
   ignores them.
6. **Keep-alive**: still every 2 s (≤ 10 s required). `GetAvailableServers`
   (0x0420) polling is `PollAuto`: on only when the negotiated protocol is
   < 2.34 (iDRAC6), because the packet does not exist on iDRAC7/8.
7. **Tolerated iDRAC8 server types** (decoded as `RawPacket`, trace-logged,
   no error): 33027 AbsoluteMouseSyncResponse, 33285
   DisplayResolutionInvalidResponse, 33539
   UserDisconnectPendingTimeoutMessage, 33809 ExclusiveModeResp, 33813
   ApplianceOptionsResponse, 33845 chat, 40704 CustomServerMacroMessage,
   21845 MgmtSessionResponse, 40464 MgmtBootDeviceStruct, 40465
   MgmtBootDeviceResult, 0x2009 InventoryResponseMsg (`IsIDRAC8Only`).
8. **TLS**: unchanged (`DefaultTLSConfig`, TLS 1.2 ECDHE accepted live).
9. Launch parameters: nothing to do in this package (`password=` alias is
   the launcher's concern).

Live, unauthenticated, 192.168.11.221:5900: APCP type 3 and 4 both answer
2.34 / caps 4 and complete TLS 1.2 (`TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384`).

Additional unverified items for iDRAC8: the CSCO length encoding (assumed
BEEF-style u16 at offset 6); whether iDRAC8 honours the 0x0102 port byte
value at all (both 1 and KMPORT&0xFF are believed accepted); the 0x8305
vs 0x8300 login-response type on iDRAC8 for a 0x0102 request (the notes
list 33536/0x8300 as the common response — both decode identically here).
