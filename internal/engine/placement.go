package engine

import (
	"cmp"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	kindPlacement = "placement" // a route placed in another tunnel, and the steps of its move

	// divergeFor is how long one replica must fail a route another reaches
	// before the route is pinned; promoteAfter how long every replica must
	// reach a pinned route before it is shared again; waitFor how long a
	// new route waits for every replica to report it before it is pinned.
	divergeFor   = 30 * time.Second
	promoteAfter = 10 * time.Minute
	waitFor      = 60 * time.Second

	reasonWaiting  = "waiting for %s to verify it"
	reasonHeld     = "node %s has not reported to this controller; the route is held as it was"
	reasonBlocked  = "blocked on every node: its MAC was seen where it must not be"
	whyDiverged    = "%s does not reach it while %s does"
	whyCoverage    = "not every node watches its forwarding table"
	whyNodeAddress = "it reaches an address of node %s"
	whyHosted      = "its guest runs on %s"
	whyNotEvery    = "not every replica reached it when it was placed"
	whyJoining     = "node %s joins and does not reach it"
)

// reach is how a replica fares with a target in this cycle.
type reach int

const (
	reachUnknown reach = iota // the replica has not said, or not lately
	reachYes                  // verified, at the minimum, in its egress set
	reachNo
)

// placed is where an owner's routes go in this cycle: the tunnel the
// records point at, a second tunnel a move keeps or adds the rules in, and
// why it is pinned.
type placed struct {
	kind, node string // shared, or pinned to node
	why        string
}

func (p placed) input() planner.PlacementInput {
	if p.kind == store.PlacementPinned {
		return planner.PlacementInput{Kind: planner.PlacementPinned, Node: p.node}
	}
	return planner.PlacementInput{Kind: planner.PlacementShared}
}

func (p placed) same(q placed) bool { return p.kind == q.kind && p.node == q.node }

func (p placed) String() string {
	if p.kind == store.PlacementPinned {
		return "pinned to " + p.node
	}
	return "shared"
}

// placement is what the cycle decided of the placement of the routes, by
// hostname: their placement for the planner, the second tunnel of a move,
// the target the planner serves, and the replicas the state shows.
type placement struct {
	inputs   map[string]planner.PlacementInput
	also     map[string]planner.PlacementInput
	targets  map[string]planner.ResolvedTarget
	views    map[string]*PlacementView
	replicas map[string][]ReplicaView
	decided  map[string]decidedPlace // by owner
}

// decidedPlace is where an owner's routes went: the tunnel their records
// point at and the second one a move keeps their rules in.
type decidedPlace struct {
	record placed
	also   *placed
}

// listsFor says whether the snapshot of node lists the target of a route:
// a node of its placement runs it; a replica of the shared tunnel, and a
// node joining to become one, verifies a shared route, one not placed yet,
// and a pinned one that may be shared again, which takes every replica to
// have it in its set first. Any other node does not have it: a node that
// left a route's placement, by a drain, a demotion or as long as coverage
// is lost, has it leave its egress set in the same cycle.
func (c *cycleRun) listsFor(node string, rt model.Route) bool {
	decided := c.e.place.decided
	if c.placing != nil {
		decided = c.placing.decided
	}
	replica := slices.Contains(c.replicas(), node) || c.nodes[node].Node.State == store.NodeJoining
	on := func(p placed) bool {
		if p.kind == store.PlacementPinned {
			return p.node == node
		}
		return replica
	}
	d, ok := decided[rt.Owner()]
	if c.cover.lost() && rt.Guest != nil && c.hostingNode([]model.Route{rt}) != node {
		// Without coverage no node proves another node's guest: the route
		// leaves every replica but its node's at once, while it moves.
		pinnedHere := func(p *placed) bool { return p != nil && p.kind == store.PlacementPinned && p.node == node }
		return ok && (pinnedHere(&d.record) || pinnedHere(d.also))
	}
	switch {
	case !ok:
		return replica
	case on(d.record), d.also != nil && on(*d.also):
		return true
	}
	fixed, _ := c.fixedNode([]model.Route{rt})
	return replica && fixed == "" && !c.cover.lost()
}

// replicas is the replica set, worked out once per cycle.
func (c *cycleRun) replicas() []string {
	if c.replicaCache == nil {
		c.replicaCache = c.replicaSet()
		if c.replicaCache == nil {
			c.replicaCache = []string{}
		}
	}
	return c.replicaCache
}

