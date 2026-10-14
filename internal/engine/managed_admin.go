package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Managed returns the managed network as the last cycle found it.
func (e *Engine) Managed() (ManagedView, error) { return e.State().Managed, nil }

// ApproveManaged consents to a card on the managed network for a guest, in
// the identity the last listing showed for it: a guest re-created under its
// VMID, or a clone, needs a consent of its own. identity, when it is not
// empty, is the identity the admin was shown. acceptFirewallImpact confirms
// for this guest that the appliance may turn its firewall on although
// another of its cards has firewall=1.
func (e *Engine) ApproveManaged(ctx context.Context, owner, identity string, acceptFirewallImpact bool) (store.Consent, error) {
	ref, err := guestOwner(owner)
	if err != nil {
		return store.Consent{}, err
	}
	if err := e.acquireWriter(ctx); err != nil {
		return store.Consent{}, err
	}
	defer e.release()

	l := e.lastListing()
	g, listed := l.guests[ref]
	switch {
	case !l.complete:
		return store.Consent{}, fmt.Errorf("%w: the last cycle did not list every guest, so %s cannot be approved as it is now; "+
			"approve what you can see once a cycle has listed them all", ErrRefused, owner)
	case !listed:
		return store.Consent{}, fmt.Errorf("%w: %s is not in the last listing of Proxmox; approve what you can see", ErrRefused, owner)
	case g.identity == "":
		return store.Consent{}, fmt.Errorf("%w: %s has no identity Proxmox reports, and a consent is of one", ErrRefused, owner)
	case identity != "" && identity != g.identity:
		return store.Consent{}, fmt.Errorf("%w: %s changed since it was shown: it was shown in identity %s and has identity %s now; "+
			"look at it again", ErrRefused, owner, identity, g.identity)
	}
	c := store.Consent{Owner: owner, Identity: g.identity, At: e.d.Now(), Actor: ActorOf(ctx), AcceptFirewallImpact: acceptFirewallImpact}
	if err := e.d.Store.SaveConsent(c); err != nil {
		return store.Consent{}, fmt.Errorf("saving the consent: %w", err)
	}
	msg := fmt.Sprintf("%s is approved for the managed network in identity %s", OwnerName(owner, l.guestView(owner)), g.identity)
	if acceptFirewallImpact {
		msg += ", with the firewall-impact confirmation"
	}
	e.adminEvent(ctx, owner, msg)
	e.Trigger()
	return c, nil
}

// RevokeManaged removes the consent of a guest. A card it has goes after
// the grace, unless the tag or automatic consent keeps it.
func (e *Engine) RevokeManaged(ctx context.Context, owner string) error {
	if _, err := guestOwner(owner); err != nil {
		return err
	}
	if err := e.acquireWriter(ctx); err != nil {
		return err
	}
	defer e.release()

	consents, err := e.d.Store.Consents()
	if err != nil {
		return fmt.Errorf("reading the consents: %w", err)
	}
	if _, ok := consents[owner]; !ok {
		return fmt.Errorf("%w: %s has no consent for the managed network", ErrNotFound, owner)
	}
	if err := e.d.Store.DeleteConsent(owner); err != nil {
		return fmt.Errorf("removing the consent: %w", err)
	}
	e.adminEvent(ctx, owner, fmt.Sprintf("the consent of %s for the managed network is revoked; a card it has goes after the grace",
		OwnerName(owner, e.lastListing().guestView(owner))))
	e.Trigger()
	return nil
}

