package viewer

// Key translation for the viewer: physical key (hardware scancode) to USB
// HID keyboard usage, which is what the iDRAC console protocol carries. Using
// the physical key rather than the produced character means the remote OS
// applies its own keyboard layout, exactly as with a real keyboard.

// evdevToHID maps Linux input event codes (KEY_*) to HID usages.
var evdevToHID = map[int]uint16{
	1: 0x29, 2: 0x1E, 3: 0x1F, 4: 0x20, 5: 0x21, 6: 0x22, 7: 0x23, 8: 0x24, 9: 0x25, 10: 0x26, 11: 0x27,
	12: 0x2D, 13: 0x2E, 14: 0x2A, 15: 0x2B,
	16: 0x14, 17: 0x1A, 18: 0x08, 19: 0x15, 20: 0x17, 21: 0x1C, 22: 0x18, 23: 0x0C, 24: 0x12, 25: 0x13,
	26: 0x2F, 27: 0x30, 28: 0x28, 29: 0xE0,
	30: 0x04, 31: 0x16, 32: 0x07, 33: 0x09, 34: 0x0A, 35: 0x0B, 36: 0x0D, 37: 0x0E, 38: 0x0F,
	39: 0x33, 40: 0x34, 41: 0x35, 42: 0xE1, 43: 0x31,
	44: 0x1D, 45: 0x1B, 46: 0x06, 47: 0x19, 48: 0x05, 49: 0x11, 50: 0x10, 51: 0x36, 52: 0x37, 53: 0x38,
	54: 0xE5, 55: 0x55, 56: 0xE2, 57: 0x2C, 58: 0x39,
	59: 0x3A, 60: 0x3B, 61: 0x3C, 62: 0x3D, 63: 0x3E, 64: 0x3F, 65: 0x40, 66: 0x41, 67: 0x42, 68: 0x43,
	69: 0x53, 70: 0x47,
	71: 0x5F, 72: 0x60, 73: 0x61, 74: 0x56, 75: 0x5C, 76: 0x5D, 77: 0x5E, 78: 0x57, 79: 0x59, 80: 0x5A, 81: 0x5B, 82: 0x62, 83: 0x63,
	86: 0x64, 87: 0x44, 88: 0x45,
	96: 0x58, 97: 0xE4, 98: 0x54, 99: 0x46, 100: 0xE6,
	102: 0x4A, 103: 0x52, 104: 0x4B, 105: 0x50, 106: 0x4F, 107: 0x4D, 108: 0x51, 109: 0x4E, 110: 0x49, 111: 0x4C,
	119: 0x48, 125: 0xE3, 126: 0xE7, 127: 0x65,
}

// nameToHID maps toolkit key names to HID usages. It is the fallback when no
// scancode is available (non-Linux platforms) and the reference used to
// calibrate the scancode offset.
var nameToHID = map[string]uint16{
	"Escape": 0x29, "Return": 0x28, "Tab": 0x2B, "BackSpace": 0x2A, "Insert": 0x49, "Delete": 0x4C,
	"Right": 0x4F, "Left": 0x50, "Down": 0x51, "Up": 0x52, "Prior": 0x4B, "Next": 0x4E, "Home": 0x4A, "End": 0x4D,
	"F1": 0x3A, "F2": 0x3B, "F3": 0x3C, "F4": 0x3D, "F5": 0x3E, "F6": 0x3F, "F7": 0x40, "F8": 0x41, "F9": 0x42, "F10": 0x43, "F11": 0x44, "F12": 0x45,
	"KP_Enter": 0x58, "Space": 0x2C,
	"0": 0x27, "1": 0x1E, "2": 0x1F, "3": 0x20, "4": 0x21, "5": 0x22, "6": 0x23, "7": 0x24, "8": 0x25, "9": 0x26,
	"A": 0x04, "B": 0x05, "C": 0x06, "D": 0x07, "E": 0x08, "F": 0x09, "G": 0x0A, "H": 0x0B, "I": 0x0C, "J": 0x0D, "K": 0x0E, "L": 0x0F, "M": 0x10,
	"N": 0x11, "O": 0x12, "P": 0x13, "Q": 0x14, "R": 0x15, "S": 0x16, "T": 0x17, "U": 0x18, "V": 0x19, "W": 0x1A, "X": 0x1B, "Y": 0x1C, "Z": 0x1D,
	"'": 0x34, ",": 0x36, "-": 0x2D, ".": 0x37, "/": 0x38, "\\": 0x31, "[": 0x2F, "]": 0x30, ";": 0x33, "=": 0x2E, "`": 0x35,
	"*": 0x55, "+": 0x57,
	"LeftShift": 0xE1, "RightShift": 0xE5, "LeftControl": 0xE0, "RightControl": 0xE4, "LeftAlt": 0xE2, "RightAlt": 0xE6,
	"LeftSuper": 0xE3, "RightSuper": 0xE7, "Menu": 0x65, "PrintScreen": 0x46, "CapsLock": 0x39,
}

