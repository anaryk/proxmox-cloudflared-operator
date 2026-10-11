package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Gateway is the backend of the networks pco reaches through a leg of its
// own, gateway.Backend as the engine drives it. Its methods are called from
// one goroutine at a time.
type Gateway interface {
	Up(ctx context.Context) error
	Sync(ctx context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error)
	Verify(ctx context.Context) error
	Probe(ctx context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error)
	Detach(ctx context.Context, id string) error
	// Dial connects through the leg, from its address and with its mark: the
	// way a connector's request to a service address leaves the namespace.
	Dial(ctx context.Context, leg gateway.Leg, addr netip.AddrPort) (net.Conn, error)
	// Check checks the gateway and brings it back to what was last wanted of
	// it when it differs. The cycle calls it once it knows the gateway may be
	// changed, before anything probes through it.
	Check(ctx context.Context) GatewayCheck
}

// heldGateway is why a cycle whose gateway could not be programmed does not
// write the configuration of a tunnel: a rule must not send a connector to a
// service address the gateway does not forward.
const heldGateway = "the gateway could not be programmed; no tunnel configuration is written until it is"

const (
	problemServiceHeld    = "hostname %s is not served through network %s: service address not allocated: %s"
	problemServiceKept    = "hostname %s moved to %s on network %s; its rule is kept, and its service address %s forwards nothing until it can follow: %s"
	problemGatewaySync    = "programming the gateway: %v; no tunnel configuration is written until it is programmed"
	problemLegDown        = "network %s: the leg is not up: %s"
	problemLegNote        = "network %s: the leg is up, but %s"
	problemGatewayCache   = "saving the cache of the gateway: %s; a boot before it is saved brings the gateway back as it was before"
	problemServiceRelease = "releasing the service address %s of %s on network %s: %v; the next cycle releases it"

	// reasonNoService is the reason of a target on a leg that has no service
	// address, and reasonNotMoved that of one whose service address did not
	// follow it to its new address yet.
	reasonNoService = "service address not allocated: %s"
	reasonNotMoved  = "service address not moved: %s; the rule is kept and reaches nothing until it is"

	// LegUp is PathView.Leg of a path through a leg that is up.
	LegUp = "up"
)

// The reasons the gate of an expanding write gives, and verdictNoRun, the
// verdict of a cycle that did not get to its tunnel run.
const (
	gateNoCycle    = "no cycle has run since the start"
	gateNoRun      = "the previous cycle did not get to the tunnel run"
	gatePrevious   = "the previous cycle found a %s writer"
	gateUnknown    = "the previous cycle could not tell the writer"
	gateBehind     = "the previous cycle found this appliance behind its last write at Cloudflare"
	gateNoWriter   = "the writer identity could not be read this cycle"
	gateChanged    = "leader.json changed since the cycle began"
	gateReread     = "leader.json could not be read again: %v"
	gateNotProven  = "the self-identification of the appliance did not pass this cycle"
	gateNotQuorate = "the cluster is not quorate"
	gateQuorum     = "the quorum of the cluster could not be read: %v"
	gateEpoch      = "this process drew a new epoch and none of its writes is verified at Cloudflare yet"
	gateNoGateway  = "no gateway runs on this node"
	gateNoLeg      = "network %s has no leg on this node"
	gateFrozen     = "service address %s of this address waits for the route that left it"

	verdictNoRun = "no run"
)

// gateOpen is the gate of an expanding write that happens before the tunnel
// run, whose verdict does not exist yet then: the creation of a service
// address, a lease or a network card. On the host it opens when readWriter
// read the writer identity this cycle, the previous cycle's tunnel run found
// this process the writer, and leader.json read again now names the same
// writer. In the appliance the self-identification passed this cycle, the
// cluster is quorate when asked again, and a process that drew a new epoch at
// its start has had a write verified at Cloudflare since; a previous verdict
// that found the appliance behind closes it too. The reason names what is
// missing. Only creations ask it; the lookup of what exists, and any
// restrictive change, never do. It never runs a reconciler.
func (c *cycleRun) gateOpen(what string) (ok bool, reason string) {
	reason = c.gateClosed()
	if reason != "" {
		c.e.d.Log.Debug().Str("write", what).Str("why", reason).Msg("an expanding write waits for the gate")
		return false, reason
	}
	return true, ""
}

