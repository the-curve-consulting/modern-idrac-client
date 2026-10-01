# Dell iDRAC8 HTTP API Reference (for a Go client)

Target devices: `https://192.168.11.221` and `.222/.223/.224/.226`.

**Generation / firmware (verified live, unauthenticated):**

| Fact | Value | Source |
|---|---|---|
| Generation | **iDRAC8 / 13G PowerEdge** | `GET /cgi-bin/discover` → `<ENDPOINTTYPE>iDRAC8</ENDPOINTTYPE>`; `GET /data?get=prodServerGen` → `<prodServerGen>13G</prodServerGen>` |
| Firmware | **2.86.86.86 (Build 06)** (short `2.86.06`) — same on all five nodes | `GET /session?aimGetProp=fwVersionFull` → `2.86.86.86(Build06)` |
| License | Enterprise | `GET /data?get=prodClassName` → `Enterprise` |
| Redfish | RedfishVersion **1.4.0**, ServiceRoot **v1_3_0** | `GET /redfish/v1` |
| Service tag / MAC | in ServiceRoot `Oem.Dell` (e.g. `698R7J2`, `10:98:36:B1:15:FD`) | `GET /redfish/v1` |

All five hosts are identical: iDRAC8 13G, firmware 2.86.86.86, Redfish 1.4.0. Service tags: .221=`698R7J2`, .222=`97BGZG2`, .223=`7C019F2`, .224=`2RXFZ42`, .226=`8950KF2`.

**Webserver behaviour that affects a Go client:**
- Static assets are stored **pre-gzipped**. You MUST send `Accept-Encoding: gzip` (curl `--compressed`) to fetch HTML/JS/CSS, otherwise `Content-Type: application/x-gzip` raw bytes come back.
- The server issues `Strict-Transport-Security`, `X-Frame-Options: SAMEORIGIN`. TLS certificate is self-signed → use `InsecureSkipVerify` (curl `-sk`).
- Unauthenticated requests to protected UI pages **302-redirect to `/start.html`** (or `/login.html?console` for console pages); protected `/data`, `/redfish`, `/wsman` return **401**. A handful of endpoints are intentionally unauthenticated (see below).
- `ETag`/`Date` headers reflect the iDRAC clock (was set to BST here).

---

## Three entry points at a glance

1. **Legacy web-UI API** (`/data`, `/session`, `/sysmgmt/*`, `/cgi-bin/*`) — the AJAX API the built-in GUI uses. Same lineage as iDRAC6/7. XML and JSON. Form-login + `ST2` anti-CSRF header.
2. **Virtual console** — Java (Avocent viewer via `viewer.jnlp`) and HTML5, both over TCP **5900** with a one-time credential token.
3. **Redfish** (`/redfish/v1`) — the modern, documented REST API. Recommended for everything except the console.

---

# A. Legacy web-UI API

The GUI is served by an Appweb server. JS confirmed by downloading unauthenticated: `/login.html`, `/start.html`, `/functions.js`, `/js/prototype.js`, `/js/Clarity.js`, `/public/about.html`, `/public/support.html`, `/help/en/*`. All `/js/*.js` app scripts and every functional `.html` page require an authenticated session (302 → `/start.html`).

## A.1 Unauthenticated endpoints (no login needed)

| Endpoint | Method | Response | Notes |
|---|---|---|---|
| `/cgi-bin/discover` | GET | `text/xml` | `<DISCOVER><RESP><RC>0x0</RC><ENDPOINTTYPE>iDRAC8</ENDPOINTTYPE><ENDPOINTVER>1.00</ENDPOINTVER><PROTOCOLTYPE>HTTPS</PROTOCOLTYPE><PROTOCOLVER>2.0</PROTOCOLVER></RESP></DISCOVER>` — **best unauth fingerprint for generation** |
| `/data?get=prodServerGen` | GET/POST | `text/xml` | `<prodServerGen>13G</prodServerGen><Manufacturer>Dell Inc.</Manufacturer><isDellBranded>1</isDellBranded><status>ok</status>` |
| `/data?get=prodClassName` | GET/POST | `text/xml` | `<prodClassName>Enterprise</prodClassName><status>ok</status>` |
| `/session?aimGetProp=fwVersion` | GET | `application/json` | `{"aimGetProp":{"fwVersion":"2.86.06","status":"OK"}}` |
| `/session?aimGetProp=fwVersionFull` | GET | JSON | `2.86.86.86(Build06)` |
| `/session?aimGetProp=hostname` | GET | JSON | e.g. `idrac-698R7J2` |
| `/session?aimGetBoolProp=pam_bool_sso_enabled` | GET | JSON | `{"aimGetBoolProp":{"pam_bool_sso_enabled":"false","status":"OK"}}` |
| `/session?aimGetIntProp=scl_int_enabled` | GET | JSON | `{"aimGetIntProp":{"scl_int_enabled":0,"status":"OK"}}` |
| `/session?aimGetIntProp=gui_int_control_basegui` | GET | JSON | `1` |
| `/session?getDomainNames` | GET | JSON | AD/LDAP domain list (`{"getDomainNames":{"status":"OK"}}` when none) |
| `/cgi-bin/login` | GET | `text/xml` | `<LOGIN><RESP><RC>0x3009</RC>...<DEFCRED>2</DEFCRED></RESP></LOGIN>` (remote-racadm handshake; `0x3009` = no/invalid session) |
| `/cgi-bin/logout` | GET | `text/xml` | `<LOGOUT><RESP><RC>0x0</RC><SID>0</SID></RESP></LOGOUT>` |

> **Important quirk:** `/session?aimGetProp=` and `?aimGetIntProp=` only return data unauthenticated for a **whitelisted subset of keys** (those the login page needs). Whitelisted keys verified live: `fwVersion`, `fwVersionFull`, `hostname`, `scl_int_enabled`, `gui_int_control_basegui`, plus the bool/domain queries above. Any **other** key (`gui_str_title_bar`, `OEMHostName`, `sysDesc`, `gui_int_title_bar_num`, `gui_int_control_dirService`, `pam_int_ldap_enable_mode`, `ameastatus_bool_amea_present`, …) **302-redirects to `/start.html`** until authenticated. Requesting a comma-list containing any non-whitelisted key redirects the whole request. So for a Go client: only the single whitelisted keys are usable pre-login; everything else needs a session.

