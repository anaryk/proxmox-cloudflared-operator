package engine

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// oldRulesFor is how long the rules of the tunnel a hostname moved from
	// stay after its record was switched: resolvers that still have the old
	// record are served meanwhile.
	oldRulesFor = 5 * time.Minute
	// rolloutWait is how long the connectors of the tunnel a hostname moves
	// to must be ready on every replica when Cloudflare cannot confirm the
	// configuration they run.
	rolloutWait = 30 * time.Second
)

// startMoves reads the moves stored. The first cycle of this process gives
// up every move a controller before it had not switched the records of: its
// timers are gone, and the records still point where the stored placement
// says. A move past the switch is finished.
func (c *cycleRun) startMoves() {
	moves, err := c.e.d.Store.Moves()
	if err != nil {
		c.problem("reading the moves of routes: %v; no route is moved in this cycle", err)
		c.moves = nil
		c.movesFailed = true
		return
	}
	c.moves = moves
	mem := &c.e.place
	if mem.movesRead {
		return
	}
	mem.movesRead = true
	for _, owner := range slices.Sorted(maps.Keys(moves)) {
		m := moves[owner]
		if m.Step == store.MoveCNAMEsSwitched {
			continue
		}
		if err := c.e.d.Store.DeleteMove(owner); err != nil {
			c.problem("giving up the move of %s from %s to %s: %v", owner, m.From, m.To, err)
			continue
		}
		delete(c.moves, owner)
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
			Message: fmt.Sprintf("the move of %s from %s to %s is given up: it had not switched the records when this controller took over", owner, m.From, m.To)})
	}
}

// moving is where the records of an owner's routes point in this cycle and
// the second tunnel that serves their rules, by the move under way, which
// it starts when the owner should be placed elsewhere than it is.
func (c *cycleRun) moving(owner string, cur placed, stored bool, want placed) (placed, *placed) {
	m, under := c.moves[owner]
	if !under {
		if !stored || want.same(cur) || c.movesFailed {
			return want, nil
		}
		m = store.Move{Owner: owner, From: c.tunnelOf(cur), To: c.tunnelOf(want), Step: store.MoveRulesAdded, At: c.now}
		if err := c.e.d.Store.SaveMove(m); err != nil {
			c.problem("starting the move of %s from %s to %s: %v; it stays where it is", owner, m.From, m.To, err)
			return cur, nil
		}
		c.moves[owner] = m
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
			Message: fmt.Sprintf("%s moves from %s to %s: %s", owner, m.From, m.To, want.why)})
	}
	from, to := c.placedOf(m.From, cur.why), c.placedOf(m.To, want.why)
	if want.same(from) && m.Step != store.MoveCNAMEsSwitched {
		if err := c.e.d.Store.DeleteMove(owner); err == nil {
			delete(c.moves, owner)
			c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
				Message: fmt.Sprintf("the move of %s from %s to %s is given up: it is placed in %s again", owner, m.From, m.To, m.From)})
			return from, nil
		}
	}
	if m.Step == store.MoveRulesAdded {
		return from, &to
	}
	return to, &from
}

// tunnelOf is the name of the tunnel of a placement.
func (c *cycleRun) tunnelOf(p placed) string {
	if p.kind == store.PlacementPinned {
		name, _ := planner.PinnedTunnelName(c.install.ID, p.node)
		return name
	}
	return planner.TunnelName(c.install.ID)
}

// placedOf is the placement of the tunnel of a name.
func (c *cycleRun) placedOf(name, why string) placed {
	if node, ok := planner.PinnedTunnelNode(c.install.ID, name); ok {
		return placed{kind: store.PlacementPinned, node: node, why: why}
	}
	return placed{kind: store.PlacementShared, why: why}
}

