package engine

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/registry"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// problemTakeoverHold names the nodes a new controller waits for before
	// it writes, at most takeoverHold after its tenure began.
	problemTakeoverHold = "waiting for the agents of %s to report to this controller; nothing is written at Cloudflare for up to %s"
	// problemAddOnly is the first cycle that writes after a takeover hold
	// that ended without a report of the nodes it names.
	problemAddOnly = "the agents of %s did not report within the takeover hold; nothing is withdrawn at Cloudflare in this cycle"
	// problemNoStatus counts the nodes the cluster lists online whose agent
	// this controller never heard: the agent channel does not reach it.
	problemNoStatus = ".members lists %d nodes online that have sent no status (%s): check cluster.agentNetwork and the 8644 rule"
	// problemNoFallback names registered nodes whose Proxmox API the
	// controller cannot fall back on, as the members file lists no address
	// for them.
	problemNoFallback = "the Proxmox API of %s is not fallen back on: the members file lists no address for it"

	// takeoverHold is how long a new controller waits for the agents of
	// the nodes that are neither down nor removed before it writes; a node
	// that has not reported by then is unknown.
	takeoverHold = 60 * time.Second
	// silentAfter is how long a node's agent may send no status before the
	// node is unknown.
	silentAfter = 30 * time.Second

	kindNode = "node" // a node of the registry changed state, got its index, was drained
)

// registryMemory is what the registry step keeps between cycles: whether
// Proxmox last had each node in maintenance, as far as it could tell, so
// that a read that cannot tell keeps what was known.
type registryMemory struct {
	maintenance map[string]bool
	// started is when the step first ran in this process, which stands for
	// the beginning of the tenure where there is no lease.
	started time.Time
}

// since is when this controller began: its tenure of the lease, or, without
// a lease, its first registry step.
func (c *cycleRun) since() time.Time {
	if !c.e.tenureAt.IsZero() {
		return c.e.tenureAt
	}
	if c.e.registry.started.IsZero() {
		c.e.registry.started = c.now
	}
	return c.e.registry.started
}

// readRegistry reads the node registry. This node must be in it; the
// others are the cluster's.
func (c *cycleRun) readRegistry() bool {
	nodes, err := c.e.d.Store.NodesRev()
	if err != nil {
		c.hold(c.storeProblem("reading the node registry", err))
		return false
	}
	c.nodes = make(map[string]store.NodeRev, len(nodes))
	for _, n := range nodes {
		c.nodes[n.Node.Name] = n
	}
	if _, ok := c.nodes[c.e.d.Node]; !ok {
		c.hold(c.problem(problemNotRegistered, c.e.d.Node))
		return false
	}
	return true
}

// cluster says whether the registry names a node besides this one that
// was not removed.
func (c *cycleRun) clustered() bool {
	for name, n := range c.nodes {
		if name != c.e.d.Node && n.Node.State != store.NodeRemoved {
			return true
		}
	}
	return false
}

// registryStep moves the nodes of the registry through their states by what
// their agents reported and what the members file says, admits a joining
// node that reports, gives it its index, and follows Proxmox's maintenance
// mode. It runs once the lease let the cycle write, and writes only an entry
// that changed.
func (c *cycleRun) registryStep() {
	if c.nodes == nil {
		return
	}
	reports := c.reportsNow()
	listed := c.listedMembers()
	maint := c.maintenance()
	entries := make([]store.NodeEntry, 0, len(c.nodes))
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		entries = append(entries, c.nodes[name].Node)
	}
	for i := range entries {
		n := &entries[i]
		before := *n
		rep, heard := reports[n.Name]
		member, isMember := listed[n.Name]
		c.observeNode(n, rep, heard, member, isMember, maint, entries)
		if sameEntry(before, *n) {
			continue
		}
		if !c.e.leaseHolds() {
			// The lease went while the step ran: nothing more is written.
			*n = before
			return
		}
		rev, err := c.e.d.Store.SaveNodeIf(*n, c.nodes[n.Name].Rev)
		switch {
		case errors.Is(err, store.ErrRevision):
			// Setup or an admin wrote the entry meanwhile: the next cycle
			// reads it again.
			*n = before
			continue
		case err != nil:
			c.problem("saving the registry entry of node %s: %v", n.Name, err)
			*n = before
			continue
		}
		c.nodes[n.Name] = store.NodeRev{Node: *n, Rev: rev}
		c.nodeEvents(before, *n)
	}
	c.noStatus(reports, listed)
	c.noFallback()
	c.agentRule(reports, listed)
}

