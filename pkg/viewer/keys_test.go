package viewer

import "testing"

func TestKeyMapperX11AndWayland(t *testing.T) {
	m := newKeyMapper(true)
	// X11 keycode for 'A' is evdev 30 + 8.
	if u, ok := m.usage("A", 38); !ok || u != 0x04 {
		t.Fatalf("x11 A: %#x %v", u, ok)
	}
	// A UK/DE layout may name the key differently; the physical key wins.
	if u, _ := m.usage("Y", 52); u != 0x1D { // evdev 44 = physical Z position
		t.Fatalf("physical mapping: %#x", u)
	}
	// Native Wayland delivers raw evdev codes: Escape = 1. Calibration flips the offset.
	if u, ok := m.usage("Escape", 1); !ok || u != 0x29 || m.offset != 0 {
		t.Fatalf("wayland escape: %#x %v offset %d", u, ok, m.offset)
	}
	if u, _ := m.usage("A", 30); u != 0x04 {
		t.Fatalf("wayland A: %#x", u)
	}
	// Name fallback when scancodes are unavailable.
	n := newKeyMapper(false)
	if u, ok := n.usage("LeftControl", 0); !ok || u != 0xE0 {
		t.Fatalf("fallback: %#x %v", u, ok)
	}
	if _, ok := n.usage("NoSuchKey", 0); ok {
		t.Fatal("unknown key mapped")
	}
}

func TestMacrosWellFormed(t *testing.T) {
	alt, ca := FunctionKeyMacros()
	if len(alt) != 12 || len(ca) != 12 || ca[1].Usages[2] != 0x3B {
		t.Fatalf("function macros: %v %v", alt, ca)
	}
	for _, m := range Macros {
		if m.Name == "" || len(m.Usages) == 0 {
			t.Fatalf("bad macro %+v", m)
		}
	}
}
