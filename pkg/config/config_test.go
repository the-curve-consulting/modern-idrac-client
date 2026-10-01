package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndResolve(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hosts.json")
	os.WriteFile(p, []byte(`{"defaults":{"username":"admin","password_env":"X_PASS"},"hosts":{"r710":{"address":"10.0.0.1","generation":"idrac6","password":"literal"},"pmx":{"address":"10.0.0.2"}}}`), 0o600)
	f, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Resolve("r710")
	if r.Username != "admin" || r.Password != "literal" || r.Generation != GenIDRAC6 || r.KVMPort != 5900 || !r.InsecureTLS() {
		t.Fatalf("r710: %+v", r)
	}
	pm := f.Resolve("10.0.0.2")
	if pm.Name != "pmx" || pm.PasswordEnv != "X_PASS" {
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
	if _, err := Load(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatalf("missing file should be empty config, got %v", err)
	}
}
