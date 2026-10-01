# iDRAC7/8 Java KVM viewer vs iDRAC6 — wire-protocol differences for the Go client

Date: 2026-09-30. Target: extend the Go client written against the iDRAC6 viewer
(`avctKVM.jar` fw 2.92) to the iDRAC7/8 viewer (`avctKVM.jar` served by 192.168.11.221).

Sources: CFR-decompiled jars in the scratchpad (`src6d/`, `src8d/`, string tables
`z6.tsv`/`z8.tsv`), obfuscated 2-D string tables decoded by reflection (`dump/Dump.java`
run against the jars), `javap -c` where CFR output was ambiguous, and unauthenticated
live probes (TCP + APCP + TLS ClientHello only, no login) against 192.168.11.221:5900/5901.
Class names below are the obfuscated ones in each jar; "6:" = iDRAC6 jar, "8:" = iDRAC8 jar.

## TL;DR — top differences

1. **The AVSP stream is the same protocol family (AVSP 2.3x, "BEEF"-framed packets, DVC/ASpeed-JPEG video).**
   Keyboard, mouse, video, keep-alive, power, login-response, shutdown-reason packets are byte-identical.
2. **APCP pre-handshake grew.** iDRAC8 (`8: com.avocent.d.a.a`) advertises **2.41** (6: 2.34) and, in the
   Dell build, sends a **68-byte type 0x0101** request instead of the 53-byte 0x0100 one; the server answers
   type **0x8101** with a 15-byte trailer (connection id). The old 53-byte request still works (probed).
   The server **echoes whatever version the client asks for** (2.34→2.34, 2.41→2.41, 1.0→2.33).
3. **Login packet layout depends on the negotiated minor version** (`8: com.avocent.c.d.ib.b()`):
   minor ≥ 41 → new **type 0x0104 "long" login** (u16 length/offset fields, variable-length user/password);
   otherwise the iDRAC6 layout (0x0102 with share byte). Advertise 2.34 and nothing changes; advertise 2.41
   and you must implement 0x0104. Passwords are still plaintext inside TLS; no hash, no challenge/response.
4. **Both KVM sockets go to KMPORT (5900).** The Dell controller (`8: com.avocent.app.c.l.m()`) passes
   KMPORT as *both* control and video port; `vport` is not used by the KVM path. 5901 is closed unauthenticated.
5. **New optional traffic on the control socket** — APCP-framed in-band messages ("APCP" magic: Request
   Session ID 0x0102, Request OTP 0x0103, RequestInventory 0x0201 JSON, Close 0x01FF, heartbeat 0x0400),
   "MGMT"-framed next-boot messages, a few new BEEF types (33027, 33285, 33539, 33809, 33813, 33845,
   40704), a slower keep-alive (10 s, no 1056 GetAvailableServers), and max packet length 32768. Only
   reachable when the client opts in (reconnect ≥ 1 / APCP version short ≥ 258), so the Go client can
   stay on the iDRAC6-style stream and merely tolerate the new types.

---

## 1. Launch parameters

### 1.1 Parser and mapping

iDRAC8: `com.avocent.app.c.o` ("CommandLineParser") — `b[][]` maps `name=value` args (case-insensitive
name) to property keys; unknown names are logged `Not an expected command line param::` and **ignored**;
values may be double-quoted; `path=` takes an `a:host,p:port,r:rip,c:chan,e:apcp,s:name` list (`c[][]`).
Defaults set before parsing (`o.a(n)`): `KMPORT=2068`, `VPORT=2068`, `USE_APCP=0`.
iDRAC6: `com.avocent.a.a.n` (`a[][]`), defaults `KMPORT=2068`, `VPORT=8192`, `USE_APCP=0`.

