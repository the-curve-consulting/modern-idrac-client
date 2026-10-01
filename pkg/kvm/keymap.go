package kvm

// Keyboard encoding.
//
// The KeyboardDataRequest packet carries a 16-bit USB HID Keyboard/Keypad
// usage id (HID Usage Tables, page 0x07). This was determined from
// com.avocent.kvm.c.c.d (the AWT KeyEvent -> code tables: A=4 .. Z=29,
// 1=30 .. 0=39, Enter=40, Escape=41, Backspace=42, Tab=43, Space=44, F1=58,
// modifiers 224-231) and from the fallback switch in com.avocent.kvm.c.c.c
// (Power=102, Mute=127, Volume 128/129). The native-keyboard path
// (com.avocent.kvm.nativekeyboard.d) converts OS scancodes to the same
// space.
//
// The package accepts X11 keysyms as input (what RFB KeyEvent carries) and
// converts them with KeysymToUsage. Shifted characters map to the usage of
// the physical key; the VNC client sends Shift as a separate keysym.

// USB HID keyboard usage ids (subset).
const (
	HIDKeyA              uint16 = 0x04
	HIDKeyZ              uint16 = 0x1D
	HIDKey1              uint16 = 0x1E
	HIDKey0              uint16 = 0x27
	HIDKeyEnter          uint16 = 0x28
	HIDKeyEscape         uint16 = 0x29
	HIDKeyBackspace      uint16 = 0x2A
	HIDKeyTab            uint16 = 0x2B
	HIDKeySpace          uint16 = 0x2C
	HIDKeyMinus          uint16 = 0x2D
	HIDKeyEqual          uint16 = 0x2E
	HIDKeyLeftBrace      uint16 = 0x2F
	HIDKeyRightBrace     uint16 = 0x30
	HIDKeyBackslash      uint16 = 0x31
	HIDKeyNonUSHash      uint16 = 0x32
	HIDKeySemicolon      uint16 = 0x33
	HIDKeyApostrophe     uint16 = 0x34
	HIDKeyGrave          uint16 = 0x35
	HIDKeyComma          uint16 = 0x36
	HIDKeyPeriod         uint16 = 0x37
	HIDKeySlash          uint16 = 0x38
	HIDKeyCapsLock       uint16 = 0x39
	HIDKeyF1             uint16 = 0x3A
	HIDKeyF12            uint16 = 0x45
	HIDKeyPrintScreen    uint16 = 0x46
	HIDKeyScrollLock     uint16 = 0x47
	HIDKeyPause          uint16 = 0x48
	HIDKeyInsert         uint16 = 0x49
	HIDKeyHome           uint16 = 0x4A
	HIDKeyPageUp         uint16 = 0x4B
	HIDKeyDelete         uint16 = 0x4C
	HIDKeyEnd            uint16 = 0x4D
	HIDKeyPageDown       uint16 = 0x4E
	HIDKeyRight          uint16 = 0x4F
	HIDKeyLeft           uint16 = 0x50
	HIDKeyDown           uint16 = 0x51
	HIDKeyUp             uint16 = 0x52
	HIDKeyNumLock        uint16 = 0x53
	HIDKeyKPDivide       uint16 = 0x54
	HIDKeyKPMultiply     uint16 = 0x55
	HIDKeyKPSubtract     uint16 = 0x56
	HIDKeyKPAdd          uint16 = 0x57
	HIDKeyKPEnter        uint16 = 0x58
	HIDKeyKP1            uint16 = 0x59
	HIDKeyKP0            uint16 = 0x62
	HIDKeyKPDecimal      uint16 = 0x63
	HIDKeyNonUSBackslash uint16 = 0x64
	HIDKeyApplication    uint16 = 0x65 // "Menu" key (AWT CONTEXT_MENU)
	HIDKeyPower          uint16 = 0x66
	HIDKeyKPEqual        uint16 = 0x67
	HIDKeyF13            uint16 = 0x68
	HIDKeyF24            uint16 = 0x73
	HIDKeyExecute        uint16 = 0x74
	HIDKeyHelp           uint16 = 0x75
	HIDKeyMenu           uint16 = 0x76
	HIDKeySelect         uint16 = 0x77
	HIDKeyStop           uint16 = 0x78
	HIDKeyAgain          uint16 = 0x79
	HIDKeyUndo           uint16 = 0x7A
	HIDKeyCut            uint16 = 0x7B
	HIDKeyCopy           uint16 = 0x7C
	HIDKeyPaste          uint16 = 0x7D
	HIDKeyFind           uint16 = 0x7E
	HIDKeyMute           uint16 = 0x7F
	HIDKeyVolumeUp       uint16 = 0x80
	HIDKeyVolumeDown     uint16 = 0x81
	HIDKeyLeftCtrl       uint16 = 0xE0
	HIDKeyLeftShift      uint16 = 0xE1
	HIDKeyLeftAlt        uint16 = 0xE2
	HIDKeyLeftGUI        uint16 = 0xE3
	HIDKeyRightCtrl      uint16 = 0xE4
	HIDKeyRightShift     uint16 = 0xE5
	HIDKeyRightAlt       uint16 = 0xE6
	HIDKeyRightGUI       uint16 = 0xE7
)