// replicaSet is the shared tunnel's replicas: the active nodes, those
// cluster.replicas names when it names any, at most store.MaxReplicas.
func (c *cycleRun) replicaSet() []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		if n.State != store.NodeActive || n.Drained != nil {
			continue
		}
		if list := c.settings.Cluster.Replicas; len(list) > 0 && !slices.Contains(list, name) {
			continue
		}
		out = append(out, name)
	}
	if len(out) > store.MaxReplicas && len(c.settings.Cluster.Replicas) == 0 {
		c.problem("%d nodes could run the shared tunnel, more than %d: the first %d by name do; name the replicas in cluster.replicas",
			len(out), store.MaxReplicas, store.MaxReplicas)
		out = out[:store.MaxReplicas]
	}
	return out
}

// reaches is how node fares with the route of host in this cycle. This
// node's word is its own resolution; another node's is its report, of this
// controller's epoch, of a snapshot that listed the target, of the address
// the resolving node chose.
func (c *cycleRun) reaches(node string, rt model.Route) reach {
	if node == c.e.d.Node {
		res, ok := c.results[rt.Hostname]
		if _, verified := c.verifiedTarget(rt); ok && verified && res.Target.Reachable && !c.lost[res.Target.Addr] {
			return reachYes
		}
		return reachNo
	}
	f := c.fleet.node(node)
	if !f.fresh || !servedState(f.entry.State) {
		return reachUnknown
	}
	tr, ok := f.targets[rt.Hostname]
	st := f.report.Status
	since, listed := c.e.place.listed(node, rt.Hostname, c.resolverOf(rt) == node)
	if !ok || !listed || st.EpochSeen != c.epoch() || st.SeqApplied < since {
		return reachUnknown
	}
	if !tr.OK || !tr.InEgressSet {
		return reachNo
	}
	if addr, _ := c.chosen(rt, c.resolverOf(rt), nil); addr.IsValid() && tr.Addr != addr {
		return reachNo
	}
	return reachYes
}

// place decides where the routes of every owner go, from what each replica
// reaches, the placements stored and the moves under way. A node alone has
// everything shared, as before placements, and nothing is decided.
func (c *cycleRun) place() {
	if c.fleet == nil {
		return
	}
	p := &placement{
		inputs: map[string]planner.PlacementInput{}, also: map[string]planner.PlacementInput{},
		targets: map[string]planner.ResolvedTarget{}, views: map[string]*PlacementView{},
		replicas: map[string][]ReplicaView{}, decided: map[string]decidedPlace{},
	}
	c.placing = p
	mem := &c.e.place
	for _, m := range []*map[string]time.Time{&mem.diverged, &mem.reaching, &mem.waiting} {
		if *m == nil {
			*m = map[string]time.Time{}
		}
	}
	replicas := c.replicas()
	owners := map[string][]model.Route{}
	for _, rt := range c.claims.Winners {
		owners[rt.Owner()] = append(owners[rt.Owner()], rt)
	}
	c.startMoves()
	c.findPinned()
	for _, owner := range slices.Sorted(maps.Keys(owners)) {
		routes := owners[owner]
		cur, stored := c.storedOf(routes)
		if !stored && c.servedShared(routes) {
			cur, stored = placed{kind: store.PlacementShared}, true
		}
		want, wait := c.decide(owner, routes, replicas, cur, stored)
		if wait != "" {
			for _, rt := range routes {
				p.targets[rt.Hostname] = planner.ResolvedTarget{Reason: wait, Owner: rt.Owner()}
			}
			continue
		}
		record, also := c.moving(owner, cur, stored, want)
		p.decided[owner] = decidedPlace{record: record, also: also}
		for _, rt := range routes {
			p.inputs[rt.Hostname] = record.input()
			if also != nil {
				p.also[rt.Hostname] = also.input()
			}
			p.targets[rt.Hostname] = c.servedTarget(rt, record, replicas)
			p.views[rt.Hostname] = c.placementView(record)
			p.replicas[rt.Hostname] = c.replicaViews(rt, record, replicas)
		}
	}
	mem.decided = p.decided
}

// storedOf is the placement stored for an owner's routes: of its first
// hostname that has one.
func (c *cycleRun) storedOf(routes []model.Route) (placed, bool) {
	for _, rt := range routes {
		if s, ok := c.placements[rt.Hostname]; ok {
			return placed{kind: s.Kind, node: s.Node}, true
		}
	}
	return placed{}, false
}

