package engine

import (
	"maps"
	"net/netip"
	"slices"
	"time"

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
	v := &ClusterView{Controller: c.e.d.Node, Generation: c.e.us.Generation, Coverage: c.coverageOfView()}
	v.Members = append([]string{}, slices.Sorted(maps.Keys(c.listedMembers()))...)
	if q := c.e.d.Quorum; q != nil {
		v.Quorate, _ = q()
	}
	v.Blocks = c.blockViews()
	return v
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