| arg (iDRAC8) | property | notes / iDRAC6 |
|---|---|---|
| `ip` | HOST | same |
| `kmport` | KMPORT | same; **used for control and video** (see 2.6) |
| `vport` | VPORT | same name; iDRAC8 KVM path ignores it (handed to the VM menu handler `app.l.a.c`, whose ctor drops it) |
| `user` | USERNAME | same |
| `passwd` | PASSWORD | same |
| `password` | PASSWORDLONG (+ sets `LONGPWINUSE=true`) | **new**; controller prefers PASSWORDLONG when LONGPWINUSE |
| `apcp` | USE_APCP | same |
| `sslv3` | SSLV3 | same (read, effectively unused) |
| `serverkey` | SERVER_KEY | same name; **not referenced anywhere in the iDRAC8 session/APCP code** |
| `rip`, `port`, `channel` | RIP, PORT, CHANNEL | same (appliance/RIP mode; Dell passes none) |
| `title` | TITLE | same |
| `helpurl` / `help` | HELPURL / HELP_URL | 6: `helpurl`→HELP_URL |
| `debug`, `chat`, `softkeys`, `statusbar`, `power`, `color`, `scaling`, `F1`, `custom`, `language`, `vm` (VM_STYLE, parsed as int), `menu_capture`, `miniMode/miniWidth/miniHeight`, `man_scale`, `filterkeys`, `remote`, `exclusive`, `mapfolder`, `oem`, `smfs`, `nograceful` | UI/feature toggles | 6 only had `debug`, `F2`→chat |
| `reconnect` | RECONNECT_VERSION | **new**, selects APCP variant: 0→plain 53-byte, 1→68-byte 0x0101, 2→68-byte heartbeat variant (`e.e.x()` maps 0/1/2 → PROPERTY_RECONNECT_VERSION -1/0/1; absent → Dell default "0" → 0x0101 variant) |
| `sslcertcapable`, `sslanoncapable`, `allcipher` | SSL_CERT_CAPABLE / SSL_ANON_CAPABLE / ALLCIPHER | **new**: set/clear APCP caps bits 8 / 4; ALLCIPHER stops the viewer restricting cipher suites |
| `sessionID`, `csrfToken`, `webuser` | SESSIONID, CSRFTOKEN, WEB_USER_NAME | 6 had sessionID/csrfToken too; used for HTTP calls back to the web UI (`app.c.b.a.b`, `app.c.i.c`), **not** in the KVM login |
| `tempunpw`, `aimpresent`, `vm_temp`, `nextboot`, `smartcard`, `notifyrelative`, `legacy`, `playbackUrl`/`dvcurl`, `dvrplayer`, `dvr`, `export`, `psttxt`, `scursor`, `saveSession`, `config`, `platform` | misc | `legacy=true` forces the 0x0100 login (`d.d.b.b(...)` → `ib.c()`); `tempunpw` (default 1) + CAPABILITY_VM_RANDOMNUM_LOGIN gate the OTP request for virtual media |

Not present in either parser: `vmprivilege`, `version`, `ST2`, `sessionkey`, `minwinheight`, `videoborder`
— they are silently ignored if the web UI's JNLP passes them. **User/password are still accepted as plain
launch args** (Dell's web UI hands the applet a temporary user/password pair; whether the pair is the
web-login credentials or a one-time account is a web-UI question — see `idrac8-web-api.md`; the viewer
does not care and just puts them in the login packet).

### 1.2 What the Dell controller actually uses (`8: com.avocent.app.c.l.m()`)

USERNAME, PASSWORD/PASSWORDLONG, HOST, KMPORT (`n4`), TARGET_NAME, SSLV3, RIP, CHANNEL (default 0),
SHARED_SESSION. It opens the session as `ab.a(HOST, n4, n4, trustMgr, ciphers, protocols, CHANNEL)`
(`8: com.avocent.a.b.w.a(...)` → `g()` → `d.d.b.c(J=KMPORT)`, `d.d.b.d(K=KMPORT)`) and logs in with
`ab.a(user, pass, REQUEST_NORMAL)` (or the RIP-connection variant when CAPABILITY_RIPCONNECTION, not Dell).
Dell capability flags are hard-set in `8: com.avocent.idrac.kvm.b.a()` (CAPABILITY_RECONNECT=ENABLE,
CAPABILITY_POWER, CAPABILITY_NEXT_BOOT, CAPABILITY_USER_LIST, CAPABILITY_CHAT, ... , `PROPERTY_RECONNECT_VERSION="0"`,
`PROPERTY_CIPHER_STRING="ALL"`).

## 2. Connection sequence

### 2.1 iDRAC6 baseline (`6: com.avocent.kvm.b.l`)