// EnableManaged stores the managed network once its vnet exists as pco
// makes it: the subnet of the settings, the leg of this node at its first
// address, the lowest mark free, which honours a mark freed since the
// gateway was last synced, and the unit of the gateway namespace enabled
// when it is the first network of the node. The subnet is checked against
// the routes of the nodes, the addresses of the guests and the attached
// networks first. A network that is stored already is left as it is.
//
// On a cluster every other node gets a leg at the same address too, once
// the agent of the node found pconet there, the node not on it, and nobody
// at the address; the probes are asked before the cycle lock is taken. A
// node that gets none is an event with why, and a later call gives a leg to
// each node that has none yet, also of a network stored already.
func (e *Engine) EnableManaged(ctx context.Context) error {
	if err := e.networksHere(); err != nil {
		return err
	}
	ln, err := e.legNodes()
	if err != nil {
		return err
	}
	if ln.clustered {
		if err := e.controllerOnly(ctx); err != nil {
			return err
		}
	}
	probed, err := e.probeManaged(ctx, ln)
	if err != nil {
		return err
	}
	acquire := e.acquireWriter
	if len(probed.asked) > 0 {
		acquire = e.acquireFresh
	}
	if err := acquire(ctx); err != nil {
		return err
	}
	defer e.release()

	f, err := e.networkFacts(ctx)
	if err != nil {
		return err
	}
	have, stored := f.nets[gateway.ManagedBridge]
	if ln.clustered {
		subnet := f.settings.Managed.Subnet
		if stored {
			subnet = have.Prefix
		}
		if subnet != probed.subnet {
			return fmt.Errorf("%w: managed.subnet changed while the nodes were probed, from %s to %s; enable it again", ErrRefused, probed.subnet, subnet)
		}
	}
	if stored {
		if !ln.clustered {
			return nil
		}
		return e.addManagedLegs(ctx, f, ln, have, probed)
	}
	if e.d.Vnet == nil {
		return fmt.Errorf("%w: this daemon cannot read the vnet %s, so the managed network is not stored", ErrRefused, gateway.ManagedBridge)
	}
	vnet, err := e.d.Vnet(ctx)
	if err != nil {
		return fmt.Errorf("reading the vnet %s: %w", gateway.ManagedBridge, err)
	}
	if why := vnetWhy(vnet, f.appliance); len(why) > 0 {
		return fmt.Errorf("%w: the vnet %s is not as pco makes it: %s; %s", ErrRefused, gateway.ManagedBridge, strings.Join(why, "; "), makeVnetHint(f.appliance))
	}
	subnet := f.settings.Managed.Subnet
	if err := subnetOverlaps(subnet, f.nets, f.snap); err != nil {
		return err
	}
	mark, err := e.freeMark(f.nets)
	if err != nil {
		return err
	}
	if f.appliance {
		// pco made the vnet: its routes may be served at observed, as they
		// are while filtered does not hold, instead of being held back.
		if err := e.acknowledgeManaged(); err != nil {
			return err
		}
	}
	first := len(f.nets) == 0
	if first {
		if err := e.d.Networks.EnableNamespace(ctx); err != nil {
			return fmt.Errorf("enabling pco-netns.service for the first network: %w", err)
		}
	}
	n := store.Network{
		ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge, Prefix: subnet,
		Legs:   map[string]netip.Addr{e.d.Node: subnet.Addr().Next()},
		LegMAC: store.LegMACFor(store.NetworkManaged, mark), Mark: mark, Scope: store.ScopeNode, CreatedAt: e.d.Now(),
	}
	var said []string
	if ln.clustered {
		said = e.managedLegs(f, ln, probed.configs, &n, probed.answers)
	}
	if err := e.d.Store.SaveNetwork(n, 0); err != nil {
		if first {
			e.disableNamespace(ctx)
		}
		if errors.Is(err, store.ErrRevision) {
			return fmt.Errorf("%w: the managed network was enabled meanwhile", ErrRefused)
		}
		return fmt.Errorf("saving the network %s: %w", n.ID, err)
	}
	e.events.add(Event{At: e.d.Now(), Level: levelInfo, Kind: kindManaged, Subject: n.ID, Actor: ActorOf(ctx),
		Message: fmt.Sprintf("the managed network is enabled: %s with the subnet %s, the leg %s on node %s, mark %d", n.ID, subnet, n.Legs[e.d.Node], e.d.Node, mark)})
	e.managedEvents(ctx, said)
	e.Trigger()
	return nil
}

// managedProbe is what the agents of the other nodes answered for the
// managed network, with the subnet they were asked in, the nodes asked and
// the network of each as the API had it before the cycle lock.
type managedProbe struct {
	subnet  netip.Prefix
	asked   []string
	answers map[string]nodeAnswer
	configs map[string][]pve.NodeIface
}

