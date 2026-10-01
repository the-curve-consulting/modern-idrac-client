//go:build !gui

package viewer

import "errors"

// Available reports whether this binary was built with the GUI.
const Available = false

// Run reports that the GUI was not compiled in.
func Run(Options) error {
	return errors.New("this binary was built without the graphical viewer; rebuild with ./build.sh or `go build -tags gui ./cmd/idrac` (needs cgo and OpenGL/X11 headers)")
}
