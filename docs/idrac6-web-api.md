# Dell iDRAC6 HTTP API Reference (for a Go client)

Target device: `https://192.168.10.162` (Dell PowerEdge R710, iDRAC6 Enterprise), firmware **2.92 (Build 05)**.

This document mirrors the structure of `idrac8-web-api.md` (sections A and B). **iDRAC6 has no Redfish and no `/sysmgmt` or `/session` JSON API** — there is no section C. Everything the GUI does rides the single legacy XML API `/data?get=` / `/data?set=` plus a handful of `.esp`/`cgi-bin` endpoints. Where behaviour differs from iDRAC8 it is called out inline and summarised at the end.

## How this was verified

The entire web UI is served from an **Mbedthis-Appweb/2.4.2** server. All functional pages require an authenticated session, but the complete UI source (HTML + JS + the server-side `.esp`/`.jsesp` templates) was recovered by **extracting the firmware image** (`firmimg.d6` → `root.cramfs`, web root at `/usr/local/www`), so every endpoint, parameter name, field-list and the CSRF machinery below is read directly from the shipping code rather than inferred. Live behaviour (status codes, headers, cookie, the XML envelope, TLS, the unauthenticated oracle, and `authResult=1`) was confirmed against the live device with `curl -sk --ciphers 'DEFAULT@SECLEVEL=0'`. Exactly **one** bogus-credential login (`x`/`x`) was issued; no real/guessed passwords were tried.

Sources cited below as: **(JS)** = read from the firmware UI source; **(live)** = probed against 192.168.10.162; **(bin)** = extracted from the compiled Appweb modules `libDataHandler.so` / `libavctAuth.so` / `guiDataServer`; **(docs)** = third-party writeups / captured artifacts.

**Generation / firmware (verified live, unauthenticated):**

| Fact | Value | Source |
|---|---|---|
| Generation | **iDRAC6 / 11G PowerEdge** | `GET /cgi-bin/discover` → `<ENDPOINTTYPE>iDRAC</ENDPOINTTYPE>` (note: bare `iDRAC`, not `iDRAC8`) (live) |
| Firmware | **2.92 (Build 05)** | `/public/about.html` hardcodes `2.92(Build 05)`; matches device (live) |
| License tier | Enterprise (vKVM + vMedia present) | `index.html` reads `ameastatus_bool_amea_present`, `vkvm_bool_enabled` (JS) |
| Redfish | **none** | `GET /redfish/v1` → **404** (live) |
| `/sysmgmt/*`, `/session?aim*` | **none** | both → **404** (live) — these are iDRAC7/8-only |

**Webserver behaviour that affects a Go client:**
- Static assets are served **plain** (no pre-gzip). `GET /login.html` returns `text/html` 32682 bytes with or without `Accept-Encoding: gzip`. (This is the opposite of iDRAC8, which stores assets pre-gzipped and needs `--compressed`.) (live)
- TLS: the server negotiates up to **TLS 1.2** (`DHE-RSA-AES256-GCM-SHA384`) but with a weak/legacy stack — modern clients must relax the security level (`openssl`/curl `DEFAULT@SECLEVEL=0`), and some requests only succeed over **TLS 1.0**. Certificate is a self-signed **1024-bit RSA / SHA-1** `CN=iDRAC6 default certificate` → use `InsecureSkipVerify` (Go) / `-k` (curl). (live)
- `Connection: keep-alive`, `Keep-Alive: timeout=60, max=2000`. Appweb is a small embedded server; keep concurrency low (a few connections) and expect it to drop idle keep-alives at 60 s. (live)
- **Unauthenticated oracle:** protected **HTML pages 302-redirect to `/start.html`** (which then JS-redirects to `/login.html`); protected **`/data*` and `/viewer.jnlp` endpoints** redirect/parse differently — `/data?get=…` returns **401 Unauthorized** (`text/html`, 132 bytes) and `/viewer.jnlp` **302 → /start.html**; nonexistent paths return **404** (126-byte stub). So: 302→start.html = exists-but-auth, 401 = `/data` needs session, 404 = does not exist.
- XML responses are `text/xml`, UTF-8, wrapped in `<root>…<status>ok</status></root>`.
- Server banner: `Server: Mbedthis-Appweb/2.4.2`. (live)

## Entry points at a glance

1. **Legacy web-UI API** (`/data?get=`, `/data?set=`, `/data/login`, `/data/logout`, plus `*.esp` upload handlers and `/cgi-bin/*`) — the only programmatic surface. XML in/out. Form-login → **`_appwebSessionId_` cookie** + a **two-token** anti-CSRF scheme (**`ST2`** header, **`ST1`** query param).
2. **Virtual console** — Avocent Java viewer via a generated `viewer.jnlp` (and legacy ActiveX), KVM+mouse+video over TCP **5900**. **The JNLP embeds the real account username and password in cleartext** (not a one-time token — a key difference from iDRAC8).
3. No Redfish.