// servedShared says whether a route of an owner without a placement is
// served through the shared tunnel already: its record pointed there when
// a DNS run of this process last looked, or the tunnel's verified
// configuration sends a connector to its target. Such a route was served
// before a second node registered, or by a controller that stored no
// placement; it counts as placed there, so that it leaves by a move, with
// its rule in the tunnel it goes to before its record points there, and
// is not held back while a replica has not reported it.
func (c *cycleRun) servedShared(routes []model.Route) bool {
	shared := placed{kind: store.PlacementShared}
	for _, rt := range routes {
		if c.e.place.pointedAt[strings.ToLower(rt.Hostname)] == planner.TunnelName(c.install.ID) {
			return true
		}
		if dialed, ok := c.baseTarget(rt).Dialed(); ok && c.sendsTo(rt, shared, egress.Target{Addr: dialed, Port: rt.Target.Port}) {
			return true
		}
	}
	return false
}

// decide is where an owner's routes should go now; wait says why a route
// without a placement is not placed yet.
func (c *cycleRun) decide(owner string, routes []model.Route, replicas []string, cur placed, stored bool) (placed, string) {
	mem := &c.e.place
	host := c.hostingNode(routes)
	if stored && c.revoked(cur) {
		cur = placed{kind: store.PlacementShared}
	}
	if c.revoked(placed{kind: store.PlacementPinned, node: host}) {
		host = ""
	}
	if fixed, why := c.fixedNode(routes); fixed != "" {
		return placed{kind: store.PlacementPinned, node: fixed, why: why}, ""
	}
	if why := c.serviceWait(routes); why != "" && !stored && c.now.Sub(c.waitingSince(owner)) < waitFor {
		// No replica reaches it before the node that resolves it verified
		// it at its service address: it is placed by what they reach then.
		return placed{}, why
	}
	all, any, unknown, yes := c.verdicts(routes, replicas)
	hostReaches := host != "" && c.allReach(host, routes)
	cross := host != "" && slices.ContainsFunc(replicas, func(n string) bool { return n != host })
	if all {
		if _, ok := mem.reaching[owner]; !ok {
			mem.reaching[owner] = c.now
		}
	} else {
		delete(mem.reaching, owner)
	}
	if !any || all || len(unknown) > 0 {
		delete(mem.diverged, owner)
	}
	pinHost := placed{kind: store.PlacementPinned, node: host, why: fmt.Sprintf(whyHosted, host)}
	if cross && c.cover.lost() && hostReaches {
		// Without coverage no replica proves another node's guest; its
		// node serves it alone, once it reaches it.
		pinHost.why = whyCoverage
		return pinHost, ""
	}
	if stored && cur.kind == store.PlacementShared && hostReaches {
		for _, rt := range routes {
			if node, ok := c.joining[rt.Hostname]; ok {
				pinHost.why = fmt.Sprintf(whyJoining, node)
				return pinHost, ""
			}
		}
	}
	switch {
	case !stored:
		switch {
		case all:
			delete(mem.waiting, owner)
			return placed{kind: store.PlacementShared}, ""
		case len(unknown) > 0 && c.now.Sub(c.waitingSince(owner)) < waitFor:
			return placed{}, fmt.Sprintf(reasonWaiting, strings.Join(unknown, ", "))
		case hostReaches:
			delete(mem.waiting, owner)
			pinHost.why = whyNotEvery
			return pinHost, ""
		case host == "" && len(yes) > 0:
			delete(mem.waiting, owner)
			return placed{kind: store.PlacementPinned, node: yes[0], why: whyNotEvery}, ""
		}
		delete(mem.waiting, owner)
		return placed{kind: store.PlacementShared}, ""
	case cur.kind == store.PlacementShared:
		if all || len(unknown) > 0 || !any {
			return cur, ""
		}
		since, ok := mem.diverged[owner]
		if !ok {
			since, mem.diverged[owner] = c.now, c.now
		}
		if c.now.Sub(since) < divergeFor {
			return cur, ""
		}
		failing := slices.DeleteFunc(slices.Clone(replicas), func(n string) bool { return slices.Contains(yes, n) })
		why := fmt.Sprintf(whyDiverged, strings.Join(failing, ", "), strings.Join(yes, ", "))
		switch {
		case hostReaches:
			pinHost.why = why
			return pinHost, ""
		case host == "" && len(yes) > 0:
			return placed{kind: store.PlacementPinned, node: yes[0], why: why}, ""
		}
		return cur, ""
	}
	// Pinned: to the guest's node once it runs elsewhere, back to the
	// shared tunnel once every replica reached it for promoteAfter, and
	// coverage was not lost meanwhile.
	if host != "" && host != cur.node && hostReaches {
		return pinHost, ""
	}
	since, ok := mem.reaching[owner]
	if all && ok && c.now.Sub(since) >= promoteAfter && (!cross || c.cover.promotable) {
		return placed{kind: store.PlacementShared}, ""
	}
	if cur.why == "" {
		cur.why = c.storedWhy(routes)
	}
	return cur, ""
}

