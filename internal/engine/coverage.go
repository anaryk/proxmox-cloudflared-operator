package engine

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/registry"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// coverageHold is how long a controller goes without the word of a node
	// that is neither down nor removed before a loss of coverage demotes a
	// route: a node it has not heard yet is not taken to have lost its watch.
	coverageHold = 60 * time.Second

	problemCoverageLost = "node %s has no pco agent with a running watch; routes of other nodes' guests are pinned to their nodes"
	problemFlapping     = "the watch of the agent of node %s broke twice within ten minutes; routes of other nodes' guests stay pinned until it runs for ten minutes"
)

// placementMemory is what the placement of routes keeps between the cycles
// of this process, guarded by the cycle lock. Its timers start over with a
// new controller, as the stored placements do not.
type placementMemory struct {
	coverage *registry.Coverage
	// started is when this process first saw a cluster; lost and regained
	// are when coverage was last lost and regained.
	started        time.Time
	lost, regained time.Time
	covered        bool
	// refused are the refusals of what an agent said, said once each.
	refused map[string]bool
	blocks  map[netip.Addr]*conflictBlock
	// moved are, by node, MAC, bridge, VLAN and time, the moves of a MAC an
	// agent reported, with when this controller first heard each.
	moved map[string]time.Time
	// diverged are, by owner, since when its replicas diverge; reaching
	// since when every replica reaches it; waiting since when a route
	// without a placement waits for its replicas to report it.
	diverged, reaching, waiting map[string]time.Time
	// listedAt is, by node and hostname, the seq of the first snapshot of
	// the epoch listedIn that listed the target: a report of an earlier
	// seq says nothing of it.
	listedAt map[string]map[string]uint64
	listedIn agentproto.Epoch
	// withheld are, by node, the shared routes the last snapshot of the
	// epoch listedIn served to it did not list, as a guest's on another node
	// while coverage is lost, with the seq of that snapshot: the node never
	// reports them. replicasWere is the replica set of the last cycle that
	// served the agents.
	withheld     map[string]map[string]uint64
	replicasWere []string
	// movesRead says that this process read the moves stored, and gave up
	// those a controller before it had only added the rules of; addedAt is,
	// by owner, when this process first saw the rules of its move added.
	movesRead bool
	addedAt   map[string]time.Time
	// tokens are the run tokens of tunnels the agents of other nodes run,
	// by tunnel id; emptySince is since when each pinned tunnel serves
	// nothing.
	tokens     map[string]string
	emptySince map[tunnelRef]time.Time
	// retireFailed are, by pinned tunnel, the deletes of it that failed in
	// a row and when the last did: the next waits longer after each.
	retireFailed map[tunnelRef]failedDeletes
	// forgottenAsked is, by tunnel id, when its connectors were last listed
	// for the connectors of forgotten nodes.
	forgottenAsked map[string]time.Time
	// ids are the connector ids the agents of other nodes reported;
	// heard are the nodes whose agent reported to this process while it
	// controlled, and heardSince when its first such cycle noted them.
	// self are, by tunnel id, the ids of this node's ready connectors as
	// the cycle that last noted them saw them.
	ids        agentIDs
	heard      map[string]bool
	heardSince time.Time
	self       map[string][]string
	// decided is where the last placement put each owner's routes.
	decided map[string]decidedPlace
	// inherited are the placements stored when this tenure first read
	// them: what the controller before it left, which this one rewrites.
	inherited     map[string]store.Placement
	inheritedRead bool
	// pointedAt is, by hostname, the tunnel its record pointed at when a
	// DNS run of this process last listed its zone, and when that was;
	// unresolvedSince is, by hostname, since when no node resolves the
	// route.
	pointedAt       map[string]pointedRecord
	unresolvedSince map[string]time.Time
}

// pointedRecord is where the record of a hostname pointed when a DNS run
// of this process last listed its zone, and when that was.
type pointedRecord struct {
	tunnel string
	at     time.Time
}

// pointedFor is how long what a listing showed of a record stands while
// its zone cannot be listed again: this controller may have switched the
// record since, in a run that could not list the zone, and goes by a
// listing older than this no more.
const pointedFor = 5 * time.Minute

// recordPointed is the tunnel the record of host pointed at when a DNS run
// of this process listed its zone within pointedFor; empty when none did.
func (m *placementMemory) recordPointed(host string, now time.Time) string {
	p, ok := m.pointedAt[host]
	if !ok || now.Sub(p.at) >= pointedFor {
		return ""
	}
	return p.tunnel
}

// newTenure drops what the memory kept under an epoch this process no
// longer writes under, as the lease is taken again under a new one. The
// blocks and the moves are read from the store again, since another
// controller may have written them meanwhile, and the timers that let a
// move go on, a route be pinned or promoted, an address be held or a tunnel
// be deleted start over: what they counted was seen under the last tenure,
// and nothing was seen between. What the agents reported of themselves,
// the refusals said once and the coverage this process saw stand.
func (m *placementMemory) newTenure() {
	m.blocks, m.moved = nil, nil
	m.movesRead, m.addedAt = false, nil
	m.diverged, m.reaching, m.waiting = nil, nil, nil
	m.pointedAt, m.unresolvedSince = nil, nil
	m.emptySince, m.retireFailed = nil, nil
	m.inherited, m.inheritedRead = nil, false
}

// coverageView is coverage as one cycle judges it.
type coverageView struct {
	// ready says that every node that is neither down nor removed reported
	// to this process, or coverageHold passed: before, a loss demotes
	// nothing.
	ready      bool
	covered    bool
	promotable bool
	uncovered  []string
	flapping   []string
}