// observeNode applies what this cycle saw of node n to its entry: Proxmox's
// view first, then its agent's, then admission and the drain. A transition
// the registry has none for, as for a node an admin removed, leaves the
// entry as it is.
func (c *cycleRun) observeNode(n *store.NodeEntry, rep NodeReport, heard bool, member members.Member, isMember bool,
	maint map[string]bool, entries []store.NodeEntry) {
	self := n.Name == c.e.d.Node
	c.followMaintenance(n, maint)
	if n.State == "" {
		// An entry made before states, which the first start of this
		// release numbers and activates.
		return
	}
	var evs []registry.Event
	switch {
	case isMember && member.Online:
		evs = append(evs, registry.EventOnline)
	case isMember:
		evs = append(evs, registry.EventOffline)
	}
	fresh := heard && c.now.Sub(rep.ReceivedAt) < silentAfter
	switch {
	case self:
		evs = append(evs, registry.EventHeard)
	case fresh && returned(*n, rep):
		evs = append(evs, registry.EventReturned, registry.EventHeard)
	case fresh:
		evs = append(evs, registry.EventHeard)
	case heard || c.now.Sub(c.since()) >= takeoverHold:
		evs = append(evs, registry.EventSilent)
	}
	for _, ev := range evs {
		c.transition(n, ev)
	}
	// A node is admitted in a cycle after the one it began joining in, so
	// that one that returned shows as joining for a cycle at least.
	joined := n.State == store.NodeJoining && n.StateSince.Before(c.now)
	if joined && (self || fresh && !rep.ReceivedAt.Before(c.since())) && c.admits(n, rep, self) {
		if c.giveIndex(n, entries) {
			c.transition(n, registry.EventAdmitted)
			delete(c.e.nodes.admitted, n.Name)
		}
	}
	if (n.State == store.NodeActive || n.State == store.NodeDraining) && n.Index == nil {
		c.giveIndex(n, entries)
	}
	switch {
	case n.State == store.NodeActive && n.Drained != nil:
		c.transition(n, registry.EventDrain)
	case n.State == store.NodeDraining && n.Drained == nil:
		c.transition(n, registry.EventUndrain)
	}
}

// returned says that the agent of an active or draining node started since
// the node took that state: the node rebooted, or pco was installed again,
// and passes through joining.
func returned(n store.NodeEntry, rep NodeReport) bool {
	if n.State != store.NodeActive && n.State != store.NodeDraining {
		return false
	}
	return rep.Status.AgentStartedAt.After(n.StateSince)
}

// admits is the routing admission of a joining node that reports: its
// agent speaks a version of the protocol this build does, and no route has
// to move to a pinned tunnel first.
func (c *cycleRun) admits(n *store.NodeEntry, rep NodeReport, self bool) bool {
	if !self {
		ours := agentproto.Range{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax}
		if _, ok := agentproto.Negotiate(rep.Status.Protocol, ours); !ok {
			return false
		}
	}
	failing, missing := c.joinFails(n.Name, rep, self)
	if c.joinReaches == nil {
		c.joinReaches = map[string]bool{}
	}
	c.joinReaches[n.Name] = !self && len(failing) == 0 && missing == 0
	if missing > 0 {
		c.problem("node %s stays joining: it has not reported %d of the shared routes yet", n.Name, missing)
		return false
	}
	maxMoves := c.settings.Cluster.MaxMoves()
	if c.e.adminAdmitted(n.Name) {
		// The admin let every route the node fails move.
		maxMoves = max(maxMoves, len(failing))
	}
	moves, wait, why := registry.Admit(n.Name, failing, failing, maxMoves)
	if len(moves) > 0 {
		if c.joining == nil {
			c.joining = map[string]string{}
		}
		for _, host := range moves {
			c.joining[host] = n.Name
		}
	}
	if wait {
		c.problem("node %s stays joining: %s", n.Name, why)
		return false
	}
	return true
}

