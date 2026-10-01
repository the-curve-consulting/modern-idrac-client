package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/the-curve-consulting/modern-idrac-client/pkg/config"
)

// installPasswordPrompt makes the config package ask on the terminal when no
// password source is configured. It reads from /dev/tty so it works even when
// stdin or stdout are redirected, and does nothing when there is no terminal
// (cron, CI) so the clear error message is kept there.
func installPasswordPrompt() {
	config.PromptPassword = func(prompt string) (string, error) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			if term.IsTerminal(int(os.Stdin.Fd())) {
				tty = os.Stdin
			} else {
				return "", errors.New("no password configured and no terminal to ask on (set IDRAC_PASSWORD or use -password)")
			}
		} else {
			defer tty.Close()
		}
		fmt.Fprint(tty, prompt)
		pw, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		if len(pw) == 0 {
			return "", errors.New("empty password")
		}
		return string(pw), nil
	}
}
