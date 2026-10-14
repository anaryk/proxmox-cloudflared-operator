package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// pinnedEmptyFor is how long a tunnel pinned to a node serves no route and
// no record points at it before it is deleted.
const pinnedEmptyFor = 10 * time.Minute

// maxIngressRules is the most rules of hostnames pco keeps a tunnel
// under, well above the routes it is measured with. Cloudflare documents no
// limit; a tunnel above it is named, as the blocks of an account go into
// every tunnel of it, and a cluster with many pinned tunnels multiplies
// them.
const maxIngressRules = 5000

const problemTooManyRules = "tunnel %s in account %s has %d ingress rules, more than the %d pco keeps a tunnel under; " +
	"Cloudflare may refuse its configuration"

// checkRuleCount names every planned tunnel with more rules of hostnames
// than maxIngressRules.
func (c *cycleRun) checkRuleCount() {
	for _, p := range c.plan.Tunnels {
		n := 0
		for _, r := range p.Rules {
			if r.Hostname != "" && !strings.HasSuffix(r.Hostname, ".invalid") {
				n++
			}
		}
		if limit := cmp.Or(c.e.ruleLimit, maxIngressRules); n > limit {
			c.problem(problemTooManyRules, p.Name, p.AccountID, n, limit)
		}
	}
}

// sharedRole is what the agent of another node does with the shared
// tunnel.
type sharedRole int

const (
	// sharedNone: no connector of it, and one that still runs is pruned.
	sharedNone sharedRole = iota
	// sharedKept: a connector of it that runs is kept as it is, and none
	// is started.
	sharedKept
	// sharedRun: a connector of it runs with its token.
	sharedRun
)

// sharedRole is what node does with the shared tunnel. A replica runs it.
// Cloudflare sends a connector of it the requests of every shared route,
// so no other node runs one: a joining node that does not reach every
// shared route, a drained one and one outside cluster.replicas have it
// removed, and none of them is handed its token. A joining node that
// reaches every shared route keeps a connector that still runs, as after
// its daemon alone restarted, since it is admitted in the next cycle.
func (c *cycleRun) sharedRole(node string) sharedRole {
	switch {
	case slices.Contains(c.replicas(), node):
		return sharedRun
	case c.nodes[node].Node.State == store.NodeJoining && c.joinKeeps[node]:
		return sharedKept
	}
	return sharedNone
}

// handsTokens says whether the cycle hands run tokens to the agents: only
// an enforcing cycle that ran the connectors of this node and holds nothing
// does.
func (c *cycleRun) handsTokens() bool {
	return c.connected && !c.cfHold && c.mode() == reconcile.Enforce
}

// remoteTunnels are the tunnels whose connectors the agent of node runs,
// each with its run token: the shared tunnel of every account as role
// says, and the tunnel pinned to the node, to a host agent alone. A node of
// a state the cycle does not know, one that is not served, and an appliance
// agent get no pinned tunnel. A joining node does get its own pinned
// tunnel while the plan has routes there: after a reboot the node comes
// back joining with its guests, and their pinned routes are served through
// that tunnel alone, so holding its token until admission would keep them
// down for as long as the admission takes. A tunnel whose token cannot be
// had is listed without one: its connector is kept as it is.
func (c *cycleRun) remoteTunnels(node string, role sharedRole) []agentproto.Tunnel {
	n, ok := c.nodes[node]
	if !ok || !servedState(n.Node.State) || !c.handsTokens() {
		return nil
	}
	own, err := planner.PinnedTunnelName(c.install.ID, node)
	host := entryKind(n.Node) == string(pki.KindHost)
	var out []agentproto.Tunnel
	for _, t := range c.tunnels {
		if !t.Exists || t.ID == "" {
			continue
		}
		switch {
		case t.Name == planner.TunnelName(c.install.ID) && role == sharedKept:
			out = append(out, c.snapshotTunnel(t.AccountID, t.ID, t.Name, ""))
			continue
		case t.Name == planner.TunnelName(c.install.ID) && role == sharedRun:
		case err == nil && t.Name == own && host && c.planned(tunnelRef{t.AccountID, own}):
		default:
			continue
		}
		token, _ := c.tokenFor(t)
		out = append(out, c.snapshotTunnel(t.AccountID, t.ID, t.Name, token))
	}
	// The tunnels the cycle leaves as they are, as those of a frozen
	// account, are kept as they are on the node too: listed without a
	// token, so that a prune keeps their connectors.
	for _, t := range c.shown {
		mayRun := t.Name == planner.TunnelName(c.install.ID) && role != sharedNone || err == nil && t.Name == own && host
		if t.ID == "" || !mayRun || slices.ContainsFunc(out, func(o agentproto.Tunnel) bool { return o.TunnelID == t.ID }) {
			continue
		}
		out = append(out, c.snapshotTunnel(t.AccountID, t.ID, t.Name, ""))
	}
	return out
}