// joinFails are the hostnames of the shared routes a joining node does not
// reach in its egress set, of those its report says, and how many it has
// not reported on: a node is admitted only once it reported every shared
// route, and those it fails were moved to the tunnels pinned to their
// nodes. A shared route its snapshot does not list, as a guest's on
// another node while coverage is lost, it never reports and would not
// serve: it fails it. This node, which serves its routes itself, has none.
func (c *cycleRun) joinFails(node string, rep NodeReport, self bool) (failing []string, missing int) {
	if self || c.placements == nil {
		return nil, 0
	}
	reported := map[string]agentproto.TargetReport{}
	for _, t := range rep.Status.Targets {
		reported[t.Key] = t
	}
	for _, host := range slices.Sorted(maps.Keys(c.placements)) {
		if c.placements[host].Kind != store.PlacementShared {
			continue
		}
		t, ok := reported[host]
		switch {
		case c.e.place.withholds(node, host, c.epoch()):
			failing = append(failing, host)
		case !ok || rep.Status.EpochSeen != c.epoch():
			missing++
		case !t.OK || !t.InEgressSet:
			failing = append(failing, host)
		}
	}
	return failing, missing
}

// transition moves n by ev. One that leaves it where it is changes nothing,
// not even since when.
func (c *cycleRun) transition(n *store.NodeEntry, ev registry.Event) {
	next, err := registry.Next(agentproto.NodeState(n.State), ev)
	if err != nil || string(next) == n.State {
		return
	}
	n.State, n.StateSince = string(next), c.now
}

// giveIndex gives n the index of its name, or the lowest free one; false
// when none is free, which keeps the node from admission.
func (c *cycleRun) giveIndex(n *store.NodeEntry, entries []store.NodeEntry) bool {
	if n.Index != nil {
		return true
	}
	i, err := registry.AssignIndex(entries, n.Name, beforeStates(entries))
	if err != nil {
		c.problem("node %s: %v", n.Name, err)
		return false
	}
	n.Index = &i
	for k := range entries {
		if entries[k].Name == n.Name {
			entries[k].Index = &i
		}
	}
	return true
}

// beforeStates keeps index 0 for the node an earlier release registered,
// whose entry has no state until the first start of this release numbers
// it: its leg MACs were made with 0.
func beforeStates(entries []store.NodeEntry) map[string]int {
	for _, e := range entries {
		if e.State == "" && e.Index == nil {
			return map[string]int{e.Name: 0}
		}
	}
	return nil
}

// followMaintenance sets the intent to drain a node Proxmox has in
// maintenance mode, and clears it once the node left it, when the settings
// follow maintenance. A node whose mode cannot be told keeps what was known.
func (c *cycleRun) followMaintenance(n *store.NodeEntry, maint map[string]bool) {
	on, known := maint[n.Name]
	switch {
	case !known:
	case on && n.Drained == nil:
		n.Drained = &store.NodeDrain{Reason: store.DrainMaintenance, Since: c.now}
	case !on && n.Drained != nil && n.Drained.Reason == store.DrainMaintenance:
		n.Drained = nil
	}
}