func (c *cycleRun) gateClosed() string {
	if c.e.d.Identity != nil {
		if id := c.st.Identity; id == nil || !id.OK {
			return gateNotProven
		}
		if c.e.d.Quorate != nil {
			switch ok, err := c.e.d.Quorate(c.ctx); {
			case err != nil:
				return fmt.Sprintf(gateQuorum, err)
			case !ok:
				return gateNotQuorate
			}
		}
		if c.epochDrawn() {
			return gateEpoch
		}
	}
	if !c.writerRead {
		return gateNoWriter
	}
	switch v := c.e.lastVerdict; v {
	case VerdictOK:
	case "":
		return gateNoCycle
	case verdictNoRun:
		return gateNoRun
	case VerdictBehind:
		return gateBehind
	case VerdictUnknown:
		return gateUnknown
	default:
		return fmt.Sprintf(gatePrevious, v)
	}
	us, stored, err := c.e.writer()
	switch {
	case err != nil:
		return fmt.Sprintf(gateReread, err)
	case stored.Generation != us.Generation || stored.Nonce != us.Nonce:
		return gateChanged
	}
	return ""
}

// setAddr is the address the egress set holds for the target of a binding:
// its service address for one on a leg, which is zero until it was published
// under one, and its own address otherwise.
func setAddr(b resolve.Binding) netip.Addr {
	if b.Via == "" {
		return b.Addr
	}
	return b.Service
}

// mappingKey is a real address on a network, which has one service address.
type mappingKey struct {
	network string
	addr    netip.Addr
}

// mappings are the service addresses as the step knows them, by the real
// address each forwards to and by itself.
type mappings struct {
	byKey     map[mappingKey]store.ServiceAddr
	byService map[netip.Addr]store.ServiceAddr
}

func newMappings(all []store.ServiceAddr) mappings {
	ms := mappings{byKey: map[mappingKey]store.ServiceAddr{}, byService: map[netip.Addr]store.ServiceAddr{}}
	for _, m := range all {
		ms.add(m)
	}
	return ms
}

func (ms mappings) add(m store.ServiceAddr) {
	ms.byKey[mappingKey{m.Network, m.Addr}] = m
	ms.byService[m.Service] = m
}

func (ms mappings) remove(m store.ServiceAddr) {
	delete(ms.byKey, mappingKey{m.Network, m.Addr})
	delete(ms.byService, m.Service)
}

// list is every mapping, by network and then by address.
func (ms mappings) list() []store.ServiceAddr {
	out := slices.Collect(maps.Values(ms.byKey))
	slices.SortFunc(out, func(a, b store.ServiceAddr) int {
		return cmp.Or(cmp.Compare(a.Network, b.Network), a.Addr.Compare(b.Addr))
	})
	return out
}

// gateway is the step between the resolution and the plan. Every winner
// proven through a leg is a target on a leg, which a connector may dial at
// its service address only: one that exists is looked up, a new one made when
// the gate is open; without one, the target is held. A route that left the
// address its service address forwards to takes that service address along
// when the gate is open, and keeps a rule that reaches nothing while it is
// not; either way nothing forwards to the address it left. The gateway is
// then given the legs of this node, every service address the egress set of
// this cycle holds with the ports it holds it at, and the neighbours proven
// behind them, before anything is written at Cloudflare; a service address
// neither in the set nor in a configuration verified at Cloudflare is
// released after that.
func (c *cycleRun) gateway() {
	c.legKept, c.legIfaces, c.legNets, c.frozen = map[string]string{}, map[string]string{}, map[string]bool{}, map[netip.Addr]bool{}
	legs := c.markLegTargets()
	if c.e.d.Gateway == nil {
		for _, rt := range legs {
			if serves(rt, c.results[rt.Hostname]) {
				c.withoutService(rt, gateNoGateway)
			}
		}
		return
	}
	nets, err := c.e.d.Store.Networks()
	if err != nil {
		c.hold(c.storeProblem("reading the networks", err))
		return
	}
	for id, n := range nets {
		c.legIfaces[id] = gateway.LegIface(n.Mark)
		if _, ok := n.Legs[c.e.d.Node]; ok {
			c.legNets[id] = true
		}
	}
	all, err := c.e.d.Store.ServiceAddrs()
	if err != nil {
		c.hold(c.storeProblem("reading the service addresses", err))
		return
	}
	ms := newMappings(all)
	followed := c.followMovers(ms)
	for _, rt := range legs {
		if !followed[rt.Hostname] {
			c.serviceAddr(rt, ms)
		}
	}
	if c.storeHold {
		return
	}
	c.syncGateway(nets, ms.list())
}

