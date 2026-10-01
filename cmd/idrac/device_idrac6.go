package main

import (
	"context"
	"errors"
)

// openIDRAC6 returns the legacy web-API implementation. It is wired up in
// idrac6.go once the pkg/idrac6 client exists; until then the racadm, ssh and
// kvm commands still work against an iDRAC6.
func openIDRAC6(ctx context.Context, g *globals) (Device, error) {
	if newIDRAC6Device == nil {
		return nil, errors.New("iDRAC6 web API support is not compiled in; use the racadm command")
	}
	return newIDRAC6Device(ctx, g)
}

var newIDRAC6Device func(ctx context.Context, g *globals) (Device, error)
