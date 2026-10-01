// Package config loads the list of iDRACs the CLI knows about and resolves
// their credentials.
//
// The file is JSON (so the tool stays dependency-free) and is looked up in
// this order: the --config flag / IDRAC_CONFIG env, ./idrac.json,
// $XDG_CONFIG_HOME/idrac/hosts.json (default ~/.config/idrac/hosts.json).
//
//	{
//	  "defaults": { "username": "root", "password_ref": "op://Infra/iDRAC office/password" },
//	  "hosts": {
//	    "r710":  { "address": "192.168.10.162", "generation": "idrac6" },
//	    "pmx01": { "address": "192.168.11.221" },
//	    "pmx02": { "address": "192.168.11.222", "password_env": "PMX02_IDRAC_PASS" }
//	  }
//	}
//
// A password may be given as a literal ("password"), an environment variable
// name ("password_env"), a 1Password secret reference resolved with `op read`
// ("password_ref"), or a shell command whose stdout is the password
// ("password_cmd"). Per-host fields override "defaults".
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Generation identifies the iDRAC family, which decides which API surface to use.
type Generation string

const (
	GenAuto   Generation = ""       // probe the device
	GenIDRAC6 Generation = "idrac6" // legacy /data XML API, no Redfish
	GenIDRAC7 Generation = "idrac7" // Redfish + legacy API
	GenIDRAC8 Generation = "idrac8"
	GenIDRAC9 Generation = "idrac9"
)

// HasRedfish reports whether the generation exposes a Redfish service.
func (g Generation) HasRedfish() bool {
	switch g {
	case GenIDRAC7, GenIDRAC8, GenIDRAC9:
		return true
	}
	return false
}

// Host is one iDRAC entry after defaults have been applied.
type Host struct {
	Name        string     `json:"-"`
	Address     string     `json:"address"`
	Generation  Generation `json:"generation,omitempty"`
	Username    string     `json:"username,omitempty"`
	Password    string     `json:"password,omitempty"`
	PasswordEnv string     `json:"password_env,omitempty"`
	PasswordRef string     `json:"password_ref,omitempty"`
	PasswordCmd string     `json:"password_cmd,omitempty"`
	HTTPSPort   int        `json:"https_port,omitempty"`
	SSHPort     int        `json:"ssh_port,omitempty"`
	KVMPort     int        `json:"kvm_port,omitempty"` // control + video port, default 5900
	Insecure    *bool      `json:"insecure,omitempty"` // skip TLS verification (default true: iDRACs ship self-signed certs)
	Description string     `json:"description,omitempty"`
}

// File is the on-disk shape.
type File struct {
	Defaults Host             `json:"defaults"`
	Hosts    map[string]*Host `json:"hosts"`
	path     string
}

// DefaultPath returns the user-level config path.
func DefaultPath() string {
	if p := os.Getenv("IDRAC_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat("idrac.json"); err == nil {
		return "idrac.json"
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "idrac", "hosts.json")
}

// Load reads path (or DefaultPath when empty). A missing file yields an empty
// config rather than an error so ad-hoc `--host 1.2.3.4` use keeps working.
func Load(path string) (*File, error) {
	if path == "" {
		path = DefaultPath()
	}
	f := &File{Hosts: map[string]*Host{}, path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return f, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, h := range f.Hosts {
		if h == nil {
			h = &Host{}
			f.Hosts[name] = h
		}
		h.Name = name
		applyDefaults(h, &f.Defaults)
	}
	return f, nil
}

// Path returns where the config was loaded from.
func (f *File) Path() string { return f.path }

// Names returns host names sorted.
func (f *File) Names() []string {
	names := make([]string, 0, len(f.Hosts))
	for n := range f.Hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Resolve returns the host for a name or, when the name is not configured,
// treats it as an address and builds an ad-hoc entry from defaults.
func (f *File) Resolve(nameOrAddr string) *Host {
	if h, ok := f.Hosts[nameOrAddr]; ok {
		return h
	}
	for _, h := range f.Hosts {
		if h.Address == nameOrAddr {
			return h
		}
	}
	h := &Host{Name: nameOrAddr, Address: nameOrAddr}
	applyDefaults(h, &f.Defaults)
	return h
}

func applyDefaults(h, d *Host) {
	if h.Username == "" {
		h.Username = d.Username
	}
	if h.Username == "" {
		h.Username = "root"
	}
	if h.Password == "" && h.PasswordEnv == "" && h.PasswordRef == "" && h.PasswordCmd == "" {
		h.Password, h.PasswordEnv, h.PasswordRef, h.PasswordCmd = d.Password, d.PasswordEnv, d.PasswordRef, d.PasswordCmd
	}
	if h.Generation == "" {
		h.Generation = d.Generation
	}
	if h.HTTPSPort == 0 {
		h.HTTPSPort = d.HTTPSPort
	}
	if h.HTTPSPort == 0 {
		h.HTTPSPort = 443
	}
	if h.SSHPort == 0 {
		h.SSHPort = d.SSHPort
	}
	if h.SSHPort == 0 {
		h.SSHPort = 22
	}
	if h.KVMPort == 0 {
		h.KVMPort = d.KVMPort
	}
	if h.KVMPort == 0 {
		h.KVMPort = 5900
	}
	if h.Insecure == nil {
		h.Insecure = d.Insecure
	}
	if h.Insecure == nil {
		t := true
		h.Insecure = &t
	}
}

// InsecureTLS reports whether certificate verification should be skipped.
func (h *Host) InsecureTLS() bool { return h.Insecure == nil || *h.Insecure }

// BaseURL is the https://host[:port] prefix.
func (h *Host) BaseURL() string {
	if h.HTTPSPort == 443 {
		return "https://" + h.Address
	}
	return fmt.Sprintf("https://%s:%d", h.Address, h.HTTPSPort)
}

// ResolvePassword returns the password, consulting (in order) the literal,
// IDRAC_PASSWORD / IDRAC_PASS env, the configured env var, `op read` on a
// 1Password reference, and finally the configured command.
func (h *Host) ResolvePassword(ctx context.Context) (string, error) {
	if h.Password != "" {
		return h.Password, nil
	}
	for _, k := range []string{"IDRAC_PASSWORD", "IDRAC_PASS"} {
		if v := os.Getenv(k); v != "" {
			return v, nil
		}
	}
	if h.PasswordEnv != "" {
		if v := os.Getenv(h.PasswordEnv); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("password_env %q is not set", h.PasswordEnv)
	}
	if h.PasswordRef != "" {
		return runForSecret(ctx, "op", "read", "--no-newline", h.PasswordRef)
	}
	if h.PasswordCmd != "" {
		return runForSecret(ctx, "sh", "-c", h.PasswordCmd)
	}
	if PromptPassword != nil {
		return PromptPassword(fmt.Sprintf("Password for %s@%s: ", h.Username, h.Address))
	}
	return "", fmt.Errorf("no password configured for %s (set IDRAC_PASSWORD, or password/password_env/password_ref/password_cmd in %s)", h.Name, DefaultPath())
}

// PromptPassword, when set, is called as a last resort to ask the user
// interactively. The CLI installs a terminal prompt here when stdin is a TTY.
var PromptPassword func(prompt string) (string, error)

func runForSecret(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}