// X11 keysyms used by the tables below (subset of X11/keysymdef.h).
const (
	XKBackSpace       = 0xff08
	XKTab             = 0xff09
	XKLinefeed        = 0xff0a
	XKClear           = 0xff0b
	XKReturn          = 0xff0d
	XKPause           = 0xff13
	XKScrollLock      = 0xff14
	XKSysReq          = 0xff15
	XKEscape          = 0xff1b
	XKHome            = 0xff50
	XKLeft            = 0xff51
	XKUp              = 0xff52
	XKRight           = 0xff53
	XKDown            = 0xff54
	XKPageUp          = 0xff55
	XKPageDown        = 0xff56
	XKEnd             = 0xff57
	XKBegin           = 0xff58
	XKSelect          = 0xff60
	XKPrint           = 0xff61
	XKExecute         = 0xff62
	XKInsert          = 0xff63
	XKUndo            = 0xff65
	XKRedo            = 0xff66
	XKMenu            = 0xff67
	XKFind            = 0xff68
	XKCancel          = 0xff69
	XKHelp            = 0xff6a
	XKBreak           = 0xff6b
	XKNumLock         = 0xff7f
	XKKPSpace         = 0xff80
	XKKPTab           = 0xff89
	XKKPEnter         = 0xff8d
	XKKPHome          = 0xff95
	XKKPLeft          = 0xff96
	XKKPUp            = 0xff97
	XKKPRight         = 0xff98
	XKKPDown          = 0xff99
	XKKPPageUp        = 0xff9a
	XKKPPageDown      = 0xff9b
	XKKPEnd           = 0xff9c
	XKKPBegin         = 0xff9d
	XKKPInsert        = 0xff9e
	XKKPDelete        = 0xff9f
	XKKPEqual         = 0xffbd
	XKKPMultiply      = 0xffaa
	XKKPAdd           = 0xffab
	XKKPSeparator     = 0xffac
	XKKPSubtract      = 0xffad
	XKKPDecimal       = 0xffae
	XKKPDivide        = 0xffaf
	XKKP0             = 0xffb0
	XKKP9             = 0xffb9
	XKF1              = 0xffbe
	XKF12             = 0xffc9
	XKF13             = 0xffca
	XKF24             = 0xffd5
	XKShiftL          = 0xffe1
	XKShiftR          = 0xffe2
	XKControlL        = 0xffe3
	XKControlR        = 0xffe4
	XKCapsLock        = 0xffe5
	XKShiftLock       = 0xffe6
	XKMetaL           = 0xffe7
	XKMetaR           = 0xffe8
	XKAltL            = 0xffe9
	XKAltR            = 0xffea
	XKSuperL          = 0xffeb
	XKSuperR          = 0xffec
	XKHyperL          = 0xffed
	XKHyperR          = 0xffee
	XKDelete          = 0xffff
	XKISOLevel3Shift  = 0xfe03
	XKISOLeftTab      = 0xfe20
	XF86AudioLowerVol = 0x1008ff11
	XF86AudioMute     = 0x1008ff12
	XF86AudioRaiseVol = 0x1008ff13
	XF86PowerOff      = 0x1008ff2a
	XF86Copy          = 0x1008ff57
	XF86Cut           = 0x1008ff58
	XF86Paste         = 0x1008ff6d
	XF86Stop          = 0x1008ff28
)

