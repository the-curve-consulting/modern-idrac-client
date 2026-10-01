package main

import (
	"context"
	"fmt"
	"runtime"

	"idrac/pkg/viewer"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func init() {
	register(&command{name: "version", usage: "version", help: "print the version and build details", noHost: true, run: cmdVersion})
}

func cmdVersion(ctx context.Context, g *globals, args []string) error {
	window := "no (use `kvm vnc`)"
	if viewer.Available {
		window = "yes"
	}
	fmt.Fprintf(g.out, "idrac %s\n%s/%s, %s\nviewer window: %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version(), window)
	return nil
}
