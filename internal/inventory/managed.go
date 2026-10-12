package inventory

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// AttachedPrefix is the prefix of a network a leg of pco is attached to.
type AttachedPrefix struct {
	ID     string
	Prefix netip.Prefix
}

// ManagedSubnetConflict says what the subnet of the managed network would
// collide with, in the words that follow "managed.subnet <subnet>": a route
// of a node, an address of a guest (static or reported), or the prefix of an
// attached network, the first found; "" when nothing. The nodes and the
// guests are the snapshot's, so the caller makes sure it is complete.
func (s Snapshot) ManagedSubnetConflict(subnet netip.Prefix, attached []AttachedPrefix) string {
	for _, n := range s.Nodes {
		for _, ifc := range n.Ifaces {
			for _, p := range ifc.Addrs {
				if p.Masked().Overlaps(subnet) {
					return fmt.Sprintf("overlaps the route %s of node %s on %s", p.Masked(), n.Name, ifc.Name)
				}
			}
		}
	}
	for _, g := range s.Guests {
		for _, a := range guestAddrs(g) {
			if subnet.Contains(a) {
				name := g.Ref.String()
				if n := strings.TrimSpace(g.Name); n != "" {
					name += " (" + n + ")"
				}
				return fmt.Sprintf("holds %s, an address of %s", a, name)
			}
		}
	}
	for _, n := range attached {
		if n.Prefix.Overlaps(subnet) {
			return fmt.Sprintf("overlaps %s, the prefix of network %s", n.Prefix, n.ID)
		}
	}
	return ""
}

// guestAddrs are the addresses of a guest's cards, static and reported, in
// order.
func guestAddrs(g model.Guest) []netip.Addr {
	var out []netip.Addr
	for _, n := range g.NICs {
		out = append(out, n.Static...)
	}
	for _, r := range g.Reported {
		out = append(out, r.Addr)
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}