```
client → "APCP" u32 53 | u16 0x0100 | u16 0x0000 | u8 reqType | u8 2 | u8 34 | u8 0 | u32 caps=5 | u8 32 | rnd[32]
server → "APCP" u32 53 | u16 0x8100 | u16 x     | u8 maj | u8 min | u32 caps | u16 redirPort | u8 rndLen | 32 bytes
```
reqType 3 = control, 4 = video. caps&1 → clear text; caps&4 → TLS on the same socket (`SSLContext "SSL"`,
anon+RSA+DSS suites); redirPort≠0 → reconnect to that port first. Negotiated version taken from the reply.

### 2.2 iDRAC8 (`8: com.avocent.d.a.a`) — three request variants

Version advertised: **2.41** (`8: com.avocent.d.d.b` fields `W=2`, `X=41`), caps `fb=5`, adjusted by
`sslanoncapable`/`sslcertcapable` (bit 4 / bit 8; `d.a.a.a()`). Which variant is sent depends on
`CAPABILITY_RECONNECT` (Dell: ENABLE) and `PROPERTY_RECONNECT_VERSION` (`d.a.a.a(byte,Socket)`, verified with javap):

| PROPERTY_RECONNECT_VERSION | request | reply parser |
|---|---|---|
| -1 (`reconnect=0`) or CAPABILITY_RECONNECT disabled | **A** `a(byte,byte[])`: 53 bytes, `u16 0x0100 | u16 0x0104` (second short now 0x0104 instead of 0) | `a(Socket,0)` |
| **0 (Dell default, `reconnect=1`)** | **B** `a(byte,byte[],int)`: **68 bytes**, `u16 0x0101 | u16 0x0000`, then the 53-byte body, then `u8 0 | u16 0 | u32 0 | u32 reconnectId(0) | u32 0` | `a(Socket,101)` |
| 1 (`reconnect=2`) | **C** `a(byte,byte[],boolean,int)`: 68 bytes, `u16 0x0100 | u16 0x0104`, caps forced to **0x605**, body, then `u16 hbInterval(PROPERTY_HBTIMEOUT_INTERVAL, 0) | u8 0 | u16 180 | u16 30 | u32 reconnectId | u32 0` | `a(Socket,true)` |

Reply parsing (all variants): header as iDRAC6, but the type may be 0x8100 **or 0x8101**, and the second u16
is an "APCP version" (`d.d.b.e(int)` → `L()`, default 256). caps bit 0x200 → RECONNECT_SUPPORT. Trailer:
* 0x8101 with version 0 (variant B): `u8 echoedReqType | u16 0 | u32 connectionId (0 → abort "connection closed due to 0 con id") | u32 | u32`.
* variant C: `u8 echoedReqType | u16 serverHbTimeout | u16 180 | u8 packetUpdateInterval | u8 connIdNonZero | u32 | u32`.
  If serverHbTimeout>0 the client starts `8: com.avocent.d.a.d`: every 10 s writes `"APCP" u32 12 u16 0x0400 u16 0`
  (APCP heartbeat) on the control socket; packetUpdateInterval+2 becomes the link-interrupted timeout.
* caps&1 clear, caps&4 TLS (trust manager `d.a.c`), **caps&8 TLS with certificate validation** (`d.a.b`). Default
  Java suite list is the old SSL_RSA_*/SSL_DHE_DSS_* set unless `allcipher=true`.
The reply's major/minor bytes overwrite W/X (`d.a.a` → `d.d.b.d(int,int)`), so **the negotiated version is what the
server echoes**.

### 2.3 Live probe results (192.168.11.221:5900, unauthenticated)

| request | reply |
|---|---|
| 53 B, type 3, 2.41, caps 5 | `APCP 0x35 0x8100 ver=0 2.41 caps=0x4 port=0 rndlen=0` (53 B) |
| 53 B, type 3, 2.34, caps 5 | same, echoes **2.34** |
| 53 B, type 3, 1.0, caps 5 | echoes **2.33** (server floor) |
| 53 B, type 4 (video), 2.41 | same as control |
| 53 B, caps 13 (=5|8) | caps=0x4 still (no cert-mode bit) |
| 68 B variant B (0x0101) | `0x8101 ver=0 2.41 caps=0x4` + trailer `03 0000 00000001 00000000 00000000` (68 B) — connectionId 1 |
| 68 B variant C (0x0100/0x0104) | `0x8100 ver=0x0104(260) 2.41 caps=0x4` + trailer `03 001e 00b4 0a 01 00000000 00000000` — HB timeout 30 s, update interval 10 |
| after any reply: TLS ClientHello | **TLS 1.2, ECDHE-RSA-AES256-GCM-SHA384**, server cert 1045 B DER (same cert on both sockets); server then silent until the client sends the login |
| 5901 TCP | connection refused (virtual media not listening unauthenticated) |

