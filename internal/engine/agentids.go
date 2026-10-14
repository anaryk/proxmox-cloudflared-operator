package engine

import (
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const problemSecondConnector = "agent of node %s reports a second connector %s on tunnel %s: it is treated as rogue"

// agentIDs are, by node and tunnel, the connector ids the agent of a node
// reported as its own, with when each was last reported.
type agentIDs map[string]map[string]map[string]time.Time

// noteAgentIDs takes the connector ids the agents of the other nodes report
// as their own, of the tunnels each may run: the shared tunnel, for a node
// that is served, and the tunnel pinned to the node itself. An agent owns at
// most one id per tunnel, and the one before it for accountsFreshFor after
// a restart; a further id it reports is not taken, and named. A connector a
// node reports of a tunnel pinned to another node is never its own.
func (c *cycleRun) noteAgentIDs(own func(tunnel, id string)) {
	if c.fleet == nil {
		return
	}
	mem := &c.e.place
	if mem.ids == nil {
		mem.ids = agentIDs{}
	}
	ids := map[string]string{} // tunnel id -> name, of the tunnels this cycle knows
	for _, t := range slices.Concat(c.tunnels, c.shown) {
		if t.ID != "" {
			ids[t.ID] = t.Name
		}
	}
	for _, node := range slices.Sorted(maps.Keys(c.fleet.nodes)) {
		f := c.fleet.nodes[node]
		if !f.fresh || !servedState(f.entry.State) {
			continue
		}
		byTunnel := mem.ids[node]
		if byTunnel == nil {
			byTunnel = map[string]map[string]time.Time{}
			mem.ids[node] = byTunnel
		}
		for _, st := range f.report.Status.Connectors {
			name, known := ids[st.TunnelID]
			if !st.Ready || st.ConnectorID == "" || !known || !c.mayRun(node, name) {
				continue
			}
			seen := byTunnel[st.TunnelID]
			if seen == nil {
				seen = map[string]time.Time{}
				byTunnel[st.TunnelID] = seen
			}
			maps.DeleteFunc(seen, func(_ string, at time.Time) bool { return !c.ownStill(at) })
			if _, had := seen[st.ConnectorID]; !had && len(seen) >= 2 {
				c.problem(problemSecondConnector, node, st.ConnectorID, name)
				continue
			}
			seen[st.ConnectorID] = c.now
			own(st.TunnelID, st.ConnectorID)
		}
	}
}

// mayRun says whether the agent of node may run a connector of the tunnel
// of name: the shared one, or the one pinned to it.
func (c *cycleRun) mayRun(node, name string) bool {
	if name == planner.TunnelName(c.install.ID) {
		return true
	}
	pinnedTo, ok := planner.PinnedTunnelNode(c.install.ID, name)
	return ok && pinnedTo == node
}

// agentRuns says whether the agent of another node reported, in this
// cycle, a ready connector of a tunnel as its own.
func (c *cycleRun) agentRuns(tunnelID string) bool {
	for _, byTunnel := range c.e.place.ids {
		for id, at := range byTunnel[tunnelID] {
			if id != "" && at.Equal(c.now) {
				return true
			}
		}
	}
	return false
}
