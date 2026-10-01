package kvm

import (
	"strconv"
	"strings"
)

// keyNames maps human-friendly names (as typed on the command line or used
// by xdotool) to USB HID usages. Lookup is case-insensitive; single
// characters go through CharToKey.
var keyNames = map[string]uint16{
	"return": HIDKeyEnter, "enter": HIDKeyEnter, "kp_enter": HIDKeyKPEnter,
	"escape": HIDKeyEscape, "esc": HIDKeyEscape,
	"backspace": HIDKeyBackspace, "tab": HIDKeyTab, "space": HIDKeySpace,
	"delete": HIDKeyDelete, "del": HIDKeyDelete, "insert": HIDKeyInsert, "ins": HIDKeyInsert,
	"home": HIDKeyHome, "end": HIDKeyEnd, "pageup": HIDKeyPageUp, "prior": HIDKeyPageUp,
	"pagedown": HIDKeyPageDown, "next": HIDKeyPageDown,
	"up": HIDKeyUp, "down": HIDKeyDown, "left": HIDKeyLeft, "right": HIDKeyRight,
	"ctrl": HIDKeyLeftCtrl, "control": HIDKeyLeftCtrl, "control_l": HIDKeyLeftCtrl, "control_r": HIDKeyRightCtrl, "rctrl": HIDKeyRightCtrl,
	"alt": HIDKeyLeftAlt, "alt_l": HIDKeyLeftAlt, "alt_r": HIDKeyRightAlt, "ralt": HIDKeyRightAlt, "altgr": HIDKeyRightAlt,
	"shift": HIDKeyLeftShift, "shift_l": HIDKeyLeftShift, "shift_r": HIDKeyRightShift, "rshift": HIDKeyRightShift,
	"super": HIDKeyLeftGUI, "super_l": HIDKeyLeftGUI, "super_r": HIDKeyRightGUI, "win": HIDKeyLeftGUI, "meta": HIDKeyLeftGUI, "cmd": HIDKeyLeftGUI,
	"menu": HIDKeyMenu, "application": HIDKeyApplication,
	"capslock": HIDKeyCapsLock, "caps_lock": HIDKeyCapsLock, "numlock": HIDKeyNumLock, "num_lock": HIDKeyNumLock,
	"scrolllock": HIDKeyScrollLock, "scroll_lock": HIDKeyScrollLock,
	"print": HIDKeyPrintScreen, "printscreen": HIDKeyPrintScreen, "sysrq": HIDKeyPrintScreen, "pause": HIDKeyPause, "break": HIDKeyPause,
	"minus": HIDKeyMinus, "equal": HIDKeyEqual, "grave": HIDKeyGrave, "comma": HIDKeyComma, "period": HIDKeyPeriod,
	"slash": HIDKeySlash, "backslash": HIDKeyBackslash, "semicolon": HIDKeySemicolon, "apostrophe": HIDKeyApostrophe,
	"bracketleft": HIDKeyLeftBrace, "bracketright": HIDKeyRightBrace,
	"kp_add": HIDKeyKPAdd, "kp_subtract": HIDKeyKPSubtract, "kp_multiply": HIDKeyKPMultiply, "kp_divide": HIDKeyKPDivide, "kp_decimal": HIDKeyKPDecimal,
	"power": HIDKeyPower, "mute": HIDKeyMute, "volumeup": HIDKeyVolumeUp, "volumedown": HIDKeyVolumeDown,
}

// KeyNameToUsage resolves names like "F1", "Return", "ctrl", "a", "KP_5".
// Single printable characters map to their unshifted key (the shift state
// is the caller's business; TypeString handles it for text).
func KeyNameToUsage(name string) (uint16, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if u, ok := keyNames[n]; ok {
		return u, true
	}
	if strings.HasPrefix(n, "f") && len(n) <= 3 {
		if i, err := strconv.Atoi(n[1:]); err == nil && i >= 1 && i <= 24 {
			if i <= 12 {
				return HIDKeyF1 + uint16(i-1), true
			}
			return HIDKeyF13 + uint16(i-13), true
		}
	}
	if strings.HasPrefix(n, "kp_") && len(n) == 4 && n[3] >= '0' && n[3] <= '9' {
		if n[3] == '0' {
			return HIDKeyKP0, true
		}
		return HIDKeyKP1 + uint16(n[3]-'1'), true
	}
	r := []rune(name)
	if len(r) == 1 {
		if u, _, ok := CharToKey(r[0]); ok {
			return u, true
		}
	}
	return 0, false
}