Conclusions: the iDRAC6 53-byte request is fully accepted; caps=4 → TLS mandatory; there is no redirect port;
the random bytes are echoed as length 0 (not used). Advertising 2.34 keeps the iDRAC6 login layout (see 3).

### 2.4 Protocol version numbers seen in the iDRAC8 jar
* AVSP session request: 2.41 (`d.d.b`), fallback constants 2.37 (`d.d.b.pb()` server-macros request) and 2.33.
* `8: com.avocent.c.f.d` RequestInventory JSON advertises `APCP 1.3, AVSP 2.40, MGMT 1.00` (only sent when `L()>=260`).
* APCP "version short" thresholds: 258 → Request Session ID (`c.f.f`) / OTP; 260 → RequestInventory.

### 2.5 TLS
Same as iDRAC6: upgrade the *same* TCP socket right after the APCP reply when caps&4. Go: `tls.Client(conn, &tls.Config{InsecureSkipVerify:true or pin})`,
TLS 1.2 works. `SSLV3`/`SERVER_KEY` launch params are not used by the iDRAC8 connection code.

### 2.6 Sockets and ports
* Control socket: `HOST:KMPORT`, APCP reqType 3, then AVSP.
* Video socket: opened after login success (`d.g.a.b.a(c,boolean)` → `d.d.b.D()`) to `HOST:K` where **K = KMPORT** in the Dell
  build (`app.c.l.m()` passes `n4,n4`); APCP reqType 4 with the *same* variant/version, TLS, then Video Channel Auth (see 4.1).
  iDRAC6 used VPORT for this socket — with Dell's JNLP both are 5900 anyway.
* Virtual media: separate client (`com.avocent.vm.*`, native `avctVM` library, `VirtualMediaMainController.activateVMedia()` also passes KMPORT);
  not part of the KVM stream. Port 5901 refuses unauthenticated connections.
* Reconnect path (`d.a.a.a(int)`, `reconType` 101/102): re-sends the 68-byte request with the saved `reconnectId`
  to get a new socket without re-login; optional.

## 3. Login packet

### 3.1 iDRAC6 (`6: com.avocent.kvm.b.a.ab`, sent by `kvm.b.r.a(String,int,boolean)`)
```
BEEF | u16 type (0x0100, or 0x0102 when negotiated major==0||>=2) | u16 total (216 / 217)
u8 ulen | user[96] (UTF-8, NUL padded) | u8 plen | pass[96] | rip[8] | u8 o (0) | u8 p (=PORT launch param, default 1; a.a.j)
| u32 clientRandom (Math.random()*1e7) | [u8 shareMode 0/1 only for 0x0102]
```

### 3.2 iDRAC8 (`8: com.avocent.c.d.ib`, built in `d.d.b.a(int,int,boolean)` / `d.d.b.b(...)`)
`new ib(W, X)` picks the layout from the **negotiated** version (`ib.b()`):

| `b()` | condition | type | layout |
|---|---|---|---|
| 1 | major == 1 | 0x0100 | iDRAC6 layout, no share byte (`t=false`) |
| 2 | major==0 or ≥2, minor < 41 | **0x0102** | iDRAC6 layout **with** share byte — identical to iDRAC6 fw 2.92 |
| 3 | major ≥ 2 **and minor ≥ 41** | **0x0104** | new "long" layout below |

Type 0x0104 (`ib.o()` else-branch, `ib.a()` length = 31 + ulen + plen incl. header):
```
BEEF | u16 0x0104 | u16 total
u16 ulen | u16 uoff (= total - ulen - plen = 31) | u16 plen | u16 poff (= total - plen)
rip[8] | u8 p | u8 q | u32 clientRandom | u8 shareMode | user[ulen] | pass[plen]     (strings not padded)
```
Field values in the Dell path (`d.d.b.a(int,int,boolean)`, verified with javap): user/pass from USERNAME/PASSWORD(LONG);
`rip` = 8 zero bytes; **`p` = KMPORT & 0xFF** (the controller's port int written as one byte; iDRAC6 wrote PORT=1 here —
almost certainly ignored by iDRAC); `q` = CHANNEL (0); `clientRandom` = `(int)(Math.random()*1e7)` (`d.d.b` field `x`,
also stored as CLIENT_RANDOM); shareMode = 1 for REQUEST_SHARED else 0. Overrides: `legacy=true` → `ib.c()` forces 0x0100;
CAPABILITY_RIPCONNECTION (appliance mode) → `ib.d()` forces 0x0102 and caps minor to 40.
`8: com.avocent.c.d.x` type **0x0101 "PreemptionUserLoginReq"** = 0x0100 layout without share byte, only in RIP mode (`n4==257`).