// leaving is a service address some of whose routes left the address it
// forwards to: movers are the winners whose binding is elsewhere now, and
// stayers says that a winner published under it is still there.
type leaving struct {
	m       store.ServiceAddr
	movers  []model.Route
	stayers bool
}

// leavers are the service addresses a route left in this cycle, as the
// stored bindings, which say what each route was published under, and the
// bindings of this cycle tell it.
func (c *cycleRun) leavers(ms mappings) []*leaving {
	by := map[netip.Addr]*leaving{}
	for _, rt := range c.claims.Winners {
		prev, bound := c.bindings[rt.Hostname]
		res, ok := c.results[rt.Hostname]
		if !bound || !ok || res.Binding == nil || prev.Via == "" || !prev.Service.IsValid() {
			continue
		}
		m, mapped := ms.byService[prev.Service]
		if !mapped {
			continue
		}
		l := by[m.Service]
		if l == nil {
			l = &leaving{m: m}
			by[m.Service] = l
		}
		if (mappingKey{res.Binding.Via, res.Binding.Addr}) == (mappingKey{m.Network, m.Addr}) {
			l.stayers = true
		} else {
			l.movers = append(l.movers, rt)
		}
	}
	var out []*leaving
	for _, service := range slices.SortedFunc(maps.Keys(by), netip.Addr.Compare) {
		if l := by[service]; len(l.movers) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// followMovers deals with every service address a route left, and returns
// the hostnames it published. Routes still at the address keep it. Otherwise
// the routes that left take it along to the address they all went to, which
// has none of its own, when the gate is open: their rules do not change, and
// it forwards to their new address from now on. While the gate is closed
// their rules are kept, and the service address forwards nothing. When they
// cannot take it along, it is released at once, and they find their own.
func (c *cycleRun) followMovers(ms mappings) map[string]bool {
	followed := map[string]bool{}
	for _, l := range c.leavers(ms) {
		if l.stayers {
			continue
		}
		to, ok := c.followTo(l, ms)
		if !ok {
			c.abandon(l.m, ms)
			continue
		}
		host := l.movers[0].Hostname
		open, why := c.gateOpen("service address for " + host)
		if open {
			moved, err := c.e.d.Store.MoveServiceAddr(l.m.Network, l.m.Addr, to.network, to.addr, c.now)
			if err == nil {
				ms.remove(l.m)
				ms.add(moved)
				for _, rt := range l.movers {
					c.publishAt(rt, moved.Service)
					followed[rt.Hostname] = true
				}
				c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindNetwork, Subject: moved.Network, Route: host,
					Message: fmt.Sprintf("service address %s moved from %s on network %s to %s on network %s, with %s",
						moved.Service, l.m.Addr, l.m.Network, moved.Addr, moved.Network, host)})
				continue
			}
			why = err.Error()
		}
		c.freeze(l.m.Service)
		for _, rt := range l.movers {
			c.publishAt(rt, l.m.Service)
			res := c.results[rt.Hostname]
			res.Target.Reachable, res.Target.Reason = false, fmt.Sprintf(reasonNotMoved, why)
			c.results[rt.Hostname] = res
			c.legKept[rt.Hostname] = res.Target.Reason
			c.problem(problemServiceKept, rt.Hostname, to.addr, to.network, l.m.Service, why)
			followed[rt.Hostname] = true
		}
	}
	return followed
}

// followTo is the address every route that left l's service address went
// to, when it can take the service address along: each is served there,
// through a leg of this node, and the address has no service address yet.
func (c *cycleRun) followTo(l *leaving, ms mappings) (mappingKey, bool) {
	var to mappingKey
	for i, rt := range l.movers {
		res := c.results[rt.Hostname]
		key := mappingKey{res.Binding.Via, res.Binding.Addr}
		switch {
		case !serves(rt, res), key.network == "", !c.legNets[key.network], i > 0 && key != to:
			return mappingKey{}, false
		}
		to = key
	}
	if _, mapped := ms.byKey[to]; mapped {
		return mappingKey{}, false
	}
	return to, true
}

