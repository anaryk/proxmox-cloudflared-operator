package engine

import (
	"cmp"
	"encoding/json"
	"maps"
	"net"
	"net/netip"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// netsRead is what the snapshots of the other nodes give of the networks,
// read once per cycle: the networks, the leases and the attachments of the
// managed network. err says why they could not be read.
type netsRead struct {
	nets        map[string]store.Network
	leases      map[string]store.Lease
	attachments map[string]store.Attachment
	err         error
}

// readNets reads the networks for the snapshots of the other nodes, once.
func (c *cycleRun) readNets() *netsRead {
	if c.netsOfNodes != nil {
		return c.netsOfNodes
	}
	r := &netsRead{}
	c.netsOfNodes = r
	if r.nets, r.err = c.e.d.Store.Networks(); r.err != nil {
		return r
	}
	if _, managed := r.nets[gateway.ManagedBridge]; !managed {
		return r
	}
	if r.leases, r.err = c.e.d.Store.Leases(); r.err != nil {
		return r
	}
	r.attachments, r.err = c.e.d.Store.Attachments()
	return r
}

// networksFor is the networks of another node as its snapshot gives them,
// in the gateway's own form: the legs of the networks with a leg address on
// the node, each with the node's own MAC, and, while the managed network has
// its leg there, every lease pinned to its MAC and the sticky entries of the
// managed cards of the guests that run on the node. A node with no leg gets
// a form without one, which takes away what its gateway had. Networks that
// could not be read give nothing, which leaves its gateway as it is.
func (c *cycleRun) networksFor(node string) json.RawMessage {
	r := c.readNets()
	if r.err != nil {
		if !c.netsSaid {
			c.netsSaid = true
			c.problem("reading the networks for the other nodes: %v; their gateways are left as they are", r.err)
		}
		return nil
	}
	cache := gateway.Cache{Legs: c.legsOn(node, r.nets)}
	if slices.ContainsFunc(cache.Legs, func(l gateway.Leg) bool { return l.Managed }) {
		pins := leasePins(r.leases)
		for _, key := range slices.SortedFunc(maps.Keys(pins), compareBehindLegs) {
			cache.Pins = append(cache.Pins, *pins[key])
		}
		cache.Sticky = c.stickyOn(node, r.attachments)
	}
	raw, err := json.Marshal(cache)
	if err != nil {
		c.problem("writing the networks of node %s: %v; its gateway is left as it is", node, err)
		return nil
	}
	return raw
}

func compareBehindLegs(a, b behindLeg) int {
	return cmp.Or(cmp.Compare(a.leg, b.leg), a.addr.Compare(b.addr))
}

// legsOn are the legs of the networks with a leg address on node, by
// network, each with the MAC of the node's leg: the managed leg has one on
// every node, and an attached network's leg carries the index the registry
// gave the node. A leg whose MAC is not known is left out, with a problem
// line: two legs with one MAC on one LAN would answer for each other.
func (c *cycleRun) legsOn(node string, nets map[string]store.Network) []gateway.Leg {
	var out []gateway.Leg
	for _, id := range slices.Sorted(maps.Keys(nets)) {
		n := nets[id]
		addr, ok := n.Legs[node]
		if !ok {
			continue
		}
		text, err := c.legMAC(n, node)
		if err != nil {
			c.problem("network %s: %v; its leg on node %s is left out", id, err, node)
			continue
		}
		mac, err := net.ParseMAC(text)
		if err != nil {
			c.problem("network %s: the leg MAC %q of node %s: %v; its leg is left out", id, text, node, err)
			continue
		}
		out = append(out, gateway.Leg{
			ID: id, Mark: n.Mark, Bridge: n.Bridge, VLAN: n.VLAN, Prefix: netip.PrefixFrom(addr, n.Prefix.Bits()),
			MAC: mac, Managed: n.Kind == store.NetworkManaged, Uplink: n.Uplink,
		})
	}
	return out
}

// legMAC is the MAC of the leg of n on node, as the index the registry gave
// the node makes it. A node alone, which the registry may not number yet,
// keeps the MAC the network was saved with.
func (c *cycleRun) legMAC(n store.Network, node string) (string, error) {
	mac, err := n.LegMACOn(node, c.nodeIndexes())
	if err != nil && node == c.e.d.Node && !c.clustered() {
		return n.LegMAC, nil
	}
	return mac, err
}

// nodeIndexes are the indexes of the nodes of the registry, by name.
func (c *cycleRun) nodeIndexes() map[string]int {
	out := map[string]int{}
	for name, n := range c.nodes {
		if n.Node.Index != nil {
			out[name] = *n.Node.Index
		}
	}
	return out
}

// legServed is a target on a leg as it is published: its address on a
// network and the service address the connectors dial for it.
type legServed struct {
	network string
	addr    netip.Addr
	service netip.Addr
}

// legService is how the target of a route is published on a leg in this
// cycle; false for one that is not on a leg, or has no service address.
func (c *cycleRun) legService(host string) (legServed, bool) {
	if rl, ok := c.remoteLegs[host]; ok {
		if !rl.service.IsValid() {
			return legServed{}, false
		}
		return legServed{network: rl.key.network, addr: rl.key.addr, service: rl.service}, true
	}
	res, ok := c.results[host]
	if !ok || res.Binding == nil || res.Binding.Via == "" || !res.Target.Leg || !res.Target.Via.IsValid() {
		return legServed{}, false
	}
	return legServed{network: res.Binding.Via, addr: res.Target.Addr, service: res.Target.Via}, true
}

// giveService gives a target on a leg the service address it is published
// under, and the address that maps to, which the node's binding must have
// for the gateway to forward to it: a node dials such a target at its
// service address only.
func (c *cycleRun) giveService(t *agentproto.Target) {
	ls, ok := c.legService(t.Key)
	if !ok || t.Addr.IsValid() && t.Addr != ls.addr {
		return
	}
	t.Addr, t.Service = ls.addr, ls.service
}