**No hashing, no challenge/response.** Neither the APCP random bytes nor SERVER_KEY enter the login; the only "random" is the
client's u32, echoed back later for the video-channel auth. Credentials are protected solely by TLS.

### 3.3 Login responses (unchanged layout, one new field)
* **33536 / 0x8300 User Login Response** (`6: kvm.b.a.vb`, `8: com.avocent.d.d.a.c`, 105-byte payload):
  `u8 status | u8 | u8 | u8 flags (bit0, bit1=viewOnly) | u32 applianceRandom | u8 nameLen | name[96]`.
  Preemption variant 33541: `u8 status | u8 preemptSharing | u8 timeout | u8 | u32 applianceRandom | u16 flags | u8 nameLen | name`.
  Status codes (`8: com.avocent.a.b.u`): 0 OK, 1 invalid user, 2 invalid password, 3 access denied, 4 in use,
  **6 in use but pre-emptable (new)**, 8 all channels in use, 15 sharing denied (9 also sharing denied in 8), others → LOGIN_FAILED.
  `applianceRandom` → `d.d.b.f(int)` (APPLIANCE_RANDOM) — needed for the video socket.
* **33280 / 0x8200 Input Resolution Response** (`6: lb` 4-byte payload w,h; `8: c.d.tb`): `u16 w | u16 h | u16 platformTag`.
  iDRAC8 decodes the tag: `'A''S'` (0x4153) → PLATFORM_TYPE 2 (ASpeed = iDRAC7/8), `'D''5'` → 1, `'P''3'` → 3 (also triggers
  SetVideoParameters + RLE video, see 4.4), `'P''4'` → 5. 33286 is dispatched to the same class.
* 33538 Protocol Version (`u8 major, u8 minor`), 33824 User Privilege Parameters, 33808 SharedUserResponse — unchanged.

### 3.4 What the iDRAC8 viewer sends right after login success (`8: com.avocent.d.g.a.b.a(c,boolean)`)
1. start keep-alive thread (`d.d.b.B()` → `d.d.n`: BEEF 1024 every 10 s);
2. open the video socket (`d.d.b.D()`), Video Channel Auth, wait for 132 Video Connect Status;
3. `d.d.b.C()`: Video Enable 782 (`01 01`), Set Display Area 770 (1024x768), SetScaleMode1to1 772 — same three as `6: kvm.b.r.y()`;
4. optional, version-gated: server-macros APCP request `d.d.h` (only if negotiated ≥ 2.37), next-boot `c.f.c` (CAPABILITY_NEXT_BOOT),
   RequestInventory `c.f.d` (if `L()>=260`), Request Session ID `c.f.f` / OTP `c.f.e` (reconnect-capable APCP only),
   screen refresh `c.d.s` when view-only. None of these are required for video to flow.

## 4. Video (details from the video comparison; `8: com.avocent.c.d.*`, `d.g.a.a`, `kvm.a.*`)

### 4.1 Video channel handshake — identical
Second connection (see 2.6), APCP reqType 4, TLS, then the client sends **Video Channel Auth** type 1 (`6: kvm.b.a.ac`,
`8: c.d.ic`), 16 bytes with the *zero-prefixed* header used by this family (`6: zb.a()`, `8: hc.n()`):
`00 00 00 00 | 01 | 01 | u16 0x0010 | u32 clientRandom | u32 applianceRandom`. Then wait for **132 Video Connect Status**
(`6: xb`, `8: fc`; payload[0]==0 → OK). Video socket SO_TIMEOUT: 6 = 60 s, 8 = 30 s (`PROP_VIDEO_SESSION_TIMEOUT`).