// maintenance is, by node, whether Proxmox has it in maintenance mode, as far
// as this and earlier reads could tell; empty unless the settings follow
// maintenance and pco runs on more than one node.
func (c *cycleRun) maintenance() map[string]bool {
	read := c.e.d.Maintenance
	if read == nil || !c.settings.Cluster.FollowsMaintenance() || !c.clustered() {
		return nil
	}
	mem := &c.e.registry
	if mem.maintenance == nil {
		mem.maintenance = map[string]bool{}
	}
	modes, err := read(c.ctx)
	if err != nil {
		c.e.d.Log.Debug().Err(err).Msg("reading the maintenance mode of the nodes failed; what was known stays")
	}
	for node, m := range modes {
		switch m {
		case pve.MaintenanceOn:
			mem.maintenance[node] = true
		case pve.MaintenanceOff:
			mem.maintenance[node] = false
		}
	}
	return maps.Clone(mem.maintenance)
}

// listedMembers are the nodes the members file lists, by name; none where
// there is no members file.
func (c *cycleRun) listedMembers() map[string]members.Member {
	out := map[string]members.Member{}
	if c.e.d.Members == nil {
		return out
	}
	for _, m := range c.e.d.Members().Nodes {
		out[m.Name] = m
	}
	return out
}

// noStatus says how many registered nodes the members file lists online
// whose agent never reported to this controller, once the takeover hold is
// over.
func (c *cycleRun) noStatus(reports map[string]NodeReport, listed map[string]members.Member) {
	if c.now.Sub(c.since()) < takeoverHold {
		return
	}
	var silent []string
	for name, n := range c.nodes {
		if _, heard := reports[name]; heard || name == c.e.d.Node || n.Node.State == store.NodeRemoved {
			continue
		}
		if m, ok := listed[name]; ok && m.Online {
			silent = append(silent, name)
		}
	}
	if len(silent) > 0 {
		slices.Sort(silent)
		c.problem(problemNoStatus, len(silent), strings.Join(silent, ", "))
	}
}

