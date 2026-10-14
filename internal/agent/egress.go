package agent

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// The states of the egress filter, as the status says them.
const (
	egressOn        = "on"
	egressOff       = "off"
	egressNotLoaded = "not loaded"
	egressChanged   = "changed"
)

// takeOutBlocked takes every address a block of the install names out of the
// egress filter at once, before the targets are resolved, which may take long:
// a block holds on every node while it is in force.
func (p *pass) takeOutBlocked() {
	for _, b := range p.s.Blocks {
		if b.Until.IsZero() || p.now.Before(b.Until) {
			p.blocked[b.Addr.Unmap()] = true
		}
	}
	p.removeHeld(p.blocked)
}

// removeHeld takes the addresses out of the filter that the set holds.
func (p *pass) removeHeld(addrs map[netip.Addr]bool) {
	for _, addr := range slices.SortedFunc(maps.Keys(addrs), netip.Addr.Compare) {
		if !slices.ContainsFunc(p.a.set, func(t egress.Target) bool { return t.Addr == addr }) {
			continue
		}
		if err := p.a.d.Egress.Remove(p.ctx, addr); err != nil {
			p.problem("taking %s out of the egress filter: %v", addr, err)
		}
	}
}

// blockLost takes out of the egress filter, at once, every target whose
// binding lost its proof in this pass, and keeps their addresses out of the
// set: what the rest of the pass takes may be long, and the address may now
// be someone else's. A target is taken out by the address the set holds for
// it, the service address of one on a leg. Only the addresses the last set
// holds can be in the table.
func (p *pass) blockLost() {
	for _, t := range p.s.Targets {
		res, ok := p.results[t.Key]
		prev, bound := p.bindings[t.Route.Hostname]
		if !ok || !bound {
			continue
		}
		if _, lost := resolve.LostProof(t.Route, &prev, res); lost && setAddr(prev).IsValid() {
			p.lost[setAddr(prev)] = true
		}
	}
	p.removeHeld(p.lost)
}

// legAddr is an address on the network of a leg.
type legAddr struct {
	via  string
	addr netip.Addr
}

// freezeMoved takes out of the egress filter, at once, the service address
// of every target on a leg whose guest this pass found at another address or
// on another network, and keeps it out of the set: it forwards to the
// address the guest left, where anyone may answer now, until the controller
// gives the target a service address where it is. One that another target of
// this pass is still served behind stays.
func (p *pass) freezeMoved() {
	standing := map[legAddr]bool{}
	for _, t := range p.s.Targets {
		if res, ok := p.results[t.Key]; ok && res.Binding != nil && res.Binding.Via != "" && serves(t, res) {
			standing[legAddr{res.Binding.Via, res.Binding.Addr}] = true
		}
	}
	frozen := map[netip.Addr]bool{}
	for _, t := range p.s.Targets {
		res, ok := p.results[t.Key]
		prev, bound := p.bindings[t.Route.Hostname]
		if !ok || !bound || res.Binding == nil || prev.Via == "" {
			continue
		}
		was := legAddr{prev.Via, prev.Addr}
		if (legAddr{res.Binding.Via, res.Binding.Addr}) == was {
			continue
		}
		p.moved[t.Route.Hostname] = true
		if standing[was] || !prev.Service.IsValid() || p.lost[prev.Service] {
			continue
		}
		frozen[prev.Service], p.lost[prev.Service] = true, true
		p.a.event(levelWarn, kindEgress, prev.Service.String(),
			fmt.Sprintf("%s left %s on network %s; its service address %s forwards nothing until the controller moves it", t.Route.Hostname, prev.Addr, prev.Via, prev.Service))
	}
	p.removeHeld(frozen)
}