### 4.2 Framing / receive loop (`6: kvm.b.a.a`, `8: d.g.a.a` "PacketReceiver")
* 8-byte header; `"APCP"` prefix = APCP control packet (u32 len at [4..8], u16 type at payload start); 8 also recognises
  `"ASCP"` (smart-card, `d.b.*`) and `"AVPT"`/`"MGMT"`/`"CSCO"` families. Otherwise type = `hdr[5]` on the video socket,
  `hdr[4]<<8|hdr[5]` on the control socket, length = `hdr[6..7]` incl. header.
* **Limits: 6 rejects len<16 or >15000; 8 rejects len<8 or >32768** (`d.g.a.a` lines 137-140). Raise the Go limit.
* Sub-types: 128 stream reset, 129 DVC15, 130 DVC7, 131 DVC7Gray, 138 DVC23 (decoders `6: kvm.d.c/f/e/d` ≡ `8: kvm.a.a.e/h/g/f`,
  same constants), 133 VideoStopped, 134 ASpeed JPEG (`6: kvm.b.a.gb`, `8: kvm.a.c.g`; identical 12-byte sub-header),
  135 Text Mode, 136 Colour Palette, 137 Font Table. DVC packet payload header unchanged (`[4..5],[6..7] BE16, [8] flags
  bit0 BOF bit1 EOF, [10..11] checksum on EOF, data from 12`).
* **New 139 / 0x860B "RLE Video"** (`8: c.d.cc`, decoder `kvm.a.d.a`) — only produced by "P3"-platform servers after the client
  sends SetVideoParameters; an ASpeed iDRAC8 never sends it.

### 4.3 Codec / version
Same DVC 7/7-gray/15/23-bit family and ASpeed JPEG in both; no 2.33/2.34/2.40 gating inside the decoders. JPEG/MJPG classes
(`a.a.a.b/c/d`) are AVI/MJPEG *export writers*, not wire encodings. SetDVCColorDepth 1027 (`6: k`, `8: p`) identical
(`[0]=disable,[1]=depth,[2],[3]`, 8 adds `[4]` secure-card flag); both auto-send it when the incoming sub-type changes.
No 16-bit DVC mode; only the RLE decoder has a 16-bpp raw path.

### 4.4 Ack / flow control / new response types
* No per-frame acks in either; flow control = TCP. Keep-alive: 6 sends 1024 KeepAlive **and** 1056 GetAvailableServers every 2 s
  (`6: kvm.b.x`); 8 sends only 1024 every 10 s (`8: d.d.n`); 1056 does not exist in 8.
* 33281 Display Resolution Response, 33282 VideoSetupData, 33284 DVCColorModeResponse unchanged.
* New: 33285 DisplayResolutionInvalidResponse (`u16 w,u16 h`), 33813 ApplianceOptionsResponse (TLV; tag 2 = supported
  resolution list), **784 / 0x0310 SetVideoParameters** (`8: c.d.fb`, 28-byte payload `"P3" | u16 w | u16 h | ... | [22]=15 | [23]=1 | [26]=1`)
  sent only when the 33280 platform tag is "P3"; 774 AutoVideoAdjust, 775 TestPattern (client→server, optional).

## 5. Keyboard / mouse — byte-identical

| type | 6 | 8 | payload |
|---|---|---|---|
| 512 / 0x0200 Keyboard | `kvm.b.a.f` | `c.d.f` | `[0]=0, [1]=0 down / 1 up, [2..3]=USB HID usage BE16, [4..7]=0` |
| 516 / 0x0204 Keyboard LED request | `g` | `c.d.g` | zeros |
| 520 / 0x0208 Focus control | `d` | `c.d.e` | `[0]=focused` |
| 513 / 0x0201 Mouse absolute | `gc` | `c.d.rc` | `[0]=0, [1]=buttons (1 L,2 R,4 M), [2..3]=x, [4..5]=y, [6..7]=wheel s16` |
| 521 / 0x0209 Mouse relative | `hc` | `c.d.sc` | same layout, deltas |
| 514 / 0x0202 MouseOrigin, 522 / 0x020A SetMouseAccel | `h`, `t` | `c.d.h`, `c.d.ab` | zeros / `[0]=accel` |
| 33024 Keyboard LED status | `mb` | `c.d.ub` | `[0]` bits 1 Scroll, 2 Num, 4 Caps |
| 33025 / 33026 mouse ack / accel resp | `pb`/`ob` | `yb`/`xb` | `[0]` |