`/cgi-bin/discover`, `/cgi-bin/login`, `/cgi-bin/exec`, `/cgi-bin/logout` are the **remote-racadm transport** (the `racadm -r` protocol). `/cgi-bin/exec` without a valid session returns `CMDOUTPUT>ERROR: Session is not valid.` A Go client generally should not reimplement this; use Redfish or SSH racadm instead.

## A.2 Login flow (form login → session cookie + ST2 token)

Verified live and from `/login.html` + `/functions.js`.

**Step 1 (what the GUI does first — optional):** `GET /data/logout` (or `POST`) to clear any prior session → `303` to `/login.html`, drops the cookie. Safe to skip on a fresh client.

**Step 2 — login:**
```
POST /data/login          (or  POST /data/login?console  to launch straight into vConsole)
Content-Type: application/x-www-form-urlencoded
Body: user=<urlencoded>&password=<urlencoded>
```
- Username/password are URL-encoded with `encodeURIComponent`, except the section-sign `§` (char 167) which is `escape`d (German-keyboard workaround). Max lengths: user 512, password 254.
- For an AD/LDAP domain, the GUI appends `@<domain>` to the username unless it already contains `@`, `\`, or `/`.

**Response** (`text/xml`, verified with bogus `x`/`x` — exactly one attempt made, never guess real passwords, lockout risk):
```xml
<?xml version="1.0" encoding="UTF-8"?>
<root>
  <status>ok</status>
  <authResult>1</authResult>
  <blockingTime>0</blockingTime>
  <forwardUrl>index.html</forwardUrl>
  <errorMsg></errorMsg>