// probeManaged asks the agent of every other node that has no leg of the
// managed network whether pconet is there and its first address free, and
// reads the network of those nodes. On a node alone it asks nothing.
func (e *Engine) probeManaged(ctx context.Context, ln legNodes) (managedProbe, error) {
	var out managedProbe
	if !ln.clustered {
		return out, nil
	}
	settings, err := e.d.Store.Settings()
	if err != nil {
		return out, fmt.Errorf("reading the settings: %w", err)
	}
	nets, err := e.d.Store.Networks()
	if err != nil {
		return out, fmt.Errorf("reading the networks: %w", err)
	}
	out.subnet = settings.Managed.Subnet
	have, stored := nets[gateway.ManagedBridge]
	if stored {
		out.subnet = have.Prefix
	}
	first := out.subnet.Addr().Next()
	addrs := map[string]netip.Addr{}
	for node := range e.writes.agentNodes() {
		if _, has := have.Legs[node]; node == e.d.Node || !ln.known[node] || stored && has {
			continue
		}
		addrs[node] = first
		out.asked = append(out.asked, node)
	}
	api, err := e.readNodes(ctx, out.asked, false)
	if err != nil {
		return out, err
	}
	out.configs = api.configs
	out.answers = e.probeNodes(ctx, resolve.Segment{Bridge: gateway.ManagedBridge}, addrs)
	return out, nil
}

// managedLegs gives n the leg of each other node that has none whose agent
// found pconet there, the node not on it and nobody at the first address,
// and returns what is said of each other node: the leg it got, or why it
// got none.
func (e *Engine) managedLegs(f networkFacts, ln legNodes, configs map[string][]pve.NodeIface, n *store.Network,
	answers map[string]nodeAnswer) []string {
	first := n.Prefix.Addr().Next()
	var said []string
	for _, node := range slices.Sorted(maps.Keys(ln.known)) {
		if _, has := n.Legs[node]; has || node == e.d.Node {
			continue
		}
		got, asked := answers[node]
		why := ""
		switch cfg, read := configs[node]; {
		case !asked:
			// Its agent may have become fresh since the others were asked:
			// a node is given a leg only on what its own agent answered.
			why = cmp.Or(e.unasked([]string{node})[node], "its agent was not asked")
		case !read:
			why = "its network was not read"
		default:
			why = managedLegWhy(onNode(f, node, cfg), first, got)
		}
		if why != "" {
			said = append(said, fmt.Sprintf("the managed network has no leg on node %s: %s", node, why))
			continue
		}
		n.Legs[node] = first
		said = append(said, fmt.Sprintf("the managed network has a leg on node %s now: %s", node, first))
	}
	return said
}

// managedLegWhy says why the node of g gets no leg of the managed network
// at addr by what its agent answered; empty when it gets one.
func managedLegWhy(g networkFacts, addr netip.Addr, got nodeAnswer) string {
	node := g.node
	if got.why != "" {
		return got.why
	}
	g = g.withAnswer(got.answer)
	seg := resolve.Segment{Bridge: gateway.ManagedBridge}
	if !g.isBridge(seg.Bridge) {
		return fmt.Sprintf("%s is no bridge or vnet of node %s", seg.Bridge, node)
	}
	c := g.candidate(seg)
	who, taken := g.taken(c)[addr]
	switch {
	case c.Direct:
		return fmt.Sprintf("node %s has an address of its own on %s", node, seg.Bridge)
	case taken:
		return fmt.Sprintf("%s is %s", addr, who)
	case got.answer.NotProbed != "":
		return fmt.Sprintf("%s was not probed on %s: %s", addr, g.where(seg), clip(got.answer.NotProbed))
	case len(got.answer.AnsweredBy) > 0:
		return fmt.Sprintf("%s is in use on %s: %s answered", addr, g.where(seg), strings.Join(got.answer.AnsweredBy, ", "))
	}
	return ""
}

// addManagedLegs gives the managed network stored a leg on each other node
// that has none and may.
func (e *Engine) addManagedLegs(ctx context.Context, f networkFacts, ln legNodes, have store.Network, probed managedProbe) error {
	n := have
	n.Legs = maps.Clone(have.Legs)
	said := e.managedLegs(f, ln, probed.configs, &n, probed.answers)
	if len(n.Legs) > len(have.Legs) {
		if err := e.d.Store.SaveNetwork(n, have.Rev); err != nil {
			if errors.Is(err, store.ErrRevision) {
				return fmt.Errorf("%w: the managed network changed meanwhile; enable it again", ErrRefused)
			}
			return fmt.Errorf("saving the network %s: %w", n.ID, err)
		}
		e.Trigger()
	}
	e.managedEvents(ctx, said)
	return nil
}

