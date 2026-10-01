// Package racadm runs racadm commands on an iDRAC over SSH.
//
// Every iDRAC generation exposes racadm through its SSH service (the r710's
// iDRAC6 and the iDRAC8s both run OpenSSH 7.4 with modern algorithms), so this
// is the one management surface that is identical across the fleet.
package racadm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Client holds connection settings; it opens a fresh SSH session per command
// because iDRAC SSH servers cap concurrent sessions and idle out quickly.
type Client struct {
	Address  string // host or host:port (default port 22)
	Username string
	Password string
	Timeout  time.Duration // dial + command timeout, default 60s
	// HostKeyCallback defaults to ssh.InsecureIgnoreHostKey: iDRACs regenerate
	// keys on firmware resets and are only reachable on trusted LANs.
	HostKeyCallback ssh.HostKeyCallback
}

// Result is the output of one command.
type Result struct {
	Command  string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// Err converts a non-zero exit or a racadm "ERROR:" line into an error.
func (r *Result) Err() error {
	if r.ExitCode != 0 {
		return fmt.Errorf("racadm exit %d: %s", r.ExitCode, firstLine(r.Stderr+r.Stdout))
	}
	for _, l := range strings.Split(r.Stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ERROR") {
			return errors.New(strings.TrimSpace(l))
		}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (c *Client) addr() string {
	if _, _, err := net.SplitHostPort(c.Address); err == nil {
		return c.Address
	}
	return net.JoinHostPort(c.Address, "22")
}

func (c *Client) config() *ssh.ClientConfig {
	hk := c.HostKeyCallback
	if hk == nil {
		hk = ssh.InsecureIgnoreHostKey() //nolint:gosec // see field doc
	}
	to := c.Timeout
	if to == 0 {
		to = 60 * time.Second
	}
	cfg := &ssh.ClientConfig{
		User: c.Username,
		Auth: []ssh.AuthMethod{
			ssh.Password(c.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				ans := make([]string, len(questions))
				for i := range questions {
					ans[i] = c.Password
				}
				return ans, nil
			}),
		},
		HostKeyCallback: hk,
		Timeout:         to,
	}
	// iDRAC6 (fw 2.9x) still offers ssh-rsa host keys and hmac-sha1; allow the
	// legacy names in addition to the defaults so both generations connect.
	cfg.HostKeyAlgorithms = append([]string{}, ssh.SupportedAlgorithms().HostKeys...)
	cfg.HostKeyAlgorithms = append(cfg.HostKeyAlgorithms, ssh.InsecureAlgorithms().HostKeys...)
	cfg.KeyExchanges = append(append([]string{}, ssh.SupportedAlgorithms().KeyExchanges...), ssh.InsecureAlgorithms().KeyExchanges...)
	cfg.MACs = append(append([]string{}, ssh.SupportedAlgorithms().MACs...), ssh.InsecureAlgorithms().MACs...)
	cfg.Ciphers = append(append([]string{}, ssh.SupportedAlgorithms().Ciphers...), ssh.InsecureAlgorithms().Ciphers...)
	return cfg
}

// Run executes one racadm command (pass it without the leading "racadm";
// it is added when missing) and returns its output.
func (c *Client) Run(ctx context.Context, command string) (*Result, error) {
	command = strings.TrimSpace(command)
	if !strings.HasPrefix(command, "racadm") {
		command = "racadm " + command
	}
	start := time.Now()
	to := c.Timeout
	if to == 0 {
		to = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	d := net.Dialer{Timeout: to}
	raw, err := d.DialContext(ctx, "tcp", c.addr())
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", c.addr(), err)
	}
	// Enforce the context on the whole exchange.
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()

	conn, chans, reqs, err := ssh.NewClientConn(raw, c.addr(), c.config())
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("ssh %s: %w", c.addr(), err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	res := &Result{Command: command}
	err = sess.Run(command)
	res.Duration = time.Since(start)
	res.Stdout = normalise(stdout.String())
	res.Stderr = normalise(stderr.String())
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitStatus()
			return res, nil
		}
		if ctx.Err() != nil {
			return res, fmt.Errorf("racadm timed out after %s", to)
		}
		return res, fmt.Errorf("racadm: %w", err)
	}
	return res, nil
}

// Shell opens an interactive SSH session to the iDRAC's SMCLP/racadm shell,
// wiring it to the given streams (used by `idrac ssh`).
func (c *Client) Shell(ctx context.Context, stdin interface{ Read([]byte) (int, error) }, stdout, stderr interface{ Write([]byte) (int, error) }, cols, rows int) error {
	client, err := ssh.Dial("tcp", c.addr(), c.config())
	if err != nil {
		return fmt.Errorf("ssh %s: %w", c.addr(), err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = stdin, stdout, stderr
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		return err
	}
	if err := sess.Shell(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func normalise(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n") + "\n"
}

// ParseKeyValues turns typical racadm "Key = Value" output into a map,
// keeping the first occurrence of each key.
func ParseKeyValues(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, dup := m[k]; !dup {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}