// egressSet is the set of this pass: every target this node verified that
// the snapshot lists, and every target of the last set that the tunnel
// configuration last verified at Cloudflare still sends a connector to,
// unless its binding is withdrawn; less the addresses that lost their proof
// and those a block names. A target thus enters before the rule that names
// it is written, and leaves only once it is in neither list, or at once when
// its proof is lost or it is blocked.
func (p *pass) egressSet() []egress.Target {
	set := []egress.Target{}
	for _, t := range p.s.Targets {
		if et, ok := p.verifiedTarget(t); ok {
			set = append(set, et)
		}
	}
	sent := p.sentTargets()
	withdrawn := p.withdrawnAddrs()
	for _, t := range p.a.set {
		// A target whose proof only fell below its minimum is still proven
		// and leaves after its rule. A withdrawn one is not fed back, whether
		// or not its proof was lost in this pass: the set kept may be older
		// than the binding that says so.
		if sent[egress.Target{Addr: t.Addr, Port: t.Port}] && !withdrawn[t.Addr] {
			set = append(set, t)
		}
	}
	set = slices.DeleteFunc(set, func(t egress.Target) bool { return p.lost[t.Addr] || p.blocked[t.Addr] })
	// A target of allowNode sorts first among its equals, and is the one
	// kept: one of an earlier process comes back from disk without the mark.
	slices.SortFunc(set, egress.CompareAllowNodeFirst)
	return slices.CompactFunc(set, func(a, b egress.Target) bool { return egress.CompareEndpoints(a, b) == 0 })
}

// verifiedTarget is the target this pass verified for t, if any: the address
// resolution proved for the owner of the route, at the route's port, as the
// connector dials it, the service address of a target on a leg. A target
// held back by its minimum has no address any more. The target of a manual
// route with allowNode is marked so: it may be an address of the node, which
// the filter refuses for any other.
func (p *pass) verifiedTarget(t agentproto.Target) (egress.Target, bool) {
	res, ok := p.served[t.Key]
	rt := res.Target
	dialed, served := rt.Dialed()
	switch {
	case !ok, !rt.Addr.IsValid(), !served, rt.Withdrawn, rt.Rejected, rt.Owner != "" && rt.Owner != t.Route.Owner(), t.Route.Target.Port == 0:
		return egress.Target{}, false
	}
	return egress.Target{Addr: dialed, Port: t.Route.Target.Port, AllowNode: t.Route.Source == model.SourceManual && t.Route.Options.AllowNode}, true
}

// withdrawnAddrs are the addresses the set holds for the bindings withdrawn
// after this pass's resolution, and for every stored binding that was
// withdrawn when the pass began, which covers a route gone since.
func (p *pass) withdrawnAddrs() map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	add := func(b resolve.Binding) {
		if addr := setAddr(b); b.Withdrawn && addr.IsValid() {
			out[addr] = true
		}
	}
	for _, b := range p.bindings {
		add(b)
	}
	for _, res := range p.served {
		if res.Binding != nil {
			add(*res.Binding)
		}
	}
	return out
}

// sentTargets are the targets the configurations last verified at Cloudflare
// of the tunnels this node may run send a connector to: the shared tunnel of
// the install and the one pinned to this node, by name. The configuration of
// a tunnel pinned to another node vouches for nothing on this one.
func (p *pass) sentTargets() map[egress.Target]bool {
	shared := planner.TunnelName(p.a.d.InstallID)
	own, _ := planner.PinnedTunnelName(p.a.d.InstallID, p.a.d.Node)
	out := map[egress.Target]bool{}
	for _, t := range p.s.Tunnels {
		if t.Name != shared && (own == "" || t.Name != own) {
			continue
		}
		for _, ap := range t.Verified {
			out[egress.Target{Addr: ap.Addr().Unmap(), Port: ap.Port()}] = true
		}
	}
	return out
}

// feedEgress gives the egress filter the set of this pass, less the
// addresses whose MAC moved and that were not verified again yet, and keeps
// it. A filter the admin switched off is no failure.
func (p *pass) feedEgress(set []egress.Target) {
	a := p.a
	a.egMu.Lock()
	a.set = set
	a.egMu.Unlock()
	if err := a.saveSet(); err != nil {
		p.problem("%v", err)
	}
	blocked, err := a.d.Blocked()
	if err != nil {
		p.problem("reading the addresses blocked on this node: %v", err)
	}
	p.localBlocks = blocked
	a.egMu.Lock()
	err = a.d.Egress.Set(p.ctx, a.unsuspected())
	a.egMu.Unlock()
	switch {
	case err == nil:
		p.egress = egressOn
	case errors.Is(err, egress.ErrOff):
		p.egress = egressOff
	case errors.Is(err, egress.ErrChanged):
		p.egress, p.setErr = egressChanged, err
		p.problem("setting the egress filter: %v", err)
	default:
		p.egress, p.setErr = egressNotLoaded, err
		p.problem("setting the egress filter: %v", err)
	}
}

