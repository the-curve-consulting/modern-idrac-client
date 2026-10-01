package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"idrac/pkg/config"
)

// capturedTable is one table a command produced.
type capturedTable struct {
	Header []string
	Rows   [][]string
}

// cmdResult is the outcome of running a CLI command in-process.
type cmdResult struct {
	Text   string // everything the command printed
	Tables []capturedTable
	Err    error
}

// runCaptured runs a CLI command (e.g. "sensors", or "sel", "-n", "100")
// against host without touching the terminal: output and tables are collected
// instead of printed. The GUI is built on this, so every CLI function is
// available there with identical behaviour. host must be a resolved copy
// (config.File.Resolve) and should already carry its password so nothing
// prompts.
func runCaptured(ctx context.Context, base *globals, host *config.Host, args ...string) *cmdResult {
	res := &cmdResult{}
	if len(args) == 0 {
		res.Err = fmt.Errorf("no command")
		return res
	}
	cmd := lookup(args[0])
	if cmd == nil {
		res.Err = fmt.Errorf("unknown command %q", args[0])
		return res
	}
	var buf bytes.Buffer
	g := &globals{
		configPath: base.configPath, insecure: base.insecure, verbose: base.verbose, trace: base.trace,
		timeout: base.timeout, cfg: base.cfg, logger: base.logger,
		out: &buf, errw: &buf,
	}
	g.tableSink = func(header []string, rows [][]string) {
		res.Tables = append(res.Tables, capturedTable{Header: header, Rows: rows})
	}
	if host != nil {
		h := *host
		g.target = &h
		g.host = h.Name
	}
	res.Err = cmd.run(ctx, g, args[1:])
	res.Text = strings.TrimRight(buf.String(), "\n")
	return res
}