// managedEvents records what was said of the legs of the managed network.
func (e *Engine) managedEvents(ctx context.Context, said []string) {
	for _, msg := range said {
		level := levelInfo
		if strings.Contains(msg, " has no leg ") {
			level = levelWarn
		}
		e.events.add(Event{At: e.d.Now(), Level: level, Kind: kindManaged, Subject: gateway.ManagedBridge, Actor: ActorOf(ctx), Message: msg})
	}
}

// recordUnknown records a report of an unknown MAC and says whether it has
// an event: the first in five minutes of a MAC, while the record has room.
// The room is looked at before the MAC is taken in.
func (e *Engine) recordUnknown(key string, at time.Time) bool {
	e.macMu.Lock()
	defer e.macMu.Unlock()
	last, seen := e.unknownMACs[key]
	if seen && at.Sub(last) < unknownMACEvery {
		return false
	}
	if !seen && len(e.unknownMACs) >= maxUnknownMACs {
		// What is older than the window says nothing any more.
		for other, when := range e.unknownMACs {
			if at.Sub(when) >= unknownMACEvery {
				delete(e.unknownMACs, other)
			}
		}
		if len(e.unknownMACs) >= maxUnknownMACs {
			e.unknownDropped++
			return false
		}
	}
	e.unknownMACs[key] = at
	return true
}

// takeUnknownDropped returns how many reports of unknown MACs had no event
// since it was last asked.
func (e *Engine) takeUnknownDropped() int {
	e.macMu.Lock()
	defer e.macMu.Unlock()
	n := e.unknownDropped
	e.unknownDropped = 0
	return n
}

// acknowledgeManaged acknowledges the segment of the managed network, once.
func (e *Engine) acknowledgeManaged() error {
	acked, err := e.d.Store.Segments()
	if err != nil {
		return fmt.Errorf("reading the acknowledged segments: %w", err)
	}
	if _, ok := acked[store.SegmentID(gateway.ManagedBridge, 0)]; ok {
		return nil
	}
	if err := e.d.Store.SaveSegment(store.Segment{Bridge: gateway.ManagedBridge, AcknowledgedAt: e.d.Now(), By: "managed"}); err != nil {
		return fmt.Errorf("acknowledging the segment %s: %w", gateway.ManagedBridge, err)
	}
	return nil
}

// makeVnetHint names the command that makes the vnet for the profile.
func makeVnetHint(appliance bool) string {
	if appliance {
		return "run pco appliance managed enable on the node"
	}
	return "run pco setup --managed, or pco network managed enable as root"
}

// UnknownMAC takes what the source-MAC watch of the managed network found: a
// frame of a MAC no lease knows. Every report has the guests of the node
// read again and, in the appliance, the guests count as not isolated until a
// passing cycle: a card that keeps speaking is acted on whatever was said of
// it before. The event is once per MAC and five minutes, for at most
// maxUnknownMACs MACs at once; a report past that has no event, and the next
// cycle says how many there were.
func (e *Engine) UnknownMAC(mac net.HardwareAddr, at time.Time) {
	key := mac.String()
	e.d.Inventory.ForceRefresh(e.d.Node)
	e.noteUnknown(key, at)
	if e.recordUnknown(key, at) {
		e.events.add(Event{At: at, Level: levelWarn, Kind: kindManaged, Subject: gateway.ManagedBridge,
			Message: fmt.Sprintf("a frame from %s, a MAC no lease knows, was seen on the managed network; the guests of %s are read again", key, e.d.Node)})
	}
	if profile, _ := e.RunsAs(); profile != store.ProfileAppliance {
		return
	}
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	if f := e.state.Managed.Filtered; f != nil && f.Holds {
		copied := *f
		copied.Holds = false
		copied.Why = append(slices.Clone(f.Why), "a frame from "+key+", a MAC no lease knows, was seen on the managed network")
		e.state.Managed.Filtered = &copied
		e.redigest()
	}
}