// usPunct maps US-layout printable characters to (usage, shifted).
var usPunct = map[rune]struct {
	usage uint16
	shift bool
}{
	' ': {HIDKeySpace, false},
	'-': {HIDKeyMinus, false}, '_': {HIDKeyMinus, true},
	'=': {HIDKeyEqual, false}, '+': {HIDKeyEqual, true},
	'[': {HIDKeyLeftBrace, false}, '{': {HIDKeyLeftBrace, true},
	']': {HIDKeyRightBrace, false}, '}': {HIDKeyRightBrace, true},
	'\\': {HIDKeyBackslash, false}, '|': {HIDKeyBackslash, true},
	';': {HIDKeySemicolon, false}, ':': {HIDKeySemicolon, true},
	'\'': {HIDKeyApostrophe, false}, '"': {HIDKeyApostrophe, true},
	'`': {HIDKeyGrave, false}, '~': {HIDKeyGrave, true},
	',': {HIDKeyComma, false}, '<': {HIDKeyComma, true},
	'.': {HIDKeyPeriod, false}, '>': {HIDKeyPeriod, true},
	'/': {HIDKeySlash, false}, '?': {HIDKeySlash, true},
	'!': {HIDKey1, true}, '@': {HIDKey1 + 1, true}, '#': {HIDKey1 + 2, true},
	'$': {HIDKey1 + 3, true}, '%': {HIDKey1 + 4, true}, '^': {HIDKey1 + 5, true},
	'&': {HIDKey1 + 6, true}, '*': {HIDKey1 + 7, true}, '(': {HIDKey1 + 8, true},
	')':  {HIDKey0, true},
	'\n': {HIDKeyEnter, false}, '\r': {HIDKeyEnter, false},
	'\t': {HIDKeyTab, false}, '\b': {HIDKeyBackspace, false}, 0x1b: {HIDKeyEscape, false},
	0x7f: {HIDKeyDelete, false},
}

// CharToKey maps a printable character (US layout) to the HID usage of the
// key that produces it and whether Shift must be held.
func CharToKey(r rune) (usage uint16, shift bool, ok bool) {
	switch {
	case r >= 'a' && r <= 'z':
		return HIDKeyA + uint16(r-'a'), false, true
	case r >= 'A' && r <= 'Z':
		return HIDKeyA + uint16(r-'A'), true, true
	case r == '0':
		return HIDKey0, false, true
	case r >= '1' && r <= '9':
		return HIDKey1 + uint16(r-'1'), false, true
	}
	if p, ok := usPunct[r]; ok {
		return p.usage, p.shift, true
	}
	return 0, false, false
}

