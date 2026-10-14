package apifake

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// The cluster of a scenario is the one node of its state: there is no lease
// to hand off, no other node to admit, drain or forget, and no agent channel.
// Each call is recorded and refused as the daemon of a node alone refuses it.

// ClusterStatus is the cluster as the state shows it.
func (e *Engine) ClusterStatus() engine.ClusterStatus {
	e.record(context.Background(), "ClusterStatus", noArgs)
	e.mu.Lock()
	defer e.mu.Unlock()
	out := engine.ClusterStatus{Lease: e.state.Lease, Nodes: e.state.Nodes}
	if out.Nodes == nil {
		out.Nodes = []engine.NodeView{}
	}
	if e.state.Cluster != nil {
		out.Cluster = *e.state.Cluster
	}
	return out
}

// Nodes are the nodes of the state.
func (e *Engine) Nodes() []engine.NodeView {
	e.record(context.Background(), "Nodes", noArgs)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Nodes == nil {
		return []engine.NodeView{}
	}
	return e.state.Nodes
}

func (e *Engine) alone(ctx context.Context, method string, args any) error {
	if err := e.begin(ctx, method, args); err != nil {
		return err
	}
	return fmt.Errorf("%w: this node stands alone: there is no cluster to act on", engine.ErrRefused)
}

type nodeArgs struct {
	Node string `json:"node"`
}

// Handoff refuses: a node alone holds no lease another could take.
func (e *Engine) Handoff(ctx context.Context, to string) (engine.HandoffResult, error) {
	return engine.HandoffResult{}, e.alone(ctx, "Handoff", struct {
		To string `json:"to"`
	}{to})
}

// NewJoinToken refuses: the scenario has no CA.
func (e *Engine) NewJoinToken(ctx context.Context, node string) (engine.JoinTokenView, error) {
	return engine.JoinTokenView{}, e.alone(ctx, "NewJoinToken", nodeArgs{node})
}

// FinishUpgrade refuses: the scenario has no unfinished upgrade.
func (e *Engine) FinishUpgrade(ctx context.Context) (engine.FinishUpgradeResult, error) {
	if err := e.begin(ctx, "FinishUpgrade", noArgs); err != nil {
		return engine.FinishUpgradeResult{}, err
	}
	return engine.FinishUpgradeResult{}, fmt.Errorf("%w: no upgrade from an earlier release is unfinished", engine.ErrRefused)
}

// Unblock finds nothing blocked.
func (e *Engine) Unblock(ctx context.Context, addr netip.Addr) error {
	if err := e.begin(ctx, "Unblock", struct {
		Address netip.Addr `json:"address"`
	}{addr}); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s is not blocked", engine.ErrNotFound, addr)
}

// UninstallCluster has no other node to remove.
func (e *Engine) UninstallCluster(ctx context.Context, force bool) (engine.UninstallResult, error) {
	if err := e.begin(ctx, "UninstallCluster", struct {
		Force bool `json:"force"`
	}{force}); err != nil {
		return engine.UninstallResult{}, err
	}
	return engine.UninstallResult{Clean: []string{}}, nil
}

func (e *Engine) AdmitNode(ctx context.Context, name, fingerprint string, renew bool) (engine.NodeView, error) {
	return engine.NodeView{}, e.noSuchNode(ctx, "AdmitNode", struct {
		Node        string `json:"node"`
		Fingerprint string `json:"fingerprint"`
		Renew       bool   `json:"renew"`
	}{name, fingerprint, renew}, name)
}

func (e *Engine) DrainNode(ctx context.Context, name string, force bool) (engine.NodeView, error) {
	return engine.NodeView{}, e.noSuchNode(ctx, "DrainNode", struct {
		Node  string `json:"node"`
		Force bool   `json:"force"`
	}{name, force}, name)
}

func (e *Engine) UndrainNode(ctx context.Context, name string) (engine.NodeView, error) {
	return engine.NodeView{}, e.noSuchNode(ctx, "UndrainNode", nodeArgs{name}, name)
}

func (e *Engine) ForgetNode(ctx context.Context, name string, compromised bool) (engine.ForgetResult, error) {
	return engine.ForgetResult{}, e.noSuchNode(ctx, "ForgetNode", struct {
		Node        string `json:"node"`
		Compromised bool   `json:"compromised"`
	}{name, compromised}, name)
}

func (e *Engine) SetNode(ctx context.Context, name string, p engine.NodePatch) (engine.NodeView, error) {
	return engine.NodeView{}, e.noSuchNode(ctx, "SetNode", struct {
		Node  string           `json:"node"`
		Patch engine.NodePatch `json:"patch"`
	}{name, p}, name)
}

// noSuchNode records a call on a node and refuses it: the scenario's one
// node is not in its registry to act on.
func (e *Engine) noSuchNode(ctx context.Context, method string, args any, name string) error {
	if err := e.begin(ctx, method, args); err != nil {
		return err
	}
	return fmt.Errorf("%w: node %s is not in the registry; pco node list names the nodes", engine.ErrNotFound, name)
}

// AgentView is the agent of the one node, as the state has it.
func (e *Engine) AgentView() (agentproto.AgentView, error) {
	e.record(context.Background(), "AgentView", noArgs)
	e.mu.Lock()
	defer e.mu.Unlock()
	return agentproto.AgentView{Node: e.state.Node, Kind: pki.KindHost, Controller: e.state.Node + " (this node)", Problems: []string{}}, nil
}

// ResetAgentEpoch has nothing kept to forget.
func (e *Engine) ResetAgentEpoch(ctx context.Context) error {
	return e.begin(ctx, "ResetAgentEpoch", noArgs)
}