// advanceMoves takes every move a step on, once the cycle checked what the
// step waits for: the rules are verified in the tunnel moved to and its
// connectors run them, then the DNS run switched the records, then the old
// rules have served oldRulesFor longer.
func (c *cycleRun) advanceMoves() {
	if c.placing == nil || c.cfHold || c.movesFailed {
		return
	}
	for _, owner := range slices.Sorted(maps.Keys(c.moves)) {
		m := c.moves[owner]
		next := m
		switch m.Step {
		case store.MoveRulesAdded:
			if !c.rolledOutTo(m) {
				continue
			}
			next.Step, next.At = store.MoveRolledOut, c.now
		case store.MoveRolledOut:
			if !c.checked || c.recordsPending(owner) {
				continue
			}
			next.Step, next.At, next.CNAMEAt = store.MoveCNAMEsSwitched, c.now, c.now
		case store.MoveCNAMEsSwitched:
			if c.now.Sub(m.CNAMEAt) < oldRulesFor {
				continue
			}
			if err := c.e.d.Store.DeleteMove(owner); err != nil {
				c.problem("finishing the move of %s: %v", owner, err)
				continue
			}
			delete(c.moves, owner)
			c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
				Message: fmt.Sprintf("%s moved from %s to %s; the old rules are gone", owner, m.From, m.To)})
			continue
		}
		if err := c.e.d.Store.SaveMove(next); err != nil {
			c.problem("saving the move of %s: %v", owner, err)
			continue
		}
		c.moves[owner] = next
	}
}

// rolledOutTo says whether the tunnel a move goes to serves its rules: its
// configuration was verified in this cycle, and Cloudflare confirmed that
// its connectors run it, or every node that runs it has a ready connector
// of it and rolloutWait passed since the rules were added.
func (c *cycleRun) rolledOutTo(m store.Move) bool {
	i := slices.IndexFunc(c.tunnels, func(t reconcile.TunnelState) bool { return t.Name == m.To && t.Verified && t.ID != "" })
	if i < 0 {
		return false
	}
	t := c.tunnels[i]
	if r, ok := c.e.rolledOut[t.ID]; ok && r.Version >= t.Version {
		return true
	}
	if c.now.Sub(m.At) < rolloutWait {
		return false
	}
	nodes := c.replicaSet()
	if node, pinned := planner.PinnedTunnelNode(c.install.ID, m.To); pinned {
		nodes = []string{node}
	}
	for _, n := range nodes {
		if !c.readyOn(n, t.ID) {
			return false
		}
	}
	return len(nodes) > 0
}

// readyOn says whether node runs a ready connector of a tunnel, as its last
// fresh status says.
func (c *cycleRun) readyOn(node, tunnelID string) bool {
	var conns []agentStatusConnector
	if node == c.e.d.Node {
		for _, s := range c.st.Connectors {
			conns = append(conns, agentStatusConnector{s.TunnelID, s.Ready})
		}
	} else if f := c.fleet.node(node); f.fresh {
		for _, s := range f.report.Status.Connectors {
			conns = append(conns, agentStatusConnector{s.TunnelID, s.Ready})
		}
	}
	return slices.Contains(conns, agentStatusConnector{tunnelID, true})
}

type agentStatusConnector struct {
	tunnel string
	ready  bool
}

// recordsPending says whether the DNS run of this cycle left a record of an
// owner's routes unwritten.
func (c *cycleRun) recordsPending(owner string) bool {
	hosts := map[string]bool{}
	for _, rt := range c.claims.Winners {
		if rt.Owner() == owner {
			hosts[rt.Hostname] = true
		}
	}
	return slices.ContainsFunc(c.st.Actions, func(a reconcile.Action) bool {
		return !a.Applied && hosts[a.Target] && (a.Kind == reconcile.CreateRecord || a.Kind == reconcile.UpdateRecord)
	})
}

// moveView is a move under way as the state shows it.
func moveView(m store.Move) *MoveView {
	return &MoveView{From: m.From, To: m.To, Step: m.Step, Since: m.At}
}

// listed is the seq of the first snapshot of this epoch that listed host to
// node; a report of an earlier one says nothing of it.
func (m *placementMemory) listed(node, host string) uint64 {
	return m.listedAt[node][host]
}

// noteListed keeps, for the snapshot s served to its node, the seq that
// first listed each of its targets.
func (m *placementMemory) noteListed(s agentproto.Snapshot) {
	if m.listedIn != s.Epoch {
		m.listedAt, m.listedIn = map[string]map[string]uint64{}, s.Epoch
	}
	if m.listedAt == nil {
		m.listedAt = map[string]map[string]uint64{}
	}
	had := m.listedAt[s.Node]
	now := make(map[string]uint64, len(s.Targets))
	for _, t := range s.Targets {
		if seq, ok := had[t.Key]; ok {
			now[t.Key] = seq
		} else {
			now[t.Key] = s.Seq
		}
	}
	m.listedAt[s.Node] = now
}