</root>
```
- **Set-Cookie:** `-http-session-=<id>; path=/; secure; httponly` (cookie name is literally `-http-session-`).
- `<authResult>` codes (from `errMsg[]` in login.html): `0` = success, `1` = verification failed (bad credentials), `2` = missing username, `3` = missing password, `4` = insufficient privilege, `5` = session count exceeded, `99` = generic failure.
- On `authResult==1` the `<blockingTime>` (seconds) is the lockout backoff — **respect it**; repeated failures raise it.
- On success (`authResult==0`), `<forwardUrl>` is `index.html?ST1=<tok1>&ST2=<tok2>` (tokens are embedded in the forward URL query string). The two tokens (`ST1`, `ST2`) are the session's anti-CSRF tokens.

**Step 3 — use the ST2 token on every subsequent request.** From `/functions.js`:
- `top.TOKEN_NAME` = the header name (`"ST2"`), `top.TOKEN_VALUE` = the ST2 value parsed from `forwardUrl`.
- `loadXMLDocument()` sends every request as **POST** with `Content-Type: application/x-www-form-urlencoded` and, if `TOKEN_VALUE.length >= 8`, adds request header **`ST2: <token>`**.
- Auto-refresh requests additionally send header `idracAutoRefresh: 1`.
- Example verified in JS (`getExtHlthStatus`): `GET /sysmgmt/2016/server/extended_health` with `requestHeaders: ["ST2", <token>, "idracAutoRefresh", "1"]`.

So the Go client session state = **cookie `-http-session-`** + **header `ST2: <token2>`**. `ST1` is used for the console JNLP URL (section B). A `401` on any `/data`/`/sysmgmt` call means the session expired → re-login.

**Logout:** `GET /data/logout` → `303` → `/login.html`.

## A.3 `/data?get=` and `/data?set=` (XML key/value API)

All require an authenticated session + `ST2` header (else `401`). The GUI issues these as POST.

- **Read:** `POST /data?get=key1,key2,...` → `text/xml` `<root><key1>…</key1>…<status>ok</status></root>`. Parsing: the GUI walks the requested key names and reads `getElementsByTagName(key)`; long values may be split across multiple text child-nodes (must be concatenated).
- **Write:** the GUI builds a POST body of `key:value,key:value` pairs (see `formatPostRequest`, separator `:` between key and value, `,` between pairs) to a `set` URL. Values are run through `encodeSetValue`.
- Response envelope always carries `<status>ok</status>` on success; `waitWithCallback` treats non-`ok` status as failure and reads `<message>`.

Because every functional page is auth-gated, the exhaustive live key list could not be enumerated without logging in. Keys **confirmed** from unauthenticated responses / JS: `prodServerGen`, `prodClassName`, `Manufacturer`, `isDellBranded`, `fwVersion`. The `aimGetProp`/`aimGetIntProp` names in A.4 are the authenticated equivalents the GUI actually relies on. **Recommendation: do not build a Go client on `/data?get=` scraping** — the keyset is large, undocumented, and version-specific. Use Redfish (section C) for inventory/power/logs and only fall back to `/data` for the few iDRAC-only settings Redfish 1.4 doesn't expose.

## A.4 `/session?aim*` (JSON property API)

`GET /session?aimGetProp=<comma-keys>` / `?aimGetIntProp=` / `?aimGetBoolProp=` → JSON `{ "aimGetProp": { "<key>": "<val>", …, "status": "OK" } }`. Response header `X_Language` carries the UI locale (e.g. `en`). Keys used by the GUI login/summary code:
- `aimGetProp`: `hostname`, `gui_str_title_bar`, `OEMHostName` (service tag), `fwVersion`, `fwVersionFull`, `sysDesc`.
- `aimGetIntProp`: `scl_int_enabled` (smart-card), `gui_int_control_basegui`, `gui_int_title_bar_num`, `gui_int_control_dirService`, `pam_int_ldap_enable_mode`, `ameastatus_bool_amea_present`.
- `aimGetBoolProp`: `pam_bool_sso_enabled`.
- `/session?getDomainNames`, `/session?getSrvPrcName`.

(Again: only the whitelisted subset works pre-auth; the rest need the session.)

## A.5 `/sysmgmt/*` (JSON REST used by the newer GUI pages)

All auth-gated (302 → start.html when unauthenticated; they are true endpoints, not files). They take the **`ST2`** header like `/data`. The year in the path is an API-version namespace, not a date. **Confirmed live from `/functions.js`:**

| Endpoint | Method | Purpose |
|---|---|---|
| `/sysmgmt/2016/server/extended_health` | GET | Aggregate health status → JSON `{"healthStatus":[…]}`; sent with headers `ST2` + `idracAutoRefresh: 1` |

Other `/sysmgmt/YYYY/...` paths exist in this firmware family (the GUI's power, sensor, inventory, SEL/LC log, user, network, virtual-media, vconsole and job pages use them) but their exact spellings/shapes are behind auth and are **not stable across firmware**. Documented/observed patterns in the iDRAC7/8 lineage (treat as hints, verify against a logged-in session before coding):
- `/sysmgmt/2015/bmc/info`, `/sysmgmt/2015/bmc/session`
- `/sysmgmt/2012/server/inventory/hardware`, `/sysmgmt/2012/server/inventory/software`
- `/sysmgmt/2012/server/configgroup/<group>` (e.g. `iDRAC.Users`)
- `/sysmgmt/2012/server/power`, `/sysmgmt/2012/server/sensor/...`, `/sysmgmt/2012/server/eventlog`, `/sysmgmt/2012/server/lclog`
- `/sysmgmt/2013/server/license`

**Recommendation:** treat `/sysmgmt/*` as an implementation detail of the GUI. Prefer Redfish. Note also that recent iDRAC firmware (2.9x+) enforces the `ST2` token on *every* `/sysmgmt` and `/data` call; 2.86 already sends it for the calls above.

---

# B. Virtual console launch

Two plug-in types are offered (GUI "Plug-in Type": **Native / Java / HTML5**; see help GUID-62708D36). Both carry KVM+mouse over TCP **5900** ("Remote Presence Port", configurable, `0x`-prefixed = hex). Traffic on 5900 is **always encrypted** (help text), video-encryption separately toggleable. Up to **6** concurrent sessions (Enterprise license).

The KVM wire protocol is **Avocent APCP** (`apcp=1`) with the Avocent **DVC** video codec — *not* RFB/VNC. The client jars are shipped by the iDRAC under `/software/` (unauthenticated — verified). Reimplementing the 5900 protocol in Go is a large effort; the practical path is to launch the stock Avocent client or the HTML5 page.

## B.1 Java console (Avocent viewer via JNLP)

**How the GUI obtains it:** an authenticated request to the console launch URL returns a generated `viewer.jnlp`. The iDRAC7/8 filename scheme embeds the session in the *filename* (so JNLP helpers keep it):
```
GET https://<host>/viewer.jnlp(<host>@0@<title>@<epoch_ms>@ST1=<st1token>)
```
(unauthenticated probe of `/viewer.jnlp` 302-redirects; the real request carries the `-http-session-` cookie and the `ST1` token from login.) The response is `application/x-java-jnlp-file`.

**JNLP contents** (Avocent iDRAC viewer; argument set confirmed from a captured Avocent JNLP and from the shipped jar):
- `<jnlp codebase="https://<host>:443/">`, main class **`com.avocent.idrac.kvm.Main`** (verified: `avctKVM.jar` contains `com/avocent/idrac/kvm/Main.class` with a `main(String[])`).
- Jar resources (all live & unauthenticated under `/software/`, verified):
  - `avctKVM.jar` (1.42 MB, main)
  - per-platform native IO: `avctKVMIOLinux64.jar`, `avctKVMIOWin64.jar`, `avctKVMIOMac64.jar`
  - per-platform virtual-media: `avctVMLinux64.jar`, `avctVMWin64.jar`
  - the Linux jars wrap `libavctKVMIO.so` / `libavmLinux64.so` (JNI).
- `<argument>` list (order as seen in a real Avocent JNLP), values the iDRAC fills in:
  - `title=<text>`
  - `ip=<host>`
  - `vmprivilege=true|false` (Virtual Media privilege)
  - `user=<numeric one-time id>` — **temporary, per-launch credential**, not the real username
  - `passwd=<numeric one-time token>` — **temporary one-time token**, not the real password
  - `kmport=5900` (keyboard/mouse), `vport=5900` (video)
  - `apcp=1` (use Avocent APCP protocol), `version=2`
  - `platform=<asic>` (e.g. an ASPEED variant), `helpurl=https://<host>:443/help/...`
  - plus UI toggles: `color`, `chat`, `softkeys`, `statusbar=ip,un,fr,bw,kp,led`, `power`, `language`.

**Confirmed one-time-token design (from `avctKVM.jar`):** class `com.avocent.vm.jni.VMConnectionInfo` exposes `CREDENTIALTYPE_TEMPCREDENTIAL`, `CREDENTIALTYPE_RANDOMNUM`, `CREDENTIALTYPE_CERTIFICATE`, `isUseTempCredentials()`, `DEFAULT_VM_PORT`, `DEFAULT_CREDENTIALS_PORT`, `isForceEncryption()`, and reconnect/heartbeat timeouts. So the `user`/`passwd` in the JNLP are temporary credentials generated by the iDRAC for that launch, consumed by the APCP handshake on 5900. A Go client would: login (A.2) → GET the `viewer.jnlp(...)` URL with cookie+ST1 → parse the `<argument>`s → hand them to the Avocent `Main` class (or reimplement APCP, large).

**To launch the stock viewer headless:**
```
java -cp avctKVM.jar -Djava.library.path=<dir-with-native-jars-unpacked> \
     com.avocent.idrac.kvm.Main ip=<host> kmport=5900 vport=5900 \
     user=<tok> passwd=<tok> apcp=1 version=2 vmprivilege=true title=<t> helpurl=<u>
```
Requires Java 7+ (help note: Java 7+ required, mandatory for IPv6). The jars are Dell-signed (`META-INF/DELL.RSA`).

## B.2 HTML5 console

Introduced on iDRAC7/8 in firmware **2.30.30.30** (web-confirmed). Selected via GUI "Plug-in Type = HTML5" with "Virtual Console Type = HTML5". Launched from the GUI or directly at `https://<host>/console`.

**Live findings (this device, 2.86):**
- `/console`, `/console/ws`, `/consoleredir.html`, `/v1/console` all **302 → `/login.html?console`** when unauthenticated (so they exist and are the console entry points; they route through the console-login redirect rather than the generic `/start.html`).
- `/vkvm.html`, `/html5console.html`, `/vconsole*`, `/kvm`, `/vmedia`, and all `/js/*.js` console scripts are auth-gated (302 → `/start.html`). The HTML5 console JS bundle is therefore **not served unauthenticated** and its exact WebSocket URL/framing could not be captured without a login on this firmware.
- `/start.html` JS shows the console deep-link path: visiting `/console` (or `…?console`) redirects to `/login.html?console`, and after login `POST /data/login?console` is used so the session lands directly in the console.

**Protocol (documented / inferred, medium confidence):** the iDRAC8 HTML5 viewer is the Avocent/Vertiv "vKVM" HTML5 player. It opens a **secure WebSocket to the iDRAC on 443** that tunnels the same Avocent APCP/DVC KVM stream used on 5900 (video-encryption and the 5900 "Remote Presence Port" apply to HTML5 too — help text). It is **not** noVNC/RFB. The one-time session credential is obtained the same way as the Java path (temp credential issued at launch); the browser passes it inside the WebSocket handshake/first APCP message. Exact `wss://` path and framing require a logged-in capture to confirm.

**Recommendation:** for a Go client, HTML5 is not meaningfully easier to drive programmatically than Java, because both ride the proprietary APCP stream. If you only need "open a console for a human", have the Go client perform the login and then hand the browser a cookie'd `https://<host>/console` URL, or emit the `viewer.jnlp`. If you need headless KVM automation, that is a separate, large reverse-engineering project (APCP on 5900).

---

# C. Redfish (`/redfish/v1`)

The clean, documented surface. **Auth:** session token or HTTP Basic. Dell's last iDRAC8 Redfish API Guide is for **2.70.70.70** (`https://dl.dell.com/topicspdf/idrac8redfishguide_en-us.pdf`); 2.75→2.86 were maintenance releases with no new Redfish features, so that guide (plus the 2.75 fixes) is authoritative for 2.86. Beware: parts of the 2.70 guide are copy-pasted from iDRAC9 — where field evidence disagrees, it's noted below.

**Live-verified on this device (unauth):** ServiceRoot, `/odata`, `/$metadata` (namespace list), `/Registries`, `/JSONSchemas`, all `/Schemas/*.xml|json`. Everything else needs auth and was confirmed from the Dell guide + `dell/iDRAC-Redfish-Scripting` (which branches on RedfishVersion / 12G-13G for iDRAC8 paths).

## C.1 Service root & metadata (unauthenticated)

- `GET /redfish/v1` → ServiceRoot (`#ServiceRoot.v1_3_0`). Links: `Systems`, `Chassis`, `Managers`, `SessionService`, `Links.Sessions` = **`/redfish/v1/Sessions`**, `AccountService` = `/redfish/v1/Managers/iDRAC.Embedded.1/AccountService`, `EventService`, `TaskService`, `UpdateService`, `Registries`, `JsonSchemas`, `Fabrics`. `Oem.Dell` = `#DellServiceRoot.v1_0_0` with `ServiceTag`, `ManagerMACAddress`, `IsBranded`. `ProtocolFeaturesSupported`: ExpandQuery (MaxLevels 1), FilterQuery, SelectQuery.
- `GET /redfish/v1/odata` → service document.
- `GET /redfish/v1/$metadata` → CSDL EDMX. **Dell OEM namespaces present in this firmware:** `DellServiceRoot.v1_0_0`, `DellManager.v1_0_0`, `DellJob.v1_0_1`, `DellJobCollection`, `DellComputerSystem.v1_0_0`, `DellUpdateService.v1_0_0`, `DellBootSources.v1_0_0`, `DellBootSourcesRegistry.v1_0_0`, `OemManager.v1_1_1`, `EID_674_Manager.v1_1_1`.
  - **Absent (iDRAC9-only):** `DellLCService`, `DellJobService`/`JobService`, `DellAttributes`, `DellSoftwareInstallationService`, telemetry/`TelemetryService`, `SSE`, `MultipartUpload`/`HttpPushUri`, `DellOem*`.
  - **Present in schema but NOT functional on iDRAC8** (see C.6): `DellManager.ResetToDefaults` (405 on iDRAC8 2.75; Dell staff: use racadm), Manager `Attributes` resource (404). Schema presence ≠ implementation.
- `GET /redfish/v1/Registries` → `Messages` (`iDRAC.1.6.1` → `…/Messages/EEMIRegistry`), `BaseMessages` (`Base.1.2.0`), `BiosAttributeRegistry.v1_0_0` (→ `…/Bios/BiosRegistry`), `BootSourcesRegistry.v1_0_0` (→ `…/BootSources/BootSourcesRegistry`).
- `GET /redfish/v1/JSONSchemas`, `/redfish/v1/Schemas/<X>_v1.xml`, `/redfish/v1/Schemas/<X>.json` → schema files.

Everything else returns a uniform **401** until authenticated (any path outside the unauth allowlist → 401, so 401 does not prove existence).

## C.2 Sessions & auth

| Action | Request | Response |
|---|---|---|
| Create session | **`POST /redfish/v1/Sessions`** `Content-Type: application/json` body `{"UserName":"...","Password":"..."}` | **201**, headers `X-Auth-Token: <token>` + `Location: /redfish/v1/Sessions/<id>` |
| Use | header `X-Auth-Token: <token>` on every call | — |
| Delete session | `DELETE /redfish/v1/Sessions/<id>` (with token) | 200 or 204 |
| Session timeout | `GET/PATCH /redfish/v1/SessionService` (`SessionTimeout`) | — |
| Basic auth | `Authorization: Basic …` on any request | Supported (guide p.10) |

- `/redfish/v1/SessionService/Sessions` is the RedfishVersion ≥ 1.6 path; Dell's own `CreateXAuthTokenSessionREDFISH.py` uses `/redfish/v1/Sessions` when `RedfishVersion < 1.6.0` (this device: 1.4.0). **Always follow `ServiceRoot.Links.Sessions`.**
- Send `Content-Type: application/json` on every JSON body (415 `SYS401`/`SYS4011` otherwise). Responses are `application/json;charset=utf-8`, `OData-Version: 4.0`.
- `If-Match` honoured **only** for `AccountService` and `FirmwareInventory` URIs (ignored elsewhere); mandatory for firmware upload (412/428).
- Errors: unknown property → 400 `Base.1.0.PropertyUnknown`; bad enum → `Base.1.0.PropertyValueNotInList`; OEM codes `IDRAC.1.6.SYSxxx`/`RACxxxx`. Unsupported methods may not return 405.
- **Host-header check (2.81+)**: the `Host` header must be the iDRAC IP, RAC name, or configured FQDN/`ManualDNSEntry`, else `400 Bad Request` (disable: `racadm set idrac.webserver.HostHeaderCheck 0`). Connecting by IP is fine.
- Redfish can be globally disabled (`iDRAC.Redfish.Enable`) → every URI 404.

## C.3 Power / boot (System)

- `GET /redfish/v1/Systems/System.Embedded.1` (`#ComputerSystem.v1_5_0`): `PowerState` (read-only — no PATCH), `Boot`, summaries, health, links.
- **Reset:** `POST /redfish/v1/Systems/System.Embedded.1/Actions/ComputerSystem.Reset` body `{"ResetType":"<v>"}` → **204**. iDRAC8 allowables: `On`, `ForceOff`, `GracefulShutdown`, `PushPowerButton`, `Nmi`, plus **exactly one of** `GracefulRestart` / `ForceRestart` depending on build (the 2.70 guide contradicts itself; iDRAC7 field dumps show `GracefulRestart`). **`PowerCycle` is not available on iDRAC8.** → **Read `Actions["#ComputerSystem.Reset"]["ResetType@Redfish.AllowableValues"]` at runtime** and pick from it.
- Chassis-level: `POST /redfish/v1/Chassis/System.Embedded.1/Actions/Chassis.Reset` `{"ResetType":"On"|"ForceOff"}` → 204.
- **One-time boot override:** `PATCH /redfish/v1/Systems/System.Embedded.1` body
  ```json
  {"Boot":{"BootSourceOverrideEnabled":"Once","BootSourceOverrideTarget":"Pxe"}}
  ```
  → 200; **no config job needed**. Optional `BootSourceOverrideMode: "UEFI"|"Legacy"`, `UefiTargetBootSourceOverride` (BIOS-version dependent). iDRAC8 targets (field): `None, Pxe, Cd, Floppy, Hdd, BiosSetup, Utilities, UefiTarget, SDCard` (+`USB`); `UefiHttp` in the guide is iDRAC9 copy-paste → read `BootSourceOverrideTarget@Redfish.AllowableValues`. `Enabled`: `Disabled|Once|Continuous` — `Continuous` returned 400 on 2.70, **fixed in 2.75** (fine on 2.86). Set-boot-source support arrived in **2.70.70.70**.
- **Persistent boot order** (Dell OEM): `GET /redfish/v1/Systems/System.Embedded.1/BootSources`, `PATCH …/BootSources/Settings {"Attributes":{…}}` → 202, then a config job with `TargetSettingsURI: /redfish/v1/Systems/System.Embedded.1/BootSources/Settings` (C.5). Registry `…/BootSources/BootSourcesRegistry`. `GET …/BootOptions` also exists.

## C.4 BIOS (since 2.50.50.50)

- `GET /redfish/v1/Systems/System.Embedded.1/Bios` (`Attributes`), `GET /redfish/v1/Registries/BiosAttributeRegistry.v1_0_0` / `…/Bios/BiosRegistry` (types, allowables).
- **Stage:** `PATCH /redfish/v1/Systems/System.Embedded.1/Bios/Settings` `{"Attributes":{"<Name>":<value>,…}}` → **202** (400 `SYS426/SYS011/SYS428/SYS405`). Keep batches modest.
- **Apply (config job):** `POST /redfish/v1/Managers/iDRAC.Embedded.1/Jobs` body
  ```json
  {"TargetSettingsURI":"/redfish/v1/Systems/System.Embedded.1/Bios/Settings","StartTime":"TIME_NOW","EndTime":"TIME_NA"}
  ```
  → **200** (or 202) with `Location: /redfish/v1/Managers/iDRAC.Embedded.1/Jobs/JID_…`. ISO timestamps allowed for a maintenance window. Then reboot the host (C.3) to run the job; poll the JID.
- **Discard staged values:** `POST /redfish/v1/Systems/System.Embedded.1/Bios/Settings/Actions/Oem/DellManager.ClearPending` → 200 (400 `RAC1035` if nothing pending). Same for `…/BootSources/Settings/Actions/Oem/DellManager.ClearPending`. (This is what `DellManager.v1_0_0` in `$metadata` is actually for.)
- Actions: `POST …/Bios/Actions/Bios.ResetBios` → 200 (`RAC1133`); `POST …/Bios/Actions/Bios.ChangePassword` `{"PasswordName":"SysPassword"|"SetupPassword","OldPassword":"…","NewPassword":"…"}` → 200.

## C.5 Jobs (Dell config-job model)

- `GET /redfish/v1/Managers/iDRAC.Embedded.1/Jobs` (`#DellJobCollection`); `GET`/`DELETE /redfish/v1/Managers/iDRAC.Embedded.1/Jobs/<JID>` (`#DellJob.v1_0_1`).
- Job fields: `JobState` (`New|Scheduled|Running|Completed`), `PercentComplete`, `Message`, `MessageId`, `MessageArgs`, `JobType`, `StartTime`, `EndTime`, `TargetSettingsURI`.
- `JobType` enum (schema): `FirmwareUpdate, FirmwareRollback, RepositoryUpdate, RebootPowerCycle, RebootForce, RebootNoForce, Shutdown, RAIDConfiguration, BIOSConfiguration, NICConfiguration, FCConfiguration, iDRACConfiguration, SystemInfoConfiguration, InbandBIOSConfiguration, ExportConfiguration, ImportConfiguration, RemoteDiagnostics, RealTimeNoRebootConfiguration, LCLogExport, HardwareInventoryExport, FactoryConfigurationExport, LicenseImport, LicenseExport, ThermalHistoryExport, LCConfig, LCExport, SACollect*, SystemErase, MessageRegistryExport, OSDeploy, Unknown`.
- `/redfish/v1/TaskService/Tasks/<id>` also exists: **SCP export/import/preview return a `Location` there** (GET → 200 done / 202 running / 404); BIOS/BootSources/firmware jobs return `…/Jobs/JID_…`. **Always poll whatever `Location` the action returned.** No `/redfish/v1/JobService` on iDRAC8.

## C.6 Manager (iDRAC) & Dell OEM manager actions

- `GET /redfish/v1/Managers/iDRAC.Embedded.1` (`#Manager.v1_3_3`); children `EthernetInterfaces`, `NetworkProtocol` (PATCH HTTP/HTTPS/IPMI/KVMIP/SNMP/SSH/Telnet/VirtualMedia ports), `SerialInterfaces`, `Jobs`, `LogServices`, `VirtualMedia`, `HostInterfaces`.
- **iDRAC reboot:** `POST …/iDRAC.Embedded.1/Actions/Manager.Reset` `{"ResetType":"GracefulRestart"}` → 204 (only value allowed).
- **`DellManager.ResetToDefaults` — NOT functional on iDRAC8** (schema lists it; 2.75 returns "method not allowed"; Dell staff: use `racadm racresetcfg`). Exclude.
- **iDRAC/LC/System attributes — NOT available via Redfish on iDRAC8, any firmware:** `…/Managers/iDRAC.Embedded.1/Attributes` → 404 "resource Attributes … not found"; `…/Oem/Dell/DellAttributes/*` → 404 `SYS403` (confirmed on an R730 at 2.86.86.86, dell/iDRAC-Redfish-Scripting#365). Dell's scripts say: *"If using iDRAC8 you will need to leverage SCP to set these attributes."* The Ansible `community.general.idrac_redfish_config` module therefore does not work on iDRAC8. **Use SCP import (below) with `Target: "iDRAC"`.**
- **Server Configuration Profile (SCP)** — `EID_674_Manager` OEM actions (need Administrator; Lifecycle Controller enabled; since 2.40, JSON since 2.60, HTTP/HTTPS shares since 2.70):
  - `POST /redfish/v1/Managers/iDRAC.Embedded.1/Actions/Oem/EID_674_Manager.ExportSystemConfiguration`
    ```json
    {"ExportFormat":"XML"|"JSON","ExportUse":"Default"|"Clone"|"Replace",
     "IncludeInExport":"Default"|"IncludeReadOnly"|"IncludePasswordHashValues",
     "ShareParameters":{"Target":"ALL","ShareType":"LOCAL"|"NFS"|"CIFS"|"HTTP"|"HTTPS",
                        "IPAddress":"…","ShareName":"…","FileName":"…","Username":"…","Password":"…","Workgroup":"…",
                        "IgnoreCertificateWarning":"Enabled"|"Disabled"}}
    ```
    → **202** + `Location: /redfish/v1/TaskService/Tasks/<id>`; for `LOCAL`, `GET` the finished task to receive the SCP body.
  - `POST …/Oem/EID_674_Manager.ImportSystemConfiguration`
    ```json
    {"ImportBuffer":"<xml or json string>",
     "ShareParameters":{"Target":"ALL"|"iDRAC"|"BIOS"|"System"|"LifecycleController"|"<FQDD>","ShareType":"LOCAL"},
     "ShutdownType":"Graceful"|"Forced"|"NoReboot","HostPowerState":"On"|"Off","TimeToWait":300}
    ```
    → 202 + `Location` (task). `ImportBuffer` only for `LOCAL`; `TimeToWait` 300–3600.
  - `POST …/Oem/EID_674_Manager.ImportSystemConfigurationPreview` (same shape) → 202.
  - `Target` is a plain string on iDRAC8 (iDRAC9 accepts a list). Errors: 400 `RAC013/SYS406/SYS433/RAC1155`, 500 `SWC0058`, 503 `RAC052/RAC0679`.
  - Minimal iDRAC-attribute set example (`ImportBuffer`, XML): `<SystemConfiguration><Component FQDD="iDRAC.Embedded.1"><Attribute Name="NIC.1#DNSRacName">name</Attribute></Component></SystemConfiguration>` with `Target: "iDRAC"`, `ShutdownType: "NoReboot"`.

## C.7 Virtual media (Redfish; added 2.70, reliable from 2.75)

- `GET /redfish/v1/Managers/iDRAC.Embedded.1/VirtualMedia` → members **`CD`** and **`RemovableDisk`** (named, not `/1`,`/2`; there is no `/Systems/…/VirtualMedia` on iDRAC8).
- `GET …/VirtualMedia/CD` (`#VirtualMedia.v1_2_0`): `Image`, `Inserted`, `WriteProtected`, `ConnectedVia`, `MediaTypes`.
- **Insert:** `POST …/VirtualMedia/CD/Actions/VirtualMedia.InsertMedia` `{"Image":"http://host/x.iso","Inserted":true,"WriteProtected":true}` → **204**. (Optional `UserName`/`Password`, but see limits.)
- **Eject:** `POST …/VirtualMedia/CD/Actions/VirtualMedia.EjectMedia` `{}` → **204**. Same paths for `RemovableDisk`.
- Insert while attached / eject while empty → 500. Poll `Inserted`/`ConnectedVia` afterwards.
- **iDRAC8 share limits:** HTTP, HTTPS, NFS work; **CIFS is not supported**; **HTTPS/HTTP with credentials is not supported**; `@` in vmedia user/password unsupported; no built-in ignore-certificate for the VirtualMedia action (use HTTP or a trusted cert). Shipped undocumented in 2.70 (buggy), works from 2.75 — Metal3 requires ≥ 2.75 for iDRAC8.

## C.8 Logs (SEL, LC, FaultList)

- **Entries (iDRAC8 layout):** `GET /redfish/v1/Managers/iDRAC.Embedded.1/Logs/Sel`, `…/Logs/Lclog`, `…/Logs/FaultList` (LogEntryCollection). iDRAC8 uses **`Lclog`** (iDRAC9: `LC`) and exposes entries at **`/Logs/<name>`**, *not* `/LogServices/<name>/Entries` (Dell's `GetIdracLcLogsREDFISH.py` branches: 12G/13G → `/Logs/Lclog`). Treat `LogServices/*/Entries` as unsupported.
- Services: `GET /redfish/v1/Managers/iDRAC.Embedded.1/LogServices` → `Sel`, `Lclog`, `FaultList`.
- **Clear SEL:** `POST …/LogServices/Sel/Actions/LogService.ClearLog` (empty body) → **204**, needs ClearLogs privilege. Only SEL is clearable via Redfish (not Lclog).
- Filtering: `$filter`, `$select`, `$expand` documented (e.g. `Logs/Sel?$filter=Severity eq "OK"`). `$skip`/`$top` are **not documented** for iDRAC8 — follow `Members@odata.nextLink` when present instead of assuming `$top`.

## C.9 Firmware update (since 2.60.60.60)

Flow (Dell DUP `.exe` payloads):
1. `GET /redfish/v1/UpdateService/FirmwareInventory` → members `Installed-…`, `Previous-…`, `Available-…`; **capture the response `ETag` header**.
2. `POST /redfish/v1/UpdateService/FirmwareInventory` as `multipart/form-data`, file part named **`file`**, header **`If-Match: <ETag>`** → **201**, `Location: /redfish/v1/UpdateService/FirmwareInventory/Available-<ComponentID>-<version>`. (412 `SYS400` / 428 `SYS404` without valid If-Match; 415 `SYS4011` wrong content type.)
3. `POST /redfish/v1/UpdateService/Actions/Oem/DellUpdateService.Install` `{"SoftwareIdentityURIs":["/redfish/v1/UpdateService/FirmwareInventory/Available-…"],"InstallUpon":"Now"|"NowAndReboot"|"NextReboot"}` → **202** (`SYS408`), `Location: /redfish/v1/Managers/iDRAC.Embedded.1/Jobs/JID_…`. **One URI per call** (`SYS442` otherwise). Poll the job.
- `POST /redfish/v1/UpdateService/Actions/UpdateService.SimpleUpdate` `{"ImageURI":"/redfish/v1/UpdateService/FirmwareInventory/Available-…"}` or `{"ImageURI":"http://server/file.exe","TransferProtocol":"HTTP"}` → 202 + job Location — documented in the 2.70 guide, so available on 2.86 (one component at a time; iDRAC image applies immediately and reboots the iDRAC).
- `DELETE …/FirmwareInventory/Available-…` needs `If-Match`. `MultipartUpload`, `HttpPushUri`, `redfish://` URIs, repository update (`DellSoftwareInstallationService`) are iDRAC9-only.

