# iDRAC6 (Avocent) KVM video channel

Byte-level description of what the iDRAC6 virtual console sends on the
**video socket**, reverse engineered from `avctKVM.jar` (fw 2.92, decompiled
with CFR). Class names below refer to that jar; the iDRAC8 viewer
(`com.avocent.kvm.a.a.*`) uses an identical DVC decoder and identical
packet layouts for the types covered here.

Go implementation: `pkg/kvm/video.go` (framing, dispatch, acks),
`pkg/kvm/dvc.go` (DVC decoder), `pkg/kvm/textmode.go` (VGA text mode),
`pkg/kvm/framebuffer.go` (pixel store).

Nothing in video decoding is native: `avctKVMIOLinux64.jar` only contains
`libavctKVMIO.so` whose JNI exports are all
`Java_com_avocent_kvm_nativekeyboard_NativeKVM_*` (keyboard passthrough).

---

## 1. Socket life cycle (context, owned by the session code)

`com.avocent.kvm.b.r` (session):

1. Control channel logs in. Login response (`vb`, type 0x8300) with
   `p()==false` -> `r.z()` opens the video socket to the same host on the
   video port `r.Q` (set from the applet's `vport` parameter through
   `r.d(int)`; hard-coded default 8192), optionally through APCP (`b.l`)
   and/or TLS, `SoTimeout` 60 s.
2. `r.a(InputStream, OutputStream)` (inherited plumbing): wraps streams,
   creates the video packet writer `Y` (`com.avocent.kvm.b.w` extends
   `c.d.g`, a queue + thread that writes `header || payload` and flushes) and
   starts the reader thread `com.avocent.kvm.b.z` ("AVSP Video Input").
3. Client sends **Video Channel Auth** (`b.a.ac`, see §3.1) through `Y`.
4. `this.a.e()` starts the decoder thread (`c.nb`, "Video Decoder").
5. Every packet read by `z` goes to `com.avocent.kvm.b.n.a(c)` (dispatcher).
   Video packets are queued to the decoder session `com.avocent.kvm.b.s`
   (extends `c.l`) which pulls them one at a time (`l.f()`).

The Go `VideoStream` takes over at step 4: `NewVideoStream(rw, fb, log)`
with `rw` positioned right after the `ac` packet was written.

---

## 2. Packet framing (`com.avocent.kvm.b.a.a.a(DataInputStream, OutputStream, boolean)`)

Every message on the video socket (both directions):

```
offset  size  field
0       4     magic: "BEEF" (0x42 0x45 0x45 0x46) on client->server packets
              (b.a.b.a(int,int)). The reader NEVER checks it. Server->client
              video packets seen in the code are built by the appliance; the
              reader only looks at bytes 5..7.
4       1     type, high byte. Ignored by the reader (it dispatches on byte 5
              only; the `boolean` argument that would enable 16-bit types is
              always false on the video socket). Known values: 0x86 for the
              video packet family (34305 = 0x8601 etc. appear as aliases in
              the switch tables), 0x01 for the auth packet.
5       1     type, low byte -> dispatch key (§3)
6       2     total length, big endian, INCLUDING the 8-byte header.
              Must satisfy 16 <= len <= 15000, otherwise IOException
              ("Bad messages length" / "Error: Packet length").
8       len-8 payload
```

Client-built packets from `zb` subclasses (`ac`) use a different header:
`00 00 00 00 01 <type> <len16>` (no BEEF, byte 4 = 1).

If the 8 header bytes start with `"APCP"` the Java reads
`u32be(hdr[4:8]) - 8` more bytes (APCP frame) and then, buggily, continues
to interpret the same header as a BEEF header. The Go reader skips APCP
frames entirely (**unverified** whether any appear after the auth packet).

The reader thread treats a `null` packet (IOException while reading) as end
of session.

---

## 3. Message types on the video socket

Dispatch is in `com.avocent.kvm.b.n.a(com.avocent.kvm.c.d.c)`; factory in
`b.a.a.a(int)`. `ACK` marks packets that count towards the Video Ack (§4).