// abandon releases a service address no route is at any more, at once: a
// rule still sending a connector there reaches nothing, and the address it
// forwarded to gets a new one when a route is proven there.
func (c *cycleRun) abandon(m store.ServiceAddr, ms mappings) {
	c.freeze(m.Service)
	if err := c.e.d.Store.ReleaseServiceAddr(m.Network, m.Addr); err != nil {
		c.problem(problemServiceRelease, m.Service, m.Addr, m.Network, err)
		return
	}
	ms.remove(m)
	c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindNetwork, Subject: m.Network,
		Message: fmt.Sprintf("service address %s of %s on network %s released", m.Service, m.Addr, m.Network)})
}

// freeze has a service address forward nothing in this cycle: it leaves the
// egress filter at once and the set of this cycle, and so the services the
// gateway is given.
func (c *cycleRun) freeze(service netip.Addr) {
	c.frozen[service] = true
	if c.lost[service] {
		return
	}
	c.lost[service] = true
	if !slices.ContainsFunc(c.e.egress, func(t egress.Target) bool { return t.Addr == service }) {
		return
	}
	if err := c.e.d.Egress.Remove(c.ctx, service); err != nil {
		c.problem("taking %s out of the egress filter: %v", service, err)
	}
}

// markLegTargets marks the target of every winner whose binding is on a leg,
// and returns those winners. A binding of the address its stored binding had
// through the same leg keeps the service address that was published under,
// so that the egress set can take it out by it whatever this cycle finds; the
// binding of any other winner was published under none.
func (c *cycleRun) markLegTargets() []model.Route {
	var out []model.Route
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		if !ok || res.Binding == nil {
			continue
		}
		b := res.Binding
		if b.Via == "" {
			b.Service = netip.Addr{}
			continue
		}
		if prev, bound := c.bindings[rt.Hostname]; bound && !b.Service.IsValid() && prev.Via == b.Via && prev.Addr == b.Addr {
			b.Service = prev.Service
		}
		res.Target.Leg = true
		c.results[rt.Hostname] = res
		out = append(out, rt)
	}
	return out
}

// serves reports whether the target of a winner is one a rule would send a
// connector to.
func serves(rt model.Route, res resolve.Result) bool {
	t := res.Target
	return t.Addr.IsValid() && !t.Withdrawn && !t.Rejected && (t.Owner == "" || t.Owner == rt.Owner())
}

// serviceAddr gives the target of a winner on a leg its service address: the
// one its real address on the network has, or one made now, when the gate is
// open. A target that is not served keeps what its binding was published
// under, for the egress set to take it out by; one on a network with no leg
// on this node, or at an address whose service address forwards nothing
// while it waits for the route that left it, gets none.
func (c *cycleRun) serviceAddr(rt model.Route, ms mappings) {
	res := c.results[rt.Hostname]
	if !serves(rt, res) {
		return
	}
	b := res.Binding
	if !c.legNets[b.Via] {
		c.withoutService(rt, fmt.Sprintf(gateNoLeg, b.Via))
		return
	}
	key := mappingKey{b.Via, res.Target.Addr}
	if m, ok := ms.byKey[key]; ok {
		if c.frozen[m.Service] {
			c.withoutService(rt, fmt.Sprintf(gateFrozen, m.Service))
			return
		}
		c.publishAt(rt, m.Service)
		return
	}
	open, why := c.gateOpen("service address for " + rt.Hostname)
	if open {
		m, err := c.e.d.Store.AllocateServiceAddr(key.network, key.addr, c.now)
		if err == nil {
			ms.add(m)
			c.publishAt(rt, m.Service)
			c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindNetwork, Subject: m.Network, Route: rt.Hostname,
				Message: fmt.Sprintf("service address %s allocated for %s on network %s", m.Service, m.Addr, m.Network)})
			return
		}
		why = err.Error()
	}
	c.withoutService(rt, why)
}