## C.10 Inventory & config resources

- System: `/redfish/v1/Systems/System.Embedded.1/{Processors, Memory (+ /<id>/Metrics), Storage, Storage/<ctrl>/Volumes, Storage/<ctrl>/Drives/<id>, SimpleStorage/Controllers, EthernetInterfaces (+/Vlans), NetworkInterfaces, SecureBoot (PATCH SecureBootEnable; Actions/SecureBoot.ResetKeys), BootSources, BootOptions, Bios}`. Storage/network/memory inventory since 2.60; PCIe/HostInterface/Memory metrics since 2.70.
- Chassis: `/redfish/v1/Chassis/System.Embedded.1/{Thermal, Power}`; individual sensors in the pre-1.5 Dell style `…/Chassis/System.Embedded.1/Sensors/Voltages/<id>`, `…/Sensors/Temperatures/<id>` (**no generic `Sensors` collection**). `Chassis/Chassis.Embedded.1/Thermal` may appear for enclosures.
- Accounts: documented iDRAC8 paths **`/redfish/v1/Managers/iDRAC.Embedded.1/Accounts/{1..16}`** and `…/AccountService`, `…/Roles` (`Administrator|Operator|ReadOnly|None`); `/redfish/v1/AccountService/Accounts/<id>` also resolves on this era — follow `ServiceRoot.AccountService`. `PATCH …/Accounts/<n>` `{"UserName":"…","Password":"…","RoleId":"Administrator","Enabled":true}` → 200 (400 `RAC0288/RAC0291`); `If-Match` optional here. Account 1 is reserved (IPMI anonymous). **Delete a user by PATCHing `UserName` to `""`.**
- Events: `GET/PATCH /redfish/v1/EventService`; `POST /redfish/v1/EventService/Subscriptions` `{"Destination":"https://receiver/…","EventTypes":["Alert"],"Context":"…","Protocol":"Redfish"}` → 201; `GET`/`DELETE …/Subscriptions/<id>`; `POST …/Actions/EventService.SubmitTestEvent`. Limits: **HTTPS destinations only, `Alert` only, max 20 subscriptions**, no SSE. Retry knobs via racadm `iDRAC.RedfishEventing.*`.

