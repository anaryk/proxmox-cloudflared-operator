package engine

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	kindAttestation = "attestation" // what agents said of the ports of guests, and what of it was refused
	kindBlock       = "block"       // an address blocked on every node, and its end

	// maxAttestations and maxObservations are the most of each the
	// controller takes of one node's status; a status with more is the
	// node's word that something is wrong, and the rest is not read.
	maxAttestations = 1000
	maxObservations = 500
	// blockQuiet is how long no agent may report what blocked an address
	// before the block may end.
	blockQuiet = 10 * time.Minute
	// migrateGrace is how long a moved MAC waits for a sign that its guest
	// migrates before the address is blocked on every node.
	migrateGrace = 10 * time.Second

	problemConflictBlocked = "%s is blocked on every node: its MAC %s was seen on %s of %s"
)

// cardRef names one card of a guest.
type cardRef struct {
	guest model.GuestRef
	nic   int
}

// card is a card of a guest a route of this cycle publishes.
type card struct {
	ref  cardRef
	nic  model.NIC
	node string // the node the inventory has the guest on
}

// agentFacts is what the controller goes by of one node's agent in a cycle:
// its last report, checked again here whatever the agent endpoint checked.
type agentFacts struct {
	entry  store.NodeEntry
	report NodeReport
	// heard says that the node reported, fresh that it reported within
	// silentAfter, and host that it is a host agent by its entry and by the
	// kind the endpoint authenticated: only a host agent's word about the
	// ports of a bridge counts.
	heard, fresh, host bool
	targets            map[string]agentproto.TargetReport
}

// fleetView is what the agents reported, as one cycle takes it: the facts of
// every node of the registry that is neither removed nor contested, and the
// attestations that stand, by card.
type fleetView struct {
	nodes    map[string]*agentFacts
	attested map[cardRef][]agentproto.Attestation
	cards    map[string]card // by MAC, lower case with colons
}

// node returns the facts of a node; a node the view has none of is unheard.
func (f *fleetView) node(name string) *agentFacts {
	if f == nil || f.nodes[name] == nil {
		return &agentFacts{}
	}
	return f.nodes[name]
}

// conflictBlock is an address blocked on every node because an agent saw a
// MAC bound to its guest where it must not be: on the port of another guest,
// or gone from the guest's port while the guest did not migrate.
type conflictBlock struct {
	addr       netip.Addr
	guest      model.GuestRef
	mac        string
	node, port string
	why        string // conflict or moved
	since      time.Time
	lastReport time.Time
}

// observeFleet takes the reports of the agents for this cycle, when the
// registry names more than this node: each is checked again, the
// attestations and observations it may give are kept, and the blocks follow
// what they say. A report changes what the node that made it reaches;
// another node's set is changed only by a host agent's attestation, which
// lets it prove the port of a guest, and by a conflict or a move, which only
// ever blocks.
func (c *cycleRun) observeFleet() bool {
	if !c.clustered() {
		return true
	}
	placements, err := c.e.d.Store.Placements()
	if err != nil {
		c.hold(c.storeProblem("reading the placements of the routes", err))
		return false
	}
	c.placements = placements
	c.fleet = &fleetView{nodes: map[string]*agentFacts{}, attested: map[cardRef][]agentproto.Attestation{}, cards: c.publishedCards()}
	reports := c.reportsNow()
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		if n.State == store.NodeRemoved || n.State == store.NodeContested {
			continue
		}
		facts := &agentFacts{entry: n, targets: map[string]agentproto.TargetReport{}}
		c.fleet.nodes[name] = facts
		rep, heard := reports[name]
		if !heard || rep.Status.Node != name {
			// A status that names another node is not this node's word.
			continue
		}
		facts.heard, facts.report = true, rep
		age := c.now.Sub(rep.ReceivedAt)
		facts.fresh = age >= -time.Second && age < silentAfter
		facts.host = entryKind(n) == string(pki.KindHost) && rep.Status.Kind == pki.KindHost
		for _, t := range rep.Status.Targets {
			if _, dup := facts.targets[t.Key]; !dup {
				facts.targets[t.Key] = t
			}
		}
		c.takeAttestations(name, facts)
	}
	c.observeCoverage()
	c.followBlocks()
	c.hearOwnController()
	return true
}

// hearOwnController hands the agent of this node what a heartbeat of the
// controller tells the agents of the other nodes: how fresh each node's
// status is, by which it judges an attestation it verifies against, and
// that more than one node runs. Its own controller is in this process; it
// does not call the lease valid, so the agent never takes itself for a live
// controller of another node.
func (c *cycleRun) hearOwnController() {
	if c.node == nil || !c.writerRead {
		return
	}
	hb := agentproto.Heartbeat{
		Epoch: agentproto.Epoch{InstallID: c.install.ID, Generation: c.e.us.Generation, Nonce: c.e.us.Nonce},
		Nodes: map[string]agentproto.NodeFreshness{}, ReadyReplicas: map[string]int{},
	}
	for _, name := range slices.Sorted(maps.Keys(c.fleet.nodes)) {
		f := c.fleet.nodes[name]
		if !f.heard {
			continue
		}
		hb.Nodes[name] = agentproto.NodeFreshness{
			StatusAge:    max(c.now.Sub(f.report.ReceivedAt), 0),
			WatchRunning: f.host && !f.report.Status.WatchRunningSince.IsZero(),
		}
	}
	c.node.Heartbeat(hb)
}