| low type | class (`b.a.*`) | name (string table)     | dir | ACK | meaning |
|---------:|-----------------|-------------------------|-----|-----|---------|
| 0x00 | `cb`  | Video Ack               | C->S | | §4 |
| 0x01 | `ac`  | Video Channel Auth      | C->S | | §3.1 |
| 0x80 | `fb`  | Video Packet            | S->C | ACK | payload ignored; only counted for the ack and clears the "video stopped" UI state |
| 0x81 | `hb`  | Video Packet (DVC15)    | S->C | ACK | DVC bitstream, 15-bit colour (`d.c`) |
| 0x82 | `hb`  | Video Packet (DVC7)     | S->C | ACK | DVC bitstream, 7-bit palette (`d.f`) |
| 0x83 | `hb`  | Video Packet (DVC7_GRAY)| S->C | ACK | DVC bitstream, 7-bit grey (`d.e`) |
| 0x84 | `xb`  | Video Connect Status    | S->C | | §3.2 |
| 0x85 | `v`   | VideoStopped            | S->C | ACK | §3.3 |
| 0x86 | `gb`  | ASpeed JPEG Video       | S->C | ACK | iDRAC7+ ASpeed JPEG/VQ frames ("BishaDecode"), §10 |
| 0x87 | `ub`  | Text Mode Video         | S->C | ACK | §6 |
| 0x88 | `ib`  | Color Palette           | S->C | ACK | §6.3 |
| 0x89 | `kb`  | Font Table              | S->C | no  | §6.2 |
| 0x8A | `hb`  | Video Packet (DVC23)    | S->C | ACK | DVC bitstream, 23-bit colour (`d.d`) |

Any other low byte -> `qb` ("unknown message") and is ignored. The iDRAC8
string table names the modes `DVC7`, `DVC15`, `DVC23`, `DVC7_GRAY`,
`ASPEED`, `RVAS`.

### 3.1 Video Channel Auth (0x01, `ac`, client -> server)

```
header:  00 00 00 00 01 01 00 10
payload: u32be clientRandom (r.C = random()*1e7)   ; u32be applianceRandom (r.D, from login response)
```
Total 16 bytes. Sent once, immediately after connecting. (Session code.)

### 3.2 Video Connect Status (0x84, `xb`)

`payload[0] == 0` -> connected, else failed. Handler `n.a(xb)` calls
`r.y()`, i.e. the SESSION must then send on the **control channel**:

* `db` **Video Enable Request** (type 782 = 0x030E, len 16, payload
  `[enable=1, 1, 0...]`),
* `x` **Set Display Area** (type 770 = 0x0302, len 16, payload
  `u16be 1024, u16be 768, 0...`),
* `o` **SetScaleMode1to1** (type 772 = 0x0304, len 16, zero payload),

set `SESSION_STATE=RUNNING` and create the mouse request manager. The Go
stream surfaces this as `VideoStream.OnConnectStatus(connected bool)`.

### 3.3 VideoStopped (0x85, `v`)