---

# A. Legacy web-UI API

The GUI is served by Appweb. The compiled request filter is **`AVCTAuthHandler`** (in `libavctAuth.so`), which gates every request, checks the session cookie, and enforces the CSRF tokens. (bin)

## A.1 Unauthenticated endpoints (no login needed)

| Endpoint | Method | Response | Notes |
|---|---|---|---|
| `/cgi-bin/discover` | GET | `text/xml` | `<DISCOVER><RESP><RC>0x0</RC><ENDPOINTTYPE>iDRAC</ENDPOINTTYPE><ENDPOINTVER>1.00</ENDPOINTVER><PROTOCOLTYPE>HTTPS</PROTOCOLTYPE><PROTOCOLVER>2.0</PROTOCOLVER></RESP></DISCOVER>` — **best unauth fingerprint**; `ENDPOINTTYPE=iDRAC` (bare) identifies iDRAC6 vs `iDRAC8` (live) |
| `/login.html` | GET | `text/html` | The login page (also `/sclogin.html` smart-card, `/adlogin.html` AD, `/ssologin.html` SSO) (live) |
| `/start.html` | GET | `text/html` | Unauth landing; JS redirects to sclogin/index/login per `scl_int_enabled`/`pam_bool_sso_enabled` (JS) |
| `/public/about.html`, `/public/support.html` | GET | `text/html` | About box (carries fw version string) (live) |
| `/help/en/*`, `/images/*`, `/css/*`, `/stylesheet.css`, `/js/Clarity.js`, `/functions.js` | GET | — | Login-page resources; the auth filter explicitly skips these (bin: `Skip resources needed by login page`) |
| `/software/*.jar` | GET | `application/octet-stream` | **vKVM client jars, unauthenticated** (`avctKVM.jar` ≈ 1.0 MB, platform IO/VM jars) — see B (live) |
| `/data/login` | POST (GET also returns XML) | `text/xml` | Login (A.2). A credential-less GET returns `<authResult>1</authResult>` (live) |
| `/cgi-bin/{login,exec,logout,ssn_stat,putfile}` | GET/POST | `text/xml` | Remote-racadm transport (A.6). `/cgi-bin/` itself → 503 (live/bin) |

Everything else (every functional `.html` page, all `/data?get=`/`set=` keys) needs a session.

## A.2 Login flow (form login → session cookie + ST1/ST2 tokens)

Verified from `/login.html`, `/functions.js`, the compiled `libavctAuth.so`/`libDataHandler.so`, and one live attempt.

**Step 1 (optional):** `GET /data/logout` or `POST /data/logout` to clear any prior session. Handlers `doLogout` / `doSessionLogout` exist in `libDataHandler.so`. (bin)

**Step 2 — login.** From `login.html` `sendLoginRequest()`:
```
POST /data/login
Content-Type: application/x-www-form-urlencoded
Body: user=<encoded>&password=<encoded>
```
- Username/password are URL-encoded with `encodeURIComponent`, except the section-sign `§` (char 167) which is `escape`d — the exact German-keyboard workaround (`handleGermanChar()`) seen on iDRAC8. (JS)
- For an AD domain the GUI prepends/appends the domain to the username; plain local login sends the bare username (`root`). (JS)

