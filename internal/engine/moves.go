package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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
	// recordsWaitLong is how long a move may wait at its rollout for the
	// records of its owner before it says which one it waits for.
	recordsWaitLong = 10 * time.Minute

	problemMoveWaits = "the move of %s from %s to %s waits for the record of %s: %s"
)

// startMoves reads the moves stored. The first cycle of this process gives
// up every move a controller before it had only added the rules of: its
// timers are gone, and the records still point where they did. A move whose
// rules were rolled out goes on: the last controller may have switched the
// records before it noted so, and only a DNS run tells which tunnel they
// point at, so the rules stay in both until one did. A move past the switch
// is finished.
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
		if m.Step != store.MoveRulesAdded {
			continue
		}
		if err := c.e.d.Store.DeleteMove(owner); err != nil {
			c.problem("giving up the move of %s from %s to %s: %v", owner, m.From, m.To, err)
			continue
		}
		delete(c.moves, owner)
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
			Message: fmt.Sprintf("the move of %s from %s to %s is given up: its rules were not rolled out when this controller took over", owner, m.From, m.To)})
	}
}

// giveUp ends a move whose owner is wanted elsewhere than the tunnel it goes
// to, for why. One that had only added the rules is dropped: its records
// never left. One whose rules were rolled out, and whose owner is wanted in
// the tunnel it came from, is turned around instead, as a move back at the
// same step: its records may point at the tunnel it went to already, so
// both tunnels keep the rules until a DNS run put every record back, and
// the rules of the tunnel left go as the old ones of any move do. One whose
// rules were rolled out and whose owner is wanted in a third tunnel goes on
// to its end: a move back toward a tunnel it never had the rules in would
// be turned around again in the next cycle, and the next; the move to where
// it is wanted starts once it is done. It returns the move as it stands,
// and whether it is still under way.
func (c *cycleRun) giveUp(owner string, m store.Move, want placed, why string) (store.Move, bool) {
	if m.Step == store.MoveRulesAdded {
		if err := c.e.d.Store.DeleteMove(owner); err != nil {
			return m, true
		}
		delete(c.moves, owner)
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
			Message: fmt.Sprintf("the move of %s from %s to %s is given up: %s", owner, m.From, m.To, why)})
		return m, false
	}
	if !want.same(c.placedOf(m.From, "")) {
		return m, true
	}
	back := store.Move{Owner: owner, From: m.To, To: m.From, Step: store.MoveRolledOut, At: c.now}
	if err := c.e.d.Store.SaveMove(back); err != nil {
		c.problem("giving up the move of %s from %s to %s: %v", owner, m.From, m.To, err)
		return m, true
	}
	c.moves[owner] = back
	c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
		Message: fmt.Sprintf("the move of %s from %s to %s is given up: %s, and its records go back there", owner, m.From, m.To, why)})
	return back, true
}

