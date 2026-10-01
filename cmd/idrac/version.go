package main

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/viewer"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func init() {
	register(&command{name: "version", usage: "version", help: "print the version and build details", noHost: true, run: cmdVersion})
}

// buildVersion prefers the stamped version and falls back to the module
// version recorded by `go install module@version`.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func cmdVersion(ctx context.Context, g *globals, args []string) error {
	window := "no (use `kvm vnc`)"
	if viewer.Available {
		window = "yes"
	}
	fmt.Fprintf(g.out, "idrac %s\n%s/%s, %s\nviewer window: %s\n", buildVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version(), window)
	return nil
}