**Response** (`text/xml`; verified live with bogus `x`/`x`):
```xml
<?xml version="1.0" encoding="UTF-8"?>
<root>
  <status>ok</status>
  <authResult>1</authResult>
  <forwardUrl>index.html</forwardUrl>
  <errorMsg></errorMsg>
</root>
```
- **Set-Cookie:** `_appwebSessionId_=<32-hex>; path=/; secure` (cookie name is literally `_appwebSessionId_` — **differs from iDRAC8's `-http-session-`**; note **no `HttpOnly`** on iDRAC6). The cookie is set on the very first request (even unauth) and upgraded to an authenticated session on success. (live)
- **`<authResult>` codes** (from `errMsg[]` in `login.html`): `0` = success; `1` = login failed / bad credentials (`loginFailedVerifyMsg`); `2` = missing username; `3` = missing password; `4` = insufficient privilege (`loginFailedPrvMsg`); `5` = **session count exceeded** (`loginSessionCtExceeded`); `99` = generic failure. (JS)
- The success handler simply does `document.location = forwardUrl`. There is **no `<blockingTime>` element** in the iDRAC6 XML (iDRAC8 adds one). (JS)
- **On success (`authResult==0`)**, `forwardUrl` is built by `makeFwdURL` as:
  ```
  index.html?ST1=<token1>,ST2=<token2>
  ```
  Note the **comma** separating the two token params (iDRAC8 uses `&`). On the failed attempt the device returned the bare `index.html` (no tokens — the session has none yet). (bin: literals `index.html?`, `,ST2=`, `ST1`; `Adding tokens to session: %s=<%s>, %s=<%s>`)

**The two tokens — this is the crux of the iDRAC6 CSRF scheme** (compiled filter `AVCTAuthHandler`, `libavctAuth.so`):
- **`ST2` = HTTP request-header token** (filter routine `token2HeaderVerify`). Sent as header `ST2: <token2>` on every `/data?get=`/`/data?set=` POST.
- **`ST1` = URI query-param token** (filter routine `token1UriVerify`). Appended as `?ST1=<token1>` (or `&ST1=`) to GET-style requests the filter lists: `/viewer.jnlp`, `/playVideo.jnlp`, `/bindata`, `/csvdatajnlp`, `/data?type=jnlp`, the `*.esp`/`*.vfk` upload handlers, `/bootcapture`, `/crashcapture`, firmware upload, SSH-key upload, etc. (bin)
- Both tokens are also stored server-side in the session, so `index.html` (a server-parsed `.esp` page) re-reads them with the built-ins `getToken2Name()/getToken2Value()` and `getToken1Name()/getToken1Value()` and exposes them to page JS as:
  ```js
  var TOKEN_NAME  = "ST2"; var TOKEN_VALUE  = "<token2>";   // header token
  var TOKEN_NAME1 = "ST1"; var TOKEN_VALUE1 = "<token1>";   // query token
  ```
  (JS: `index.html` lines 69-72; names resolve to `ST2`/`ST1` per the bin literals.)

**Step 3 — attach the tokens.** From `functions.js` `loadXMLDocument()`:
- Every `/data` request is sent as **POST**, `Content-Type: application/x-www-form-urlencoded`, and **iff `TOKEN_VALUE.length >= 8` adds header `ST2: <token2>`.** (JS lines 671-675, 1200-1205)
- Auto-refresh requests additionally send header `idracAutoRefresh: <0|1>`. (JS)
- For GET-style requests (uploads, viewer, CSV, images) the page appends `ST1=<token1>` to the query string instead. (JS)

So a Go client's session state = **cookie `_appwebSessionId_`** + **header `ST2: <token2>`** for `/data`, and **`?ST1=<token1>`** for the file/console/image URLs. A `401` on a `/data` call = session expired/invalid → re-login. (The filter can be globally relaxed via `AVCT_CSRF_PROTECTION_DISABLED`, but assume it is on.) (bin)

**Logout:** `GET /data/logout` (or `POST`). Also `GET /data?get=logout` appears in some pages. (JS/bin)

**Session timeout / lockout:** the session timeout comes from property `gui_int_session_timeout` (configurable on the Services page; default 30 min idle). (bin) Session-count limits are enforced: exceeding the max concurrent GUI sessions yields `authResult=5` (XML) or the redirect `login.html?ErrorMsg=RAC0218: The maximum number of user sessions is reached.` There is no explicit failed-password lockout timer in the iDRAC6 web XML (no `blockingTime`); IP blocking, if enabled, is governed by the iDRAC `IPBlocking` config group, not surfaced in this response. Do not brute-force. (bin)

## A.3 `/data?get=` and `/data?set=` (the XML key/value API)

All require session + `ST2` header (else 401). The GUI always issues these as **POST** (empty body; the query string carries the request). (JS)

**Read:** `POST /data?get=key1,key2,key3`
→ `text/xml`: `<root><key1>…</key1><key2>…</key2>…<status>ok</status></root>`.
- `functions.js` `loadData()` builds the URL by joining each field's `m_dataName` with commas; `getXMLValue(xmlDoc, name)` reads `getElementsByTagName(name)[0]` and concatenates text child-nodes (Firefox splits long values into 4096-byte nodes — **must be re-joined**). (JS lines 460-487, 536-567)
- Some keys take **function-call arguments** in the query, e.g. `user(3)`, `ca_certificate(1)`, `powergraphdata(<seconds>)`, `vfkJobId(<id>)`, `consolepreview[auto <epoch_ms>]`. (JS)
- List results are nested XML (e.g. `<eventLogEntries>` containing `<eventLogEntry …/>` children; `<temperatures>`, `<fans>`, `<voltages>`, `<powerSupplies>` similarly). (JS)

**Write:** `POST /data?set=name1:value1,name2:value2`
- `formatPostRequestForFields()` builds `name:value` pairs — **`:` between name and value, `,` between pairs** — and values are passed through `encodeSetValue()`. Some setters are function-call style, e.g. `user(idx,…)`, `sendmail(addr,subj,body)`, `addiag(user,pwd,mode)`. (JS lines 511-530)
- Response envelope carries `<status>ok</status>`; `waitWithCallback` treats non-`ok` as failure.

### A.3.1 `/data?get=` keys (enumerated from the UI source)

| Area | Keys | Page / source |
|---|---|---|
| System summary | `sysDesc, svcTag, expSvcCode, hostName, osName, osVersion, biosVer, fwVersion, LCCfwVersion, sysRev, macAddr, pwState` | `sysSummaryData.html` |
| Net (summary) | `v4Enabled, v4IPAddr, v6Enabled, v6Addr, v6LinkLocal, v6SiteLocal, macAddr` | `sysSummaryData.html` |
| Network (full) | `netMode, macAddr, netEnabled, autoNeg, netSpeed, duplex, nicMTU, hostname, dnsDomain, v4IPAddr, v4Gateway, v4NetMask, v4DNS1, v4DNS2, v6Addr, v6Prefix, v6Gateway, v6DNS1, v6DNS2, totalSharedLOMs, ameaPresent, …` | `network.html`, `config-Network-Adv.html` |
| Power state | `pwState` | `powercontrol.html` |
| Power monitoring | `powermonitordata, systemLevel, voltages, budgetpowerdata, powergraphdata(<secs>)` | `powermonitor.html`, `powerbudget.html`, `powerGraph.html` |
| Power supplies | `powerSupplies, psRedundancy, psRedundancyPolicy` | `listpowersup.html` |
| Temp sensors | `temperatures` | `listtemp.html` (+ `sensors.jsesp`) |
| Fans | `fans, fansRedundancy` | `listfan.html` |
| Voltages | `voltages` | `listvolt.html` |
| Intrusion | `intrusion` | `listintr.html` |
| Batteries | `batteries` | `listbat.html` |
| Removable storage | `removableStorage, rmvsRedundancy` | `rmstorage.html` |
| SEL (system event log) | `eventLogEntries` (CSV: `GET /csvdata?get=eventLogEntriesCSV`) | `sel.html` |
| RAC log | `racLogEntries` (CSV: `GET /csvdata?get=racLogEntriesCSV`) | `raclog.html` |
| Last crash / boot capture | `bootCapFileData`, `lastcrash` data | `lastcrash.html`, `bootCapture` |
| Active sessions | `activeSessions` | `ssninfo.html` |
| Users | `user(<index>)` → user row XML | `userlist.html`, `useredit.html` |
| vKVM / vMedia config | `kvmEnabled, kvmEncEnabled, kvmActSes, kvmMaxSessions, kvmPort, kvmPluginType, localVideo, bootonceEnabled, vmAttachStatus, vmConnectStatus, vmActSes, vmMaxSessions, vmEncEnabled, vmFloppyEmul, vfkEnable` | `vkvmconfig.html` |
| Console preview (screenshot) | `consolepreview[auto <ms>]` / `consolepreview[manual <ms>]` → then fetch image `GET /capconsole/scapture0.png?<ms>` | `sysSummaryData.html` |
| First boot device | `firstBootDevice, vmBootOnce` | `firstboot.html` |
| Identify LED | `IdentifyEnable, IdentifyTimeout` | `troubleShootIdentify.html` |
| LCD / front panel | LCD color/text/blink (`xmlLcdColor, xmlLcdText, xmlLcdBlink`) | `lcd.html` |
| Remote file share (vMedia) | `remoteFileshrImage, remoteFileshrUser, remoteFileshrPwd, remoteFileshrStatus, remoteFileshrDisconnectStatus` | `remotefileshr.html` |
| vFlash SD partitions | `vfkListPartition, vfkJobId(<id>), vfkStsUpldErr(<ssn>)` | `vflash*.html` |
| Firmware update state | `fwVersion, rollBackfwVersion, fwUpdateState, fwSemStatus, fwProgress, fwUpdate, spfwInfo, spfwVer, firmwareIntStatus` | `fwupdate.html` |
| Certificates / SSH keys | `ca_certificate(<i>), user_certificate(<i>), user_sshkey(<i>), ad_certificate` | `get_*view.html` |
| PEF / alerts | `getPEF, propertyList, SupCmdsXML` | `listpef.html`, etc. |
| Work notes | work-notes data | `Worknotes.html` |

(Property/value semantics for each key resolve through the AIM property manager in `libDataHandler.so`; the XML tag names above are exactly what the GUI requests and parses.)

### A.3.2 `/data?set=` operations (enumerated from the UI source)

| Operation | Request (`POST /data?set=…`) | Codes / notes | Page |
|---|---|---|---|
| **Power control** | `pwState:<code>` | `0`=off, `1`=on, `2`=power-cycle (cold reboot), `3`=reset/warm-reboot, `4`=NMI, `5`=graceful shutdown. From `var State = {OFF:0, ON:1, POWERCYCLE:2, REBOOT:3, NMI:4, SHUTDOWN:5}` | `powercontrol.html` |
| **Identify LED** | `IdentifyEnable:<0\|1>,IdentifyTimeout:<0-255>` | blink chassis ID LED for N seconds | `troubleShootIdentify.html` |
| **Clear SEL** | `clearSEL:1` | needs ClearLogs priv | `sel.html` |
| **Clear RAC log** | `clearRACLog:1` | | `raclog.html` |
| **Clear ASR/last crash** | `clearASR:0` | | crash pages |
| **First boot device** | `vmBootOnce:<0\|1>,firstBootDevice:<n>` | boot-device enum per `firstboot.html` (PXE, Floppy, CD, HDD, BIOS setup, vFlash=11, …); `vmBootOnce` = one-time | `firstboot.html` |
| **iDRAC reset** | `iDracReset:1` | reboots the iDRAC (racreset equivalent) | `sysSummaryData.html` / `diagnostics.html` |
| **Add/edit user** | `user(<idx>,<enable>,<username>,<privBitmask>,<password>,<lanPriv>,<serialPriv>,<serialEnable>)` | function-style; `privBitmask` from checkbox set `DracGetCheckboxes()` | `useredit.html` |
| **Kill session** | `killSession(<sessionId>)` | | `ssninfo.html` |
| **vKVM/vMedia config** | e.g. `kvmEnabled:<0\|1>, kvmEncEnabled:…, kvmMaxSessions:…, kvmPort:…, kvmPluginType:…, localVideo:…, vmAttachStatus:…, vmFloppyEmul:…` (built from the page field-list) | | `vkvmconfig.html` |
| **Remote file share (vMedia) connect/disconnect** | `remoteFileshrImage:<url>,remoteFileshrUser:<u>,remoteFileshrPwd:<p>,remoteFileshrAction:<connect\|disconnect>` | NFS/CIFS ISO path | `remotefileshr.html` |
| **PSU redundancy policy** | `psRedundancyPolicySet:<n>` | | `powerbudget.html` |
| **PEF / SNMP alerts** | `setAllPEFalerts:<v>,setPEF(<n>)`, `snmpCommunity:<c>,snmpTrapIP4:<ip>,snmpTrapDest4Ena:<0\|1>…`, `sendSNMPtrap(<destOffset>)` | | `listpef.html`, trap pages |
| **Test email** | `sendmail(<addr>,<subject>,<body>)` | | alert pages |
| **Reset one sensor/entity** | `resetEntity:<i>` | | sensor pages |
| **Diagnostics login** | `addiag(<user>,<pwd>,<mode>)` | | `diagnostics.html` |
| **vFlash** | `vfkSDInitialize:1`, `vfkPartDelete:<id>` | | `vflash*.html` |
| **CSR / cert** | `serverCSR(<cn>,<org>,…)` | | `ssl.html` |
| **Work notes** | `PutWorkNotes(<text>)` | | `Worknotes.html` |
| **Firmware update state machine** | `fwUpdateState:<0\|2\|3\|4\|6>`, `fwUpdate:<0\|1>`, `fwRollback:<0\|1>,fwUpdateState:4` | see A.5 | `fwupdate.html` |

## A.4 `/csvdata?get=` and image endpoints

- `GET /csvdata?get=eventLogEntriesCSV`, `GET /csvdata?get=racLogEntriesCSV` — CSV export of SEL / RAC log (links in `sel.html`/`raclog.html`). Need session; use `ST1` query token for the GET. (JS)
- **Console preview / screenshot:** first `POST /data?get=consolepreview[auto <epoch_ms>]` (or `[manual …]`) to trigger a capture, then `GET /capconsole/scapture0.png?<epoch_ms>` for the PNG; on error the UI falls back to `/images/nosignal.png`. (Note: on iDRAC6 the capture lives at **`/capconsole/scapture0.png`**, not `/capconsole.jpg` — the latter 404s live.) (JS/live)

## A.5 Firmware update

Multi-step, driven from `fwupdate.html` (JS):
1. Upload the DUP/image via an HTML form POST to **`/fwupload/fwupload.esp?ST1=<token1>`** (`multipart/form-data`; the `ST1` query token is required). (JS line 284)
2. Drive the state machine with `POST /data?set=fwUpdateState:<n>` (`2`=verify, `3`/`4`/`6` = transitions) and poll `POST /data?get=fwSemStatus,fwUpdateState,spfwInfo,fwProgress`. (JS)
3. Commit with `POST /data?set=fwUpdate:1` (or `fwUpdate:0` to cancel); rollback with `fwRollback:1,fwUpdateState:4`. (JS)
4. Poll `fwProgress`, `fwUpdate`, `firmwareIntStatus`. (JS)

## A.6 `/cgi-bin/*` (remote-racadm transport)

The compiled cgi handlers present in the firmware are `/cgi-bin/login`, `/cgi-bin/exec`, `/cgi-bin/logout`, `/cgi-bin/ssn_stat`, `/cgi-bin/putfile`, `/cgi-bin/discover`. This is the **`racadm -r` (remote racadm) protocol**, independent of the GUI session: `login` opens a racadm session, `exec` runs a racadm command, `logout` closes it, `ssn_stat` reports session status, `putfile` uploads. (bin/live) For a Go client, prefer SSH racadm over reimplementing this. `/cgi-bin/discover` (the one unauthenticated member) is the generation fingerprint (A.1). `/cgi-bin/` with no handler returns 503. (live)

---

# B. Virtual console launch

iDRAC6 offers the **Avocent Java viewer** (via a generated JNLP) and, on older setups, a **Native ActiveX** viewer (`activeXViewer.esp`). Both carry keyboard/mouse + video + virtual-media over TCP **5900** using the Avocent **APCP** protocol (`apcp=1`) with the Avocent DVC video codec — **not** RFB/VNC. Enterprise license allows multiple concurrent sessions (`kvmMaxSessions`). The client jars live under `/software/` and are **downloadable unauthenticated** (verified live, `avctKVM.jar` ≈ 1.0 MB).

## B.1 How the GUI builds the launch URL

From `vkvm.html` (and `sysSummary.jsesp`), the page first assembles arguments:
```js
var ipAddr       = <iDRAC IP>;
var ipv6Format   = <0|1>;
var cacheVariable= "@" + Date.now();          // cache-buster
var delim        = "@";
var token1       = top.TOKEN_NAME1 + "=" + top.TOKEN_VALUE1;   // "ST1=<token1>"
var title        = urlencode("<dnsName>, <sysName>, User:<username>");
```
Then, for the **Java/JNLP KVM** path (`arg==2`, Java plugin):
```
GET https://<host>/viewer.jnlp(<ipAddr>@<ipv6Format>@<title>@<epoch_ms>@ST1=<token1>)
```
and for **virtual-media-only** (`arg==1`):
```
GET https://<host>/data?type=jnlp&get=vmStart(<ipAddr>@<ipv6Format>@<urlencoded title>@<epoch_ms>@ST1=<token1>)
```
(JS: `vkvm.html` lines 218, 241, 170.) Note the **function-call-in-the-path** scheme `viewer.jnlp(arg@arg@…@ST1=…)` — the whole `(...)` is part of the URL (an Appweb route, handled by the auth filter's `type=jnlp` / `/viewer.jnlp` cases). The request must carry the **`_appwebSessionId_` cookie** and the **`ST1`** token. Unauthenticated `/viewer.jnlp` 302-redirects to `/start.html` (live). Response `Content-Type: application/x-java-jnlp-file`.

The **ActiveX** path instead opens `activeXViewer.esp?ipAddr=<ip>&kvmPort=<port>&vmOnly=<bool>&title=<t>&lang=<l>&TokenName=ST1&TokenKey=<token1>`. (JS)

## B.2 JNLP contents

A real iDRAC6 viewer JNLP (captured from a 2.9x device) — the generated form differs from the static sample `viewer.esp` in the firmware:
```xml
<jnlp codebase="https://<host>:443" spec="1.0+">
  <information>
    <title>iDRAC6 Virtual Console Client</title>
    <vendor>Dell Inc.</vendor>
    <icon href="https://<host>:443/images/logo.gif" kind="splash"/>
    <shortcut online="true"/>
  </information>
  <application-desc main-class="com.avocent.idrac.kvm.Main">
    <argument>ip=<host></argument>
    <argument>vmprivilege=true</argument>
    <argument>helpurl=https://<host>:443/help/contents.html</argument>
    <argument>title=<dnsName>%2C+<model>%2C+User%3A<user></argument>
    <argument>user=root</argument>          <!-- REAL username -->
    <argument>passwd=calvin</argument>       <!-- REAL password, cleartext -->
    <argument>kmport=5900</argument>
    <argument>vport=5900</argument>
    <argument>apcp=1</argument>
    <argument>version=2</argument>
  </application-desc>
  <security><all-permissions/></security>
  <resources>
    <j2se version="1.6+"/>
    <jar href="https://<host>:443/software/avctKVM.jar" download="eager" main="true"/>
  </resources>
  <!-- per-OS/arch nativelib blocks: avctKVMIO{Win32,Win64,Linux32,Linux64,Mac64}.jar
       + avctVM{Win32,Win64,Linux32,Linux64,Mac64}.jar -->
</jnlp>
```

**Critical difference from iDRAC8:** on iDRAC6 the JNLP `user` and `passwd` arguments are the **real account credentials in cleartext** (here the factory `root`/`calvin`), *not* a per-launch one-time token. The compiled handler fills `user`/`passwd` from the authenticated session's account. (docs: captured JNLP; firmware sample `viewer.esp` uses `user=root`/`passwd=password` placeholders.) This means:
- Anyone who can fetch the JNLP (i.e. holds a valid GUI session) obtains the account password.
- A Go client does **not** need to mint a token — after login it fetches the `viewer.jnlp(...)` URL (cookie + `ST1`), parses the `<argument>`s, and either hands them to the stock Avocent `com.avocent.idrac.kvm.Main` class or connects to 5900 itself.

**Prerequisite endpoints before the JNLP works:** an authenticated session (A.2); vKVM must be enabled (`kvmEnabled`); the `ST1` token must be valid. The JNLP jars (`/software/avctKVM.jar` + platform native jars) are fetched by Java Web Start with no auth. (JS/live)

**Launching the stock viewer headless** (as third-party scripts do, confirmed working):
```
java -cp avctKVM.jar -Djava.library.path=lib/ com.avocent.idrac.kvm.Main \
     apcp=1 ip=<host> vmprivilege=true kmport=5900 vport=5900 user=<user> passwd=<pwd>
```
where `lib/` holds the `.so`/`.dll` unpacked from `avctKVMIO<plat>.jar` and `avctVM<plat>.jar`. (docs)

## B.3 Video replay

`/playVideo.jnlp` is a sibling route (in the auth filter) that generates a JNLP to replay captured boot/crash videos through the same Avocent client. Needs session + `ST1`. (bin)

---

# C. (No Redfish)

iDRAC6 predates Redfish. `GET /redfish/v1` → 404, `GET /sysmgmt/*` → 404, `GET /session?aim*` → 404 (all verified live). Modern REST inventory/power is simply not available; use the legacy `/data` API above, or SSH/remote racadm.

---

# Endpoint reference table

| Endpoint | Purpose | Auth required | Verified how |
|---|---|---|---|
| `GET /cgi-bin/discover` | Generation fingerprint (`ENDPOINTTYPE=iDRAC`) | no | live |
| `GET /login.html`, `/sclogin.html`, `/adlogin.html`, `/ssologin.html` | Login pages | no | live |
| `GET /start.html` | Unauth landing → redirect | no | live + JS |
| `GET /public/about.html` | About (fw version) | no | live |
| `GET /software/*.jar` | vKVM client jars | no | live |
| `POST /data/login` | Form login → cookie + ST1/ST2 | no (establishes it) | live + JS + bin |
| `GET\|POST /data/logout` | Logout | session | bin/JS |
| `POST /data?get=<keys>` | Read system/sensor/log/config XML | session + `ST2` header | JS + bin; live 401 unauth |
| `POST /data?set=<pairs>` | Power / LED / logs / users / config writes | session + `ST2` header | JS + bin |
| `POST /data?set=pwState:<0-5>` | Power off/on/cycle/reset/NMI/graceful | session + `ST2` | JS |
| `POST /data?set=iDracReset:1` | Reboot the iDRAC | session + `ST2` | JS |
| `POST /data?set=clearSEL:1` / `clearRACLog:1` | Clear logs | session + `ST2` | JS |
| `GET /csvdata?get=eventLogEntriesCSV` / `racLogEntriesCSV` | Log CSV export | session + `ST1` | JS |
| `POST /data?get=consolepreview[...]` + `GET /capconsole/scapture0.png` | Console screenshot | session (+`ST1` for image) | JS |
| `GET /viewer.jnlp(ip@fmt@title@ts@ST1=<t>)` | Java vKVM launch (JNLP w/ real creds) | session + `ST1` | JS + bin; live 302 unauth; docs for JNLP body |
| `GET /data?type=jnlp&get=vmStart(...)` | Virtual-media-only JNLP | session + `ST1` | JS |
| `GET /playVideo.jnlp` | Captured-video replay JNLP | session + `ST1` | bin |
| `GET activeXViewer.esp?...&TokenName=ST1&TokenKey=<t>` | Native ActiveX viewer launch | session + `ST1` | JS |
| `POST /fwupload/fwupload.esp?ST1=<t>` (multipart) | Firmware image upload | session + `ST1` | JS |
| `POST /data?set=fwUpdateState:…/fwUpdate:1` | Firmware update state machine | session + `ST2` | JS |
| `POST /upload/upload.esp?ST1=<t>`, `/upload/sshkey_upload.esp?ST1=<t>`, `/vfkupload/vfkupload.vfk?ST1=<t>` | Cert / SSH-key / vFlash uploads | session + `ST1` | JS |
| `/cgi-bin/{login,exec,logout,ssn_stat,putfile}` | Remote-racadm transport | racadm session | bin; live (`/cgi-bin/`→503) |
| `GET /redfish/v1`, `/sysmgmt/*`, `/session?aim*` | — (do not exist on iDRAC6) | n/a | live 404 |

---

# Differences from iDRAC8

| Aspect | iDRAC8 (2.86) | iDRAC6 (2.92) |
|---|---|---|
| **Redfish** | Yes (`/redfish/v1`, 1.4.0) — the recommended surface | **None** (404). Legacy `/data` API only |
| `/sysmgmt/*` JSON, `/session?aim*` JSON | Present (GUI uses them) | **Absent** (404) |
| Web server | Newer EmbedThis Appweb; **assets pre-gzipped** (need `Accept-Encoding: gzip`) | **Mbedthis-Appweb/2.4.2**; assets served **plain** |
| Session cookie | `-http-session-` (`; secure; httponly`) | **`_appwebSessionId_`** (`; secure`, **no HttpOnly**) |
| CSRF tokens | `ST1`+`ST2`, embedded in `forwardUrl` with **`&`** (`index.html?ST1=..&ST2=..`) | `ST1`+`ST2`, in `forwardUrl` with a **comma** (`index.html?ST1=..,ST2=..`); also re-exposed by `index.html` ESP via `getToken1/2Name/Value()` |
| Token usage | same model: `ST2` = header on `/data`; `ST1` = query param on JNLP/uploads | **identical** (`token2HeaderVerify`/`token1UriVerify` in `libavctAuth.so`) |
| Login XML | `<root><status><authResult><blockingTime><forwardUrl><errorMsg>` | same **minus `<blockingTime>`** (no lockout-backoff field) |
| `authResult` codes | 0/1/2/3/4/5/99 | **same** set |
| Fingerprint | `/cgi-bin/discover` → `ENDPOINTTYPE=iDRAC8`; `/data?get=prodServerGen`→`13G` | `/cgi-bin/discover` → `ENDPOINTTYPE=iDRAC` (bare) |
| **vConsole JNLP creds** | `user`/`passwd` are **per-launch one-time tokens** (`VMConnectionInfo.CREDENTIALTYPE_TEMPCREDENTIAL`) | **`user`/`passwd` are the real account credentials in cleartext** |
| vConsole transport | Avocent APCP on 5900 (Java) + HTML5 WebSocket on 443 | Avocent APCP on 5900 (Java) + legacy Native **ActiveX**; **no HTML5 console** |
| Power reset values | Redfish `ComputerSystem.Reset` (read `AllowableValues`; no `PowerCycle`) | `/data?set=pwState:` with `0`=off `1`=on `2`=powercycle `3`=reset `4`=NMI `5`=graceful |
| Power/inventory/logs/users/vmedia | Redfish resources | all via `/data?get=`/`set=` keys (A.3) |
| TLS / cert | TLS incl. 1.2, host-header check (2.81+) | TLS up to 1.2 but weak stack (needs `SECLEVEL=0`, sometimes TLS 1.0); **1024-bit SHA-1 self-signed** cert; no host-header check |

---

# Recommended surface for a Go client

1. **Fingerprint** unauthenticated with `GET /cgi-bin/discover` (`ENDPOINTTYPE=iDRAC` = this is a 6, not 7/8/9).
2. **Login:** `POST /data/login` with `user=`&`password=` (URL-encoded). On `authResult==0`, store cookie **`_appwebSessionId_`** and parse **`ST1`/`ST2`** from `forwardUrl` (`index.html?ST1=<a>,ST2=<b>`). Honour `authResult` codes; back off on `5` (session limit); never brute-force.
3. **Every `/data` call:** POST, `Content-Type: application/x-www-form-urlencoded`, header **`ST2: <token2>`**. Read: `data?get=k1,k2`; write: `data?set=k:v,k:v` (`:` pairs, `,` separators). Re-join multi-node text values. Re-login on 401.
4. **Power/LED/logs/boot/users/vmedia:** use the `/data?set=` operations in A.3.2 (`pwState:<0-5>`, `iDracReset:1`, `clearSEL:1`, `vmBootOnce`+`firstBootDevice`, `IdentifyEnable`, `user(...)`, `remoteFileshr*`).
5. **Virtual console:** after login, `GET /viewer.jnlp(<ip>@<fmt>@<title>@<ts>@ST1=<token1>)` (cookie + `ST1`), parse `<argument>`s, hand to `com.avocent.idrac.kvm.Main` (jars at `/software/`, unauth) or connect APCP on 5900. **Treat the JNLP's `passwd` as the live account password — it is cleartext; handle/secure accordingly.**
6. **Transport:** `InsecureSkipVerify`; configure a TLS stack that permits the weak cert/ciphers (equivalent of `SECLEVEL=0`, allow TLS 1.0 fallback); keep connections few (embedded Appweb, keep-alive 60 s/max 2000). No gzip needed.
