package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// firewallConcurrency is how many guests' firewalls the managed step
	// reads and writes at a time, as many as the inventory reads guests.
	firewallConcurrency = 4
	// canaryConcurrency is how many canaries are out at a time: all of the
	// guests of a large vnet in one go, so that a cycle waits for one
	// timeout, not one per guest.
	canaryConcurrency = 64

	problemFilteredLost = "filtered lost: %s; the routes on pconet are served at observed"

	reasonFilteredNotEnabled = "filtered is proven only on the managed network pconet, which is not enabled on this node"
)

// filteredMemory is what the steps of the appliance's isolation keep: what
// the managed step of cycle read of the node's firewall and of the guests'
// firewalls, for the filtered step of the same cycle, which lets it go; the
// port of each guest's canary; and when the predicate was last judged and
// how. Guarded by the cycle lock.
type filteredMemory struct {
	cycle       *cycleRun
	backend     backendRead
	reads       map[string]firewallRead // by owner
	attachments map[string]store.Attachment
	net         store.Network

	canary   map[string]uint16 // by owner
	judged   time.Time
	lastSaid *FilteredView
}

// backendRead is what the managed step read of the node's firewall.
type backendRead struct {
	read                bool
	node                pve.NodeFirewallOptions
	nodeErr, clusterErr error
	cluster             pve.ClusterFirewallOptions
	service             string
	serviceErr          error
}

// firewallRead is the firewall of an attached guest as the managed step left
// it: the card pco writes, and the guest's firewall read after any write.
type firewallRead struct {
	card managednet.FirewallCard
	fw   *managednet.GuestFirewall
	err  error
}

// fresh makes the memory that of the cycle c.
func (m *filteredMemory) fresh(c *cycleRun) {
	if m.cycle == c {
		return
	}
	m.cycle, m.backend, m.reads, m.attachments = c, backendRead{}, map[string]firewallRead{}, nil
	if m.canary == nil {
		m.canary = map[string]uint16{}
	}
}

// filtered judges, in the appliance, whether every guest on the managed
// network is isolated by its firewall, and raises the routes proven at
// observed on it to filtered while that holds, or takes them back to
// observed. It runs after the resolution and before the minimum is applied.
func (c *cycleRun) filtered() {
	mem := &c.e.filtered
	defer mem.letGo()
	if !c.appliance() {
		return
	}
	m := &c.st.Managed
	if !m.Enabled {
		m.Filtered = nil
		mem.lastSaid = nil
		c.raiseFiltered(false)
		return
	}
	in := c.filteredInput()
	res := managednet.Filtered(in, managednet.FirewallComment(c.install.ID))
	mem.judged = c.now
	view := &FilteredView{Holds: res.Holds, Backend: res.Backend, Offered: res.Offered, Why: res.Why}
	m.Filtered = view
	c.raiseFiltered(view.Holds)
	if !view.Holds {
		c.problem(problemFilteredLost, firstWhy(view))
	}
	c.filteredEvent(view)
}

// letGo drops what the managed step left for the filtered step once it is
// used.
func (m *filteredMemory) letGo() {
	m.cycle, m.backend, m.reads, m.attachments, m.net = nil, backendRead{}, nil, nil, store.Network{}
}

// filteredInput gathers what the predicate reads: the node's firewall and the
// guests' as the managed step of this cycle read them, the cards on the vnet
// in the snapshot, and the canaries.
func (c *cycleRun) filteredInput() managednet.FilteredInput {
	mem := &c.e.filtered
	in := managednet.FilteredInput{Node: c.e.d.Node}
	notRead := errors.New("not read in this cycle")
	b := mem.backend
	if mem.cycle != c || !b.read {
		b = backendRead{nodeErr: notRead, clusterErr: notRead, serviceErr: notRead}
	}
	in.NodeFirewall, in.NodeFirewallErr = b.node, b.nodeErr
	in.ClusterFirewall, in.ClusterFirewallErr = b.cluster, b.clusterErr
	in.Service, in.ServiceErr = b.service, b.serviceErr
	if !c.st.Managed.Vnet.OK {
		in.Vnet = slices.Clone(c.st.Managed.Vnet.Why)
	}
	in.Unknown = c.e.unknownSince(mem.judged)
	if mem.cycle != c || mem.attachments == nil {
		in.Unread = []string{"the attachments of the managed network were not read in this cycle"}
		return in
	}
	var self model.GuestRef
	if c.install.Appliance != nil {
		self = model.GuestRef{Kind: model.KindLXC, VMID: c.install.Appliance.VMID}
	}
	in.Foreign = managednet.Classify(c.snap.Guests, mem.attachments, self, mem.net.LegMAC)
	for _, owner := range slices.SortedFunc(maps.Keys(mem.attachments), model.CompareOwners) {
		if card, ok := c.cardOnVnet(owner, mem.attachments[owner]); ok {
			in.Cards = append(in.Cards, card)
		}
	}
	c.canaries(in.Cards)
	return in
}

