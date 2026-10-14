package engine

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Remote is the agents of the other nodes of a cluster, which reach the
// controller over the network: it serves each its snapshot when the agent
// asks, and keeps the last status each reported.
type Remote interface {
	// Serve hands the agents their snapshots, by node. A node not in snaps
	// is served none until a later call names it.
	Serve(snaps map[string]agentproto.Snapshot)
	// Reports returns the last report of every node; with wait > 0 it waits
	// up to wait for each node named in seq to report that seq or a later
	// one.
	Reports(ctx context.Context, wait time.Duration, seq map[string]uint64) map[string]NodeReport
}

// agentReports are the last reports of every agent: the one of this node, which
// is in this process, and those of the others.
func (e *Engine) agentReports(ctx context.Context, wait time.Duration, seq map[string]uint64) map[string]NodeReport {
	out := map[string]NodeReport{}
	if r := e.d.Remote; r != nil {
		maps.Copy(out, r.Reports(ctx, wait, seq))
	}
	maps.Copy(out, e.fleet.Reports(ctx, 0, nil))
	return out
}

// servedState says whether a node in state is served a snapshot: one that is
// admitted, about to be, or leaving. A node in any other state gets none.
func servedState(state string) bool {
	switch state {
	case store.NodeJoining, store.NodeActive, store.NodeDraining:
		return true
	}
	return false
}

// publishRemote serves the agents of the other nodes their snapshots, once
// the lease let the cycle write, the inventory was read whole and the
// reports of the agents were taken: each node resolves the targets of the
// guests it hosts and verifies the others, keeps the targets of the
// configurations last verified of the tunnels it may run, and blocks what
// every node blocks.
func (c *cycleRun) publishRemote() {
	r := c.e.d.Remote
	if r == nil || c.leaseHeld || !c.writerRead || !c.snap.Complete || c.nodes == nil || c.fleet == nil {
		return
	}
	reports := c.reportsNow()
	c.e.writes.note(c.fleet, c.epoch())
	snaps := map[string]agentproto.Snapshot{}
	var endpoints []string
	if c.e.d.Endpoints != nil {
		endpoints = c.e.d.Endpoints()
	}
	for _, name := range slices.Sorted(maps.Keys(c.nodes)) {
		n := c.nodes[name].Node
		if name == c.e.d.Node || !servedState(n.State) {
			continue
		}
		if why := c.agentMoved(n); why != "" {
			c.problem("%s", why)
			continue
		}
		s := c.desired()
		s.Node, s.State = name, agentproto.NodeState(n.State)
		s.Endpoints = slices.Clone(endpoints)
		s.Targets = c.targetsFor(name)
		s.Guests = c.snapshotGuests(s.Targets)
		role := c.sharedRole(name)
		s.Tunnels = c.withVerified(c.remoteTunnels(name, role), name, role != sharedNone)
		// A node that may run nothing is pruned too, of a shared connector
		// it still runs, once the cycle hands tokens out.
		s.Prune = c.pruned && (len(s.Tunnels) > 0 || c.handsTokens())
		s.Networks = c.networksFor(name)
		s.Actions = append(c.e.writes.actionsFor(name, c.epoch()), c.e.asks.actionsFor(name, c.epoch())...)
		s.Guests = c.withWritten(s.Guests, s.Targets, s.Actions)
		rep := reports[name]
		c.e.seqs.stamp(&s, rep.Status.EpochSeen, rep.Status.SeqApplied)
		c.e.place.noteListed(s, c.withheldFrom(s.Targets))
		snaps[name] = s
	}
	r.Serve(snaps)
}

// withheldFrom are the hostnames of the shared routes targets leave out.
func (c *cycleRun) withheldFrom(targets []agentproto.Target) map[string]bool {
	listed := make(map[string]bool, len(targets))
	for _, t := range targets {
		listed[t.Key] = true
	}
	out := map[string]bool{}
	for _, rt := range c.claims.Winners {
		if p, ok := c.placements[rt.Hostname]; ok && p.Kind == store.PlacementShared && !listed[rt.Hostname] {
			out[rt.Hostname] = true
		}
	}
	return out
}

// reportsNow are the reports this cycle goes by, read once.
func (c *cycleRun) reportsNow() map[string]NodeReport {
	if c.reports == nil {
		c.reports = c.e.agentReports(c.ctx, 0, nil)
	}
	return c.reports
}