// waitingSince is when a route without a placement first waited.
func (c *cycleRun) waitingSince(owner string) time.Time {
	mem := &c.e.place
	since, ok := mem.waiting[owner]
	if !ok {
		since, mem.waiting[owner] = c.now, c.now
	}
	return since
}

// storedWhy is why a stored pinned placement is pinned, as far as the
// cycle tells.
func (c *cycleRun) storedWhy(routes []model.Route) string {
	if host := c.hostingNode(routes); host != "" {
		return fmt.Sprintf(whyHosted, host)
	}
	return whyNotEvery
}

// verdicts are how the replicas fare with every route of an owner: all
// reach all, any reaches all, the ones that have not said, and the ones
// that reach all.
func (c *cycleRun) verdicts(routes []model.Route, replicas []string) (all, any bool, unknown, yes []string) {
	all = len(replicas) > 0
	for _, n := range replicas {
		switch r := c.reachAll(n, routes); r {
		case reachYes:
			any = true
			yes = append(yes, n)
		case reachUnknown:
			all = false
			unknown = append(unknown, n)
		default:
			all = false
		}
	}
	return all, any, unknown, yes
}

// reachAll is how a node fares with every route of an owner: unknown when
// it has not said of one, no when it fails one.
func (c *cycleRun) reachAll(node string, routes []model.Route) reach {
	out := reachYes
	for _, rt := range routes {
		switch c.reaches(node, rt) {
		case reachNo:
			return reachNo
		case reachUnknown:
			out = reachUnknown
		}
	}
	return out
}

func (c *cycleRun) allReach(node string, routes []model.Route) bool {
	return c.reachAll(node, routes) == reachYes
}

// hostingNode is the node the guest of an owner's routes runs on; empty for
// a manual route.
func (c *cycleRun) hostingNode(routes []model.Route) string {
	for _, rt := range routes {
		if rt.Guest == nil {
			return ""
		}
		if g, ok := c.snap.Guest(*rt.Guest); ok {
			return g.Node
		}
	}
	return ""
}

// fixedNode is the node an owner's routes are pinned to whatever the
// replicas reach: a target behind a leg of this node, which no other node
// reaches, and a manual route to an address of a node.
func (c *cycleRun) fixedNode(routes []model.Route) (string, string) {
	for _, rt := range routes {
		if node, why, ok := c.legFixed(rt); ok {
			return node, why
		}
		if rt.Source != model.SourceManual || !rt.Options.AllowNode || !rt.Target.Addr.IsValid() {
			continue
		}
		for _, n := range c.snap.Nodes {
			addrs := []netip.Addr{n.Addr}
			for _, ifc := range n.Ifaces {
				for _, p := range ifc.Addrs {
					addrs = append(addrs, p.Addr())
				}
			}
			if slices.Contains(addrs, rt.Target.Addr) {
				return n.Name, fmt.Sprintf(whyNodeAddress, n.Name)
			}
		}
	}
	return "", ""
}