// publishedCards are the cards of the guests of the winners, by MAC: only
// what a route publishes is attested, blocked or watched for.
func (c *cycleRun) publishedCards() map[string]card {
	out := map[string]card{}
	for _, rt := range c.claims.Winners {
		if rt.Guest == nil {
			continue
		}
		g, ok := c.snap.Guest(*rt.Guest)
		if !ok {
			continue
		}
		for _, nic := range g.NICs {
			mac, err := model.NormalizeMAC(nic.MAC)
			if err != nil {
				continue
			}
			out[mac] = card{ref: cardRef{g.Ref, nic.Index}, nic: nic, node: g.Node}
		}
	}
	return out
}

// takeAttestations keeps the attestations of one node's report that stand:
// those of a host agent whose watch runs, each by the node itself, of a
// published card on that card's own bridge and VLAN, of a time not before
// its watch began and not after its status was made. An appliance agent
// reads no bridge: what it says of one is dropped, and said once.
func (c *cycleRun) takeAttestations(name string, facts *agentFacts) {
	st := facts.report.Status
	if !facts.host {
		if len(st.Attestations) > 0 || len(st.Observations) > 0 {
			c.refuseFacts(name, fmt.Sprintf("the agent of node %s is not a host agent; what it says of the ports of guests is not taken", name))
		}
		return
	}
	if !facts.fresh || st.WatchRunningSince.IsZero() {
		return
	}
	for i, a := range st.Attestations {
		if i == maxAttestations {
			c.refuseFacts(name, fmt.Sprintf("the agent of node %s attests more than %d cards; the rest is not read", name, maxAttestations))
			break
		}
		if c.attests(name, st, a) {
			ref := cardRef{a.Guest, a.NIC}
			c.fleet.attested[ref] = append(c.fleet.attested[ref], a)
		}
	}
}

// attests says whether an attestation of node's status stands.
func (c *cycleRun) attests(node string, st agentproto.Status, a agentproto.Attestation) bool {
	mac, err := model.NormalizeMAC(a.MAC)
	if err != nil {
		return false
	}
	cd, ok := c.fleet.cards[mac]
	switch {
	case a.Node != node, !ok, cd.ref != (cardRef{a.Guest, a.NIC}):
		return false
	case a.Since.IsZero(), a.Since.Before(st.WatchRunningSince), a.Since.After(st.Clock):
		return false
	}
	return attestsCard(a, cd.nic)
}

// attestsCard says whether an attestation names the card's own bridge and VLAN,
// or the bridge Proxmox makes for a VLAN on a bridge that filters none.
func attestsCard(a agentproto.Attestation, nic model.NIC) bool {
	switch {
	case a.Bridge == nic.Bridge && a.VLAN == nic.VLAN:
		return true
	case nic.VLAN != 0 && a.VLAN == 0 && a.Bridge == fmt.Sprintf("%sv%d", nic.Bridge, nic.VLAN):
		return true
	}
	return false
}

// attestation is the attestation the hosting node of a guest gives for the
// card with mac: none when it gives none that stands.
func (c *cycleRun) attestation(host, mac string) *agentproto.Attestation {
	if c.fleet == nil {
		return nil
	}
	mac, err := model.NormalizeMAC(mac)
	if err != nil {
		return nil
	}
	cd, ok := c.fleet.cards[mac]
	if !ok {
		return nil
	}
	for _, a := range c.fleet.attested[cd.ref] {
		if a.Node == host {
			at := a
			return &at
		}
	}
	return nil
}

// refuseFacts says once per node and reason what of an agent's report the
// controller did not take.
func (c *cycleRun) refuseFacts(node, msg string) {
	mem := &c.e.place
	if mem.refused == nil {
		mem.refused = map[string]bool{}
	}
	if mem.refused[node+"\x00"+msg] {
		return
	}
	mem.refused[node+"\x00"+msg] = true
	c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindAttestation, Subject: node, Node: node, Message: msg, Security: true})
}