// cardOnVnet is the attachment's card when the snapshot has it on the vnet.
func (c *cycleRun) cardOnVnet(owner string, att store.Attachment) (managednet.FilteredCard, bool) {
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return managednet.FilteredCard{}, false
	}
	g, ok := c.snap.Guest(ref)
	if !ok {
		return managednet.FilteredCard{}, false
	}
	nic, ok := g.NIC(att.NIC)
	if !ok || nic.Bridge != gateway.ManagedBridge || !strings.EqualFold(nic.MAC, att.MAC) {
		return managednet.FilteredCard{}, false
	}
	card := managednet.FilteredCard{Owner: owner, Firewall: nic.Firewall, Running: g.Running && g.Node == c.e.d.Node}
	read, ok := c.e.filtered.reads[owner]
	switch {
	case !ok:
		card.Card = c.firewallCard(c.e.filtered.net, g, att)
		card.ReadErr = errors.New("the managed step did not read it")
	default:
		card.Card, card.Read, card.ReadErr = read.card, read.fw, read.err
	}
	return card, true
}

// canaries probes each running card from the leg, all of them at once, on a
// port no route of its guest publishes, and puts what each found on it.
func (c *cycleRun) canaries(cards []managednet.FilteredCard) {
	mem := &c.e.filtered
	leg, legOK := c.managedLeg()
	var wg sync.WaitGroup
	slots := make(chan struct{}, canaryConcurrency)
	for i := range cards {
		card := &cards[i]
		if !card.Running {
			continue
		}
		port := managednet.CanaryPort(card.Card.Ports, mem.canary[card.Owner])
		mem.canary[card.Owner] = port
		switch {
		case port == 0:
			card.Canary = &managednet.CanaryResult{Verdict: managednet.CanaryNotSent, Detail: "every port below 1024 is published"}
			continue
		case c.e.d.Gateway == nil || !legOK:
			card.Canary = &managednet.CanaryResult{Port: port, Verdict: managednet.CanaryNotSent, Detail: "the leg of " + gateway.ManagedBridge + " is not known here"}
			continue
		}
		addr := netip.AddrPortFrom(card.Card.Lease, port)
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			dial := func(ctx context.Context, a netip.AddrPort) (net.Conn, error) { return c.e.d.Gateway.Dial(ctx, leg, a) }
			res := managednet.Canary(c.ctx, dial, addr, managednet.CanaryTimeout)
			card.Canary = &res
		}()
	}
	wg.Wait()
	for owner := range mem.canary {
		if !slices.ContainsFunc(cards, func(fc managednet.FilteredCard) bool { return fc.Owner == owner }) {
			delete(mem.canary, owner)
		}
	}
}

// managedLeg is the leg of the managed network on this node.
func (c *cycleRun) managedLeg() (gateway.Leg, bool) {
	n := c.e.filtered.net
	legs := c.wantedLegs(map[string]store.Network{n.ID: n})
	if len(legs) != 1 || n.ID != gateway.ManagedBridge {
		return gateway.Leg{}, false
	}
	return legs[0], true
}

// raiseFiltered raises every route proven at observed through the managed
// network to filtered while the predicate holds, its binding with it; and,
// while it does not, takes a binding raised before back to observed, so that
// nothing stands on a filtered proof that no longer holds.
func (c *cycleRun) raiseFiltered(holds bool) {
	for host, res := range c.results {
		if res.Binding == nil || res.Binding.Via != gateway.ManagedBridge {
			continue
		}
		b := *res.Binding
		switch {
		case holds && res.Level == resolve.LevelObserved:
			res.Level, b.Level = resolve.LevelFiltered, resolve.LevelFiltered
		case !holds && (b.Level == resolve.LevelFiltered || res.Level == resolve.LevelFiltered):
			b.Level = resolve.LevelObserved
			if res.Level == resolve.LevelFiltered {
				res.Level = resolve.LevelObserved
			}
		default:
			continue
		}
		res.Binding = &b
		c.results[host] = res
	}
}

// filteredEvent says once when the predicate stops holding, or holds for
// another first reason, and once when it holds again.
func (c *cycleRun) filteredEvent(view *FilteredView) {
	mem := &c.e.filtered
	last := mem.lastSaid
	mem.lastSaid = view
	switch {
	case !view.Holds && (last == nil || last.Holds || firstWhy(last) != firstWhy(view)):
		c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindManaged, Subject: gateway.ManagedBridge,
			Message: fmt.Sprintf(problemFilteredLost, firstWhy(view))})
	case view.Holds && last != nil && !last.Holds:
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindManaged, Subject: gateway.ManagedBridge,
			Message: "filtered holds on pconet again: every guest on the vnet is isolated by its firewall"})
	}
}

func firstWhy(v *FilteredView) string {
	if len(v.Why) == 0 {
		return "no reason was found"
	}
	return v.Why[0]
}

// unknownSince are the MACs the source-MAC watch reported after t.
func (e *Engine) unknownSince(t time.Time) []string {
	e.macMu.Lock()
	defer e.macMu.Unlock()
	var out []string
	for mac, at := range e.unknownMACs {
		if at.After(t) {
			out = append(out, mac)
		}
	}
	slices.Sort(out)
	return out
}

// filteredNote says why a minimum of filtered holds back the routes of the
// appliance: the managed network is not enabled, the backend does not offer
// the level, or the predicate does not hold now.
func (c *cycleRun) filteredNote() string {
	if !c.appliance() || c.requiredLevel() != resolve.LevelFiltered {
		return ""
	}
	f := c.st.Managed.Filtered
	switch {
	case !c.st.Managed.Enabled:
		return reasonFilteredNotEnabled
	case f == nil:
		return ""
	case !f.Offered:
		return firstWhy(f)
	case !f.Holds:
		return "filtered lost: " + firstWhy(f)
	}
	return ""
}