// lost says that coverage counts as lost for placement: every cross-node
// route is pinned to its guest's node.
func (v coverageView) lost() bool { return v.ready && !v.covered }

// observeCoverage feeds what the members file and the agents say of every
// node of the registry into the coverage of this process, and judges it.
func (c *cycleRun) observeCoverage() {
	mem := &c.e.place
	if mem.coverage == nil {
		mem.coverage, mem.started = registry.NewCoverage(), c.now
	}
	listed := c.listedMembers()
	ready := c.now.Sub(mem.started) >= coverageHold
	all := true
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		facts := c.fleet.node(name)
		if n.State == store.NodeRemoved || n.State == store.NodeContested {
			mem.coverage.Observe(name, registry.NodeFacts{}, c.now)
			continue
		}
		online := n.State != store.NodeDown
		if m, ok := listed[name]; ok {
			online = m.Online
		}
		f := registry.NodeFacts{Online: online}
		if facts.heard {
			st := facts.report.Status
			f.HasAgent, f.AgentStartedAt, f.StatusAt = true, st.AgentStartedAt, facts.report.ReceivedAt
			if facts.host {
				f.WatchRunningSince = st.WatchRunningSince
			}
		}
		mem.coverage.Observe(name, f, c.now)
		if online && (!facts.heard || facts.report.ReceivedAt.Before(mem.started)) {
			all = false
		}
	}
	covered, uncovered := mem.coverage.Covered(c.now)
	c.cover = coverageView{
		ready: ready || all, covered: covered, uncovered: uncovered,
		promotable: mem.coverage.Promotable(c.now), flapping: mem.coverage.Flapping(c.now),
	}
	if c.cover.ready && (covered != mem.covered || mem.lost.IsZero() && mem.regained.IsZero()) {
		if covered {
			mem.regained = c.now
		} else {
			mem.lost = c.now
		}
		mem.covered = covered
	}
	if c.cover.lost() {
		for _, name := range uncovered {
			c.problem(problemCoverageLost, name)
		}
	}
	for _, name := range c.cover.flapping {
		c.problem(problemFlapping, name)
	}
}

// clusterView is the cluster as this cycle found it: the controller, the
// members, coverage and the blocks.
func (c *cycleRun) clusterView() *ClusterView {
	v := c.quorumView(c.e.d.Node)
	v.Generation, v.Coverage = c.e.us.Generation, c.coverageOfView()
	v.Blocks = c.blockViews()
	v.CARotation = c.rotationView()
	v.Upgrade = c.upgradeView()
	v.JudgedAt = c.now
	return v
}

// heldClusterView is the cluster as a cycle that held before it took the
// agents' reports shows it, as while a lost node keeps the inventory from
// being read whole: the controller, the generation, the members and the
// quorum are known without the reports; the coverage, the blocks, the
// rotation and the upgrade are what the last cycle that judged them found,
// and the view says since when the cycles hold.
func (c *cycleRun) heldClusterView() *ClusterView {
	controller, generation := c.e.d.Node, c.e.us.Generation
	if c.leaseHeld {
		controller, generation = "", 0
		if l := c.st.Lease; l != nil {
			controller, generation = l.Holder, l.Generation
		}
	}
	v := c.quorumView(controller)
	v.Generation, v.HeldSince = generation, c.now
	if l := c.st.Lease; controller == c.e.d.Node && l != nil && !l.Valid {
		v.LeaseExpired = true
	}
	if prev := c.st.Cluster; prev != nil {
		v.Coverage, v.Blocks, v.CARotation, v.Upgrade = prev.Coverage, prev.Blocks, prev.CARotation, prev.Upgrade
		v.JudgedAt = prev.JudgedAt
		if !prev.HeldSince.IsZero() {
			v.HeldSince = prev.HeldSince
		}
	}
	return v
}

// quorumView is the cluster view of controller with the members and the
// quorum as Proxmox has them now.
func (c *cycleRun) quorumView(controller string) *ClusterView {
	v := &ClusterView{Controller: controller}
	v.Members = append([]string{}, slices.Sorted(maps.Keys(c.listedMembers()))...)
	if q := c.e.d.Quorum; q != nil {
		v.Quorate, _ = q()
	}
	if read := c.e.d.Corosync; read != nil {
		if cs, err := read(); err == nil {
			v.QuorumDevice = cs.QuorumDevice
			v.Overrides = quorumOverrides(cs)
		}
	}
	return v
}

// quorumOverrides names the quorum overrides of corosync.conf.
func quorumOverrides(cs members.Corosync) []string {
	var out []string
	if cs.TwoNode {
		out = append(out, "two_node")
	}
	if cs.ExpectedVotes > 0 {
		out = append(out, fmt.Sprintf("expected_votes %d", cs.ExpectedVotes))
	}
	if cs.WaitForAll != nil && !*cs.WaitForAll {
		out = append(out, "wait_for_all 0")
	}
	return out
}

// coverageOfView is coverage as the state shows it.
func (c *cycleRun) coverageOfView() CoverageView {
	mem := &c.e.place
	v := CoverageView{Covered: c.cover.covered, Uncovered: c.cover.uncovered, Flapping: c.cover.flapping}
	switch {
	case !c.cover.covered:
		v.Since = mem.lost
	default:
		v.Since = mem.regained
		if !c.cover.promotable && !mem.regained.IsZero() {
			v.PromoteAt = mem.regained.Add(registry.PromoteAfter)
		}
	}
	return v
}
