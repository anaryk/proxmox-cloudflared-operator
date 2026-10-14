package engine

import (
	"encoding/binary"
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

// problemCatchUp says why the cycle makes no service address and no managed
// card: the store is not known to hold all that the nodes hold.
const problemCatchUp = "no service address and no managed card is made until the store is checked against every node: %s"

// catchUp makes sure that a store that lags behind what the nodes hold gives
// nothing twice, before the cycle allocates anything: the pool of service
// addresses moves on past every service address an agent's gateway maps or
// a configuration verified at Cloudflare sends a connector to, and a managed
// card that a guest has keeps its lease. Until every node the registry
// serves has reported in an epoch, and while any report names an address in
// use that is not taken from it, the gate of an expanding write stays
// closed: nothing is allocated from a store that may not be current. A node
// alone has its store to itself, which nothing else writes, and does not
// run it.
func (c *cycleRun) catchUp() {
	if !c.writerRead || !c.clustered() {
		return
	}
	if c.e.saidIn == nil {
		c.e.saidIn = map[string]agentproto.Epoch{}
	}
	why := c.raisePool()
	if why == "" && c.e.caughtUp != c.epoch() {
		switch silent := c.silentNode(); {
		case !c.keepCardLeases():
			why = "the leases are not yet checked against the cards of the guests"
		case silent != "":
			why = fmt.Sprintf("node %s has not reported to this controller yet", silent)
		default:
			c.e.caughtUp = c.epoch()
		}
	}
	if why != "" {
		c.catchUpWhy = why
		c.problem(problemCatchUp, why)
	}
}

// silentNode is the first node the registry serves a snapshot, or that went
// silent and is not known to be down, that has no fresh report; empty when
// every one has.
func (c *cycleRun) silentNode() string {
	reports := c.reportsNow()
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		if s := c.nodes[name].Node.State; !servedState(s) && s != store.NodeUnknown {
			// A node that went silent and is not known to be down may
			// still map what the store lost.
			continue
		}
		rep, ok := reports[name]
		if !ok || !rep.Fresh || c.now.Sub(rep.ReceivedAt) >= silentAfter {
			return name
		}
	}
	return ""
}

// maxCatchUp is how far past the next address of the pool an address an
// agent reports may move it: an agent's word can hold back an address the
// store lost, and never give the pool away.
const maxCatchUp = 1024

// maxReported is the most service addresses a node's report may name to be
// taken at all, and maxLeasesAgain the most leases catchUp saves again in a
// cycle: what a report or the inventory holds never grows the work of a
// cycle, or the store, without bound. The rest waits for the next cycle.
const (
	maxReported    = 4096
	maxLeasesAgain = 64
)

// raisePool moves the pool of service addresses on past the highest service
// address the gateway of an admitted node maps or a verified configuration
// names. It says why nothing may be allocated now: the pool could not be
// read or moved, or a report names an address at or past the pool's next
// that is not taken from it.
func (c *cycleRun) raisePool() string {
	next, _, _, err := c.e.d.Store.ServicePool()
	if err != nil {
		c.storeHold = true
		return fmt.Sprintf("reading the pool of service addresses: %v", err)
	}
	held, refused := c.reportedServices(next)
	for t := range c.e.sentTargets() {
		held[t.Addr] = true
	}
	want, highest := next, netip.Addr{}
	for addr := range held {
		if addr = addr.Unmap(); gateway.ServicePrefix.Contains(addr) && !addr.Less(want) {
			want, highest = addr.Next(), addr
		}
	}
	if want != next {
		if err := c.e.d.Store.SaveServicePool(want); err != nil {
			c.storeHold = true
			return fmt.Sprintf("moving the pool of service addresses on to %s: %v", want, err)
		}
		c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindNetwork,
			Message: fmt.Sprintf("the pool of service addresses moved on from %s to %s: an agent's gateway or a configuration verified at Cloudflare holds %s",
				next, want, highest)})
	}
	for _, r := range refused {
		if r.addr.IsValid() && r.addr.Less(want) {
			continue
		}
		return fmt.Sprintf("node %s reports that its gateway maps %s, which is not taken from it: %s", r.node, r.what(), r.why)
	}
	return ""
}

// refusedReport is what a report names that is not taken from it: an
// address, or every address of a report that names too many.
type refusedReport struct {
	node string
	addr netip.Addr // zero for the whole report
	n    int
	why  string
}

func (r refusedReport) what() string {
	if r.addr.IsValid() {
		return r.addr.String()
	}
	return fmt.Sprintf("%d service addresses", r.n)
}

// reportedServices are the service addresses the agents of the admitted
// nodes report their gateways map, each from the report of the node its
// certificate names, and what is not taken: the addresses of a node that is
// not Active or Draining or of a report that names another node, those past
// maxCatchUp beyond next, and every one of a report that names more than
// maxReported. What is not taken moves the pool nowhere, so that no node
// can give the pool away; it holds every allocation instead, while it names
// an address the pool would give.
func (c *cycleRun) reportedServices(next netip.Addr) (map[netip.Addr]bool, []refusedReport) {
	out := map[netip.Addr]bool{}
	var refused []refusedReport
	limit := next
	for range maxCatchUp {
		limit = limit.Next()
	}
	reports := c.reportsNow()
	for _, name := range slices.Sorted(maps.Keys(reports)) {
		rep := reports[name]
		n, known := c.nodes[name]
		admitted := known && (n.Node.State == store.NodeActive || n.Node.State == store.NodeDraining)
		if count := len(rep.Status.ServiceAddrs); count > maxReported {
			why := fmt.Sprintf("more than %d", maxReported)
			c.saidOnce("many "+name, Event{At: c.now, Level: levelWarn, Kind: kindNetwork, Subject: name,
				Message: fmt.Sprintf("node %s reports that its gateway maps %d service addresses, %s; none of them is taken", name, count, why)})
			refused = append(refused, refusedReport{node: name, n: count, why: why})
			continue
		}
		for _, addr := range rep.Status.ServiceAddrs {
			addr = addr.Unmap()
			if !gateway.ServicePrefix.Contains(addr) {
				continue
			}
			var why string
			switch {
			case !admitted:
				why = "the node is not admitted"
			case rep.Status.Node != name:
				why = fmt.Sprintf("its report names node %s", rep.Status.Node)
			case limit.Less(addr):
				why = fmt.Sprintf("more than %d past %s, the next address of the pool", maxCatchUp, next)
			default:
				out[addr] = true
				continue
			}
			c.saidOnce(name+" "+addr.String(), Event{At: c.now, Level: levelWarn, Kind: kindNetwork, Subject: name,
				Message: fmt.Sprintf("node %s reports that its gateway maps %s, which is not taken from it: %s", name, addr, why)})
			refused = append(refused, refusedReport{node: name, addr: addr, why: why})
		}
	}
	return out, refused
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
	saved := 0
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
			if saved >= maxLeasesAgain {
				// Not every card was looked at: the epoch is not caught up.
				return false
			}
			saved++
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
	base := subnet.Masked().Addr()
	last := binary.BigEndian.Uint32(b[:]) | (uint32(1)<<(32-subnet.Bits()) - 1)
	lastB := [4]byte{}
	binary.BigEndian.PutUint32(lastB[:], last)
	// The subnet's own address, pco's leg and the broadcast address are no card's.
	if addr == base || addr == base.Next() || addr == netip.AddrFrom4(lastB) {
		return netip.Addr{}, false
	}
	return addr, subnet.Contains(addr) && store.ManagedMAC(addr) == mac.String()
}