`payload[0]` = reason code, forwarded as property `video_state` with values
0,1,2,3,4 or 10 (constants `com.avocent.kvm.b.e.a..f`; 100 = "video
resumed", set when the next video packet arrives). Names are not in the
string table. Handler clears the screen to black (`hb.i()`), signals frame
end (`hb.b()`) and counts an ack. Go: framebuffer cleared,
`Frames()` incremented, `OnVideoStopped(reason)` called.

---

## 4. Client -> server: Video Ack (0x00, `cb`) — flow control

`com.avocent.kvm.b.r.B()`: a counter `V` is incremented for every ACK
packet (table above). When `V` reaches `W = 20`:

```
42 45 45 46  00 00  00 10   <count=20> 00 00 00 00 00 00 00
"BEEF"       type0  len16    payload (8 bytes, count in byte 0)
```
is written on the video socket (falls back to the control channel only if
the video writer does not exist). `V` is reset to 0. Without acks the
appliance stops sending after some number of packets (**unverified** how
many), so `VideoStream` sends them automatically (`AckInterval`, default
20). `EncodeVideoAck(n)` returns the 16 wire bytes.

---

## 5. DVC video packets (0x81/0x82/0x83/0x8A, `com.avocent.kvm.b.a.hb`)

### 5.1 Payload layout (offsets relative to payload start, i.e. header+8)

```
0..3   unused by the client (unverified: sequence/timestamp)
4..5   u16be frame height in pixels   (hb.l, `i()`)
6..7   u16be frame width  in pixels   (hb.m, `j()`)
8      flags: bit0 = BOF  (start of a new frame; cursor reset to pixel 0)
              bit1 = EOF  (last packet of the frame; checksum valid)
9      unused
10..11 u16be checksum (only meaningful when EOF; 0 = none)
12..   DVC bitstream bytes (payload length - 12)
```
Frame size validity (`d.b.a`, `c.i.a`): 1..4096 accepted by the decoder but
the pixel store ignores sizes > 2000 x 2000 ("Ignoring invalid resolution
change"). A size change re-allocates a black frame (Go: `Framebuffer.Resize`).

### 5.2 Frame / packet semantics (`com.avocent.kvm.b.s.f`, `d.b.d`)

* Packets before the first BOF are skipped.
* BOF: frame-begin notification, cursor := 0 (`d.b.f()` -> `hb.a()`,
  `hb.c(0)`). A command left half-read at the end of the previous packet
  is effectively cancelled (run length 0 / pixel dropped).
* Consecutive non-EOF packets are one continuous byte stream: run-length
  extension bytes, Make Pixel operand bytes and Make Series continuation
  bytes may be in the next packet (`d.b.g`, `d.b.b`, `d.b.f` call `b.f()`
  to pull the next packet).
* EOF: no continuation is attempted for run lengths / series (applied with
  the bytes available). For a truncated Make Pixel the Java pulls the
  next packet anyway; the Go port drops the pixel (**unverified** edge).
  Then `hb.b()` (frame end) and the checksum (§5.5) are evaluated when the
  NEXT packet is dequeued.
* When the packet type (colour mode) changes, the viewer sends
  **SetDVCColorDepthMessage** (`b.a.k`, type 1027 = 0x0403, len 16,
  payload `01 00 00 00 00 00 00 00`: byte0 = "not default"=1, byte1 =
  depth=0, byte2/3 = flags 0) on the **control channel** (`n.a(hb)`).
  Go: `VideoStream.Control.SendControlPacket(0x0403, ...)`.

### 5.3 Pixel store (`com.avocent.kvm.c.j`)

Linear `int[width*height]` of ARGB, row-major, plus cursor `i` (index of
the next pixel to write; -1 before the first frame, treated as 0).
Primitive semantics that the decoder relies on (ported 1:1 in
`dvc.go`):

| primitive | Java | behaviour |
|-----------|------|-----------|
| set pixel | `b(int)` | write if `0<=i<n`, always `i++` |
| set cursor | `c(int)` | `i = min(v, n)` |
| copy | `b(src,len)` | return if `i>n`; clamp so `src+len<n` and `i+len<n`; copy; `i+=len` |
| fill | `c(color,len)` | return if `i>=n`; clamp `i+len<n`; return if `len<=0`; fill; `i+=len` |
| write array | `a(int[])` | if `i>=n` **or `i+len>=n`: drop silently, no advance**; else copy, `i+=len` |
| read | `a(idx)` | 0 when out of range |

Note the `>=` clamps: the very last pixel of the frame can never be written
by copy/fill/series, only by Make Pixel. Faithfully reproduced.

### 5.4 Bitstream grammar (`com.avocent.kvm.d.b.d()` and helpers)

Read one command byte `c`:

```
c & 0x80 != 0            MAKE PIXEL      (d.{c,d,e,f}.b)
else switch c & 0xE0:
  0x00                   NO CHANGE       (d.b.c)  cursor += len
  0x20                   COPY LEFT       (d.b.e)  fill len pixels with pixel[cursor-1] (0 if cursor==0)
  0x40                   COPY ABOVE      (d.b.d)  copy len pixels from cursor-width;
                                                  if cursor < width: IGNORED, cursor NOT advanced
  0x60                   MAKE SERIES     (d.b.f)
```

**Run length** (`d.b.g`) for 0x00/0x20/0x40: `len = c & 0x1F`; then up to
4 further bytes are consumed while `next & 0xE0 == c & 0xE0` (same opcode
in the top 3 bits), each adding `(next & 0x1F) << (5*k)`, k = 1..4
(max 25 bits). A byte with a different opcode ends the run (not
consumed). In modes 0x82 and 0x83 (one byte per pixel) **len += 2**
(`d.e.g`, `d.f.g` override). Modes 0x81 / 0x8A: no bias.

**Make Series** (two-colour bitmap): `A = pixel[cursor-1]`; `B` = the most
recent pixel before that whose value != A, scanning backwards to index 0
(`B = A` if none). Then write 4 pixels from bits 3,2,1,0 of `c`
(bit set -> B, clear -> A) via "write array". If `c & 0x10`: read
continuation bytes; each yields 7 pixels from bits 6..0 (same mapping),
and bit 7 set means another continuation byte follows.

**Make Pixel** operand (7 low bits of `c` = `p`):

| mode | extra bytes | colour |
|------|-------------|--------|
| 0x81 DVC15 | 1 (`b1`) | `v = p<<8 \| b1`; R = `(v&0x7C00)>>7`, G = `(v&0x3E0)>>2`, B = `(v&0x1F)<<3` (RGB555, low 3 bits zero) |
| 0x8A DVC23 | 2 (`b1 b2`) | `v = p<<16 \| b1<<8 \| b2`; R = `(v&0x7F8000)>>15`, G = `(v&0x7F80)>>7`, B = `(v&0x7F)<<1` |
| 0x83 DVC7_GRAY | 0 | `g = p<<1`; RGB = (g,g,g) |
| 0x82 DVC7 | 0 | `palette[p]` (§5.6) |

### 5.5 Frame checksum (`com.avocent.kvm.b.s.r`, `c.j.a(...)`)

Evaluated when an EOF packet had a non-zero checksum:

* DVC15 (`d.c`): `sum over all pixels with rgb!=0 of ((rgb&0xFF0000)>>9 | (rgb&0xFF00)>>6 | (rgb&0xFF)>>3)`, mod 65536.
* every other mode: `sum of paletteIndex(rgb)` (reverse lookup of §5.6,
  0 for unknown colours), mod 65536. Only sensible for DVC7; the Java
  computes it for grey/23-bit too (**unverified** whether the appliance
  sends non-zero checksums in those modes).

On mismatch the viewer logs `Checksum failed` and, only with
`-DrefreshOnError=true`, sends **ScreenRefresh** (§7). Go: counted in
`Stats().DVC.ChecksumErrors`, optional `RefreshOnChecksumError`.

### 5.6 7-bit palette (`com.avocent.kvm.d.i`)

Levels `L = {0, 70, 127, 191, 255}`. Base table
`base[25*b + 5*g + r] = ARGB(L[r], L[g], L[b])` for r,g,b in 0..4
(indices 0..124), `base[125..127] = 0x5F5F5F, 0x9F9F9F, 0xDFDFDF`.
The wire index `p` maps through the permutation `e[128]` (in `dvc.go`,
`dvcPalettePermutation`): `palette[p] = base[e[p]]`. E.g. `palette[1] =
0x000046`, `palette[16] = 0x460000`, `palette[127] = 0xDFDFDF`.

---

## 6. Text mode (0x87 / 0x88 / 0x89)

When the host is in VGA text mode the appliance sends the character buffer
and lets the client render it (`com.avocent.kvm.b.t`, "TextModeDecoder").
The session flips a `mode` property between `graphicsmode` and `textmode`;
the first text packet after graphics forces geometry re-initialisation
(`modechanged`).

### 6.1 Text Mode Video (0x87, `ub`)

```
0..3    unused
4..5    u16be pixel height (e.g. 400)
6..7    u16be pixel width  (e.g. 720)
8       flags: bit0 BOF, bit1 EOF, bit2 unused, bit3 blinking enabled,
               bit4 line-graphics (9th column replication for glyphs 0xC0..0xDF)
9       underline scanline (1-based; row K-1 is drawn in fg for attr&0x77==1)
10      rows    (e.g. 25)
11      cols    (e.g. 80)
12..    cell data: 2 bytes per cell {char, attr}, row-major, possibly split
        over several packets (BOF on the first, EOF on the last)
last 4  (only when EOF) u16be cursor cell index, u8 cursor start scanline, u8 cursor end scanline
```
Special cases: payload length 12 -> no-op; payload length 16 with BOF|EOF
-> cursor-only update (bytes 12..15 = cursor). Cell size = width/cols x
height/rows (integer division). Attribute decoding
(`b.t.a(int,int,int,int,int)`):

* fg index = `attr & 0xF`, or `attr & 7` when 2 font tables are loaded
  (then `attr & 8` selects the table: set -> primary, clear -> secondary).
* bg index = `(attr>>4) & 0xF`, or `& 7` when blinking is enabled (bit 7 =
  blink; the Go port renders blinking text steadily).
* colours from the 16-entry palette (§6.3); without a palette fg=white on
  black, except fg index 0 -> black on white.
* glyph row `r` bits come from `font[char*32 + r]`, MSB = leftmost pixel.
  Column 8 (when cell width is 9) = copy of column 7 for line-graphics
  glyphs 0xC0..0xDF when bit4 is set, else background.
* cursor: the bottom `end-start+1` scanlines of the cursor cell are filled
  with the cell's fg colour (Java blinks it at 250 ms; Go draws it steady).

Frame end on EOF: only cells whose {char,attr} changed are repainted.

### 6.2 Font Table (0x89, `kb`, assembled by `com.avocent.kvm.b.k`)

```
0      table index (0 = primary, else secondary)
1      number of font tables (1 or 2)
2..3   offset field (see quirk)
4..    glyph bytes; a full table is 8192 bytes = 256 glyphs x 32 rows
```
Quirk: `kb.a()` reads the destination offset as `u16be(payload[0:2]) - 1`
(a decompiler-visible bug: `b(byArray2, this.l)` with `l==0`), so chunks
land at offset 0/1 and the table is expected in a single 8196-byte payload
(fits under the 15000 limit). The Go port reproduces this and falls back to
`u16be(payload[2:4])` when the chunk would not fit (**unverified**). The
font is published to the renderer once 8192 bytes of the primary table
have arrived; all cells are then repainted. No ack is counted for font
packets.

### 6.3 Color Palette (0x88, `ib`)

```
0..1   u16be entry count (16)
2      unused
3      pad
4..    count x {r, g, b, pad}
```
Installs the text-mode palette (ARGB) and repaints all cells. Counted for
the ack.

---

## 7. Control-channel messages the video layer depends on

All are `BEEF <type16> <len16=0x0010>` + 8-byte payload, sent through the
control writer `X` (`r.F()`):

| type | class | name | when | payload |
|-----:|-------|------|------|---------|
| 0x0301 (769) | `b.a.n` | ScreenRefresh | `r.b()`: user "Refresh", or checksum error with refreshOnError; also on `jb` Display Resolution Response (0x8201) | 8 zero bytes |
| 0x0403 (1027) | `b.a.k` | SetDVCColorDepthMessage | DVC packet type changed | `01 00 00 00 00 00 00 00` |
| 0x030E (782) | `b.a.db` | Video Enable Request | `r.y()` after Video Connect Status / login | `01 01 00 00 00 00 00 00` |
| 0x0302 (770) | `b.a.x` | Set Display Area | `r.y()` | `04 00 03 00 00 00 00 00` (1024, 768) |
| 0x0304 (772) | `b.a.o` | SetScaleMode1to1 | `r.y()` | 8 zero bytes |

`VideoStream` needs from the session:

* `Control VideoControlSender` — to send 0x0403 and 0x0301.
* A handler for `OnConnectStatus(true)` that performs the `r.y()` sequence.
* Nothing else: acks are written on the video socket by `VideoStream`.

Control-channel packets that are informational for video: `jb` Display
Resolution Response (0x8201: u16 w, u16 h -> viewer sends ScreenRefresh),
`lb` Input Resolution (0x8200/0x8206), `l` colour model response (0x8204:
depth, colour flag).

---

## 8. Go API (`pkg/kvm`)

```go
fb := kvm.NewFramebuffer()                 // 1024x768 black until the first frame
v  := kvm.NewVideoStream(conn, fb, logger) // conn: video socket after the ac packet
v.Control = session                        // implements SendControlPacket(typ uint16, payload []byte) error
v.OnConnectStatus = func(ok bool) { /* send 782, 770, 772 on control channel */ }
v.SetTrace(true)                           // hex dump each packet header
err := v.Run(ctx)                          // blocks; io.EOF when the server closes
v.RequestRefresh()                         // -> control channel 0x0301 (ErrNoControlChannel without Control)
v.Stats()                                  // counters incl. DVC commands/pixels/checksum errors
```
`Framebuffer`: `Size`, `Resize`, `Snapshot`, `CopyRect`, `Subscribe`,
`WritePNG`, `Frames`.

---

## 9. Unverified assumptions

1. The 4 leading payload bytes of DVC/text packets and payload byte 9 are
   unused by the client; their meaning is unknown.
2. APCP frames never appear after the auth packet on the video socket; if
   they do, the Go reader skips them (the Java would desynchronise).
3. The exact number of unacknowledged packets after which the appliance
   stalls is unknown; the ack cadence (20) matches the viewer.
4. A Make Pixel truncated by an EOF packet is dropped (Java would read its
   operands from the next packet unless that packet has BOF).
5. Mode change mid-frame: the Go port decodes each byte with the colour
   mode of the packet that carried it; the Java keeps stale state in the
   previous decoder object (undefined behaviour). Assumed not to happen.
6. Checksums for DVC7_GRAY / DVC23 use the palette reverse lookup exactly
   like the Java; probably always 0 from the appliance.
7. Font Table offset field / chunking, see §6.2.
8. `Copy Above` on the first row: length is consumed but the cursor does
   not move (Java behaviour reproduced); presumably never emitted.
9. The 0x86 ASpeed JPEG format (iDRAC7/8) is decoded by `aspeed.go`; its
   own open points are listed in §10.6.
10. Text-mode blinking (attribute bit 7 and cursor) is rendered steadily.
11. `Video Connect Status` is assumed to arrive on the video socket (it is
    in the 0x8x family and the login path relies on it to trigger
    `r.y()`); if it arrives on the control channel instead the session must
    perform the `r.y()` sequence itself.

---

## 10. ASpeed JPEG video (type 0x86)

iDRAC7/8 (and iDRAC6 firmware with the ASpeed path enabled) send the
remote console as frames compressed by the AST2x00 video engine. The viewer
decodes them with the "BishaDecode" codec. Java classes (iDRAC6 viewer;
the iDRAC8 viewer carries the same classes renamed, the logic is identical
down to the constants):

| iDRAC6 | iDRAC8 | role |
|--------|--------|------|
| `com.avocent.kvm.b.a.gb` | `com.avocent.kvm.a.c.g` | packet parser, `a(byte[],byte[],int)` |
| `com.avocent.kvm.b.n.a(gb)` | `com.avocent.d.g.a.b.a(g)` | fragment reassembly, ack |
| `com.avocent.kvm.b.s.q()` | `com.avocent.d.d.g` (line ~230) | hands the header fields to the codec |
| `com.avocent.kvm.a.a.a` | `com.avocent.kvm.a.c.e` | frame setup `a(...)`, block loop `d()` / `b()` |
| `com.avocent.kvm.a.a.d` | `com.avocent.kvm.a.c.b` | bit reader, Huffman, IDCT, YUV->RGB, tables |
| `com.avocent.kvm.a.a.e` / `f` | `com.avocent.kvm.a.c.c` / `d` | VQ palette / Huffman table struct |
| `com.avocent.kvm.a.a.b` | `com.avocent.kvm.a.c.a` | cursor bitmap (mode 2/3) |
| `com.avocent.kvm.c.nb` | | decoder thread: calls `d()` until it returns -4 |

Go: `pkg/kvm/aspeed.go` (`aspeedDecoder`), `pkg/kvm/aspeed_tables.go`
(constant tables), `pkg/kvm/aspeed_test.go`.

### 10.1 Payload layout (`gb.a(byte[],byte[],int)`)

```
off  len  field
0    1    mode. bit0: 0 = YUV 4:4:4, 8x8 blocks; 1 = YUV 4:2:0, 16x16 macroblocks.
          Values 2 and 3: the payload is a hardware cursor bitmap, not video
          (§10.5). Other bits are ignored (mode & 1).
1    3    u24be: cursor x (bits 23..12) / cursor y (bits 11..0)
4    2    u16be height
6    2    u16be width
8    1    flags: bit0 = first fragment of a frame (gb.r() / g.j())
                 bit1, bit2, bit3 = parsed (gb.l/n/y, g.k/l/m) but never read
                 bit4 = pad the block-row count to a multiple of 16 lines (gb.s() / g.v())
9    2    u16be: cursor hotspot x (bits 15..10) / hotspot y (bits 9..4) / cursor id (bits 3..0)
11   1    high nibble = luminance quantisation table selector (0..7)
          low nibble  = chrominance quantisation table selector (0..7)
12   ..   compressed data
```

The session passes to the codec, in this order
(`a.a(width, height, mode, YTable=payload[11]>>4, UVTable=payload[11]&15,
data, len, cursorId, cursorX, cursorY, hotX, hotY, padFlag)`), so the
selector nibbles are the only "quality" information on the wire; the
`S..V` scale divisors in the codec are constant 16 (factor 1).

Fragments: a packet with flags bit0 set starts a frame; every following
packet without it is appended (`gb.a(gb)` copies only the data). The
accumulated frame is decoded **when the next first-fragment packet
arrives** (`n.a(gb)`: `if (gb2.r()) { if (k != null) model.a(k); k = gb2; }
else k.a(gb2)`). There is no explicit end-of-frame flag; the picture is
therefore always one frame behind the wire. Every 0x86 packet counts
towards the Video Ack (§4). The first packet of a frame also switches the
viewer to `graphicsmode` (text mode off).

### 10.2 Bitstream and block grammar (`a.d()`, `d.a(int)`)

The data is read as little-endian 32-bit words (`d.a(byte[],int,int)`; a
trailing partial word is dropped), bits consumed MSB first through a
32-bit window `nb` with a one-word lookahead `mb` (`d.a(int)`/`d.c(int)`
consume, `d.b(int)` peek). The reader pre-fetches a word as soon as the
lookahead is exactly used up, so a frame needs one spare word after its
last consumed bit; at least two words must be present.

Each block starts with a 4-bit code taken from the top of the window:

| code | name (iDRAC8 string table) | header | payload |
|-----:|----------------------------|--------|---------|
| 0  | `JPEG_NO_SKIP_CODE`          | 4 bits  | JPEG macroblock, tables Y/UV |
| 8  | `JPEG_SKIP_CODE`             | 20 bits | same, with position |
| 4  | `LOW_JPEG_NO_SKIP_CODE`      | 4 bits  | JPEG macroblock, fixed coarse tables (selector 0) |
| 12 | `LOW_JPEG_SKIP_CODE`         | 20 bits | same, with position |
| 5  | `VQ_NO_SKIP_1_COLOR_CODE`    | 4 bits  | 1 palette entry; 64 pixels, no index bits |
| 13 | `VQ_SKIP_1_COLOR_CODE`       | 20 bits | |
| 6  | `VQ_NO_SKIP_2_COLOR_CODE`    | 4 bits  | 2 palette entries; 64 x 1-bit indices |
| 14 | `VQ_SKIP_2_COLOR_CODE`       | 20 bits | |
| 7  | `VQ_NO_SKIP_4_COLOR_CODE`    | 4 bits  | 4 palette entries; 64 x 2-bit indices |
| 15 | `VQ_SKIP_4_COLOR_CODE`       | 20 bits | |
| 9  | frame end (`d.t`)            | 4 bits  | stops the frame |
| 1,2,3,10,11 | unknown              |         | `d()` returns -1 -> "Decoder error" |

A 20-bit skip header is `code(4) column(8) row(8)` in block units; the
block position is set from it. Non-skip blocks follow the previous block
in row-major order (`d.e()`: column wraps at `width/bs`, row wraps at
`paddedHeight/bs`, bs = 8 or 16). Positions start at (0,0) for each frame.

JPEG macroblock: 4:4:4 = one Y block, one Cb, one Cr; 4:2:0 = four Y
blocks (top-left, top-right, bottom-left, bottom-right), one Cb, one Cr
covering 16x16. Each block is a baseline-JPEG Huffman coded 8x8
coefficient block: DC category symbol + magnitude bits (differential
against a per-component predictor reset at frame start), then
run/size AC symbols with EOB (0x00) and ZRL (0xF0, skips 16). Y uses DC
table 0 / AC table 0 (JPEG Annex K luminance tables), Cb and Cr use DC
table 1 / AC table 1 (chrominance tables). Dequantisation uses the AAN
scaled float table, the IDCT is the fixed-point AAN variant in
`d.a(short[],char[],char)` (constants 362, 473, 277, -669 with 8
fraction bits, output `>> 3` then +128 via the wrap-around clamp table
`d.qb`). Colour conversion (`d.a(int,int,char[][],int[])`):
`R = clamp(1.164(Y-16) + 1.597656(Cr-128))`, `G = clamp(1.164(Y-16) -
0.390625(Cb-128) - 0.8125(Cr-128))`, `B = clamp(1.164(Y-16) +
2.015625(Cb-128))` with the exact fixed-point rounding of `d.a()`. In
4:2:0 each chroma sample covers a 2x2 pixel group.

VQ macroblock: each palette entry header is `newColour(1) slot(2)`; when
`newColour` is 1, 24 bits `Y Cb Cr` follow and are stored in that slot.
The four-slot palette starts every frame as black, white, grey, light grey
(`e.a()`: 0x008080, 0xFF8080, 0x808080, 0xC08080) and persists across
blocks within the frame. The 64 pixel indices select among the entries
*named in this block's header* (slot = `entry[index]`). Only an 8x8 pixel
block is generated; in 4:2:0 mode the Java conversion fails on the missing
planes (exception caught and printed) and nothing is drawn, which the Go
port reproduces (bits are still consumed).

### 10.3 Table selection

* Quantisation (`d.d()`, `d.a/b/c/d(float[])`): the Y selector picks one of
  eight luminance tables (`d.Tb, Rb, Pb, Nb, Lb, Jb, Hb, Fb` for 0..7, 0 is
  coarsest), the UV selector one of eight chrominance tables (`d.Ub, Sb,
  Qb, Ob, Mb, Kb, Ib, Gb`). `LOW_JPEG` blocks use luminance table 0 and
  chrominance table 0 regardless of the header (`d.Y = d.Z = 0`, never
  assigned). Every entry is `q*16/16` clamped to 1..255 (so the tables are
  used as-is) and scaled by the AAN factors. Go: `aspeedQuantLuma`,
  `aspeedQuantChroma`, `aspeedBuildQuant`.
* Huffman: four DC and four AC table slots exist (`d.tb[]`, `d.ub[]`), only
  0 (luma) and 1 (chroma) are filled (`d.c()`) and used.
* Selector values 8..15 have no `case` in the Java switch (the table
  reference stays `null` -> NullPointerException). The Go decoder returns
  an error for the frame.

### 10.4 Frame setup and placement (`a.a(...)` private, `d.a(int,int)`)

* Mode/table change rebuilds the quant tables and logs
  `CompressionMode`/`YTable`/`UVTable`.
* Resolution change: width/height must be 1..4096, else "Bad video frame
  size". The padded height used for the block-row wrap is 608 for 600-line
  4:2:0 frames, otherwise rounded up to a multiple of 16 when flags bit4 is
  set. The pixel model (`c.i.a(int,int)`) ignores sizes above 2000 in
  either dimension; the Go port then logs and leaves the framebuffer as
  is, clipping blocks.
* Blocks are written through the model's `k.b(x,y,w,h,int[])`, which copies
  rows out of a linear full-frame buffer and drops only rows past the end
  of the buffer. The Go port clips each block to the image rectangle
  instead (a block past the right edge would wrap into the next row in the
  Java).
* The frame is finished at code 9; `VideoStream` counts it in
  `ASpeedFrames` and `Framebuffer.Frames()`.
* Cursor: `cursorId > 0` shows cursor image `cursorId` at (cursorX,
  cursorY) with the given hotspot, `cursorId == 0` hides it
  (`k.c(boolean)`, `k.a(Integer)`, `k.a(int,int,int,int)`). The viewer
  draws it as an overlay in the Swing component, never into the pixel
  model; the Go decoder parses and stores the state (`aspeedDecoder.cursor`)
  but does not render it.

### 10.5 Cursor bitmaps (mode 2 and 3, class `b`)

`width*height` 16-bit little-endian pixels must be present (else
RuntimeException). Mode 3 is ARGB4444 (each nibble replicated to 8 bits).
Mode 2: bit15 = 0 -> opaque RGB444; bit15 = 1 and bit14 = 1 -> inverted
colour (XOR cursor, drawn opaque with `~r,~g,~b`); otherwise transparent.
The image is stored under the packet's cursor id.

### 10.6 Unverified assumptions (ASpeed)

1. Frame latency: decoding only on the next first-fragment packet is the
   viewer's behaviour; it assumes the appliance keeps sending frames. Flags
   bits 1..3 are unused by the viewer and might mark the last fragment; the
   Go decoder offers `flush()` but nothing calls it.
2. VQ blocks in 4:2:0 mode are consumed but not drawn (Java exception
   path); presumably the engine never emits them in that mode.
3. Huffman decode failures (no code matches 16 bits), AC coefficient
   indices >= 64 and unknown block codes abort the frame with an error. The
   Java keeps going in the first two cases (DC stays 0 / coefficient
   skipped) and throws "Decoder error" in the third; valid streams never
   hit them.
4. The `hb*4 <= f` end-of-data test in `a.b()` can never trigger (the
   reader throws first), so a frame without the end code ends with a
   truncation error in Go, after the blocks decoded so far have been drawn.
5. Block clipping instead of linear wrap (10.4); never relevant for widths
   that are multiples of the block size.
6. The float IDCT dequantisation follows Java `float` arithmetic; Go
   `float32` should round identically but this has not been checked
   against the viewer on real frames.
7. Only one cursor-state consumer existed in the viewer (the overlay); the
   Go port keeps the cursor data internal.
8. No real 0x86 capture was available; the decoder is verified against
   hand-built bitstreams only (`aspeed_test.go`).

### 10.7 Go API

`VideoStream` decodes 0x86 automatically. `VideoStats` gains
`ASpeedPackets`, `ASpeedFrames`, `ASpeedErrors` and the detail struct
`ASpeed ASpeedStats` (`Packets, Fragments, Dropped, Frames, Blocks,
JPEGBlocks, VQBlocks, SkipCodes, Errors, Resizes, CursorPackets`). The
decoder resizes the framebuffer when the advertised size changes (<= 2000
per axis) and reports `Framebuffer.Frames()` per completed frame.