// servedTarget is the target the planner serves for a route placed at
// record: what the resolving node found, reachable only while every
// replica of the tunnel reaches it. A target the tunnel's verified
// configuration does not send a connector to yet waits for every replica
// to have it verified in its egress set, so that no rule names an address a
// replica would refuse. A route whose node has not reported keeps what is
// stored of it; a blocked address is withdrawn.
func (c *cycleRun) servedTarget(rt model.Route, record placed, replicas []string) planner.ResolvedTarget {
	nodes := replicas
	if record.kind == store.PlacementPinned {
		nodes = []string{record.node}
	}
	t := c.baseTarget(rt)
	var unknown, failing []string
	for _, n := range nodes {
		switch c.reaches(n, rt) {
		case reachUnknown:
			unknown = append(unknown, n)
		case reachNo:
			failing = append(failing, n)
		}
	}
	if len(nodes) == 0 {
		failing = append(failing, "no replica")
	}
	dialed, ok := t.Dialed()
	published := ok && c.sendsTo(rt, record, egress.Target{Addr: dialed, Port: rt.Target.Port})
	switch {
	case !t.Addr.IsValid() && !t.Withdrawn && !t.Rejected:
		// No node resolves it now, which withdraws nothing: only an
		// identity that failed, a rejection or a block does. A route
		// published before keeps its rule, unreachable, and no egress set
		// holds its target meanwhile.
		if s, ok := c.placements[rt.Hostname]; ok && s.Addr.IsValid() && c.e.place.blocks[s.Addr] == nil {
			why := "no node resolves it now; the route is held as it was"
			if len(unknown) > 0 {
				why = fmt.Sprintf(reasonHeld, unknown[0])
			}
			return planner.ResolvedTarget{Addr: s.Addr, Owner: rt.Owner(), Reason: why}
		}
	case !t.Addr.IsValid() || t.Withdrawn || t.Rejected:
	case c.e.place.blocks[t.Addr] != nil:
		t.Withdrawn, t.Reachable, t.Reason = true, false, reasonBlocked
	case !published && (len(unknown) > 0 || len(failing) > 0):
		waiting := slices.Concat(unknown, failing)
		return planner.ResolvedTarget{Owner: rt.Owner(), Reason: fmt.Sprintf(reasonWaiting, strings.Join(waiting, ", ")), Level: t.Level}
	case len(failing) > 0:
		t.Reachable = false
		t.Reason = cmp.Or(t.Reason, fmt.Sprintf("not reached on %s", strings.Join(failing, ", ")))
	}
	return t
}

// baseTarget is what the resolving node of a route found of its target.
func (c *cycleRun) baseTarget(rt model.Route) planner.ResolvedTarget {
	resolver := c.resolverOf(rt)
	if resolver == c.e.d.Node {
		res := c.results[rt.Hostname]
		t := res.Target
		t.Level = string(res.Level)
		return t
	}
	tr, ok := c.reportOn(resolver, rt, true)
	if !ok {
		return planner.ResolvedTarget{Owner: rt.Owner(), Reason: "not resolved on " + resolver + " yet"}
	}
	t := planner.ResolvedTarget{Addr: tr.Addr, Reachable: tr.OK, Reason: tr.Reason, Owner: rt.Owner(), Level: tr.Level}
	if res, err := agent.DecodeResult(tr.Result); err == nil {
		t.Withdrawn, t.Rejected = res.Target.Withdrawn, res.Target.Rejected
	}
	if rl, ok := c.remoteLegs[rt.Hostname]; ok {
		// A target behind a leg is dialed at its service address only.
		t.Leg, t.Via = true, rl.service
		if !rl.service.IsValid() {
			t.Reachable, t.Reason = false, fmt.Sprintf(reasonNoService, rl.why)
		}
	}
	return t
}

// sendsTo says whether the configuration last verified of the tunnel of a
// placement sends a connector to target: the rule is published, and a
// replica that lost it holds the route back only as unreachable.
func (c *cycleRun) sendsTo(rt model.Route, record placed, target egress.Target) bool {
	zone, ok := c.zoneOf(rt.Hostname)
	if !ok {
		return false
	}
	name := planner.TunnelName(c.install.ID)
	if record.kind == store.PlacementPinned {
		var err error
		if name, err = planner.PinnedTunnelName(c.install.ID, record.node); err != nil {
			return false
		}
	}
	return slices.Contains(c.e.verified[tunnelRef{zone.AccountID, name}], target)
}

// zoneOf is the planned zone of a hostname.
func (c *cycleRun) zoneOf(host string) (planner.Zone, bool) {
	var best planner.Zone
	for _, z := range c.zones.planned {
		if (host == z.Name || strings.HasSuffix(host, "."+z.Name)) && len(z.Name) > len(best.Name) {
			best = z
		}
	}
	return best, best.Name != ""
}

// epoch is the epoch this controller writes under.
func (c *cycleRun) epoch() agentproto.Epoch {
	return agentproto.Epoch{InstallID: c.install.ID, Generation: c.e.us.Generation, Nonce: c.e.us.Nonce}
}

