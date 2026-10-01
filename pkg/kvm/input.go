package kvm

import (
	"errors"
	"fmt"
	"time"
)

// Keyboard and mouse helpers on top of Session. Wire details:
//
//   - KeyboardDataRequest (0x0200): HID usage id, press/release
//     (com.avocent.kvm.b.g -> com.avocent.kvm.b.a.f).
//   - Mouse Data Request (0x0201): absolute position in video pixels plus
//     the full button state and a wheel delta (com.avocent.kvm.b.h ->
//     com.avocent.kvm.b.a.gc). The viewer keeps the button state in
//     r.ab/E() and ORs/ANDs the pressed button on every event; wheel events
//     are sent as a "press" packet with the wheel field set.
//   - MouseOrigin (0x0202): sent when the pointer enters the video panel.
//
// The Java viewer refuses to send input unless the session is RUNNING and
// not view-only (com.avocent.kvm.c.h.q()); the same rule applies here.

// ErrNotRunning is returned by input helpers before the session is RUNNING.
var ErrNotRunning = errors.New("kvm: session not running")

// ErrViewOnly is returned when the appliance granted a view-only session.
var ErrViewOnly = errors.New("kvm: session is view-only")

// ErrUnmappedKey is returned for keysyms without a US keyboard equivalent.
var ErrUnmappedKey = errors.New("kvm: keysym has no HID mapping")

func (s *Session) inputAllowed() error {
	if s.State() != StateRunning {
		return ErrNotRunning
	}
	if lr := s.LoginResponse(); lr != nil && lr.ViewOnly() {
		return ErrViewOnly
	}
	return nil
}

// KeyDown sends a key press for a USB HID usage id.
func (s *Session) KeyDown(usage uint16) error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	return s.Send(&KeyboardData{Usage: usage, Down: true})
}

// KeyUp sends a key release for a USB HID usage id.
func (s *Session) KeyUp(usage uint16) error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	return s.Send(&KeyboardData{Usage: usage, Down: false})
}

// Press taps a key (press then release).
func (s *Session) Press(usage uint16) error {
	if err := s.KeyDown(usage); err != nil {
		return err
	}
	return s.KeyUp(usage)
}

// KeyEvent implements the VNC bridge's InputSink: an X11 keysym press or
// release. Unmapped keysyms return ErrUnmappedKey.
func (s *Session) KeyEvent(keysym uint32, down bool) error {
	usage, ok := KeysymToUsage(keysym)
	if !ok {
		return fmt.Errorf("%w: 0x%x", ErrUnmappedKey, keysym)
	}
	if down {
		return s.KeyDown(usage)
	}
	return s.KeyUp(usage)
}

// TypeString types text using the US layout, holding Left Shift for
// characters that need it. Characters without a mapping are skipped.
// delay, when > 0, is inserted between key events (BIOS/OS keyboard
// drivers can drop very fast input).
func (s *Session) TypeString(text string, delay time.Duration) error {
	for _, r := range text {
		usage, shift, ok := CharToKey(r)
		if !ok {
			s.log.Printf("kvm: TypeString: no key for %q", r)
			continue
		}
		if shift {
			if err := s.KeyDown(HIDKeyLeftShift); err != nil {
				return err
			}
			pause(delay)
		}
		if err := s.Press(usage); err != nil {
			return err
		}
		pause(delay)
		if shift {
			if err := s.KeyUp(HIDKeyLeftShift); err != nil {
				return err
			}
			pause(delay)
		}
	}
	return nil
}

