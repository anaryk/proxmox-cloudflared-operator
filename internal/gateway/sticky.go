package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// GuestPort is the port a guest's card takes on its bridge: tap<vmid>i<n>
// of a virtual machine and veth<vmid>i<n> of a container, or for either
// fwpr<vmid>p<n> when the card has the firewall on, as the guest's own port
// then sits in a firewall bridge of its own, which that port links to the
// card's bridge.
func GuestPort(container bool, vmid, nic int, firewall bool) string {
	switch {
	case firewall:
		return fmt.Sprintf("fwpr%dp%d", vmid, nic)
	case container:
		return fmt.Sprintf("veth%di%d", vmid, nic)
	}
	return fmt.Sprintf("tap%di%d", vmid, nic)
}

// IsGuestPort reports whether a link is named as GuestPort names the port of
// a guest's card.
func IsGuestPort(name string) bool {
	for _, p := range [...]struct{ prefix, sep string }{{"tap", "i"}, {"veth", "i"}, {"fwpr", "p"}} {
		rest, ok := strings.CutPrefix(name, p.prefix)
		if !ok {
			continue
		}
		vmid, nic, ok := strings.Cut(rest, p.sep)
		return ok && digits(vmid) && digits(nic)
	}
	return false
}

func digits(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}

// stickyKeeper is a backend that keeps sticky entries on the ports of
// guests.
type stickyKeeper interface {
	keepSticky(ctx context.Context, sticky []Sticky) error
}

// ApplySticky makes the sticky entries those given, on the ports that exist
// and parked on the managed leg for the others, and the guard table hold the
// cards they name, and changes nothing else of the gateway: what the daemon runs when the
// port of a managed guest appears between two cycles, which Sync the rest.
// The appliance's backend keeps none, as the bridge is the node's: for it
// this is nothing.
func ApplySticky(ctx context.Context, b Backend, sticky []Sticky) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if k, ok := b.(stickyKeeper); ok {
		return k.keepSticky(ctx, sticky)
	}
	return nil
}

func (b *hostBackend) keepSticky(ctx context.Context, sticky []Sticky) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	stickyErr := b.syncSticky(sticky, false)
	b.sticky = slices.Clone(sticky)
	return errors.Join(stickyErr, b.ensureGuard(ctx))
}
