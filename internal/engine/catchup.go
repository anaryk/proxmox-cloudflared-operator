package engine

import (
	"encoding/binary"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
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
// serves has reported in an epoch the gate of an expanding write stays
// closed: nothing is allocated from a store that may not be current. A node
// alone has its store to itself, which nothing else writes, and does not
// run it.
func (c *cycleRun) catchUp() {
	if !c.writerRead || !c.clustered() {
		return
	}
	m := c.e.caught.of(c.epoch())
	s := c.silentNodes()
	for _, name := range s.overdue {
		c.problem(problemSilentNode, name, silentWait, name, name)
	}
	why := c.raisePool(m, s.waiting == "" && s.unknown == "", s.overdue)
	if why == "" && !m.caughtUp {
		switch {
		case !c.keepCardLeases():
			why = "the leases are not yet checked against the cards of the guests"
		case s.unknown != "":
			why = fmt.Sprintf("node %s has not reported to this controller, so what its gateway maps is not known: "+
				"bring its agent back, or pco node forget %s", s.unknown, s.unknown)
		case s.waiting != "":
			why = fmt.Sprintf("node %s has not reported to this controller yet", s.waiting)
		default:
			m.caughtUp = true
		}
	}
	if why != "" {
		c.catchUpWhy = why
		c.problem(problemCatchUp, why)
	}
}

// problemSilentNode names a node that sent nothing for silentWait, which
// allocation no longer waits for.
const problemSilentNode = "node %s has sent no status to this controller for %s; service addresses and managed cards are made without it, " +
	"past what its last report maps: bring its agent back, or pco node drain %s and pco node forget %s"

// silentWait is how long allocation waits for a node that has no fresh
// report: a node that reported to this controller, or one never admitted,
// that sent nothing for as long stops holding it back.
const silentWait = 5 * time.Minute

// silence is what catchUp knows of the nodes without a fresh report: the
// first that allocation waits for, the first admitted node that never
// reported to this controller, whose holdings are not known, and the nodes
// it no longer waits for.
type silence struct {
	waiting string
	unknown string
	overdue []string
}

// silentNodes sorts the nodes the registry serves a snapshot, or that went
// silent and are not known to be down, that have no fresh report.
func (c *cycleRun) silentNodes() silence {
	var out silence
	reports := c.reportsNow()
	since := c.since()
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		if !servedState(n.State) && n.State != store.NodeUnknown {
			// A node that went silent and is not known to be down may
			// still map what the store lost.
			continue
		}
		rep, ok := reports[name]
		if ok && c.fresh(rep) {
			continue
		}
		heard := ok && !rep.ReceivedAt.Before(since)
		from := since
		switch {
		case heard:
			from = rep.ReceivedAt
		case n.Since.After(from):
			// A node that joined in this tenure is waited for from then.
			from = n.Since
		}
		switch {
		case c.now.Sub(from) < silentWait:
			if out.waiting == "" {
				out.waiting = name
			}
		case !heard && n.Index != nil:
			// Admitted once, and never heard by this controller: what its
			// gateway maps is not known, and it holds allocation back
			// until it reports or is forgotten.
			if out.unknown == "" {
				out.unknown = name
			}
		default:
			out.overdue = append(out.overdue, name)
		}
	}
	return out
}

// keepCardLeases gives every managed card of a guest whose lease is missing
// its lease again, from its MAC, which carries its address, and says which
// leases have no card in any guest: they are kept, so that their addresses
// are not given to another card. It reports whether every card that needs
// its lease has it: false when the leases could not be read, or a lease
// could not be saved again, which is a problem line too.
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
	attached := map[string]string{} // by MAC, the owner of its attachment
	for _, a := range atts {
		// The managed step keeps the lease of an attachment, and gives it
		// back once its card is gone.
		carded[strings.ToLower(a.MAC)] = true
		attached[strings.ToLower(a.MAC)] = a.Owner
	}
	owners := cardOwners(c.snap.Guests)
	saved, done := 0, true
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
			if owner := g.Ref.String(); !g.HasTag(managednet.Tag) && attached[text] != owner {
				// Whoever may change a guest's cards can add one with
				// pco's MAC of any address: only a guest pco gave a card,
				// or that asks for one, gets a lease from it.
				c.saidOnce("stray card "+text, Event{At: c.now, Level: levelWarn, Kind: kindManaged, Subject: owner, Guest: guestOf(owner),
					Message: fmt.Sprintf("the managed card %s of %s has no lease, and %s neither carries the tag %s nor has an attachment of it; its lease is not saved again",
						text, owner, owner, managednet.Tag)})
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
				// Its address could be given to another card meanwhile.
				c.problem("saving again the lease of %s for net%d of %s: %v", addr, nic.Index, g.Ref, err)
				done = false
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
	return done
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
	said := c.e.caught.of(c.epoch()).said
	if said[key] {
		return
	}
	said[key] = true
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
