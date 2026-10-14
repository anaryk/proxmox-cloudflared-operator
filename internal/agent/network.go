package agent

import (
	"cmp"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// withLeases adds to the pins proven in this pass the pins of the leases of
// the snapshot, on the legs of this node: no card but a lease's own answers
// for its address. A pin proven for an address keeps its MAC; a lease never
// replaces it, since a service forwards only to the card whose identity was
// proven for its routes.
func (p *pass) withLeases(proven []gateway.Pin) []gateway.Pin {
	type key struct {
		leg  string
		addr netip.Addr
	}
	seen := map[key]bool{}
	for _, pin := range proven {
		seen[key{pin.Leg, pin.Addr}] = true
	}
	out := slices.Clone(proven)
	for _, pin := range p.cache.Pins {
		k := key{pin.Leg, pin.Addr}
		if seen[k] || len(pin.MAC) != 6 || !slices.ContainsFunc(p.cache.Legs, func(l gateway.Leg) bool { return l.ID == pin.Leg }) {
			continue
		}
		seen[k] = true
		out = append(out, pin)
	}
	slices.SortFunc(out, func(a, b gateway.Pin) int { return cmp.Or(cmp.Compare(a.Leg, b.Leg), a.Addr.Compare(b.Addr)) })
	return out
}

// serviceAddrs are the service addresses the gateway was given, once each,
// for the status.
func serviceAddrs(services []gateway.Service) []netip.Addr {
	var out []netip.Addr
	for _, s := range services {
		out = append(out, s.Service)
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}
