# idrac — native Go client for Dell iDRAC6/7/8/9

One binary that replaces the Java tooling for Dell iDRACs, as a
graphical manager and as a CLI: inventory, power, sensors, logs, racadm, raw
Redfish / legacy-web calls, and the **remote console (KVM) without Java** — a
native console window, screenshots, key and mouse injection, and a VNC bridge.

| Surface | iDRAC6 | iDRAC7/8 | iDRAC9 |
|---|---|---|---|
| racadm over SSH | yes | yes | yes |
| Redfish | n/a | yes | yes |
| Legacy web API (`/data?get=`) | yes | yes (unused when Redfish works) | partial |
| Virtual console (Avocent APCP/DVC protocol) | yes | yes (protocol v2.34, see `docs/kvm-idrac8-notes.md`) | untested |

## Graphical manager

Run `idrac` with no arguments to open the manager window:

- **Host list** with add, edit and remove. Entries are saved to the hosts file,
  and each shows its generation and whether it is reachable.
- **Console tab** per server: the remote console, connected when you press
  Connect. A session stays open while you look at other tabs or servers, and
  **Pop Out** moves it to a window of its own (closing that window brings it
  back), so several can be on screen at once.
- **Other tabs per server**: Overview (power, next boot, identify LED), Sensors,
  Logs (event log and lifecycle log, clear), Jobs, Virtual Media, BIOS,
  Accounts, racadm, raw API calls (Redfish and the legacy web API), and
  Maintenance (configuration profile export/import, firmware update, iDRAC
  attributes, probe, reboot the iDRAC).

Every tab runs the corresponding CLI command in-process, so the GUI and the
command line always behave the same. Anything that changes state asks first.

The manager starts with no server selected, so nothing is read (and no
password asked for) until you pick one.

Passwords follow the hosts file (stored, environment variable, 1Password
reference or command). With none configured, the manager asks once per session
and keeps the answer in memory only, unless you tick "Save in the hosts file":
then it is stored there, in plain text, once the iDRAC has accepted it. A
rejected password is forgotten and never retried automatically, because iDRACs
lock accounts.

To try it with no hardware, add a host with generation "Demo": it serves canned
data and a test-pattern console.

The interactive `ssh` shell is the one CLI function without a tab; use the
racadm tab, or `idrac -host <h> ssh` in a terminal.

## Build

```sh
# install the latest release straight from source (needs Go, cgo and the GUI headers below)
GOPRIVATE='github.com/the-curve-consulting/*' go install -tags gui github.com/the-curve-consulting/modern-idrac-client/cmd/idrac@latest

./build.sh          # vet + test + build bin/idrac for this machine
./build.sh all      # also linux/arm64, darwin/arm64, windows/amd64 into bin/
./run.sh <args>     # build if sources changed, then run: ./run.sh -host server01 kvm
go build -o idrac ./cmd/idrac   # plain go build works too
```

Go 1.27+. The CLI depends only on `golang.org/x/crypto` (SSH) and `golang.org/x/term`; the viewer adds Fyne.

### Releases

Every push and pull request runs CI (format, vet, tests, both builds).
Pushing a tag publishes a GitHub release with binaries:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

| Archive | Viewer window |
|---|---|
| `idrac-<tag>-linux-amd64.tar.gz` | yes |
| `idrac-<tag>-darwin-arm64.tar.gz` | yes |
| `idrac-<tag>-windows-amd64.zip` | yes |
| `idrac-<tag>-linux-arm64-cli.tar.gz` | no (CLI and `kvm vnc`) |

