package engine

import (
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// catchUp makes sure that a store that lags behind what the nodes hold gives
// nothing twice, before the cycle allocates anything: the pool of service
// addresses moves on past every service address an agent's gateway maps or
// a configuration verified at Cloudflare sends a connector to, and a managed
// card that a guest has keeps its lease. It runs in every cycle of an epoch
// until every node the registry serves has reported, so that what a node
// that reports late holds counts too. A node alone has its store to itself,
// which nothing else writes, and does not run it.
func (c *cycleRun) catchUp() {
	if !c.writerRead || !c.clustered() || c.e.caughtUp == c.epoch() {
		return
	}
	if c.e.saidIn == nil {
		c.e.saidIn = map[string]agentproto.Epoch{}
	}
	if c.raisePool() && c.keepCardLeases() && c.allReported() {
		c.e.caughtUp = c.epoch()
	}
}

// allReported says whether every node the registry serves a snapshot has a
// fresh report.
func (c *cycleRun) allReported() bool {
	reports := c.reportsNow()
	for name, n := range c.nodes {
		if !servedState(n.Node.State) {
			continue
		}
		rep, ok := reports[name]
		if !ok || !rep.Fresh || c.now.Sub(rep.ReceivedAt) >= silentAfter {
			return false
		}
	}
	return true
}

// maxCatchUp is how far past the next address of the pool an address an
// agent reports may move it: an agent's word can hold back an address the
// store lost, and never give the pool away.
const maxCatchUp = 1024

// raisePool moves the pool of service addresses on past the highest service
// address the gateway of an admitted node maps or a verified configuration
// names. It reports whether it could tell.
func (c *cycleRun) raisePool() bool {
	next, _, _, err := c.e.d.Store.ServicePool()
	if err != nil {
		c.problem("reading the pool of service addresses: %v; no service address is made until it can be read", err)
		c.storeHold = true
		return false
	}
	held := c.reportedServices(next)
	for t := range c.e.sentTargets() {
		held[t.Addr] = true
	}
	want, highest := next, netip.Addr{}
	for addr := range held {
		if addr = addr.Unmap(); gateway.ServicePrefix.Contains(addr) && !addr.Less(want) {
			want, highest = addr.Next(), addr
		}
	}
	if want == next {
		return true
	}
	if err := c.e.d.Store.SaveServicePool(want); err != nil {
		c.problem("moving the pool of service addresses on to %s: %v; no service address is made until it moved", want, err)
		c.storeHold = true
		return false
	}
	c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindNetwork,
		Message: fmt.Sprintf("the pool of service addresses moved on from %s to %s: an agent's gateway or a configuration verified at Cloudflare holds %s",
			next, want, highest)})
	return true
}

// reportedServices are the service addresses the agents of the admitted
// nodes report their gateways map, each from the report of the node its
// certificate names. One of a node that is not Active or Draining, or one
// past maxCatchUp beyond next, is not taken, and said once per epoch: it
// would hold back addresses no gateway maps.
func (c *cycleRun) reportedServices(next netip.Addr) map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	reports := c.reportsNow()
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		rep := reports[name]
		n, known := c.nodes[name]
		admitted := known && (n.Node.State == store.NodeActive || n.Node.State == store.NodeDraining)
		if !admitted || rep.Status.Node != name || len(rep.Status.ServiceAddrs) == 0 {
			continue
		}
		limit := next
		for range maxCatchUp {
			limit = limit.Next()
		}
		for _, addr := range rep.Status.ServiceAddrs {
			addr = addr.Unmap()
			if !gateway.ServicePrefix.Contains(addr) {
				continue
			}
			if limit.Less(addr) {
				c.refuseReported(name, addr, next)
				continue
			}
			out[addr] = true
		}
	}
	return out
}

// refuseReported says once per epoch that a node reports a service address
// too far past the pool to be taken.
func (c *cycleRun) refuseReported(node string, addr, next netip.Addr) {
	c.saidOnce(node+" "+addr.String(), Event{At: c.now, Level: levelWarn, Kind: kindNetwork, Subject: node,
		Message: fmt.Sprintf("node %s reports that its gateway maps %s, more than %d past %s, the next address of the pool; it is not taken",
			node, addr, maxCatchUp, next)})
}