// publishAt gives the target of a winner on a leg the service address it is
// published under.
func (c *cycleRun) publishAt(rt model.Route, service netip.Addr) {
	res := c.results[rt.Hostname]
	res.Target.Via, res.Binding.Service = service, service
	c.results[rt.Hostname] = res
}

// withoutService holds the target of a winner on a leg that has no service
// address this cycle, for why. Its real address is never what a rule sends
// a connector to.
func (c *cycleRun) withoutService(rt model.Route, why string) {
	res := c.results[rt.Hostname]
	res.Target.Via, res.Target.Reason = netip.Addr{}, fmt.Sprintf(reasonNoService, why)
	c.results[rt.Hostname] = res
	c.problem(problemServiceHeld, rt.Hostname, res.Binding.Via, why)
}

// syncGateway gives the gateway what this cycle wants of it and keeps what it
// says of the legs; a gateway that cannot be programmed holds the tunnel run.
// A node that never had a network asks the gateway for nothing.
func (c *cycleRun) syncGateway(nets map[string]store.Network, mappings []store.ServiceAddr) {
	legs := c.wantedLegs(nets)
	services := c.wantedServices(legs, mappings)
	pins := c.wantedPins(services)
	if len(legs) == 0 && len(services) == 0 && !c.e.gwInUse {
		// The gateway holds no leg a freed mark could move from.
		c.e.freedMarks = nil
		c.releaseUnused(mappings, services)
		return
	}
	states, err := c.e.d.Gateway.Sync(c.ctx, legs, services, pins, nil)
	if err != nil {
		c.gatewayHeld = heldGateway
		c.problem(problemGatewaySync, err)
		return
	}
	c.e.gwInUse = len(legs) > 0 || len(services) > 0
	c.e.freedMarks = nil
	c.events = append(c.events, legEvents(c.e.LegStates(), states, c.now)...)
	c.e.setLegStates(states)
	for _, st := range states {
		switch {
		case !st.Up:
			c.problem(problemLegDown, st.ID, cmp.Or(st.Problem, "the gateway does not say why"))
		case st.Problem != "":
			c.problem(problemLegNote, st.ID, st.Problem)
		}
	}
	kept := c.releaseUnused(mappings, services)
	c.saveGatewayCache(legs, services, pins, nets, kept)
}

// wantedLegs are the legs of the networks with a leg address on this node,
// by network.
func (c *cycleRun) wantedLegs(nets map[string]store.Network) []gateway.Leg {
	var out []gateway.Leg
	for _, id := range slices.Sorted(maps.Keys(nets)) {
		n := nets[id]
		addr, ok := n.Legs[c.e.d.Node]
		if !ok {
			continue
		}
		mac, err := net.ParseMAC(n.LegMAC)
		if err != nil {
			c.problem("network %s: the leg MAC %q: %v; its leg is left out", id, n.LegMAC, err)
			continue
		}
		out = append(out, gateway.Leg{
			ID: id, Mark: n.Mark, Bridge: n.Bridge, VLAN: n.VLAN, Prefix: netip.PrefixFrom(addr, n.Prefix.Bits()),
			MAC: mac, Managed: n.Kind == store.NetworkManaged, Uplink: n.Uplink,
		})
	}
	return out
}

// wantedServices are the service addresses of the networks with a leg here
// that the egress set of this cycle holds, each with the ports it holds it
// at: those of the targets served now and those a configuration verified at
// Cloudflare still sends a connector to, as the set takes them.
func (c *cycleRun) wantedServices(legs []gateway.Leg, mappings []store.ServiceAddr) []gateway.Service {
	ports := map[netip.Addr][]uint16{}
	for _, t := range c.egressSet() {
		ports[t.Addr] = append(ports[t.Addr], t.Port)
	}
	var out []gateway.Service
	for _, m := range mappings {
		p, ok := ports[m.Service]
		if !ok || !slices.ContainsFunc(legs, func(l gateway.Leg) bool { return l.ID == m.Network }) {
			continue
		}
		out = append(out, gateway.Service{Leg: m.Network, Service: m.Service, Addr: m.Addr, Ports: slices.Compact(slices.Sorted(slices.Values(p)))})
	}
	slices.SortFunc(out, func(a, b gateway.Service) int {
		return cmp.Or(cmp.Compare(a.Leg, b.Leg), a.Addr.Compare(b.Addr))
	})
	return out
}

