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
	problemAgentsBehind = "the agent of node %s follows epoch %s, above this controller's %d.%s: the store is older than what the agents " +
		"were last told, as after a restore or a relocation on replicated storage; nothing is written until pco appliance recover draws above it"
)

// agentsAhead holds a controller without a lease, the appliance's, when an
// agent follows an epoch above the one this controller writes under: its
// store went back, and whatever it would write may undo what the agents
// were told since. The host's lease does this for its own.
func (c *cycleRun) agentsAhead() bool {
	if !c.writerRead {
		return false
	}
	ours := agentproto.Epoch{InstallID: c.install.ID, Generation: c.e.us.Generation, Nonce: c.e.us.Nonce}
	reports := c.reportsNow()
	for _, node := range slices.Sorted(maps.Keys(reports)) {
		ep := reports[node].Status.EpochSeen
		if ep.InstallID != c.install.ID || ep.Generation == 0 {
			continue
		}
		if _, err := agentproto.ParseEpoch(ep.String()); err != nil {
			continue
		}
		if order, same := ep.Order(ours); order > 0 || order == 0 && !same {
			c.holdBehind(c.problem(problemAgentsBehind, node, ep, ours.Generation, ours.Nonce))
			return true
		}
	}
	return false
}

// agentMoved says why the appliance agent of entry n is served nothing: its
// container runs on another node than the one it was enrolled for, by the
// snapshot of this cycle. A container on its way somewhere is left to the
// cycle after it arrived.
func (c *cycleRun) agentMoved(n store.NodeEntry) string {
	if n.Kind != string(pki.KindAppliance) || n.VMID == 0 {
		return ""
	}
	g, ok := c.snap.Guest(model.GuestRef{Kind: model.KindLXC, VMID: n.VMID})
	if !ok || g.Node == n.Name || g.Lock == "migrate" {
		return ""
	}
	return fmt.Sprintf(problemAgentMoved, n.VMID, g.Node, n.Name)
}