// keepCardLeases gives every managed card of a guest whose lease is missing
// its lease again, from its MAC, which carries its address, and says which
// leases have no card in any guest: they are kept, so that their addresses
// are not given to another card. It reports whether the leases could be
// read; a lease that cannot be saved again is a problem line.
func (c *cycleRun) keepCardLeases() bool {
	nets, err := c.e.d.Store.Networks()
	if err != nil {
		c.problem("reading the networks: %v", err)
		return false
	}
	n, managed := nets[gateway.ManagedBridge]
	if !managed {
		return true
	}
	carded := map[string]bool{}
	leases, err := c.e.d.Store.Leases()
	if err != nil {
		c.problem("reading the leases: %v", err)
		return false
	}
	atts, err := c.e.d.Store.Attachments()
	if err != nil {
		c.problem("reading the attachments: %v", err)
		return false
	}
	for _, a := range atts {
		// The managed step keeps the lease of an attachment, and gives it
		// back once its card is gone.
		carded[strings.ToLower(a.MAC)] = true
	}
	owners := cardOwners(c.snap.Guests)
	for _, g := range c.snap.Guests {
		for _, nic := range g.NICs {
			mac, err := net.ParseMAC(nic.MAC)
			if err != nil || nic.Bridge != gateway.ManagedBridge || len(mac) != 6 {
				continue
			}
			text := mac.String()
			carded[text] = true
			if _, ok := leases[text]; ok {
				continue
			}
			if others := owners[text]; len(others) > 1 {
				// Two cards with one MAC: neither is given the address.
				c.saidOnce("two cards "+text, Event{At: c.now, Level: levelWarn, Kind: kindManaged, Subject: g.Ref.String(), Guest: guestOf(g.Ref.String()),
					Message: fmt.Sprintf("the managed card %s is a card of %s; its lease, missing from the store, is not saved again for any of them",
						text, strings.Join(others, " and "))})
				continue
			}
			addr, ok := cardAddr(n.Prefix, mac)
			if !ok {
				continue
			}
			l := store.Lease{MAC: text, Addr: addr, Owner: g.Ref.String(), Identity: g.Identity, NIC: nic.Index, Since: c.now}
			if err := c.e.d.Store.SaveLease(l, 0); err != nil {
				c.problem("saving again the lease of %s for net%d of %s: %v", addr, nic.Index, g.Ref, err)
				continue
			}
			leases[text] = l
			c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindManaged, Subject: g.Ref.String(), Guest: guestOf(g.Ref.String()),
				Message: fmt.Sprintf("the lease of %s for net%d of %s was missing from the store and is saved again", addr, nic.Index, g.Ref)})
		}
	}
	for _, mac := range slices.Sorted(maps.Keys(leases)) {
		if l := leases[mac]; !carded[strings.ToLower(mac)] {
			c.saidOnce("no card "+mac, Event{At: c.now, Level: levelWarn, Kind: kindManaged, Subject: l.Owner, Guest: guestOf(l.Owner),
				Message: fmt.Sprintf("the lease of %s of %s has no card in any guest; its address is kept and not given again until the lease is released", l.Addr, l.Owner)})
		}
	}
	return true
}

// cardOwners are, by MAC, the guests that have a card of it on the managed
// network, once each.
func cardOwners(guests []model.Guest) map[string][]string {
	out := map[string][]string{}
	for _, g := range guests {
		for _, nic := range g.NICs {
			mac, err := net.ParseMAC(nic.MAC)
			if err != nil || nic.Bridge != gateway.ManagedBridge || slices.Contains(out[mac.String()], g.Ref.String()) {
				continue
			}
			out[mac.String()] = append(out[mac.String()], g.Ref.String())
		}
	}
	return out
}

// saidOnce adds ev to the events of the cycle once per epoch and key.
func (c *cycleRun) saidOnce(key string, ev Event) {
	if c.e.saidIn[key] == c.epoch() {
		return
	}
	c.e.saidIn[key] = c.epoch()
	c.events = append(c.events, ev)
}

// cardAddr is the address a managed card's MAC carries in subnet: pco's
// prefix and the last two octets of the address.
func cardAddr(subnet netip.Prefix, mac net.HardwareAddr) (netip.Addr, bool) {
	if mac[0] != 0x06 || mac[1] != 0x50 || mac[2] != 0x43 || mac[3] != 0x4f || !subnet.Addr().Is4() {
		return netip.Addr{}, false
	}
	b := subnet.Masked().Addr().As4()
	addr := netip.AddrFrom4([4]byte{b[0], b[1], mac[4], mac[5]})
	return addr, subnet.Contains(addr) && store.ManagedMAC(addr) == mac.String()
}
