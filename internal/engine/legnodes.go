package engine

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// problemLegMissing says that a network that is one segment on every
	// node serves a route and has no leg on a replica.
	problemLegMissing = "network %s has no leg on node %s; its routes are served from the nodes that have one"

	// problemLegRefused says that what a node reported of a target behind
	// one of its legs does not match what the controller knows of the node,
	// the network and the guest: the target is not served through the leg.
	problemLegRefused = "node %s reported %s behind network %s, which is refused: %s; it is not served through the leg"

	// whyLeg is why a route behind a leg of a network of single nodes is
	// pinned to the node of the leg.
	whyLeg = "it is reached through network %s, which is a segment of node %s alone"
)

// remoteLeg is the target of a winner that another node resolves through
// one of its legs: the address it found, on the network of that node, and
// the service address the connectors dial for it, or why it has none.
type remoteLeg struct {
	rt      model.Route
	res     resolve.Result
	node    string
	key     mappingKey
	service netip.Addr
	why     string
	ok      bool // the node verified it, at its service address
	// refused says that what the node reported is not taken: it names a
	// leg, an address or a card the controller does not know of it.
	refused bool
}

// noteNets keeps the networks of the cycle, and which are networks of
// single nodes: an attached network whose bridge is a segment of each node,
// whose real addresses are mapped by node. The managed network is one of
// single nodes too, but its addresses are one IPAM's, so they are mapped
// across the cluster: a guest that migrates keeps its service address.
func (c *cycleRun) noteNets(nets map[string]store.Network) {
	c.nets, c.scoped = nets, map[string]bool{}
	for id, n := range nets {
		c.scoped[id] = n.Kind == store.NetworkAttach && n.ScopeName() == store.ScopeNode
	}
}

// serveRemoteLegs gives every target another node resolves through a leg
// its service address, as serviceAddr does for this node's: the mapping of
// its address on the network, of that node's network for a network of
// single nodes, made when the gate is open. A node alone has none.
func (c *cycleRun) serveRemoteLegs(ms mappings) {
	if c.fleet == nil {
		return
	}
	c.remoteLegs = map[string]*remoteLeg{}
	for _, rt := range c.claims.Winners {
		node := c.resolverOf(rt)
		if node == c.e.d.Node {
			continue
		}
		tr, ok := c.reportOn(node, rt, true)
		if !ok {
			continue
		}
		res, err := agent.DecodeResult(tr.Result)
		if err != nil || res.Binding == nil || res.Binding.Via == "" || !serves(rt, res) {
			continue
		}
		rl := &remoteLeg{rt: rt, res: res, node: node, key: c.keyOn(res.Binding.Via, node, res.Target.Addr), ok: tr.OK}
		if why := c.legRefused(rt, res, node); why != "" {
			rl.refused, rl.why = true, why
			c.problem(problemLegRefused, node, rt.Hostname, res.Binding.Via, why)
		}
		c.remoteLegs[rt.Hostname] = rl
	}
	for _, host := range slices.Sorted(maps.Keys(c.remoteLegs)) {
		rl := c.remoteLegs[host]
		if rl.refused {
			continue
		}
		rl.service, rl.why = c.mappingFor(rl.rt, rl.res, rl.key, ms)
		if !rl.service.IsValid() {
			c.problem(problemServiceHeld, host, rl.key.network, rl.why)
		}
	}
	c.legsMissing()
}

// legRefused says why what node reported of the target of rt behind one of
// its legs is not taken; empty when it is. An agent's word decides no leg,
// address or card: the network must be attached with a leg on that node, the
// address a guest's on the network, and the binding of the route's guest,
// which runs on that node, through a card the inventory gives the guest on
// the network's bridge and VLAN, of the MAC the address has on the managed
// network.
func (c *cycleRun) legRefused(rt model.Route, res resolve.Result, node string) string {
	b, addr := res.Binding, res.Target.Addr
	n, attached := c.nets[b.Via]
	if !attached {
		return fmt.Sprintf("network %s is not attached", b.Via)
	}
	if _, ok := n.Legs[node]; !ok {
		return fmt.Sprintf("node %s has no leg into it", node)
	}
	if !n.Prefix.Contains(addr) || slices.Contains(slices.Collect(maps.Values(n.Legs)), addr) || addr == n.Prefix.Masked().Addr() {
		return fmt.Sprintf("%v is no guest's address on %s", addr, n.Prefix)
	}
	if rt.Guest == nil || b.Guest != rt.Guest.String() || b.Addr != addr {
		return fmt.Sprintf("the binding is of %s at %v, not of the route's guest", b.Guest, b.Addr)
	}
	g, ok := c.snap.Guest(*rt.Guest)
	if !ok || g.Node != node {
		return fmt.Sprintf("%s does not run on node %s", rt.Guest, node)
	}
	mac, err := model.NormalizeMAC(b.MAC)
	if err != nil {
		return fmt.Sprintf("the binding has no MAC: %v", err)
	}
	card := slices.ContainsFunc(g.NICs, func(nic model.NIC) bool {
		own, err := model.NormalizeMAC(nic.MAC)
		return err == nil && own == mac && nic.Bridge == n.Bridge && nic.VLAN == n.VLAN
	})
	switch {
	case !card:
		return fmt.Sprintf("%s has no card %s on %s", rt.Guest, mac, store.SegmentID(n.Bridge, n.VLAN))
	case n.Kind == store.NetworkManaged && store.ManagedMAC(addr) != mac:
		return fmt.Sprintf("%v on the managed network is the address of card %s, not %s", addr, store.ManagedMAC(addr), mac)
	}
	return ""
}