// noFallback names the registered nodes whose Proxmox API the client does
// not fall back on.
func (c *cycleRun) noFallback() {
	read := c.e.d.Fallbacks
	if read == nil || !c.clustered() {
		return
	}
	have := read()
	var missing []string
	for name, n := range c.nodes {
		if name == c.e.d.Node || n.Node.State == store.NodeRemoved || slices.Contains(have, name) {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		c.problem(problemNoFallback, strings.Join(missing, ", "))
	}
}

// nodeEvents tells what changed of a node's entry.
func (c *cycleRun) nodeEvents(before, after store.NodeEntry) {
	ev := func(level, msg string) {
		c.events = append(c.events, Event{At: c.now, Level: level, Kind: kindNode, Subject: after.Name, Message: msg})
	}
	if before.State != after.State {
		level := levelInfo
		if after.State == store.NodeUnknown || after.State == store.NodeDown {
			level = levelWarn
		}
		ev(level, fmt.Sprintf("node %s is %s, was %s", after.Name, after.State, stateName(before.State)))
	}
	if before.Index == nil && after.Index != nil {
		ev(levelInfo, fmt.Sprintf("node %s has index %d", after.Name, *after.Index))
	}
	switch {
	case before.Drained == nil && after.Drained != nil:
		ev(levelInfo, fmt.Sprintf("node %s is drained: Proxmox has it in maintenance mode", after.Name))
	case before.Drained != nil && after.Drained == nil:
		ev(levelInfo, fmt.Sprintf("node %s is no longer drained: Proxmox ended its maintenance mode", after.Name))
	}
}

func stateName(s string) string {
	if s == "" {
		return "without a state"
	}
	return s
}

// sameEntry says whether the step changed nothing of an entry.
func sameEntry(a, b store.NodeEntry) bool {
	sameIndex := a.Index == nil && b.Index == nil || a.Index != nil && b.Index != nil && *a.Index == *b.Index
	sameDrain := a.Drained == nil && b.Drained == nil || a.Drained != nil && b.Drained != nil && *a.Drained == *b.Drained
	return a.State == b.State && a.StateSince.Equal(b.StateSince) && sameIndex && sameDrain
}

// nodeViews are the nodes of the registry as the state shows them.
func (c *cycleRun) nodeViews() []NodeView {
	if len(c.nodes) <= 1 {
		return nil
	}
	reports := c.reports
	uses := c.joinUsesByNode()
	shared := c.sharedTunnelIDs()
	pinned := map[string]int{}
	for _, p := range c.placements {
		if p.Kind == store.PlacementPinned {
			pinned[p.Node]++
		}
	}
	out := make([]NodeView, 0, len(c.nodes))
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		v := NodeView{
			Name: n.Name, State: n.State, StateSince: n.StateSince, Kind: entryKind(n), VMID: n.VMID,
			Index: n.Index, AgentAddress: n.AgentAddress, Controller: n.Name == c.e.d.Node && !c.leaseHeld,
			Covered: !slices.Contains(c.cover.uncovered, name), Pinned: pinned[name],
			JoinUses: uses[name], Contested: joinUseViews(n.ContestedUses), CertExpires: c.certExpiry(name),
		}
		if n.Drained != nil {
			v.Drained = n.Drained.Reason
		}
		if rep, ok := reports[name]; ok {
			v.Build, v.Protocol = rep.Status.Build, protocolText(rep.Status.Protocol)
			v.StatusAge = max(c.now.Sub(rep.ReceivedAt), 0)
			v.WatchRunning = !rep.Status.WatchRunningSince.IsZero()
			if !rep.Status.Clock.IsZero() {
				v.ClockSkew = rep.Status.Clock.Sub(rep.ReceivedAt)
			}
			for _, t := range rep.Status.Targets {
				switch {
				case !t.OK:
					v.Failing++
				case !t.InEgressSet:
					v.BelowMinimum++
				default:
					v.Verified++
				}
			}
			v.ReplicaReady = slices.ContainsFunc(rep.Status.Connectors, func(s connector.Status) bool {
				return s.Ready && slices.Contains(shared, s.TunnelID)
			})
		}
		out = append(out, v)
	}
	return out
}

// joinUsesByNode are the uses of the join tokens of every node, as the
// store keeps them; none when they cannot be read.
func (c *cycleRun) joinUsesByNode() map[string][]JoinUseView {
	tokens, err := c.e.d.Store.JoinTokens()
	if err != nil {
		return nil
	}
	out := map[string][]JoinUseView{}
	for _, id := range slices.Sorted(maps.Keys(tokens)) {
		r := tokens[id]
		if len(r.Uses) > 0 {
			out[r.Node] = append(out[r.Node], joinUseViews(r.Uses)...)
		}
	}
	return out
}

// sharedTunnelIDs are the ids of the shared tunnels the last tunnel run
// found, or the state knows.
func (c *cycleRun) sharedTunnelIDs() []string {
	var out []string
	for _, t := range c.st.Tunnels {
		if t.Kind != TunnelPinned && t.ID != "" {
			out = append(out, t.ID)
		}
	}
	return out
}

// certExpiry is when the certificate enrolled for a host node ends; zero
// when none is enrolled or it cannot be read.
func (c *cycleRun) certExpiry(name string) time.Time {
	en, found, err := c.e.d.Store.Enrolment(name)
	if err != nil || !found {
		return time.Time{}
	}
	cert, err := parseCert(en.CertPEM)
	if err != nil {
		return time.Time{}
	}
	return cert.NotAfter
}

func protocolText(r agentproto.Range) string {
	if r.Min == 0 && r.Max == 0 {
		return ""
	}
	return fmt.Sprintf("%d-%d", r.Min, r.Max)
}

// entryKind is the kind of a node's agent; an entry made before kinds is a
// host's.
func entryKind(n store.NodeEntry) string {
	if n.Kind == "" {
		return "host"
	}
	return n.Kind
}
