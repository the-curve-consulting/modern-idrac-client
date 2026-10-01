//go:build !gui

package main

import "errors"

// runGUI is replaced by the manager window in builds with the "gui" tag.
func runGUI(g *globals) error {
	return errors.New("this binary was built without the graphical interface; rebuild with ./build.sh or `go build -tags gui ./cmd/idrac`")
}
