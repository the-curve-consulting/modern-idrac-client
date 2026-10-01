package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"idrac/pkg/webapi"
)

func init() {
	register(&command{
		name:  "web",
		usage: "web get <key,...> | set k=v [k=v...] | raw <METHOD> <path> [body] | jnlp",
		help:  "legacy web-UI API (/data?get=, /data?set=; iDRAC6's only HTTP API)",
		run:   cmdWeb,
	})
}

func (g *globals) webClient(ctx context.Context) (*webapi.Client, error) {
	pw, err := g.passwd(ctx)
	if err != nil {
		return nil, err
	}
	w := webapi.New(g.target.BaseURL(), g.target.Username, pw, g.target.InsecureTLS(), g.timeout)
	if g.verbose {
		w.Logger = g.logger
	}
	if err := w.Login(ctx); err != nil {
		return nil, err
	}
	return w, nil
}

func cmdWeb(ctx context.Context, g *globals, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: web get <key,...> | web set k=v... | web raw METHOD path [body] | web jnlp")
	}
	w, err := g.webClient(ctx)
	if err != nil {
		return err
	}
	defer w.Logout(ctx)
	switch args[0] {
	case "get":
		if len(args) < 2 {
			return errors.New("web get <key,...>")
		}
		m, raw, err := w.Get(ctx, strings.Split(strings.Join(args[1:], ","), ",")...)
		if err != nil {
			return err
		}
		if g.jsonOut {
			return g.printJSON(m)
		}
		if g.verbose {
			fmt.Fprintln(g.errw, string(raw))
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(g.out, "%s=%s\n", k, m[k])
		}
		return nil
	case "set":
		pairs := map[string]string{}
		for _, a := range args[1:] {
			k, v, ok := strings.Cut(a, "=")
			if !ok {
				return fmt.Errorf("web set: expected key=value, got %q", a)
			}
			pairs[k] = v
		}
		m, err := w.Set(ctx, pairs)
		if err != nil {
			return err
		}
		return g.printJSON(m)
	case "raw":
		if len(args) < 3 {
			return errors.New("web raw <METHOD> <path> [body]")
		}
		body := ""
		if len(args) > 3 {
			body = args[3]
		}
		status, out, err := w.Raw(ctx, strings.ToUpper(args[1]), args[2], body)
		if err != nil {
			return err
		}
		fmt.Fprintf(g.errw, "HTTP %d\n", status)
		fmt.Fprintln(g.out, string(out))
		return nil
	case "jnlp":
		l, err := w.ConsoleCredentials(ctx, "idrac-go")
		if err != nil {
			return err
		}
		fmt.Fprintln(g.out, string(l.JNLP))
		return nil
	}
	return fmt.Errorf("web: unknown verb %q", args[0])
}