Scancodes are **USB HID usage IDs** in both (same 948-entry table: `6: kvm.c.c.d`, `8: b.d.b.d`; 8 adds per-locale sub-tables).
Absolute mouse (513) is the default in both. New in 8 and only used when launch param `notifyrelative=true`: 515 / 0x0203 Mouse
Reset (zeros), **523 / 0x020B MouseAbsSyncRequest** (`[0]=1,[1]=enable`) and **33027 / 0x8103 MouseAbsSyncResponse**
(`[0]=absEnabled,[1]=synced`). Ignore for Dell launches; tolerate 33027.

## 6. Session / state packets

Unchanged (same id, same payload): 770 Set Display Area, 782 Video Enable, 772 Scale 1:1, 800 server select, 1024 KeepAlive,
1027 colour depth, 1043 SharingResponse, **1060 / 0x0424 SetPowerState** (`6: kvm.b.a.dc` → `8: com.avocent.d.d.a.e`, moved package,
payload `u16 len(serverId) | serverId | u8 state` 1 On, 2 Off, 3 Cycle, 4 Reboot, 5 Graceful; serverId from 33840 AvailableServerNames),
1063 RequestCredentials, 33537 User Disconnect Pending, 33538 Protocol Version, 33540, 33793 SharingRequest, 33808 SharedUserResponse
(the "user list"; TLV form when version ≥ 2.34), 33824, 33840/33841/33842 server list/status, 33844, 0xFFFF NoOp.

New in iDRAC8 (BEEF-framed unless noted):
* 774 AutoVideoAdjust, 775 TestPattern, 784 SetVideoParameters (see 4).
* 1040 SharingResponseWithoutUserID, 1041 SharedUserRequest, 1042 SessionPreemptResponseLegacy, **1044 ExclusiveModeReq** (`[0]=on`) →
  33809 ExclusiveModeResp (`[0]=granted`), 1046 PreemptionResponse.
* **1064 Chat send** (256-byte string) / **33845 Chat receive** (NUL-terminated text + trailing u32 sessionId) — chat via the console.
* **33539 UserDisconnectPendingTimeoutMessage** (`u8 reason | u8 status | u8 seconds | u8 nameLen | name[96] | u8 addrLen | addr[40] | u8 actionTimeout`).
* 33813 ApplianceOptionsResponse, 33285 DisplayResolutionInvalidResponse.
* **40704 / 0x9F00 CustomServerMacroMessage** (`c.d.c`, macro text; may carry `"CSCO"` magic) after the in-band APCP session
  request `8: d.d.h` (type 159; only when negotiated ≥ 2.37 and SMFS).
* **Next boot ("boot once")** — `"MGMT"` family: client `c.f.c` MgmtSessionRequest (`APCP | u32 68 | u16 0x0100 | [12]=0x9E` + 50 zero
  bytes) → server 21845 MgmtSessionResponse, 40464 / 0x9E10 MgmtBootDeviceStruct (`c.f.m`: 2 bools, 3 bytes, 16×{enabled,deviceId,flag,order},
  n×{a,b,flag,order,name[80]}) → client 40592 / 0x9E90 MgmtBootDeviceSelect (same struct) → 40465 MgmtBootDeviceResult `[0]=status`.
* APCP-framed control (only when the reconnect-capable APCP reply is negotiated): 0x0102 Request Session ID (`c.f.f`), 0x0103 Request
  OTP (`c.f.e`, 48 B) → 0x1FA7 Response OTP (`c.f.i`: two length-prefixed strings = temporary VM credentials), 0x0201 RequestInventory
  (`c.f.d`) → 0x2009 InventoryResponseMsg (`c.f.g`, JSON: `usb_reset_available`, `video.rvas_compression`, `OEM.identifier_string`),
  0x01FF Close Connection (`c.f.a`: `APCP | u32 16 | u16 0x01FF | u16 0 | u32 0 | u32 connId`, sent by `d.d.b.ob()` on exit),
  0x0400 heartbeat (`d.a.d`).
* Screenshot / capture-to-file is client-side only (no packet). Virtual media is **not** tunnelled over the console.
* Absent in 8: 1056 GetAvailableServers, 1026 SetVideoTransmitLimit.