func pause(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

// Chord presses the given keys in order and releases them in reverse
// order (e.g. Chord(HIDKeyLeftCtrl, HIDKeyLeftAlt, HIDKeyDelete)).
func (s *Session) Chord(usages ...uint16) error {
	for _, u := range usages {
		if err := s.KeyDown(u); err != nil {
			return err
		}
	}
	for i := len(usages) - 1; i >= 0; i-- {
		if err := s.KeyUp(usages[i]); err != nil {
			return err
		}
	}
	return nil
}

// CtrlAltDel sends the Ctrl+Alt+Delete chord.
func (s *Session) CtrlAltDel() error {
	return s.Chord(HIDKeyLeftCtrl, HIDKeyLeftAlt, HIDKeyDelete)
}

// ReleaseAllKeys releases every modifier (useful when a VNC client
// disconnects mid-chord).
func (s *Session) ReleaseAllKeys() error {
	for _, u := range []uint16{HIDKeyLeftCtrl, HIDKeyLeftShift, HIDKeyLeftAlt, HIDKeyLeftGUI,
		HIDKeyRightCtrl, HIDKeyRightShift, HIDKeyRightAlt, HIDKeyRightGUI} {
		if err := s.KeyUp(u); err != nil {
			return err
		}
	}
	return nil
}

// RequestKeyboardLEDs asks the appliance for the LED state (answered by a
// Keyboard LED packet -> OnKeyboardLED).
func (s *Session) RequestKeyboardLEDs() error {
	return s.Send(NewKeyboardLEDRequest())
}

// SetFocus mirrors the viewer's FocusControl + Keyboard LED Request pair
// sent when the console window gains or loses focus.
func (s *Session) SetFocus(focused bool) error {
	if err := s.Send(&FocusControl{Focused: focused}); err != nil {
		return err
	}
	return s.RequestKeyboardLEDs()
}

// RefreshScreen requests a full frame (ScreenRefresh, 0x0301).
func (s *Session) RefreshScreen() error { return s.Send(NewScreenRefresh()) }

// SetDisplayArea sends Set Display Area (0x0302); the viewer always sends
// 1024x768 and the appliance streams whatever the host outputs.
func (s *Session) SetDisplayArea(w, h int) error {
	return s.Send(&SetDisplayArea{Width: w, Height: h})
}

// SetVideoEnabled sends Video Enable Request (0x030E).
func (s *Session) SetVideoEnabled(on bool) error { return s.Send(&VideoEnable{Enable: on}) }

// SetVideoTransmitLimit sets the bandwidth throttle (0 = none .. 4).
func (s *Session) SetVideoTransmitLimit(limit uint8) error {
	return s.Send(&SetVideoTransmitLimit{Limit: limit})
}

// MouseOrigin sends the MouseOrigin packet (0x0202); the viewer sends it
// when the pointer enters the video area, immediately followed by a move.
func (s *Session) MouseOrigin() error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	return s.Send(NewMouseOrigin())
}

func (s *Session) sendMouse(x, y, wheel int) error {
	s.mu.Lock()
	buttons := s.mouseButtons
	s.mouseX, s.mouseY = x, y
	s.mu.Unlock()
	return s.Send(&MouseData{Buttons: buttons, X: x, Y: y, Wheel: wheel})
}

// MouseMove sends an absolute pointer position in video pixel coordinates
// (0,0 = top-left of the remote screen) with the current button state.
func (s *Session) MouseMove(x, y int) error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	return s.sendMouse(x, y, 0)
}

// MouseButton presses (down=true) or releases the buttons in mask
// (MouseLeft|MouseRight|MouseMiddle) at the last known position.
func (s *Session) MouseButton(mask uint8, down bool) error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	s.mu.Lock()
	if down {
		s.mouseButtons |= mask
	} else {
		s.mouseButtons &^= mask
	}
	x, y := s.mouseX, s.mouseY
	s.mu.Unlock()
	return s.sendMouse(x, y, 0)
}

// MouseWheel sends a wheel event at (x,y); delta > 0 scrolls up (the
// viewer sends -getWheelRotation()).
func (s *Session) MouseWheel(x, y, delta int) error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	return s.sendMouse(x, y, delta)
}

// MouseMoveRelative sends a Mouse Delta Request (relative mode). The Java
// viewer never uses it on iDRAC; UNVERIFIED that the appliance honours it.
func (s *Session) MouseMoveRelative(dx, dy int) error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	s.mu.Lock()
	buttons := s.mouseButtons
	s.mu.Unlock()
	return s.Send(&MouseDelta{Buttons: buttons, DX: dx, DY: dy})
}

// RFB pointer-event button bits.
const (
	rfbButtonLeft      = 1 << 0
	rfbButtonMiddle    = 1 << 1
	rfbButtonRight     = 1 << 2
	rfbButtonWheelUp   = 1 << 3
	rfbButtonWheelDown = 1 << 4
)

// PointerEvent implements the VNC bridge's InputSink: absolute
// framebuffer coordinates with the RFB button mask (bit0 left, bit1
// middle, bit2 right, bit3 wheel up, bit4 wheel down). Wheel bits produce
// one wheel step per transition to pressed. Coordinates must already be
// in remote-screen pixels (the bridge serves the framebuffer 1:1).
func (s *Session) PointerEvent(x, y int, rfbMask uint8) error {
	if err := s.inputAllowed(); err != nil {
		return err
	}
	var buttons uint8
	if rfbMask&rfbButtonLeft != 0 {
		buttons |= MouseLeft
	}
	if rfbMask&rfbButtonMiddle != 0 {
		buttons |= MouseMiddle
	}
	if rfbMask&rfbButtonRight != 0 {
		buttons |= MouseRight
	}
	s.mu.Lock()
	prevWheel := s.rfbWheel
	s.rfbWheel = rfbMask & (rfbButtonWheelUp | rfbButtonWheelDown)
	s.mouseButtons = buttons
	s.mouseX, s.mouseY = x, y
	s.mu.Unlock()
	wheel := 0
	if rfbMask&rfbButtonWheelUp != 0 && prevWheel&rfbButtonWheelUp == 0 {
		wheel = 1
	}
	if rfbMask&rfbButtonWheelDown != 0 && prevWheel&rfbButtonWheelDown == 0 {
		wheel = -1
	}
	return s.Send(&MouseData{Buttons: buttons, X: x, Y: y, Wheel: wheel})
}
