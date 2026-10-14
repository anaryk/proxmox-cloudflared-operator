package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ClusterStatus is the cluster as the last cycle found it: the lease, the
// cluster and its nodes, as the state has them.
func (e *Engine) ClusterStatus() ClusterStatus {
	st := e.State()
	out := ClusterStatus{Lease: st.Lease, Nodes: nonNil(st.Nodes)}
	if st.Cluster != nil {
		out.Cluster = *st.Cluster
	}
	return out
}

// Nodes are the nodes of the registry as the last cycle found them; [] on a
// node alone.
func (e *Engine) Nodes() []NodeView { return nonNil(e.State().Nodes) }

// AgentView is the agent of this node as pco agent status shows it; the
// daemon adds what its client knows.
func (e *Engine) AgentView() (agentproto.AgentView, error) {
	a := e.fleet.current()
	if a == nil {
		return agentproto.AgentView{}, fmt.Errorf("%w: the agent of this node is not running yet: the install is not known", ErrRefused)
	}
	return a.View(), nil
}

// ResetAgentEpoch forgets the epoch the agent of this node remembers, kept
// in its local root: a node restored or copied from elsewhere, whose agent
// would hold the controller behind, follows the store's epoch again at the
// next start of the daemon. The desired state kept with it goes too: it was
// of that epoch.
func (e *Engine) ResetAgentEpoch(ctx context.Context) error {
	if err := e.acquireAdmin(ctx); err != nil {
		return err
	}
	defer e.release()
	if err := e.d.Store.DeleteAgentEpoch(); err != nil {
		return fmt.Errorf("forgetting the epoch: %w", err)
	}
	if err := e.d.Store.DeleteAgentSnapshot(); err != nil {
		return fmt.Errorf("forgetting the desired state kept with it: %w", err)
	}
	e.adminEvent(ctx, e.d.Node, "the epoch the agent of this node remembers was forgotten; it follows the store's at the next start of pco")
	return nil
}

// UninstallCluster takes every other node out of the install at once, for
// pco uninstall --cluster on the controller: each entry becomes removed and
// its connections are closed, so that its agent stands down and stops its
// connectors. It is refused while a node is not online, unless force; the
// nodes' own state is removed by pco uninstall on each, and the answer
// names the line. This node is then uninstalled as the last one.
func (e *Engine) UninstallCluster(ctx context.Context, force bool) (UninstallResult, error) {
	if err := e.acquireWriter(ctx); err != nil {
		return UninstallResult{}, err
	}
	defer e.release()
	nodes, err := e.d.Store.NodesRev()
	if err != nil {
		return UninstallResult{}, fmt.Errorf("reading the node registry: %w", err)
	}
	online := map[string]bool{}
	if m := e.d.Members; m != nil {
		for _, n := range m().Nodes {
			online[n.Name] = n.Online
		}
	}
	var offline []string
	for _, n := range nodes {
		if n.Node.Name != e.d.Node && n.Node.State != store.NodeRemoved && !online[n.Node.Name] {
			offline = append(offline, n.Node.Name)
		}
	}
	if len(offline) > 0 && !force {
		slices.Sort(offline)
		return UninstallResult{}, fmt.Errorf("%w: %s not online; their agents cannot be told to stand down, which --force does without them",
			ErrRefused, nodesAre(offline))
	}
	res := UninstallResult{Clean: []string{}}
	for _, n := range nodes {
		if n.Node.Name == e.d.Node || n.Node.State == store.NodeRemoved {
			continue
		}
		before := n.Node
		after := before
		after.State, after.StateSince, after.Drained = store.NodeRemoved, e.d.Now(), nil
		if err := e.saveEntry(ctx, before, n.Rev, after, fmt.Sprintf("node %s is removed from the install, which is uninstalled", before.Name)); err != nil {
			return res, err
		}
		if e.d.Close != nil {
			e.d.Close(before.Name)
		}
		res.Left = append(res.Left, fmt.Sprintf("%s: pco uninstall --yes --keep-cloudflare removes what pco keeps on the node", before.Name))
	}
	e.Trigger()
	return res, nil
}

func nodesAre(names []string) string {
	if len(names) == 1 {
		return "node " + names[0] + " is"
	}
	return "nodes " + strings.Join(names, ", ") + " are"
}
