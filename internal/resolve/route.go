package resolve

import (
	"context"
	"errors"
	"net/netip"
	"slices"
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

// throughGateway checks the route to a trusted static address the node has
// no address next to. The admin trusts such addresses for networks the host
// reaches through a router, so the route must go through a gateway: an
// address the kernel reaches on-link is in a network the identity check can
// look at, and its configuration alone does not vouch for it.
func (a *attempt) throughGateway(ctx context.Context, c Candidate) outcome {
	out, onLink, o := a.kernelRoute(ctx, c.Addr)
	switch {
	case !o.ok():
		return o
	case onLink && slices.Contains(ifaceNames(c.NIC), out):
		return lost("node has no address on %s in the guest's network", out)
	case onLink:
		return lost("address is on-link through %s, which is not the guest's bridge", out)
	}
	return outcome{}
}

// kernelRoute asks for the kernel's route to addr. Having none, or one that
// changes with the source address, is a failure of identity, since the
// host's traffic could not be followed; a route that could not be looked up
// proves nothing either way.
func (a *attempt) kernelRoute(ctx context.Context, addr netip.Addr) (iface string, onLink bool, o outcome) {
	if ctx.Err() != nil {
		return "", false, stopped()
	}
	iface, onLink, err := a.r.prober.Route(ctx, addr)
	switch {
	case ctx.Err() != nil:
	case errors.Is(err, ErrNoRoute):
		return "", false, lost("node has no route to %s", addr)
	case errors.Is(err, ErrRouteDiffers):
		return "", false, lost("route to %s changes with the source address", addr)
	}
	if o := answered(ctx, err, "route to %s: %v", addr); !o.ok() {
		return "", false, o
	}
	return iface, onLink, outcome{}
}
