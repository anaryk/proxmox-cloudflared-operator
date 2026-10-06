package registry

import (
	"slices"
	"sync"
	"time"
)

const (
	// CoverageGrace is how long a node keeps its coverage while its agent
	// starts again, or while it returns, before its watch has to run.
	CoverageGrace = 60 * time.Second
	// FlapWindow is the time in which a second loss of coverage gets no grace,
	// and in which two losses make a node flap.
	FlapWindow = 10 * time.Minute
	// PromoteAfter is how long every online node must have watched without a
	// break before routes that coverage demoted may go back.
	PromoteAfter = 10 * time.Minute
	// StatusFreshFor is how old a status may be and still say what a node's
	// watch does.
	StatusFreshFor = 30 * time.Second
)

// NodeFacts is what the controller knows of one node at one moment.
type NodeFacts struct {
	// Online is what Proxmox says: the node is listed online.
	Online bool
	// HasAgent says that an agent of this install is registered for the node.
	HasAgent bool
	// AgentStartedAt is when the agent's process started, from its last status.
	AgentStartedAt time.Time
	// WatchRunningSince is when the forwarding-table watch last started and
	// has run since, from the last status; zero while it does not run.
	WatchRunningSince time.Time
	// StatusAt is when the controller received that status.
	StatusAt time.Time
}

func (f NodeFacts) fresh(now time.Time) bool {
	return !f.StatusAt.IsZero() && now.Sub(f.StatusAt) <= StatusFreshFor
}

// Coverage follows whether the forwarding table of every online node is
// watched, which is what lets a node vouch for a guest on another node. It
// lives in the controller's memory and starts over with the controller.
//
// A node is covered when it is online and its agent reports, in a fresh status,
// a watch that runs. A watch that breaks is a loss: its agent restarts, or the
// watch does, the node returns after being offline, its agent disappears, its
// watch stops or its status goes stale. A loss is forgiven once, for 60 seconds,
// when it is a restart or a return and no other loss came in the ten minutes
// before: the node stays covered meanwhile, whether its watch runs again or
// not, since a watch starts from a full dump of the tables and sees what
// happened while it was away. Any other loss uncovers the node at once, and
// when its watch runs again within ten minutes of a loss that was not forgiven,
// the node is covered only after the watch has run for 60 seconds without a
// break. So an agent that restarts every 50 seconds is covered after its first
// restart only.
//
// Offline nodes do not count. Call Observe for every node, every cycle, before
// asking: the answers are worked out from what was observed and the time asked.
// A node that leaves the registry is observed once more as offline. It is safe
// for concurrent use.
type Coverage struct {
	mu    sync.Mutex
	nodes map[string]*track
}

// NewCoverage returns a Coverage that has seen no node.
func NewCoverage() *Coverage { return &Coverage{nodes: map[string]*track{}} }

// track is what Coverage remembers of one node.
type track struct {
	seen  bool
	facts NodeFacts

	// run is the watch run covering the node: the agent and watch it belongs
	// to, the time the controller first saw it, and whether it must be seen for
	// 60 seconds before it covers.
	run run
	// graceUntil is the end of the forgiveness a loss was given, if it was.
	graceUntil time.Time
	// breaks are the losses of the last FlapWindow, oldest first; last is the
	// latest, and whether it was forgiven.
	breaks []time.Time
	last   loss
}

type run struct {
	ok           bool
	agent, watch time.Time
	since        time.Time
	proof        bool
}

type loss struct {
	at       time.Time
	forgiven bool
}

// Observe records what is known of a node at now. A node's facts are reported
// again every cycle, with the status the controller last received from it.
func (c *Coverage) Observe(node string, f NodeFacts, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.nodes[node]
	if t == nil {
		t = &track{}
		c.nodes[node] = t
	}
	t.observe(f, now)
}

func (t *track) observe(f NodeFacts, now time.Time) {
	returned := t.seen && !t.facts.Online && f.Online
	t.seen, t.facts = true, f
	if !f.Online {
		// Not counted. Its return is the loss, so nothing is kept of what ran.
		t.run, t.graceUntil = run{}, time.Time{}
		return
	}
	if returned {
		t.lose(now, true)
	}

	reporting := f.HasAgent && f.fresh(now)
	watching := reporting && !f.WatchRunningSince.IsZero()
	same := watching && f.AgentStartedAt.Equal(t.run.agent) && f.WatchRunningSince.Equal(t.run.watch)
	if t.run.ok && !same {
		agentStarted := !f.AgentStartedAt.IsZero() && !f.AgentStartedAt.Equal(t.run.agent)
		watchStarted := watching && !f.WatchRunningSince.Equal(t.run.watch)
		t.run = run{}
		t.lose(now, reporting && (agentStarted || watchStarted))
	}
	if !t.run.ok && watching {
		t.run = run{
			ok: true, agent: f.AgentStartedAt, watch: f.WatchRunningSince, since: now,
			proof: !t.last.at.IsZero() && now.Sub(t.last.at) < FlapWindow && !t.last.forgiven,
		}
	}

	if !t.graceUntil.IsZero() && !now.Before(t.graceUntil) {
		if !t.run.ok {
			t.last.forgiven = false
		}
		t.graceUntil = time.Time{}
	}
}

// lose records a loss at at. A loss that can be forgiven is, unless another one
// happened less than FlapWindow before.
func (t *track) lose(at time.Time, forgivable bool) {
	t.breaks = slices.DeleteFunc(t.breaks, func(b time.Time) bool { return at.Sub(b) >= FlapWindow })
	recent := len(t.breaks) > 0
	t.breaks = append(t.breaks, at)
	t.last = loss{at: at, forgiven: forgivable && !recent}
	t.graceUntil = time.Time{}
	if t.last.forgiven {
		t.graceUntil = at.Add(CoverageGrace)
	}
}

func (t *track) covered(now time.Time) bool {
	if !t.facts.HasAgent {
		return false
	}
	if t.run.ok && t.facts.fresh(now) {
		return !t.run.proof || now.Sub(t.run.since) >= CoverageGrace
	}
	return now.Before(t.graceUntil)
}

// solid is a node that has watched without a break for PromoteAfter, with no
// forgiveness counted.
func (t *track) solid(now time.Time) bool {
	return t.facts.HasAgent && t.run.ok && t.facts.fresh(now) && now.Sub(t.run.since) >= PromoteAfter
}

// Covered reports whether every online node is covered at now, and which online
// nodes are not, by name. Without an online node it is true.
func (c *Coverage) Covered(now time.Time) (bool, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var missing []string
	for name, t := range c.nodes {
		if t.facts.Online && !t.covered(now) {
			missing = append(missing, name)
		}
	}
	slices.Sort(missing)
	return len(missing) == 0, missing
}

// Promotable reports whether routes that coverage demoted may go back: at least
// one node is online, and each online node has reported a running watch for ten
// minutes without a break, a forgiven loss included. Any loss starts the ten
// minutes over, and the controller's own start does too.
func (c *Coverage) Promotable(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	online := 0
	for _, t := range c.nodes {
		if !t.facts.Online {
			continue
		}
		if !t.solid(now) {
			return false
		}
		online++
	}
	return online > 0
}

// Flapping names the nodes that lost coverage twice within the last ten
// minutes, offline ones too, in order. A forgiven loss counts: it is the first.
func (c *Coverage) Flapping(now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for name, t := range c.nodes {
		n := 0
		for _, b := range t.breaks {
			if now.Sub(b) < FlapWindow {
				n++
			}
		}
		if n >= 2 {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