// remoteOnly is the gateway step of a node without a gateway of its own in a
// cluster: the targets other nodes resolve through their legs get their
// service addresses all the same.
func (c *cycleRun) remoteOnly() {
	if c.fleet == nil {
		return
	}
	nets, err := c.e.d.Store.Networks()
	if err != nil {
		c.hold(c.storeProblem("reading the networks", err))
		return
	}
	c.noteNets(nets)
	if ms, ok := c.readMappings(); ok {
		c.serveRemoteLegs(ms)
	}
}

// legsMissing says, of every network that is one segment on every node and
// serves a winner, which replica has no leg into it.
func (c *cycleRun) legsMissing() {
	serving := map[string]bool{}
	for _, rt := range c.claims.Winners {
		if _, network, ok := c.legOf(rt); ok {
			serving[network] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(serving)) {
		n, ok := c.nets[id]
		if !ok || n.ScopeName() != store.ScopeCluster {
			continue
		}
		for _, node := range c.replicas() {
			if _, has := n.Legs[node]; !has {
				c.problem(problemLegMissing, id, node)
			}
		}
	}
}

// legOf is the node and the network through whose leg the node that
// resolves a route reaches its target; false for one that is not behind a
// leg.
func (c *cycleRun) legOf(rt model.Route) (node, network string, ok bool) {
	if res, found := c.results[rt.Hostname]; found && c.resolverOf(rt) == c.e.d.Node && res.Binding != nil && res.Binding.Via != "" {
		return c.e.d.Node, res.Binding.Via, true
	}
	if rl, found := c.remoteLegs[rt.Hostname]; found && !rl.refused {
		return rl.node, rl.key.network, true
	}
	return "", "", false
}

// legFixed is the node a route behind a leg is pinned to, whatever the
// replicas reach: the node of the leg, on a network that is a segment of
// each node, which no other node reaches. A network that is one segment on
// every node pins nothing: every node with a leg reaches its targets.
func (c *cycleRun) legFixed(rt model.Route) (string, string, bool) {
	node, network, ok := c.legOf(rt)
	if !ok {
		return "", "", false
	}
	if n, known := c.nets[network]; known && n.ScopeName() == store.ScopeCluster {
		return "", "", false
	}
	return node, fmt.Sprintf(whyLeg, network, node), true
}

// serviceWait says why a route behind a leg is not verified at its service
// address yet by the node that resolves it: it has none, or that node was
// not given it yet. Empty for one that is, or is not behind a leg.
func (c *cycleRun) serviceWait(routes []model.Route) string {
	for _, rt := range routes {
		switch rl, ok := c.remoteLegs[rt.Hostname]; {
		case ok && !rl.service.IsValid():
			return fmt.Sprintf(reasonNoService, rl.why)
		case ok && !rl.ok:
			return fmt.Sprintf(reasonWaiting, rl.node)
		}
		res, ok := c.results[rt.Hostname]
		if ok && c.resolverOf(rt) == c.e.d.Node && res.Target.Leg && !res.Target.Via.IsValid() && serves(rt, res) {
			return res.Target.Reason
		}
	}
	return ""
}

// agentServices are the service addresses the gateways of the other nodes
// map, as their agents last reported them.
func (c *cycleRun) agentServices() map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	if c.fleet == nil {
		return out
	}
	for _, f := range c.fleet.nodes {
		for _, addr := range f.report.Status.ServiceAddrs {
			out[addr] = true
		}
	}
	return out
}

// ownerWins reports whether a winner of this cycle is of owner.
func (c *cycleRun) ownerWins(owner string) bool {
	return slices.ContainsFunc(c.claims.Winners, func(rt model.Route) bool { return rt.Owner() == owner })
}