// followBlocks keeps the blocks of addresses whose MAC an agent saw where it
// must not be: a conflict blocks at once; a move blocks once the guest did
// not show it migrates within migrateGrace. A block ends once no agent
// reported it for blockQuiet and the hosting node attests the card again.
func (c *cycleRun) followBlocks() {
	mem := &c.e.place
	if mem.blocks == nil {
		mem.blocks = map[netip.Addr]*conflictBlock{}
	}
	if mem.moved == nil {
		mem.moved = map[string]time.Time{}
	}
	seenMoves := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(c.fleet.nodes)) {
		facts := c.fleet.nodes[name]
		if !facts.fresh || !facts.host || facts.report.Status.WatchRunningSince.IsZero() {
			continue
		}
		for i, o := range facts.report.Status.Observations {
			if i == maxObservations {
				c.refuseFacts(name, fmt.Sprintf("the agent of node %s reports more than %d observations; the rest is not read", name, maxObservations))
				break
			}
			mac, err := model.NormalizeMAC(o.MAC)
			cd, published := c.fleet.cards[mac]
			if err != nil || !published {
				continue
			}
			switch o.Kind {
			case agentproto.ObservationConflict:
				c.block(cd, mac, name, o.Port, agentproto.BlockConflict)
			case agentproto.ObservationMoved:
				key := fmt.Sprintf("%s %s %s %d %s", name, mac, o.Bridge, o.VLAN, o.At.UTC().Format(time.RFC3339Nano))
				seenMoves[key] = true
				first, ok := mem.moved[key]
				if !ok {
					first, mem.moved[key] = c.now, c.now
				}
				if c.now.Sub(first) >= migrateGrace && !c.migrating(cd, name) {
					c.block(cd, mac, name, o.Port, "moved")
				}
			}
		}
	}
	maps.DeleteFunc(mem.moved, func(key string, _ time.Time) bool { return !seenMoves[key] })
	for addr, b := range mem.blocks {
		if c.now.Sub(b.lastReport) < blockQuiet || c.attestation(c.hostOf(b.guest), b.mac) == nil {
			continue
		}
		delete(mem.blocks, addr)
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindBlock, Subject: addr.String(), Guest: b.guest.String(),
			Message: fmt.Sprintf("%s is no longer blocked: no agent reported its MAC %s elsewhere for %s, and node %s attests it again",
				addr, b.mac, blockQuiet, c.hostOf(b.guest))})
	}
}

// migrating says whether the guest of a card shows that it migrates: the
// inventory has it locked for a migration, or a node other than the one
// that saw it leave attests its own port.
func (c *cycleRun) migrating(cd card, movedOn string) bool {
	if g, ok := c.snap.Guest(cd.ref.guest); ok && g.Lock == "migrate" {
		return true
	}
	return slices.ContainsFunc(c.fleet.attested[cd.ref], func(a agentproto.Attestation) bool { return a.Node != movedOn })
}

// hostOf is the node the inventory has a guest on.
func (c *cycleRun) hostOf(ref model.GuestRef) string {
	g, _ := c.snap.Guest(ref)
	return g.Node
}

// block blocks the addresses served for the guest of a card on every node,
// or notes that what blocks them was reported again.
func (c *cycleRun) block(cd card, mac, node, port, why string) {
	mem := &c.e.place
	for _, addr := range c.addrsOf(cd.ref.guest) {
		if b, ok := mem.blocks[addr]; ok {
			b.lastReport = c.now
			continue
		}
		mem.blocks[addr] = &conflictBlock{addr: addr, guest: cd.ref.guest, mac: mac, node: node, port: port, why: why, since: c.now, lastReport: c.now}
		c.events = append(c.events, Event{At: c.now, Level: levelError, Kind: kindBlock, Subject: addr.String(), Guest: cd.ref.guest.String(), Node: node,
			Security: true, Message: fmt.Sprintf(problemConflictBlocked, addr, mac, port, node)})
	}
}

// addrsOf are the addresses served for a guest: those its bindings and its
// routes' placements hold.
func (c *cycleRun) addrsOf(guest model.GuestRef) []netip.Addr {
	var out []netip.Addr
	for _, rt := range c.claims.Winners {
		if rt.Guest == nil || *rt.Guest != guest {
			continue
		}
		if b, ok := c.bindings[rt.Hostname]; ok && b.Addr.IsValid() {
			out = append(out, b.Addr)
		}
		if p, ok := c.placements[rt.Hostname]; ok && p.Addr.IsValid() {
			out = append(out, p.Addr)
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}

// snapshotBlocks are the blocks every node's snapshot carries.
func (c *cycleRun) snapshotBlocks() []agentproto.Block {
	out := []agentproto.Block{}
	for _, addr := range slices.SortedFunc(maps.Keys(c.e.place.blocks), netip.Addr.Compare) {
		out = append(out, agentproto.Block{Addr: addr, Reason: agentproto.BlockConflict})
	}
	return out
}

// blockViews are the blocks as the state shows them, with a problem line
// for each.
func (c *cycleRun) blockViews() []BlockView {
	var out []BlockView
	for _, addr := range slices.SortedFunc(maps.Keys(c.e.place.blocks), netip.Addr.Compare) {
		b := c.e.place.blocks[addr]
		c.problem(problemConflictBlocked, addr, b.mac, b.port, b.node)
		out = append(out, BlockView{Address: addr, Guest: b.guest.String(), MAC: b.mac, Node: b.node, Port: b.port, Since: b.since, LastReport: b.lastReport})
	}
	return out
}