// moving is where the records of an owner's routes point in this cycle and
// the second tunnel that serves their rules, by the move under way, which
// it starts when the owner should be placed elsewhere than it is.
func (c *cycleRun) moving(owner string, cur placed, stored bool, want placed) (placed, *placed) {
	m, under := c.moves[owner]
	if stored && c.revoked(cur) || under && c.revoked(c.placedOf(m.From, "")) || under && c.revoked(c.placedOf(m.To, "")) {
		// The tunnel of a node removed or contested keeps no rule a moment
		// longer: it is deleted, and its token with it.
		if under && c.e.d.Store.DeleteMove(owner) == nil {
			delete(c.moves, owner)
		}
		return want, nil
	}
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
		delete(c.e.place.addedAt, owner)
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: owner,
			Message: fmt.Sprintf("%s moves from %s to %s: %s", owner, m.From, m.To, want.why)})
	}
	from, to := c.placedOf(m.From, cur.why), c.placedOf(m.To, want.why)
	if !want.same(to) && m.Step != store.MoveCNAMEsSwitched {
		// Wanted back where it came from, or elsewhere: the move is given
		// up where it can be, and one to where it is wanted now starts
		// once it is over.
		why := fmt.Sprintf("it is placed in %s again", m.From)
		if !want.same(from) {
			why = fmt.Sprintf("it is wanted in %s now", c.tunnelOf(want))
		}
		var on bool
		if m, on = c.giveUp(owner, m, want, why); !on {
			return from, nil
		}
		from, to = c.placedOf(m.From, cur.why), c.placedOf(m.To, want.why)
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
			if !c.checked {
				continue
			}
			if host, why := c.recordPending(m); host != "" {
				if c.now.Sub(m.At) >= recordsWaitLong {
					c.problem(problemMoveWaits, owner, m.From, m.To, host, why)
				}
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

// rolledOutTo says whether the tunnel a move goes to serves its rules:
// every node that runs it reaches every route of the owner, its
// configuration was verified in this cycle, and Cloudflare confirmed that
// its connectors run it, or every node that runs it has a ready connector
// of it and rolloutWait passed since this process saw the rules added. A
// ready connector on a node that does not reach the target serves nothing.
func (c *cycleRun) rolledOutTo(m store.Move) bool {
	i := slices.IndexFunc(c.tunnels, func(t reconcile.TunnelState) bool { return t.Name == m.To && t.Verified && t.ID != "" })
	if i < 0 {
		return false
	}
	t := c.tunnels[i]
	nodes := c.replicas()
	if node, pinned := planner.PinnedTunnelNode(c.install.ID, m.To); pinned {
		nodes = []string{node}
	}
	routes := c.routesOf(m.Owner)
	for _, n := range nodes {
		if !c.allReach(n, routes) {
			return false
		}
	}
	if len(nodes) == 0 || len(routes) == 0 {
		return false
	}
	if r, ok := c.e.rolledOut[t.ID]; ok && r.Version >= t.Version {
		return true
	}
	if c.now.Sub(c.addedAt(m.Owner)) < rolloutWait {
		return false
	}
	for _, n := range nodes {
		if !c.readyOn(n, t.ID) {
			return false
		}
	}
	return true
}

// addedAt is when this process first saw the rules of an owner's move
// added, by its own clock: the time stored with the move was written by
// whichever controller started it, and read back without the clock's
// monotonic reading.
func (c *cycleRun) addedAt(owner string) time.Time {
	mem := &c.e.place
	if mem.addedAt == nil {
		mem.addedAt = map[string]time.Time{}
	}
	since, ok := mem.addedAt[owner]
	if !ok {
		since, mem.addedAt[owner] = c.now, c.now
	}
	return since
}

// routesOf are the routes of an owner among the winners.
func (c *cycleRun) routesOf(owner string) []model.Route {
	var out []model.Route
	for _, rt := range c.claims.Winners {
		if rt.Owner() == owner {
			out = append(out, rt)
		}
	}
	return out
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

// recordPending names a record of the owner of a move that is not known
// to point at the tunnel the move goes to after the DNS run of this cycle,
// and why: its zone could not be listed, the run left it unwritten, or it
// neither pointed there when the run listed the zone nor was written there
// by the run. A zone that was not listed says nothing of its records. A
// record the run found in conflict, or whose adoption waits, is not pco's
// to switch and is not waited for; one that lost its marker and still
// points at a tunnel of the install is, as the old rules serve it.
func (c *cycleRun) recordPending(m store.Move) (host, why string) {
	for _, rt := range c.claims.Winners {
		if rt.Owner() != m.Owner || c.notSwitched(rt.Hostname) {
			continue
		}
		z, ok := c.zoneOf(rt.Hostname)
		switch {
		case !ok:
			return rt.Hostname, "no zone of the install holds it"
		case slices.Contains(c.dnsUnlisted, z.Name):
			return rt.Hostname, fmt.Sprintf("zone %s could not be listed", z.Name)
		}
		var written bool
		for _, a := range c.st.Actions {
			if a.Target != rt.Hostname || a.Kind != reconcile.CreateRecord && a.Kind != reconcile.UpdateRecord {
				continue
			}
			if !a.Applied {
				return rt.Hostname, "the DNS run left it unwritten"
			}
			written = true
		}
		if !written && c.dnsPointsAt[strings.ToLower(rt.Hostname)] != m.To {
			return rt.Hostname, "it does not point there yet"
		}
	}
	return "", ""
}

// notSwitched says that pco leaves the record of a hostname as it is: the
// DNS run found it in conflict or its adoption waits. A record whose marker
// is lost may still point at the tunnel being left, so it is waited for
// either way.
func (c *cycleRun) notSwitched(host string) bool {
	if slices.ContainsFunc(c.st.Lost, func(name string) bool { return strings.EqualFold(name, host) }) {
		return false
	}
	if _, waits := c.adoptWaits[strings.ToLower(host)]; waits {
		return true
	}
	return slices.ContainsFunc(c.st.Conflicts, func(x reconcile.Conflict) bool { return strings.EqualFold(x.Name, host) })
}

// moveView is a move under way as the state shows it.
func moveView(m store.Move) *MoveView {
	return &MoveView{From: m.From, To: m.To, Step: m.Step, Since: m.At}
}

// listed is the seq of the first snapshot of this epoch that listed host to
// node for it to resolve, or to verify; false when none did. A report of an
// earlier snapshot, or of the target in the other role, says nothing of it.
func (m *placementMemory) listed(node, host string, resolve bool) (uint64, bool) {
	seq, ok := m.listedAt[node][listedKey(host, resolve)]
	return seq, ok
}

func listedKey(host string, resolve bool) string {
	if resolve {
		return host + " resolve"
	}
	return host + " verify"
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
		key := listedKey(t.Key, t.Resolve)
		if seq, ok := had[key]; ok {
			now[key] = seq
		} else {
			now[key] = s.Seq
		}
	}
	m.listedAt[s.Node] = now
}