## C.11 iDRAC8 Redfish feature timeline

| Feature | Introduced |
|---|---|
| Redfish 1.0 core (Systems/Chassis/Managers/Sessions/Accounts/Logs/EventService/TaskService/VirtualMedia GET) | 2.30.30.30 |
| SCP Export/Import/Preview (`EID_674_Manager`), Jobs collection | 2.40.40.40 |
| BIOS Attributes/Settings + config jobs, SecureBoot, BootSources | 2.50.50.50 |
| UpdateService (upload / `DellUpdateService.Install` / SimpleUpdate), Storage/Network/Memory inventory, SCP JSON | 2.60.60.60 |
| Boot override PATCH, `InsertMedia`/`EjectMedia`, `$select/$filter/$expand`, PCIe/HostInterface/Memory metrics, SCP over HTTP(S) | 2.70.70.70 |
| Fixes: `BootSourceOverrideEnabled=Continuous`, reliable vMedia insert | 2.75.75.75 |
| Host-header check (400 on unknown `Host`) | 2.81.81.81 |
| Final iDRAC8 release (security fixes only) — **the target firmware** | 2.86.86.86 (Apr 2024) |

Known 2.83–2.86 release-note issue: some browser/PowerShell TLS stacks fail against Redfish ("Unable to send Redfish API requests"); curl/Go/python are unaffected in field reports.