// wantedPins are the MACs proven for the real addresses the services forward
// to, once each.
func (c *cycleRun) wantedPins(services []gateway.Service) []gateway.Pin {
	forwarded := map[mappingKey]bool{}
	for _, s := range services {
		forwarded[mappingKey{s.Leg, s.Addr}] = true
	}
	seen := map[mappingKey]bool{}
	var out []gateway.Pin
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		if !ok || res.Binding == nil || !res.Target.Leg || !serves(rt, res) {
			continue
		}
		key := mappingKey{res.Binding.Via, res.Target.Addr}
		mac, err := net.ParseMAC(res.Binding.MAC)
		if err != nil || !forwarded[key] || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, gateway.Pin{Leg: key.network, Addr: key.addr, MAC: mac})
	}
	slices.SortFunc(out, func(a, b gateway.Pin) int { return cmp.Or(cmp.Compare(a.Leg, b.Leg), a.Addr.Compare(b.Addr)) })
	return out
}

// releaseUnused deletes every mapping of a network with a leg on this node
// whose service address the gateway was not given and no configuration
// verified at Cloudflare sends a connector to, and returns the others. A
// released address is never given again; the mappings of another node's
// networks are its own to release.
func (c *cycleRun) releaseUnused(mappings []store.ServiceAddr, services []gateway.Service) []store.ServiceAddr {
	inUse := map[netip.Addr]bool{}
	for _, s := range services {
		inUse[s.Service] = true
	}
	for t := range c.e.sentTargets() {
		inUse[t.Addr] = true
	}
	var kept []store.ServiceAddr
	for _, m := range mappings {
		if inUse[m.Service] || !c.legNets[m.Network] {
			kept = append(kept, m)
			continue
		}
		if err := c.e.d.Store.ReleaseServiceAddr(m.Network, m.Addr); err != nil {
			c.problem(problemServiceRelease, m.Service, m.Addr, m.Network, err)
			kept = append(kept, m)
			continue
		}
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindNetwork, Subject: m.Network,
			Message: fmt.Sprintf("service address %s of %s on network %s released", m.Service, m.Addr, m.Network)})
	}
	return kept
}

// saveGatewayCache keeps on the node what the gateway was given: the cache
// pco netns up restores the namespace from at boot, the networks, service
// addresses and leases it serves, and the pins with the time they were
// proven. One that cannot be saved is a problem line; it holds nothing.
func (c *cycleRun) saveGatewayCache(legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, nets map[string]store.Network, mappings []store.ServiceAddr) {
	var failed []string
	fail := func(err error) {
		if err != nil {
			failed = append(failed, err.Error())
		}
	}
	if dir := c.e.d.LocalDir; dir != "" {
		fail(gateway.WriteCache(dir, gateway.Cache{Legs: legs, Services: services, Pins: pins, PinsAt: c.now}))
	}
	leases, err := c.e.d.Store.Leases()
	fail(err)
	cache := store.NetCache{ServiceAddrs: nonNil(mappings), Leases: []store.Lease{}, WrittenAt: c.now}
	for _, id := range slices.Sorted(maps.Keys(nets)) {
		cache.Networks = append(cache.Networks, nets[id])
	}
	for _, mac := range slices.Sorted(maps.Keys(leases)) {
		cache.Leases = append(cache.Leases, leases[mac])
	}
	fail(c.e.d.Store.SaveNetCache(cache))
	saved := make([]store.Pin, len(pins))
	for i, p := range pins {
		saved[i] = store.Pin{Network: p.Leg, Addr: p.Addr, MAC: p.MAC.String()}
	}
	fail(c.e.d.Store.SavePins(saved, c.now))
	if len(failed) > 0 {
		c.problem(problemGatewayCache, strings.Join(failed, "; "))
	}
}

// setLegStates keeps what the last sync said of the legs.
func (e *Engine) setLegStates(states []gateway.LegState) {
	e.legMu.Lock()
	defer e.legMu.Unlock()
	e.legStates = slices.Clone(states)
}

// LegStates returns what the gateway said of the legs at the last sync, in
// the order of the networks.
func (e *Engine) LegStates() []gateway.LegState {
	e.legMu.Lock()
	defer e.legMu.Unlock()
	return slices.Clone(e.legStates)
}