// tokenFor is the run token of a tunnel for another node's agent: the one
// this node's connector runs with, or the one Cloudflare hands out, kept
// and read again whenever the accounts of its credential are listed.
func (c *cycleRun) tokenFor(t reconcile.TunnelState) (string, bool) {
	mem := &c.e.place
	if token, ok := c.runTokens[t.ID]; ok {
		return token, true
	}
	if mem.tokens == nil {
		mem.tokens = map[string]string{}
	}
	if token, ok := mem.tokens[t.ID]; ok && !c.accountsListed(t.CredentialID) {
		return token, true
	}
	token, err := c.fetchToken(t)
	if err != nil {
		c.problem("tunnel %s in account %s: fetching its token for the other nodes: %v", t.Name, t.AccountID, err)
		token, ok := mem.tokens[t.ID]
		return token, ok
	}
	mem.tokens[t.ID] = token
	return token, true
}

// revoked says whether a placement pins to a node removed from the install
// or contested: no route stays there, and its tunnel goes at once.
func (c *cycleRun) revoked(p placed) bool {
	if p.kind != store.PlacementPinned || p.node == "" {
		return false
	}
	n, ok := c.nodes[p.node]
	return ok && (n.Node.State == store.NodeRemoved || n.Node.State == store.NodeContested)
}

// findPinned has the tunnel runs list the pinned tunnels of every account
// the stored placements or the tunnels seen say had one: after a restart a
// pinned tunnel no plan names any more is found that way, emptied and in
// time deleted.
func (c *cycleRun) findPinned() {
	accounts := map[string]bool{}
	for host, p := range c.placements {
		if p.Kind != store.PlacementPinned {
			continue
		}
		if z, ok := c.zoneOf(host); ok {
			accounts[z.AccountID] = true
		}
	}
	for _, t := range c.e.seen {
		if _, pinned := planner.PinnedTunnelNode(c.install.ID, t.name); pinned {
			accounts[t.account] = true
		}
	}
	for _, account := range slices.Sorted(maps.Keys(accounts)) {
		c.e.tunnels.ListPinned(account)
	}
}

// retirePinned deletes a tunnel pinned to a node once it served no route and
// no record pointed at it for pinnedEmptyFor, or at once when its node was
// removed from the install. The tunnel run emptied it first: only a tunnel
// whose configuration this install wrote is emptied, and only one it
// verified empty is deleted.
func (c *cycleRun) retirePinned() {
	if c.fleet == nil || c.cfHold || !c.checked || c.mode() != reconcile.Enforce {
		return
	}
	mem := &c.e.place
	if mem.emptySince == nil {
		mem.emptySince = map[tunnelRef]time.Time{}
	}
	for _, t := range c.tunnels {
		node, pinned := planner.PinnedTunnelNode(c.install.ID, t.Name)
		ref := tunnelRef{t.AccountID, t.Name}
		if !pinned || !t.Exists || t.ID == "" || c.planned(ref) || c.pointedAt(ref) {
			delete(mem.emptySince, ref)
			continue
		}
		since, ok := mem.emptySince[ref]
		if !ok {
			since, mem.emptySince[ref] = c.now, c.now
		}
		state := c.nodes[node].Node.State
		removed := state == store.NodeRemoved || state == store.NodeContested
		if !t.Verified || !removed && c.now.Sub(since) < pinnedEmptyFor {
			continue
		}
		api := c.e.clients[t.CredentialID]
		if api == nil {
			continue
		}
		if removed {
			// The node may still run its connector: Cloudflare deletes no
			// tunnel with connections, and the node must keep none.
			if err := api.CleanUpConnections(c.ctx, t.AccountID, t.ID); err != nil {
				c.problem("ending the connections of tunnel %s in account %s, which node %s ran: %v", t.Name, t.AccountID, node, err)
				continue
			}
		}
		if err := api.DeleteTunnel(c.ctx, t.AccountID, t.ID); err != nil {
			c.problem("deleting tunnel %s in account %s, which serves nothing: %v", t.Name, t.AccountID, err)
			continue
		}
		delete(mem.emptySince, ref)
		delete(c.e.seen, t.ID)
		delete(c.e.verified, ref)
		c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindPlacement, Subject: t.Name, Tunnel: t.Name, Account: t.AccountID,
			Message: fmt.Sprintf("tunnel %s in account %s is deleted: %s", t.Name, t.AccountID, retireWhy(removed, node))})
	}
}

func retireWhy(removed bool, node string) string {
	if removed {
		return "node " + node + " was removed from the install, or is contested: its copy of the run token must stop working"
	}
	return fmt.Sprintf("it served no route for %s", pinnedEmptyFor)
}

// planned says whether the plan of the cycle has a tunnel.
func (c *cycleRun) planned(ref tunnelRef) bool {
	return slices.ContainsFunc(c.plan.Tunnels, func(p planner.TunnelPlan) bool { return p.AccountID == ref.account && p.Name == ref.name })
}

// pointedAt says whether a record of the plan points at a tunnel.
func (c *cycleRun) pointedAt(ref tunnelRef) bool {
	return slices.ContainsFunc(c.plan.Records, func(r planner.RecordPlan) bool { return r.AccountID == ref.account && r.TunnelName == ref.name })
}