// specialKeysyms maps non-character X11 keysyms to HID usages.
var specialKeysyms = map[uint32]uint16{
	XKBackSpace: HIDKeyBackspace, XKTab: HIDKeyTab, XKISOLeftTab: HIDKeyTab,
	XKLinefeed: HIDKeyEnter, XKReturn: HIDKeyEnter, XKClear: HIDKeyDelete,
	XKPause: HIDKeyPause, XKBreak: HIDKeyPause, XKScrollLock: HIDKeyScrollLock,
	XKSysReq: HIDKeyPrintScreen, XKPrint: HIDKeyPrintScreen, XKEscape: HIDKeyEscape,
	XKDelete: HIDKeyDelete, XKInsert: HIDKeyInsert,
	XKHome: HIDKeyHome, XKEnd: HIDKeyEnd, XKPageUp: HIDKeyPageUp, XKPageDown: HIDKeyPageDown,
	XKLeft: HIDKeyLeft, XKRight: HIDKeyRight, XKUp: HIDKeyUp, XKDown: HIDKeyDown,
	XKSelect: HIDKeySelect, XKExecute: HIDKeyExecute, XKUndo: HIDKeyUndo, XKRedo: HIDKeyAgain,
	XKMenu: HIDKeyApplication, XKFind: HIDKeyFind, XKCancel: HIDKeyStop, XKHelp: HIDKeyHelp,
	XKNumLock: HIDKeyNumLock, XKCapsLock: HIDKeyCapsLock, XKShiftLock: HIDKeyCapsLock,
	XKKPSpace: HIDKeySpace, XKKPTab: HIDKeyTab, XKKPEnter: HIDKeyKPEnter,
	XKKPHome: HIDKeyKP1 + 6, XKKPUp: HIDKeyKP1 + 7, XKKPPageUp: HIDKeyKP1 + 8,
	XKKPLeft: HIDKeyKP1 + 3, XKKPBegin: HIDKeyKP1 + 4, XKKPRight: HIDKeyKP1 + 5,
	XKKPEnd: HIDKeyKP1, XKKPDown: HIDKeyKP1 + 1, XKKPPageDown: HIDKeyKP1 + 2,
	XKKPInsert: HIDKeyKP0, XKKPDelete: HIDKeyKPDecimal,
	XKKPEqual: HIDKeyKPEqual, XKKPMultiply: HIDKeyKPMultiply, XKKPAdd: HIDKeyKPAdd,
	XKKPSeparator: HIDKeyKPDecimal, XKKPSubtract: HIDKeyKPSubtract, XKKPDecimal: HIDKeyKPDecimal,
	XKKPDivide: HIDKeyKPDivide,
	XKShiftL:   HIDKeyLeftShift, XKShiftR: HIDKeyRightShift,
	XKControlL: HIDKeyLeftCtrl, XKControlR: HIDKeyRightCtrl,
	XKAltL: HIDKeyLeftAlt, XKAltR: HIDKeyRightAlt, XKISOLevel3Shift: HIDKeyRightAlt,
	XKMetaL: HIDKeyLeftGUI, XKMetaR: HIDKeyRightGUI,
	XKSuperL: HIDKeyLeftGUI, XKSuperR: HIDKeyRightGUI,
	XKHyperL: HIDKeyLeftGUI, XKHyperR: HIDKeyRightGUI,
	XF86AudioMute: HIDKeyMute, XF86AudioLowerVol: HIDKeyVolumeDown, XF86AudioRaiseVol: HIDKeyVolumeUp,
	XF86PowerOff: HIDKeyPower, XF86Copy: HIDKeyCopy, XF86Cut: HIDKeyCut, XF86Paste: HIDKeyPaste,
	XF86Stop: HIDKeyStop,
}

// KeysymToUsage converts an X11 keysym to the USB HID usage id sent in
// KeyboardDataRequest. ok is false for keysyms that have no key on a US
// keyboard.
func KeysymToUsage(keysym uint32) (usage uint16, ok bool) {
	if u, ok := specialKeysyms[keysym]; ok {
		return u, true
	}
	switch {
	case keysym >= XKKP0 && keysym <= XKKP9:
		if keysym == XKKP0 {
			return HIDKeyKP0, true
		}
		return HIDKeyKP1 + uint16(keysym-XKKP0-1), true
	case keysym >= XKF1 && keysym <= XKF12:
		return HIDKeyF1 + uint16(keysym-XKF1), true
	case keysym >= XKF13 && keysym <= XKF24:
		return HIDKeyF13 + uint16(keysym-XKF13), true
	case keysym < 0x100:
		// Latin-1: keysym == character code.
		if u, _, ok := CharToKey(rune(keysym)); ok {
			return u, true
		}
	}
	return 0, false
}

// KeysymToScancode is an alias of KeysymToUsage kept for the name used in
// the design notes: the "scancode" on the wire is a HID usage id.
func KeysymToScancode(keysym uint32) (uint16, bool) { return KeysymToUsage(keysym) }