Shutdown reason codes (33537 payload[0]) are identical: 0 ADMINISTRATOR_DISCONNECT, 1 SESSION_IDLE_TIMEOUT_EXCEEDED,
2 APPLIANCE_REBOOT_PENDING, 3 DSRIQ_UPGRADE_PENDING, 4 CHANNEL_PREEMPTED_BY_LOCAL_USER, 5 LAST_ACTIVE_USER_HAS_DISCONNECTED,
6 PRIMARY_USER_GONE_TO_EXCLUSIVE_MODE (`6: idrac.kvm.d`, `8: a.b.u` → `a.b.r`); 8 adds internal SHUTDOWN_CONNECTION/CANCELLED for
reconnect failures and the richer 33539 message.

## 7. HTML5 console
Nothing. No `html5`, `websocket`, `vmprivilege`, `ST2` or `sessionkey` strings anywhere in the iDRAC8 jar or its string tables;
the only web-session artefacts are the `sessionID`/`csrfToken`/`webuser` launch params used for HTTP calls back to the web UI.

---

## 8. Checklist — to support iDRAC8 in the Go client

Minimal (stay on the iDRAC6-style stream; everything below has been probed or read from the jar):

1. **Connect both sockets to KMPORT (5900)**; do not require `vport` (`8: com.avocent.app.c.l.m()` passes KMPORT twice; `d.d.b.D()` opens video on `K`).
2. **Keep the 53-byte APCP request, but advertise 2.34** (or accept whatever the server echoes and branch on it). If you send 2.41 you must
   implement the 0x0104 login (`8: com.avocent.c.d.ib.b()/o()`). Accept reply type 0x8101 as well as 0x8100 (`8: com.avocent.d.a.a.a(Socket,int)`),
   parse the second u16 as APCP version and read the 15-byte trailer when the reply is 68 bytes.
3. **Login**: with 2.34 use the existing 0x0102 layout (217 bytes, share byte). Put `KMPORT&0xFF` or 1 in the "port" byte (both accepted in practice;
   iDRAC8 viewer sends `KMPORT&0xFF`, `d.d.b.a(int,int,boolean)`). Optionally implement the 0x0104 long form for ≥ 2.41.
4. **Login response**: read the 2-byte platform tag at payload[4..5] of 33280 (`8: c.d.tb`); if not "AS", expect surprises (P3 → RLE video, see 4.4).
   Add status 6 (in use, pre-emptable) to the 33536 status table (`8: com.avocent.a.b.u`).
5. **Video**: unchanged handshake (`8: c.d.ic`, 16 bytes zero-prefixed header) and codecs. Raise the receiver's max packet length to 32768 and
   min to 8 (`8: d.g.a.a`). Recognise `"APCP"`/`"MGMT"`/`"ASCP"`/`"CSCO"` magics on the control socket and skip them by their own length field
   (`"APCP"`/`"ASCP"`: u32 length at [4..8]; `"MGMT"`: BEEF-style u16 at [6..7]).
6. **Keep-alive**: send 1024 every ≤ 10 s; 1056 GetAvailableServers is unknown to iDRAC8 (`8: d.d.n`; not in `d.g.a.a`).
7. **Tolerate new server→client types**: 33027, 33285, 33539, 33809, 33813, 33845, 40704, 21845, 40464, 40465, 0x2009 (`8: d.g.a.a` factory table).
8. **TLS**: TLS 1.2 with modern suites is accepted; pin or skip-verify the 1045-byte server cert (`8: com.avocent.d.a.a`, `d.a.b/c`).
9. Launch params: read `password=` (PASSWORDLONG) as an alias of `passwd=`, and ignore unknown params (`8: com.avocent.app.c.o`).

Optional features (all iDRAC8-only, reachable only with the extended APCP or version gates):

10. Reconnect-capable APCP: 68-byte variant B/C, connection id, 10-s APCP heartbeat `"APCP" 0c 00000400 0000` (`8: d.a.a`, `d.a.d`), 0x01FF close (`c.f.a`),
    Request Session ID / OTP / Inventory (`c.f.f`, `c.f.e`, `c.f.d`).
11. Next-boot over the console (`c.f.c` → 40464 → `c.f.b` → 40465), chat (`c.d.m`/`c.d.n`), exclusive mode (`c.d.d` 1044 / 33809),
    absolute-mouse sync (`c.d.v` 523 / `c.d.wb` 33027), RLE video for P3 platforms (`c.d.fb`, `c.d.cc`, `kvm.a.d.a`).
12. Power control is unchanged (1060, `8: com.avocent.d.d.a.e`) — needs the server id from 33840 first.