---

# Summary table

| Capability | Best surface (iDRAC8 2.86) | Endpoint |
|---|---|---|
| Generation/firmware fingerprint (unauth) | Legacy | `GET /cgi-bin/discover`, `GET /data?get=prodServerGen`, `GET /session?aimGetProp=fwVersionFull` |
| Authenticate | Redfish (primary); Legacy (console/sysmgmt only) | `POST /redfish/v1/Sessions` → `X-Auth-Token`; `POST /data/login` → cookie + `ST2` |
| Power on/off/cycle | Redfish | `POST …/Systems/System.Embedded.1/Actions/ComputerSystem.Reset` (read AllowableValues; no `PowerCycle`) |
| One-time boot device | Redfish | `PATCH …/Systems/System.Embedded.1` `{"Boot":{…}}` (no job) |
| Persistent boot order | Redfish OEM | `PATCH …/BootSources/Settings` + Jobs |
| BIOS get/set | Redfish + Job | `GET/PATCH …/Bios[/Settings]` + `POST …/iDRAC.Embedded.1/Jobs` |
| iDRAC / LC settings | **SCP import** (no `Attributes` on iDRAC8) | `…/Actions/Oem/EID_674_Manager.ImportSystemConfiguration` `Target:"iDRAC"` |
| SCP export | Redfish OEM | `…/Actions/Oem/EID_674_Manager.ExportSystemConfiguration` → Task |
| Reboot iDRAC | Redfish | `Manager.Reset {"ResetType":"GracefulRestart"}` |
| Reset iDRAC to defaults | **racadm** (`racresetcfg`) — not Redfish on iDRAC8 | SSH/remote racadm |
| Virtual media mount | Redfish | `…/VirtualMedia/CD/Actions/VirtualMedia.{Insert,Eject}Media` (HTTP/NFS, no CIFS, no creds) |
| SEL / LC logs | Redfish | `…/iDRAC.Embedded.1/Logs/{Sel,Lclog,FaultList}`; clear SEL via `LogServices/Sel/Actions/LogService.ClearLog` |
| Firmware update | Redfish OEM | multipart upload (+`If-Match`) → `Oem/DellUpdateService.Install` → Job |
| Inventory (CPU/mem/disk/NIC/thermal/power) | Redfish | `…/Systems/*`, `…/Chassis/System.Embedded.1/{Thermal,Power}` |
| Users | Redfish | `…/Managers/iDRAC.Embedded.1/Accounts/<n>` (delete = `UserName:""`) |
| Alerts push | Redfish | `POST /redfish/v1/EventService/Subscriptions` (HTTPS, Alert only) |
| Job/task polling | Redfish | follow action `Location` (`…/Jobs/JID_…` or `…/TaskService/Tasks/<id>`) |
| Virtual console (human) | Java JNLP or HTML5 | `GET /viewer.jnlp(host@0@title@ts@ST1=…)` ; `https://host/console` |
| Health aggregate (GUI parity) | Legacy | `GET /sysmgmt/2016/server/extended_health` (cookie + `ST2`) |