`idrac version` shows which build you have.

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
idrac -host server01 probe              # detect generation / Redfish (no credentials)
idrac -host server01 info               # model, service tag, firmware, power, health
idrac -host server01 power [status|on|off|graceful|reset|cycle|nmi]
idrac -host server01 sensors
idrac -host server01 sel [-n 100] [clear]
idrac -host server01 lclog
idrac -host server01 led on|off
idrac -host server01 boot pxe|hdd|cd|bios|usb|none
idrac -host server01 racadm getsysinfo  # any racadm command, all generations
idrac -host server01 ssh                # interactive iDRAC shell
idrac -host server01 redfish get /redfish/v1/Systems/System.Embedded.1
idrac -host server01 redfish post /redfish/v1/Systems/System.Embedded.1/Actions/ComputerSystem.Reset '{"ResetType":"On"}'
idrac -host server01 jobs | vmedia | accounts | bios | attrs | scp export | update <uri> | reset-idrac
idrac -host legacy01 web get sysDesc,pwState # raw legacy /data?get= call (iDRAC6)
idrac -host legacy01 web set pwState=1       # raw legacy /data?set= call
idrac -host legacy01 web jnlp                # the console JNLP the browser would get
```

### Remote console: `kvm`

One command covers the console. With no verb it opens the viewer window.

```
idrac -host server01 kvm                        # console window (same as: kvm view)
idrac -host server01 kvm view -view-only
idrac -host server01 kvm -record session.rec    # window, also saving the raw video stream
idrac -host server01 kvm screenshot out.png
idrac -host server01 kvm key F2                 # F1..F24, Return, Escape, ctrl+alt+F2, a
idrac -host server01 kvm type "root\n"          # \n = Enter
idrac -host server01 kvm mouse 640 400 click
idrac -host server01 kvm ctrl-alt-del
idrac -host server01 kvm vnc                    # serve the console to any VNC viewer on 127.0.0.1:5901
idrac -host server01 kvm probe                  # APCP + TLS handshake only, no login
idrac kvm replay session.rec                          # play a recording in the window
idrac kvm replay session.rec out.png                  # decode it headless, print decoder stats
idrac kvm demo                                        # test pattern: checks rendering and input locally
```

The window is the Java client's replacement: live video scaled to the window
(or 1:1 / full screen), keyboard sent as physical keys so the server applies
its own layout, absolute mouse, and menus for

- **Macros**: Ctrl+Alt+Del, Alt+Tab, Alt+F4, SysRq, Super, Alt+F1..F12, Ctrl+Alt+F1..F12 and the other combos your window manager would swallow
- **Power**: on, graceful shutdown, forced off, reset, power cycle, NMI (each asks first)
- **Next Boot**: normal, PXE, BIOS setup, CD/DVD, hard disk
- **File**: save screenshot, paste the clipboard as keystrokes, reconnect
- **View**: refresh, actual size, full screen, smooth scaling, view only
- **Tools**: session statistics, identify LED

The status bar shows connection state, resolution, frame rate and host power
state. If the session drops, it offers to reconnect.

The window needs cgo and OpenGL, X11 and Wayland headers at build time (`-tags gui`; on Debian/Ubuntu: `gcc libgl1-mesa-dev xorg-dev libxkbcommon-dev libwayland-dev libegl1-mesa-dev`);
`./run.sh` and `./build.sh` enable it automatically and fall back to a
window-less binary if the toolchain is missing. The cross-compiled binaries
from `./build.sh all` have no window; use `kvm vnc` there, which is also the way
to reach a console from a machine with no display.

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
- `pkg/kvm` — Avocent console protocol: APCP/TLS transport, control channel,
  DVC and ASpeed video decoders, framebuffer, keyboard/mouse, record/replay,
  RFB (VNC) server bridge
- `pkg/viewer` — the graphical console window (Fyne, build tag `gui`)
- `pkg/webapi` — legacy web-UI API shared by iDRAC6/7/8
- `docs/` — byte-level protocol notes recovered from the Dell viewers

## Verification status

Confirmed on real hardware (iDRAC8, firmware 2.86): console login, ASpeed
video, the console window and the VNC bridge.

Exercised without credentials on iDRAC6 (firmware 2.92) and iDRAC8: generation
detection, the Redfish service root, SSH negotiation for racadm, and the
console transport (APCP handshake and TLS upgrade) via `kvm probe`.

Implemented from the firmware's own UI source, Dell's Redfish documentation
and the Dell viewer, but not yet run against a device: the iDRAC6 console path
(DVC video), the legacy web API commands, and the less common Redfish calls
(jobs, virtual media, BIOS, configuration profiles, firmware update).

Suggested first run on a new generation, with `-v` (and `-trace` for kvm):

1. `idrac -host <host> probe` and `idrac -host <host> kvm probe` (no credentials)
2. `idrac -host <host> racadm getsysinfo`
3. `idrac -host <host> info`, `sensors`, `sel`
4. `idrac -host <host> kvm screenshot shot.png`
5. `idrac -host <host> kvm` and test keyboard and mouse

Every assumption that could not be checked is listed under "unverified" in
`docs/kvm-control-channel.md` and `docs/kvm-video-channel.md`. iDRACs lock
accounts after repeated bad logins; the tool makes exactly one attempt per
command.

Known gaps: virtual media over the console protocol (AVMP) is not implemented
(Redfish `vmedia` covers iDRAC8); the iDRAC8 HTML5 console WebSocket path is
not used (the Java viewer's APCP path is, which the iDRAC still serves);
iDRAC9 has only been considered through Redfish and is untested.
