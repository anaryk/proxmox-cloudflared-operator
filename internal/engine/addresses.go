package engine

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// learnNodeAddrs adds the node addresses of the snapshot to the ones known,
// which start with those saved before. Only a cycle that goes on saves them.
func (c *cycleRun) learnNodeAddrs() bool {
	a := &c.e.addrs
	if !a.loaded {
		saved, err := c.e.d.Store.NodeAddrs()
		if err != nil {
			c.hold(c.problem("reading the saved node addresses: %v", err))
			return false
		}
		a.list, a.loaded = saved, true
	}
	merged := slices.Compact(slices.SortedFunc(slices.Values(append(slices.Clone(a.list), c.snap.NodeAddrs()...)), netip.Addr.Compare))
	if len(merged) != len(a.list) {
		a.list, a.dirty = merged, true
	}
	return true
}

// resolveTargets has the agent of this node resolve the address of every
// winner, with the binding of its hostname, and take the addresses that lost
// their proof out of the egress filter; then raises in the appliance what the
// managed network's isolation proves to filtered, holds back what is proven
// below the identity minimum or by a rule the admin has not acknowledged,
// gives the targets on a leg their service addresses and programs the
// gateway, and has the agent watch what is served and keep the bindings. A
// cycle whose context ends in the middle plans and changes nothing.
func (c *cycleRun) resolveTargets() bool {
	st, err := c.begin(c.snapshot())
	if err != nil {
		c.hold(c.problem(problemStoppedResolve, err))
		return false
	}
	if err := st.Refused(); err != nil {
		// The agent follows a later controller than this one: this engine
		// is behind, and changes nothing the agent would not take from it.
		c.agentRefused = true
		c.hold(c.problem(problemAgentRefused, err))
	}
	res, err := st.Resolve()
	if cerr := c.ctx.Err(); cerr != nil {
		c.hold(c.problem(problemStoppedResolve, cerr))
		return false
	}
	if err != nil {
		c.hold(c.problem("resolving the addresses: %v", err))
		return false
	}
	c.results = res.Results
	c.st.Problems = append(c.st.Problems, res.Problems...)
	c.lost = make(map[netip.Addr]bool, len(res.Lost))
	for _, addr := range res.Lost {
		c.lost[addr] = true
	}
	c.filtered()
	c.holdBelowMinimum()
	c.holdObserved()
	c.gateway()
	for _, line := range st.Bind(c.results, c.netLegs) {
		c.problem("%s", line)
	}

	if c.e.addrs.dirty {
		if err := c.e.d.Store.SaveNodeAddrs(c.e.addrs.list); err != nil {
			c.problem("saving the node addresses: %v", err)
		} else {
			c.e.addrs.dirty = false
		}
	}
	if !c.softSaved {
		if err := c.e.d.Store.SaveSoftDeny(c.soft); err != nil {
			c.problem("saving the soft deny list: %v", err)
		}
	}
	return true
}

// learnSoft makes the soft deny list of this cycle: the gateways of the nodes
// in the snapshot, the resolvers of the nodes as last read and the gateway and
// resolvers of the appliance, each seen now, and the entries saved before that
// were seen within softKeptFor. The time an entry was seen is moved on once it
// is softRefresh old, so that a list seen again is not written every cycle.
func (c *cycleRun) learnSoft(acc accessView) bool {
	saved, err := c.e.d.Store.SoftDeny()
	if err != nil {
		c.hold(c.storeProblem("reading the soft deny list", err))
		return false
	}
	seen := map[netip.Addr]string{}
	see := func(addr netip.Addr, why string) {
		if addr = addr.Unmap(); addr.Is4() {
			if _, ok := seen[addr]; !ok {
				seen[addr] = why
			}
		}
	}
	for _, n := range c.snap.Nodes {
		for _, ifc := range n.Ifaces {
			if ifc.Gateway.IsValid() {
				see(ifc.Gateway, "gateway of node "+n.Name)
			}
		}
	}
	for _, node := range slices.Sorted(maps.Keys(acc.dns)) {
		for _, addr := range acc.dns[node] {
			see(addr, "resolver of node "+node)
		}
	}
	if own := c.e.d.OwnSoft; own != nil {
		gateways, resolvers, err := own()
		if err != nil {
			c.problem("reading the gateway and resolvers of the appliance: %v; the ones seen before stay soft-denied", err)
		}
		for _, addr := range gateways {
			see(addr, "gateway of the appliance")
		}
		for _, addr := range resolvers {
			see(addr, "resolver of the appliance")
		}
	}
	c.soft, c.softSaved = mergeSoft(saved, seen, c.now), true
	if !slices.Equal(c.soft.Entries, saved.Entries) {
		c.softSaved = false
	}
	return true
}

const (
	// softKeptFor is how long an entry of the soft deny list stays once it is
	// no longer seen.
	softKeptFor = 30 * 24 * time.Hour
	// softRefresh is how old the time an entry was seen gets before it is
	// written again.
	softRefresh = time.Hour
)

// mergeSoft is the soft deny list saved, with the entries seen at now.
func mergeSoft(saved store.SoftDeny, seen map[netip.Addr]string, now time.Time) store.SoftDeny {
	out := store.SoftDeny{Entries: []store.SoftEntry{}}
	kept := map[netip.Addr]bool{}
	for _, e := range saved.Entries {
		why, again := seen[e.Addr]
		age := now.Sub(e.LastSeen)
		switch {
		case again && why == e.Why && age >= 0 && age < softRefresh:
			out.Entries = append(out.Entries, e)
			kept[e.Addr] = true
		case !again && age <= softKeptFor:
			out.Entries = append(out.Entries, e)
		}
	}
	for addr, why := range seen {
		if !kept[addr] {
			out.Entries = append(out.Entries, store.SoftEntry{Addr: addr, Why: why, LastSeen: now})
		}
	}
	slices.SortFunc(out.Entries, func(a, b store.SoftEntry) int { return a.Addr.Compare(b.Addr) })
	return out
}

// softMap is what the denylist takes of the soft deny list.
func softMap(soft store.SoftDeny) map[netip.Addr]string {
	out := make(map[netip.Addr]string, len(soft.Entries))
	for _, e := range soft.Entries {
		out[e.Addr] = e.Why
	}
	return out
}

// sdnGateways are the gateways of the SDN subnets, which are never published.
func sdnGateways(subnets []pve.Subnet) map[netip.Addr]string {
	out := map[netip.Addr]string{}
	for _, s := range subnets {
		if s.Gateway.IsValid() {
			out[s.Gateway] = fmt.Sprintf("gateway of SDN subnet %s of vnet %s", s.Prefix, s.Vnet)
		}
	}
	return out
}
