package engine

import (
	"fmt"
	"maps"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	problemAgentMoved   = "agent %d runs on %s, enrolled for %s; move it back or enrol it again"
	problemAgentUnseen  = "agent %d of node %s is not among the cluster's resources; it is served nothing until it is"
	problemAgentMoving  = "agent %d of node %s is being migrated to %s; it is served nothing until it runs on its node again"
	problemAgentsBehind = "the agent of node %s follows epoch %s, above this controller's %d.%s: the store is older than what the agents " +
		"were last told, as after a restore or a relocation on replicated storage; nothing is written until pco appliance recover draws above it"
)

// agentsAhead holds a controller without a lease, the appliance's, when the
// agents follow an epoch above the one this controller writes under: its
// store went back, and whatever it would write may undo what the agents
// were told since. The host's lease does this for its own.
//
// An appliance agent's report is its own word, and one agent, broken or
// taken over, must not stop the controller of every node: only the agents
// of the other admitted nodes with a fresh report vote, at least two of
// them, and the controller holds only while more than half of them report
// an epoch above its own. A silent agent stops voting once its report is
// stale. The agent of this node is on the controller's own volume, which
// came back with it, and does not vote; a node alone is never held here.
func (c *cycleRun) agentsAhead() bool {
	if !c.writerRead || !c.clustered() {
		return false
	}
	ours := agentproto.Epoch{InstallID: c.install.ID, Generation: c.e.us.Generation, Nonce: c.e.us.Nonce}
	reports := c.reportsNow()
	voters, above, first := 0, 0, ""
	var firstEp agentproto.Epoch
	for _, node := range slices.Sorted(maps.Keys(reports)) {
		rep := reports[node]
		if !c.votes(node, rep) {
			continue
		}
		voters++
		ep := rep.Status.EpochSeen
		if ep.InstallID != c.install.ID || ep.Generation == 0 {
			continue
		}
		if _, err := agentproto.ParseEpoch(ep.String()); err != nil {
			continue
		}
		if order, same := ep.Order(ours); order > 0 || order == 0 && !same {
			above++
			if first == "" {
				first, firstEp = node, ep
			}
		}
	}
	if voters < 2 || 2*above <= voters {
		return false
	}
	c.holdBehind(c.problem(problemAgentsBehind, first, firstEp, ours.Generation, ours.Nonce))
	return true
}

// votes says whether the report of node counts in agentsAhead: a fresh one
// of another node the registry holds admitted and active.
func (c *cycleRun) votes(node string, rep NodeReport) bool {
	if node == c.e.d.Node {
		return false
	}
	n, ok := c.nodes[node]
	return ok && rep.Fresh && c.now.Sub(rep.ReceivedAt) < silentAfter &&
		(n.Node.State == store.NodeActive || n.Node.State == store.NodeDraining)
}

// agentMoved says why the appliance agent of entry n is served nothing: its
// container runs on another node than the one it was enrolled for, is being
// migrated, or is not in the cluster at all, by the snapshot of this cycle,
// which publishRemote reads only when it is complete. Nothing of it is
// remembered: every cycle, and every controller after a restart or a
// takeover, judges it again from Proxmox, and serves nothing until it holds.
func (c *cycleRun) agentMoved(n store.NodeEntry) string {
	if n.Kind != string(pki.KindAppliance) {
		return ""
	}
	g, ok := c.snap.Guest(model.GuestRef{Kind: model.KindLXC, VMID: n.VMID})
	switch {
	case n.VMID == 0 || !ok:
		return fmt.Sprintf(problemAgentUnseen, n.VMID, n.Name)
	case g.Lock == "migrate":
		return fmt.Sprintf(problemAgentMoving, n.VMID, n.Name, g.Node)
	case g.Node != n.Name:
		return fmt.Sprintf(problemAgentMoved, n.VMID, g.Node, n.Name)
	}
	return ""
}