// placementView is what the state shows of a route's placement.
func (c *cycleRun) placementView(record placed) *PlacementView {
	name := planner.TunnelName(c.install.ID)
	if record.kind == store.PlacementPinned {
		name, _ = planner.PinnedTunnelName(c.install.ID, record.node)
	}
	v := &PlacementView{Tunnel: name, Kind: TunnelShared, Why: record.why}
	if record.kind == store.PlacementPinned {
		v.Kind, v.Node = TunnelPinned, record.node
	}
	return v
}

// replicaViews are how the target of a route fares on each replica of its
// tunnel.
func (c *cycleRun) replicaViews(rt model.Route, record placed, replicas []string) []ReplicaView {
	nodes := replicas
	if record.kind == store.PlacementPinned {
		nodes = []string{record.node}
	}
	out := make([]ReplicaView, 0, len(nodes))
	for _, n := range nodes {
		v := ReplicaView{Node: n}
		if n == c.e.d.Node {
			res := c.results[rt.Hostname]
			_, inSet := c.verifiedTarget(rt)
			v.Level, v.OK, v.Reason, v.InSet = string(res.Level), res.Target.Reachable && inSet, res.Target.Reason, inSet
		} else if tr, ok := c.fleet.node(n).targets[rt.Hostname]; ok {
			v.Level, v.Attested, v.OK, v.Reason, v.InSet = tr.Level, tr.Attested, tr.OK, tr.Reason, tr.InEgressSet
		} else {
			v.Reason = "no report"
		}
		out = append(out, v)
	}
	return out
}

// savePlacements stores the placements of the routes, once the DNS run of
// the cycle looked at the records as the writer: a new controller keeps
// them, and moves nothing at a takeover.
func (c *cycleRun) savePlacements() {
	p := c.placing
	if p == nil || !c.checked {
		return
	}
	next := map[string]store.Placement{}
	for _, rt := range c.claims.Winners {
		in, ok := p.inputs[rt.Hostname]
		if !ok {
			if s, kept := c.placements[rt.Hostname]; kept {
				next[rt.Hostname] = s
			}
			continue
		}
		pl := store.Placement{Hostname: rt.Hostname, Kind: store.PlacementShared, Tunnel: planner.TunnelName(c.install.ID), Since: c.now}
		if in.Kind == planner.PlacementPinned {
			pl.Kind, pl.Node = store.PlacementPinned, in.Node
			pl.Tunnel, _ = planner.PinnedTunnelName(c.install.ID, in.Node)
		}
		t := p.targets[rt.Hostname]
		pl.Addr, pl.ResolvingNode = t.Addr, c.resolverOf(rt)
		if t.Leg {
			pl.Addr, pl.Service = t.Addr, t.Via
		}
		if s, ok := c.placements[rt.Hostname]; ok {
			if !pl.Addr.IsValid() {
				pl.Addr, pl.Service = s.Addr, s.Service
			}
			if s.Kind == pl.Kind && s.Node == pl.Node {
				pl.Since = s.Since
			}
		}
		if !pl.Addr.IsValid() && pl.ResolvingNode == "" {
			continue
		}
		next[rt.Hostname] = pl
	}
	if maps.EqualFunc(next, c.placements, samePlacement) {
		return
	}
	if err := c.e.d.Store.SavePlacements(next); err != nil {
		c.problem("saving the placements of the routes: %v", err)
		return
	}
	for host, pl := range next {
		if old, ok := c.placements[host]; ok && (old.Kind != pl.Kind || old.Node != pl.Node) {
			c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: host, Route: host,
				Message: fmt.Sprintf("%s is placed %s, was %s", host, placed{kind: pl.Kind, node: pl.Node}, placed{kind: old.Kind, node: old.Node})})
		}
	}
	c.placements = next
}

// readPlacements reads the placements stored, in a cluster; false when they
// cannot be read, which holds the cycle: nothing is placed, served or
// handed out on what is not known.
func (c *cycleRun) readPlacements() bool {
	if !c.clustered() {
		return true
	}
	placements, err := c.e.d.Store.Placements()
	if err != nil {
		c.hold(c.storeProblem("reading the placements of the routes", err))
		return false
	}
	c.placements = placements
	return true
}

func samePlacement(a, b store.Placement) bool {
	return a.Hostname == b.Hostname && a.Tunnel == b.Tunnel && a.Kind == b.Kind && a.Node == b.Node && a.Addr == b.Addr &&
		a.Service == b.Service && a.ResolvingNode == b.ResolvingNode && a.Since.Equal(b.Since)
}
