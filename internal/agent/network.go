package agent

import (
	"cmp"
	"net"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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

// withBindings adds to the pins proven in this pass, for a service the set
// holds from a binding kept from an earlier pass, the MAC of that binding,
// when the snapshot gives the guest of its route a card of that MAC: the
// service forwards only to that card.
func (p *pass) withBindings(services []gateway.Service, proven []gateway.Pin) []gateway.Pin {
	out := slices.Clone(proven)
	pinned := func(leg string, addr netip.Addr) bool {
		return slices.ContainsFunc(out, func(pin gateway.Pin) bool { return pin.Leg == leg && pin.Addr == addr })
	}
	for _, s := range services {
		if pinned(s.Leg, s.Addr) {
			continue
		}
		for _, t := range p.s.Targets {
			b, ok := p.bindings[t.Route.Hostname]
			if !ok || b.Via != s.Leg || b.Addr != s.Addr || b.Service != s.Service || t.Guest == nil {
				continue
			}
			mac, err := net.ParseMAC(b.MAC)
			if err != nil || len(mac) != 6 || !slices.ContainsFunc(t.Guest.NICs, func(n model.NIC) bool {
				own, err := net.ParseMAC(n.MAC)
				return err == nil && own.String() == mac.String()
			}) {
				continue
			}
			out = append(out, gateway.Pin{Leg: s.Leg, Addr: s.Addr, MAC: mac})
			break
		}
	}
	return out
}

// pinnedOnly are the services whose address behind its leg has a pin: one
// that would forward to an address no MAC is pinned for forwards nothing, as
// any card could answer there.
func pinnedOnly(services []gateway.Service, pins []gateway.Pin) []gateway.Service {
	return slices.DeleteFunc(services, func(s gateway.Service) bool {
		return !slices.ContainsFunc(pins, func(pin gateway.Pin) bool { return pin.Leg == s.Leg && pin.Addr == s.Addr && len(pin.MAC) == 6 })
	})
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

// maxLegProblem bounds what a leg report says of its problem.
const maxLegProblem = 512

// legReports are the legs as the gateway found them, for the status: at
// most one for each mark.
func legReports(states []gateway.LegState) []agentproto.LegReport {
	var out []agentproto.LegReport
	for _, st := range states {
		if len(out) == agentproto.MaxLegReports {
			break
		}
		problem := st.Problem
		if len(problem) > maxLegProblem {
			problem = problem[:maxLegProblem]
		}
		out = append(out, agentproto.LegReport{ID: st.ID, Up: st.Up, Problem: problem})
	}
	return out
}
