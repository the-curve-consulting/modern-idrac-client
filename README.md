# idrac — native Go client for Dell iDRAC6/7/8/9

A single static binary that replaces the Java tooling for the office iDRACs:
inventory, power, sensors, logs, racadm, raw Redfish / legacy-web calls, and
the **remote console (KVM) without Java** — screenshots, key and mouse
injection, and a VNC bridge so any VNC viewer becomes the console.

| Surface | iDRAC6 (r710) | iDRAC7/8 (pmx nodes) | iDRAC9 |
|---|---|---|---|
| racadm over SSH | yes | yes | yes |
| Redfish | n/a | yes | yes |
| Legacy web API (`/data?get=`) | yes | yes (unused when Redfish works) | partial |
| Virtual console (Avocent APCP/DVC protocol) | yes | yes (protocol v2.34, see `docs/kvm-idrac8-notes.md`) | untested |

## Build

```sh
./build.sh          # vet + test + build bin/idrac for this machine
./build.sh all      # also linux/arm64, darwin/arm64, windows/amd64 into bin/
./run.sh <args>     # build if sources changed, then run: ./run.sh -host r710 info
go build -o idrac ./cmd/idrac   # plain go build works too
```

Go 1.27+, two dependencies (`golang.org/x/crypto` for SSH, `golang.org/x/term`).

## Configure hosts

Copy `idrac.example.json` to `~/.config/idrac/hosts.json` (or `./idrac.json`,
or point `IDRAC_CONFIG` at it). Passwords can be a literal, an env var name,
a 1Password reference (`op read`), or a command. `IDRAC_PASSWORD` overrides
everything for one-off use, `-host` accepts a bare IP when the host is not in
the file, and when no password source is configured the tool prompts for one
on the terminal (hidden input).

## Commands

```
idrac hosts                         # what is configured
idrac -host r710 probe              # detect generation / Redfish (no credentials)
idrac -host r710 info               # model, service tag, firmware, power, health
idrac -host r710 power [status|on|off|graceful|reset|cycle|nmi]
idrac -host r710 sensors
idrac -host r710 sel [-n 100] [clear]
idrac -host pmx-a lclog
idrac -host r710 led on|off
idrac -host r710 boot pxe|hdd|cd|bios|usb|none
idrac -host r710 racadm getsysinfo  # any racadm command, all generations
idrac -host r710 ssh                # interactive iDRAC shell
idrac -host pmx-a redfish get /redfish/v1/Systems/System.Embedded.1
idrac -host pmx-a redfish post /redfish/v1/Systems/System.Embedded.1/Actions/ComputerSystem.Reset '{"ResetType":"On"}'
idrac -host pmx-a jobs | vmedia | accounts | bios | attrs | scp export | update <uri> | reset-idrac
idrac -host r710 web get sysDesc,pwState   # raw legacy /data?get= call
idrac -host r710 web set pwState=1         # raw legacy /data?set= call
idrac -host r710 web jnlp                  # the console JNLP the browser would get
idrac -host r710 kvm probe                 # APCP + TLS handshake only, no login
idrac -host r710 kvm screenshot out.png
idrac -host r710 kvm key F1                # key names: F1..F24, Return, Escape, ctrl+alt+F2, ctrl-alt-del, a
idrac -host r710 kvm type "root\n"         # type text (\n = Enter)
idrac -host r710 kvm mouse 640 400 click
idrac -host r710 kvm vnc -listen :5901     # expose the console to any VNC viewer
```

`kvm` logs in to port 5900 with the configured account first (that is what the
iDRAC6 JNLP does) and, if the console refuses it, repeats the browser's launch
sequence: web login, fetch `viewer.jnlp`, use the one-time credentials inside.
`-via-web` forces the second path, `-direct` the first.

Add `-v` for request logging, `-trace` to hex-dump console protocol packets,
`-json` for machine-readable output.

## Layout

- `cmd/idrac` — CLI
- `pkg/config` — hosts file + credential resolution
- `pkg/redfish` — Redfish client with Dell OEM helpers
- `pkg/racadm` — racadm over SSH
- `pkg/idrac6` — legacy `/data` XML API (login, ST2 token, get/set, JNLP)
- `pkg/kvm` — Avocent console protocol: APCP/TLS transport, control channel,
  DVC video decoder, framebuffer, keyboard/mouse, RFB (VNC) server bridge
- `docs/` — byte-level protocol notes recovered from the Dell viewers

## Verification status

No iDRAC credentials were available while this was built, so everything that
needs a login is implemented from the firmware's own UI source, Dell's Redfish
documentation and the decompiled `avctKVM.jar`, and has **not** been run against
a device yet. What *was* exercised live (unauthenticated) on the R710 (iDRAC6
2.92) and the five iDRAC8 2.86 hosts:

- generation detection (`probe`), Redfish service root and `$metadata`
- the console transport: APCP handshake and TLS upgrade on the control and
  video sockets of both generations (`kvm probe`)
- the login endpoints' *failure* shape (one deliberate bad login per device,
  done by hand during the survey; the tool itself never retries a login)
- SSH algorithm negotiation for racadm

First real run checklist, in this order, with `-v` (and `-trace` for kvm):

1. `idrac -host r710 racadm getsysinfo` and `idrac -host <pmx> racadm getsysinfo`
2. `idrac -host <pmx> info`, `sensors`, `sel` (Redfish) and `idrac -host r710 info` (legacy API)
3. `idrac -host r710 kvm screenshot r710.png` — DVC video path
4. `idrac -host <pmx> kvm screenshot pmx.png` — ASpeed JPEG video path
5. `idrac -host r710 kvm vnc` and connect a viewer; test keyboard and mouse

Every assumption that could not be checked is listed under "unverified" in
`docs/kvm-control-channel.md` and `docs/kvm-video-channel.md`. iDRACs lock
accounts after repeated bad logins; the tool makes exactly one attempt per
command.

Known gaps: virtual media over the console protocol (AVMP) is not implemented
(Redfish `vmedia` covers iDRAC8); the iDRAC8 HTML5 console WebSocket path is
not used (the Java viewer's APCP path is, which the iDRAC still serves);
iDRAC9 has only been considered through Redfish and is untested.