// unsuspected is the set less the addresses whose MAC moved and that were
// not verified again yet. The caller holds egMu.
func (a *Agent) unsuspected() []egress.Target {
	return slices.DeleteFunc(slices.Clone(a.set), func(t egress.Target) bool { return a.suspects[t.Addr] })
}

// syncGateway gives the gateway the legs of the snapshot, every service
// address the set holds with the ports it holds it at, the neighbours proven
// behind them and the leases of the snapshot, and the sticky entries of the
// cards of the guests of this node. A snapshot that gives no networks leaves the gateway
// as it is.
func (p *pass) syncGateway(set []egress.Target) {
	if p.a.d.Gateway == nil || !p.networks {
		return
	}
	behind := p.serviceMap()
	ports := map[netip.Addr][]uint16{}
	for _, t := range set {
		ports[t.Addr] = append(ports[t.Addr], t.Port)
	}
	var services []gateway.Service
	for service, ps := range ports {
		m, ok := behind[service]
		if !ok || !slices.ContainsFunc(p.cache.Legs, func(l gateway.Leg) bool { return l.ID == m.leg }) {
			continue
		}
		services = append(services, gateway.Service{Leg: m.leg, Service: service, Addr: m.addr, Ports: slices.Compact(slices.Sorted(slices.Values(ps)))})
	}
	slices.SortFunc(services, func(a, b gateway.Service) int { return cmp.Or(cmp.Compare(a.Leg, b.Leg), a.Addr.Compare(b.Addr)) })
	pins := p.withLeases(p.withBindings(services, p.gatewayPins(services)))
	services = pinnedOnly(services, pins)
	if len(p.cache.Legs) == 0 && len(services) == 0 && !p.a.gwInUse {
		return
	}
	states, err := p.a.d.Gateway.Sync(p.ctx, p.cache.Legs, services, pins, p.cache.Sticky)
	if err != nil {
		p.problem("programming the gateway: %v", err)
		return
	}
	p.a.gwInUse = len(p.cache.Legs) > 0 || len(services) > 0
	p.a.gwServices = serviceAddrs(services)
	for _, st := range states {
		switch {
		case !st.Up:
			p.problem("network %s: the leg is not up: %s", st.ID, cmp.Or(st.Problem, "the gateway does not say why"))
		case st.Problem != "":
			p.problem("network %s: the leg is up, but %s", st.ID, st.Problem)
		}
	}
}

// behindService is the real address a service address forwards to, on the
// network of a leg.
type behindService struct {
	leg  string
	addr netip.Addr
}

// serviceMap is, by service address, what each forwards to: as this pass
// served it, or as a stored binding was published, unless this pass found
// its guest elsewhere.
func (p *pass) serviceMap() map[netip.Addr]behindService {
	out := map[netip.Addr]behindService{}
	for host, b := range p.bindings {
		if b.Via != "" && b.Service.IsValid() && !p.moved[host] {
			out[b.Service] = behindService{b.Via, b.Addr}
		}
	}
	for _, res := range p.served {
		if b := res.Binding; b != nil && res.Target.Leg && res.Target.Via.IsValid() {
			out[res.Target.Via] = behindService{b.Via, res.Target.Addr}
		}
	}
	return out
}

// gatewayPins are the MACs proven for the real addresses the services
// forward to, once each.
func (p *pass) gatewayPins(services []gateway.Service) []gateway.Pin {
	forwarded := map[behindService]bool{}
	for _, s := range services {
		forwarded[behindService{s.Leg, s.Addr}] = true
	}
	seen := map[behindService]bool{}
	var out []gateway.Pin
	for _, t := range p.s.Targets {
		res, ok := p.served[t.Key]
		if !ok || res.Binding == nil || !res.Target.Leg || !serves(t, res) {
			continue
		}
		key := behindService{res.Binding.Via, res.Target.Addr}
		mac, err := net.ParseMAC(res.Binding.MAC)
		if err != nil || !forwarded[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, gateway.Pin{Leg: key.leg, Addr: key.addr, MAC: mac})
	}
	slices.SortFunc(out, func(a, b gateway.Pin) int { return cmp.Or(cmp.Compare(a.Leg, b.Leg), a.Addr.Compare(b.Addr)) })
	return out
}
