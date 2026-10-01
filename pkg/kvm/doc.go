// Package kvm implements a native Go client for the Avocent virtual console
// protocol used by Dell iDRAC6/7/8 (the protocol behind avctKVM.jar), plus an
// RFB (VNC) server so any VNC viewer can drive the console.
//
// Files:
//
//	transport.go   APCP pre-handshake, TLS upgrade, "BEEF" frame reader/writer
//	packets.go     control-channel packet catalogue (login, input, video setup, ...)
//	session.go     Session: connect, authenticate, control loop, open video socket
//	input.go       keyboard/mouse helpers; implements InputSink for the VNC bridge
//	keymap.go      X11 keysym / rune -> USB HID usage tables
//	keynames.go    human key names ("F1", "ctrl", "Return") -> HID usage
//	video.go       video socket framing, acks, dispatch to decoders
//	dvc.go         Avocent DVC (7/15/23-bit) decoder
//	aspeed.go      ASpeed JPEG decoder (iDRAC7/8)
//	textmode.go    VGA text-mode renderer (font/palette packets)
//	framebuffer.go RGBA framebuffer with dirty-rect subscribers and PNG export
//	console.go     Console: Session + VideoStream + Framebuffer glue
//	vnc.go         RFB 3.8 server bridge
//
// See docs/kvm-control-channel.md, docs/kvm-video-channel.md and
// docs/kvm-idrac8-notes.md for the byte-level protocol notes.
package kvm
