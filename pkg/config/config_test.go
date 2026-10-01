package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadAndResolve(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts.json")
	os.WriteFile(p, []byte(`{"defaults":{"username":"admin","password_env":"X_PASS"},"hosts":{"legacy01":{"address":"10.0.0.1","generation":"idrac6","password":"literal"},"server01":{"address":"10.0.0.2"}}}`), 0o600)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Resolve("legacy01")
	if r.Username != "admin" || r.Password != "literal" || r.Generation != GenIDRAC6 || r.KVMPort != 5900 || !r.InsecureTLS() {
		t.Fatalf("legacy01: %+v", r)
	}
	pm := f.Resolve("10.0.0.2")
	if pm.Name != "server01" || pm.PasswordEnv != "X_PASS" {
		t.Fatalf("by address: %+v", pm)
	}
	adhoc := f.Resolve("10.9.9.9")
	if adhoc.Address != "10.9.9.9" || adhoc.Username != "admin" {
		t.Fatalf("adhoc: %+v", adhoc)
	}
	t.Setenv("X_PASS", "from-env")
	t.Setenv("IDRAC_PASSWORD", "")
	pw, err := pm.ResolvePassword(t.Context())
	if err != nil || pw != "from-env" {
		t.Fatalf("env password: %q %v", pw, err)
	}
	// Save keeps entries as written (defaults are not baked into hosts).
	f.Set("new", Host{Address: "10.0.0.3", Generation: GenIDRAC8})
	f.Delete("legacy01")
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	f2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := f2.Hosts["legacy01"]; ok || f2.Hosts["new"].Address != "10.0.0.3" || f2.Hosts["server01"].Username != "" || f2.Defaults.Username != "admin" {
		t.Fatalf("after save: %+v / server01 %+v", f2.Hosts, f2.Hosts["server01"])
	}
	// Windows has no Unix permission bits; everywhere else the file must be private.
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatalf("missing file should be empty config, got %v", err)
	}
}