// legState says how the leg of a network is, as PathView.Leg shows it: up,
// or why not; empty when the gateway has not said.
func (e *Engine) legState(network string) string {
	e.legMu.Lock()
	defer e.legMu.Unlock()
	i := slices.IndexFunc(e.legStates, func(s gateway.LegState) bool { return s.ID == network })
	switch {
	case i < 0:
		return ""
	case e.legStates[i].Up:
		return LegUp
	}
	return cmp.Or(e.legStates[i].Problem, "down")
}

// The words of what a check of the gateway namespace found.
const (
	problemGatewayRestored = "the gateway namespace was changed outside pco and was brought back"
	problemGatewayNotKept  = "the gateway namespace was changed outside pco and could not be brought back: %s"
	problemGatewayFailed   = "checking the gateway namespace: %s"
)

// GatewayCheck is what the daemon found at one of its checks of the gateway
// namespace. The zero value is all of it as pco left it.
type GatewayCheck struct {
	// Restored says what differed when the namespace was brought back.
	Restored string
	// NotKept says why it could not be brought back, as when it is gone,
	// Failed why it could not be checked.
	NotKept string
	Failed  string
}

// NoteGateway takes what a check of the gateway namespace found. What was
// brought back is an event and a problem line of the next cycle; what could
// not be, as a namespace that is gone until pco-netns is restarted, is a
// problem line for as long as that stands, and an event when it starts.
func (e *Engine) NoteGateway(c GatewayCheck) {
	now := e.d.Now()
	var events []Event
	add := func(level, msg string) {
		events = append(events, Event{At: now, Level: level, Kind: kindNetwork, Subject: gateway.Namespace, Message: msg})
	}

	e.noteMu.Lock()
	switch {
	case c.Failed != "":
		e.notes.lines = appendOnce(e.notes.lines, fmt.Sprintf(problemGatewayFailed, c.Failed))
	case c.NotKept != "":
		if c.NotKept != e.notes.gwFault {
			add(levelError, fmt.Sprintf(problemGatewayNotKept, c.NotKept))
		}
		e.notes.gwFault = c.NotKept
	default:
		e.notes.gwFault = ""
		if c.Restored != "" {
			add(levelWarn, problemGatewayRestored)
			e.notes.lines = appendOnce(e.notes.lines, problemGatewayRestored+" ("+c.Restored+")")
		}
	}
	e.noteMu.Unlock()
	e.events.add(events...)
}

// checkGateway brings the gateway back to what was last wanted of it when it
// is not so, before anything probes through it. The appliance's start and
// its keeper withhold that until its self-identification passed, so that a
// copy never adds the original's cards to itself; the check of this cycle is
// that proof. What the check found goes into this cycle's state, in place of
// the fault that stood when the cycle began.
func (c *cycleRun) checkGateway() {
	if c.e.d.Gateway == nil || (c.st.Identity != nil && !c.st.Identity.OK) {
		return
	}
	c.e.noteMu.Lock()
	stood := c.e.notes.gwFault
	c.e.noteMu.Unlock()
	found := c.e.d.Gateway.Check(c.ctx)
	c.e.NoteGateway(found)
	c.e.noteMu.Lock()
	lines := c.e.notes.lines
	c.e.notes.lines = nil
	c.e.noteMu.Unlock()
	c.st.Problems = append(c.st.Problems, lines...)
	if found.Failed != "" {
		return
	}
	if stood != "" {
		c.st.Problems = slices.DeleteFunc(c.st.Problems, func(l string) bool { return l == fmt.Sprintf(problemGatewayNotKept, stood) })
	}
	if found.NotKept != "" {
		c.problem(problemGatewayNotKept, found.NotKept)
	}
}

// GatewayAllowed reports whether the gateway may be changed outside a cycle:
// always on the host; in the appliance only while its last
// self-identification passed and it serves, so that a copy never adds a card
// of the original to itself.
func (e *Engine) GatewayAllowed() bool { return e.refusedAsCopy() == nil }

// Idle runs fn while no cycle and no verification of a moved address runs,
// so that nothing probes through the gateway while fn changes it. It waits
// for them until ctx ends, and then runs nothing.
func (e *Engine) Idle(ctx context.Context, fn func()) error {
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.release()
	fn()
	return nil
}