---

# Recommended surface for a Go client

1. **Use Redfish for everything transactional** — auth, power, boot, BIOS, virtual media, logs, inventory, users, firmware, SCP. Auth with a **Session** (`POST /redfish/v1/Sessions` → `X-Auth-Token`; follow `ServiceRoot.Links.Sessions` rather than hardcoding) for multi-call work; Basic auth is fine for one-shots. Send `Content-Type: application/json` on every body. Read `@Redfish.AllowableValues` at runtime for `ResetType` and `BootSourceOverrideTarget` rather than hardcoding (build-dependent on iDRAC8). Poll whatever `Location` an action returns (Dell `Jobs/JID_…` or `TaskService/Tasks/<id>`).

2. **Exclude iDRAC9-only OEM paths** (absent from this `$metadata` or non-functional on iDRAC8): `DellLCService`, `DellJobService`/`JobService`, `DellAttributes` and the Manager `Attributes` resource, `DellManager.ResetToDefaults`, `TelemetryService`, `SSE`, `MultipartUpload`/`HttpPushUri`, `LogServices/*/Entries`, `LogServices/LC`, `Sensors` collection, `/Systems/…/VirtualMedia/{1,2}`. **iDRAC/LC configuration on iDRAC8 = SCP import** (`EID_674_Manager.ImportSystemConfiguration` with `ImportBuffer`, `Target: "iDRAC"`, `ShutdownType: "NoReboot"`). iDRAC factory reset = racadm, not Redfish.

