package main

import (
	"context"
	"os"

	"golang.org/x/term"
)

// cmdSSH drops the user into the iDRAC's SSH shell (racadm / SMCLP prompt)
// with the terminal in raw mode so the remote line editing works.
func cmdSSH(ctx context.Context, g *globals, args []string) error {
	c, err := g.racadmClient(ctx)
	if err != nil {
		return err
	}
	c.Timeout = 0
	fd := int(os.Stdin.Fd())
	cols, rows := 80, 24
	if term.IsTerminal(fd) {
		if w, h, err := term.GetSize(fd); err == nil {
			cols, rows = w, h
		}
		state, err := term.MakeRaw(fd)
		if err == nil {
			defer term.Restore(fd, state)
		}
	}
	return c.Shell(ctx, os.Stdin, os.Stdout, os.Stderr, cols, rows)
}