// calibrationKeys are layout-independent keys whose name can be trusted to
// work out whether scancodes are X11 keycodes (evdev+8) or raw evdev codes.
var calibrationKeys = map[string]bool{
	"Escape": true, "Return": true, "Tab": true, "BackSpace": true, "Space": true,
	"F1": true, "F2": true, "F3": true, "F4": true, "F5": true, "F6": true, "F7": true, "F8": true, "F9": true, "F10": true, "F11": true, "F12": true,
	"Up": true, "Down": true, "Left": true, "Right": true,
}

// keyMapper turns (name, scancode) pairs into HID usages.
type keyMapper struct {
	useScancodes bool // Linux only
	offset       int  // 8 for X11 keycodes, 0 for raw evdev (native Wayland)
}

func newKeyMapper(linux bool) *keyMapper { return &keyMapper{useScancodes: linux, offset: 8} }

// usage returns the HID usage for a key event, or false if unmapped.
func (m *keyMapper) usage(name string, scancode int) (uint16, bool) {
	if m.useScancodes && scancode > 0 {
		if calibrationKeys[name] {
			want := nameToHID[name]
			if evdevToHID[scancode-m.offset] != want {
				for _, off := range []int{8, 0} {
					if evdevToHID[scancode-off] == want {
						m.offset = off
						break
					}
				}
			}
		}
		if u, ok := evdevToHID[scancode-m.offset]; ok {
			return u, true
		}
	}
	u, ok := nameToHID[name]
	return u, ok
}

// Macro is a named key combination offered in the Macros menu, matching the
// ones in Dell's viewer (keys the local window manager would otherwise eat).
type Macro struct {
	Name   string
	Usages []uint16
}

// Macros lists the built-in key macros.
var Macros = []Macro{
	{"Ctrl+Alt+Del", []uint16{0xE0, 0xE2, 0x4C}},
	{"Alt+Tab", []uint16{0xE2, 0x2B}},
	{"Alt+Esc", []uint16{0xE2, 0x29}},
	{"Ctrl+Esc", []uint16{0xE0, 0x29}},
	{"Alt+Space", []uint16{0xE2, 0x2C}},
	{"Alt+Enter", []uint16{0xE2, 0x28}},
	{"Alt+Hyphen", []uint16{0xE2, 0x2D}},
	{"Alt+F4", []uint16{0xE2, 0x3D}},
	{"PrtScrn", []uint16{0x46}},
	{"Alt+PrtScrn", []uint16{0xE2, 0x46}},
	{"SysRq+B (Alt+PrtScrn+B)", []uint16{0xE2, 0x46, 0x05}},
	{"Pause", []uint16{0x48}},
	{"Tab", []uint16{0x2B}},
	{"Ctrl+Enter", []uint16{0xE0, 0x28}},
	{"Super (Windows key)", []uint16{0xE3}},
	{"Super+L", []uint16{0xE3, 0x0F}},
	{"Ctrl+Alt+Backspace", []uint16{0xE0, 0xE2, 0x2A}},
	{"F1", []uint16{0x3A}},
	{"F2", []uint16{0x3B}},
	{"F8", []uint16{0x41}},
	{"F10", []uint16{0x43}},
	{"F11", []uint16{0x44}},
	{"F12", []uint16{0x45}},
}

// FunctionKeyMacros returns Alt+Fn and Ctrl+Alt+Fn combos (Linux VT switch).
func FunctionKeyMacros() (alt, ctrlAlt []Macro) {
	names := []string{"F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12"}
	for i, n := range names {
		u := uint16(0x3A + i)
		alt = append(alt, Macro{"Alt+" + n, []uint16{0xE2, u}})
		ctrlAlt = append(ctrlAlt, Macro{"Ctrl+Alt+" + n, []uint16{0xE0, 0xE2, u}})
	}
	return alt, ctrlAlt
}