3. **Use the legacy `/data`+`ST2` API only for narrow gaps** Redfish 1.4 doesn't cover (GUI-only toggles, the aggregate `extended_health`). Flow: `POST /data/login` → keep cookie `-http-session-` + parse `ST2` from `forwardUrl` → send calls as POST with header `ST2: <token>`. Honour `<blockingTime>`; never brute-force (lockout). Don't build broad functionality on `/data?get=` scraping — the keyset is undocumented and version-specific.

4. **Virtual console:** don't reimplement the Avocent APCP/5900 protocol. For a human operator, log in via the legacy API and either (a) fetch and serve the `viewer.jnlp(...)` (Java; jars at `/software/*.jar`, main class `com.avocent.idrac.kvm.Main`) or (b) open `https://<host>/console` in a browser carrying the session cookie (HTML5, fw ≥ 2.30). The JNLP `user`/`passwd` are one-time tokens minted per launch (`VMConnectionInfo.CREDENTIALTYPE_TEMPCREDENTIAL`).

5. **Transport:** self-signed cert → skip verification or pin per host. Set `Host` to the IP you dial (2.81+ host-header check). Send `Accept-Encoding: gzip` when fetching any static UI asset. Expect `401` mid-session = expired token → re-login. Firmware upload requires the `ETag`→`If-Match` dance and one component per `Install` call.
