package resolve

import (
	"context"
	"errors"
	"net/netip"
)

// followsRoute checks that the host's own traffic for addr leaves through
// iface, where the identity is checked, and goes to addr directly. Otherwise
// a guest could prove the address on its bridge while the host sends what is
// published for it to whoever holds the address elsewhere, such as in a more
// specific network on another bridge.
func (a *attempt) followsRoute(ctx context.Context, iface string, addr netip.Addr) outcome {
	out, onLink, o := a.kernelRoute(ctx, addr)
	switch {
	case !o.ok():
		return o
	case out != iface:
		return lost("route to %s leaves through %s, not %s", addr, out, iface)
	case !onLink:
		return lost("route to %s leaves through a gateway", addr)
	}
	return outcome{}
}

// kernelRoute asks for the kernel's route to addr. Having none is a failure of
// identity, since nothing could be reached there; a route that could not be
// looked up proves nothing either way.
func (a *attempt) kernelRoute(ctx context.Context, addr netip.Addr) (iface string, onLink bool, o outcome) {
	if ctx.Err() != nil {
		return "", false, stopped()
	}
	iface, onLink, err := a.r.prober.Route(ctx, addr)
	if ctx.Err() == nil && errors.Is(err, ErrNoRoute) {
		return "", false, lost("node has no route to %s", addr)
	}
	if o := answered(ctx, err, "route to %s: %v", addr); !o.ok() {
		return "", false, o
	}
	return iface, onLink, outcome{}
}
