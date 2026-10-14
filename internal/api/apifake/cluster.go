package apifake

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The cluster of a scenario is what its state shows of it. A scenario of
// one node has no lease to hand off and no other node to admit, drain or
// forget: each call is recorded and refused as the daemon of a node alone
// refuses it. A scenario with nodes keeps what an admin does to them, as far
// as the state can show it: an admitted node is active, a drained one
// draining, a forgotten one removed; nothing moves, rotates or is written to
// Cloudflare.

// How long the other nodes hold back from the lease after a hand-off, as
// the lease keeper does.
const handoffBackoff = 30 * time.Second

// What the disk of a forgotten node holds that pco cannot rotate, as the
// daemon lists it.
var notRotated = []string{
	"the corosync authkey (/etc/corosync/authkey): a node that keeps it can join the cluster again; pvecm updatecerts after a new key",
	"the SSH keys of root (/root/.ssh, /etc/ssh/ssh_host_*): the other nodes trust them until they are replaced",
	"the key of the cluster CA (/etc/pve/priv/pve-root-ca.key): every node's pveproxy certificate is signed with it",
}

// ClusterStatus is the cluster as the state shows it.
func (e *Engine) ClusterStatus() engine.ClusterStatus {
	e.record(context.Background(), "ClusterStatus", noArgs)
	e.mu.Lock()
	defer e.mu.Unlock()
	out := engine.ClusterStatus{Lease: e.state.Lease, Nodes: nonNil(e.state.Nodes)}
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
	return nonNil(e.state.Nodes)
}

// EnrolledFingerprint is the fingerprint the scenario enrolled a node's
// certificate under, which an admin types to admit an appliance agent: the
// SHA-256 of the node's name, so that a test can know it.
func EnrolledFingerprint(node string) string {
	sum := sha256.Sum256([]byte(node))
	return hex.EncodeToString(sum[:])
}

type nodeArgs struct {
	Node string `json:"node"`
}

// alone refuses a call of the cluster on a scenario of one node.
func alone() error {
	return fmt.Errorf("%w: this node stands alone: there is no cluster to act on", engine.ErrRefused)
}

// nodeAt is the index of a node in the state, or an error as the daemon
// gives it: a node alone has no registry, and a node not in it is not found.
// The caller holds mu.
func (e *Engine) nodeAt(name string) (int, error) {
	if len(e.state.Nodes) == 0 {
		return -1, alone()
	}
	i := slices.IndexFunc(e.state.Nodes, func(n engine.NodeView) bool { return n.Name == name })
	if i < 0 {
		return -1, fmt.Errorf("%w: node %s is not in the registry; pco node list names the nodes", engine.ErrNotFound, name)
	}
	return i, nil
}

// editNode replaces a node of the state with what edit makes of it, and
// tells every stream. The caller holds mu.
func (e *Engine) editNode(i int, edit func(n *engine.NodeView)) engine.NodeView {
	nodes := slices.Clone(e.state.Nodes)
	edit(&nodes[i])
	e.state.Nodes = nodes
	e.changed()
	return nodes[i]
}

// Handoff releases the lease for another node, as far as the state can
// show it: the lease names the node it goes to.
func (e *Engine) Handoff(ctx context.Context, to string) (engine.HandoffResult, error) {
	if err := e.begin(ctx, "Handoff", struct {
		To string `json:"to"`
	}{to}); err != nil {
		return engine.HandoffResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Lease == nil || len(e.state.Nodes) == 0 {
		return engine.HandoffResult{}, alone()
	}
	if to != "" {
		i, err := e.nodeAt(to)
		if err != nil {
			return engine.HandoffResult{}, err
		}
		switch n := e.state.Nodes[i]; {
		case n.Controller:
			return engine.HandoffResult{}, fmt.Errorf("%w: node %s is the controller already", engine.ErrRefused, to)
		case n.State != string(agentproto.Active):
			return engine.HandoffResult{}, fmt.Errorf("%w: node %s is %s; the lease goes only to an active node", engine.ErrRefused, to, n.State)
		}
	}
	lease := *e.state.Lease
	lease.Handoff = to
	e.state.Lease = &lease
	res := engine.HandoffResult{From: e.state.Node, To: to, Until: e.now().Add(handoffBackoff)}
	what := "the next node to acquire it"
	if to != "" {
		what = "node " + to
	}
	e.adminEvent(ctx, e.state.Node, fmt.Sprintf("the lease is handed off to %s after the running cycle; every other node holds back until %s",
		what, res.Until.UTC().Format(time.RFC3339)))
	e.changed()
	return res, nil
}

// NewJoinToken makes a token for a node to join with. The scenario has no
// CA: the token is of the right form and of no use.
func (e *Engine) NewJoinToken(ctx context.Context, node string) (engine.JoinTokenView, error) {
	if err := e.begin(ctx, "NewJoinToken", nodeArgs{node}); err != nil {
		return engine.JoinTokenView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.state.Nodes) == 0 {
		return engine.JoinTokenView{}, alone()
	}
	if node == "" {
		return engine.JoinTokenView{}, fmt.Errorf("%w: a join token is made for a node by name", engine.ErrInvalid)
	}
	if i := slices.IndexFunc(e.state.Nodes, func(n engine.NodeView) bool { return n.Name == node }); i >= 0 && e.state.Nodes[i].State != string(agentproto.Removed) {
		return engine.JoinTokenView{}, fmt.Errorf("%w: node %s is in the install already (%s); pco node forget %s first", engine.ErrRefused, node, e.state.Nodes[i].State, node)
	}
	var secret [16]byte
	_, _ = rand.Read(secret[:])
	now := e.now()
	v := engine.JoinTokenView{ID: hex.EncodeToString(secret[:4]), Node: node, Expires: now.Add(15 * time.Minute)}
	body, _ := json.Marshal(map[string]any{
		"v": 1, "install": "abc123", "node": node, "endpoints": []string{"10.10.0.1:8644"}, "serverName": "controller.pco-abc123.invalid",
		"ca": EnrolledFingerprint("ca"), "secret": hex.EncodeToString(secret[:]), "expires": v.Expires.UTC().Format(time.RFC3339),
	})
	v.Token = "pco1." + base64.RawURLEncoding.EncodeToString(body)
	e.adminEvent(ctx, node, fmt.Sprintf("a join token was made for node %s; it is good until %s and for one use", node, v.Expires.UTC().Format(time.RFC3339)))
	return v, nil
}

// FinishUpgrade refuses: the scenario has no unfinished upgrade.
func (e *Engine) FinishUpgrade(ctx context.Context) (engine.FinishUpgradeResult, error) {
	if err := e.begin(ctx, "FinishUpgrade", noArgs); err != nil {
		return engine.FinishUpgradeResult{}, err
	}
	return engine.FinishUpgradeResult{}, fmt.Errorf("%w: no upgrade from an earlier release is unfinished", engine.ErrRefused)
}

// Unblock ends the block of an address the state shows blocked.
func (e *Engine) Unblock(ctx context.Context, addr netip.Addr) error {
	if err := e.begin(ctx, "Unblock", struct {
		Address netip.Addr `json:"address"`
	}{addr}); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.state.Cluster
	i := -1
	if c != nil {
		i = slices.IndexFunc(c.Blocks, func(b engine.BlockView) bool { return b.Address == addr })
	}
	if i < 0 {
		return fmt.Errorf("%w: %s is not blocked", engine.ErrNotFound, addr)
	}
	b := c.Blocks[i]
	cluster := *c
	cluster.Blocks = slices.Delete(slices.Clone(c.Blocks), i, i+1)
	e.state.Cluster = &cluster
	line := fmt.Sprintf("%s is blocked on every node: its MAC %s was seen on %s of %s", b.Address, b.MAC, b.Port, b.Node)
	e.state.Problems = slices.DeleteFunc(slices.Clone(e.state.Problems), func(p string) bool { return p == line })
	e.adminEvent(ctx, addr.String(), fmt.Sprintf("the block of %s is ended by the admin: every node serves it again from its next desired state", addr))
	e.changed()
	return nil
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

// AdmitNode admits a node that joins: an appliance agent with the
// fingerprint of its certificate, as EnrolledFingerprint has it.
func (e *Engine) AdmitNode(ctx context.Context, name, fingerprint string, renew bool) (engine.NodeView, error) {
	if err := e.begin(ctx, "AdmitNode", struct {
		Node        string `json:"node"`
		Fingerprint string `json:"fingerprint"`
		Renew       bool   `json:"renew"`
	}{name, fingerprint, renew}); err != nil {
		return engine.NodeView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i, err := e.nodeAt(name)
	if err != nil {
		return engine.NodeView{}, err
	}
	n := e.state.Nodes[i]
	fp := strings.ToLower(strings.TrimSpace(fingerprint))
	switch {
	case n.State == string(agentproto.Contested):
		return n, fmt.Errorf("%w: node %s is contested: its join token was used %d times; pco node forget %s removes the entry, and a new token enrols the node again",
			engine.ErrRefused, name, len(n.Contested), name)
	case n.State == string(agentproto.Removed):
		return n, fmt.Errorf("%w: node %s was removed from the install; pco setup on the node, or a new join token, enrols it again", engine.ErrRefused, name)
	case n.State == string(agentproto.Active) || n.State == string(agentproto.Draining):
		return n, fmt.Errorf("%w: node %s is %s already", engine.ErrRefused, name, n.State)
	case fp != "" && (len(fp) < 8 || len(fp) > 64 || strings.Trim(fp, "0123456789abcdef") != ""):
		return n, fmt.Errorf("%w: a fingerprint is the SHA-256 of the certificate in hex, at least 8 digits of it, not %q", engine.ErrInvalid, fingerprint)
	case fp != "" && !strings.HasPrefix(EnrolledFingerprint(name), fp):
		return n, fmt.Errorf("%w: the fingerprint %s is not that of the certificate enrolled for node %s (%s); read it with pco agent status on the node",
			engine.ErrRefused, fp, name, EnrolledFingerprint(name))
	case fp == "" && n.Kind == string(pki.KindAppliance):
		return n, fmt.Errorf("%w: an appliance agent is admitted with the fingerprint of its certificate: pco node admit %s --fingerprint <sha256>, "+
			"as pco agent status shows it in the container", engine.ErrInvalid, name)
	case renew && n.Kind != string(pki.KindAppliance):
		return n, fmt.Errorf("%w: node %s is a host node, which issues its own certificates; --renew is for an appliance agent", engine.ErrRefused, name)
	case n.State != string(agentproto.Joining):
		return n, fmt.Errorf("%w: node %s is %s; it is admitted once its agent reports again", engine.ErrRefused, name, n.State)
	}
	index := len(e.state.Nodes)
	now := e.now()
	admitted := e.editNode(i, func(n *engine.NodeView) {
		n.State, n.StateSince, n.Index, n.JoinUses = string(agentproto.Active), now, &index, nil
		n.ReplicaReady, n.WatchRunning = true, n.Kind == string(pki.KindHost)
	})
	msg := fmt.Sprintf("node %s is admitted by the admin; every shared route it fails moves to its guest's node first", name)
	if fp != "" {
		msg = fmt.Sprintf("node %s is admitted by the admin, its certificate trusted by fingerprint %s; every shared route it fails moves to its guest's node first", name, fp)
	}
	e.adminEvent(ctx, name, msg)
	return admitted, nil
}

// DrainNode takes a node out of the shared tunnel's replicas, the last one
// only with force; a node that is not active yet keeps the intent.
func (e *Engine) DrainNode(ctx context.Context, name string, force bool) (engine.NodeView, error) {
	if err := e.begin(ctx, "DrainNode", struct {
		Node  string `json:"node"`
		Force bool   `json:"force"`
	}{name, force}); err != nil {
		return engine.NodeView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i, err := e.nodeAt(name)
	if err != nil {
		return engine.NodeView{}, err
	}
	n := e.state.Nodes[i]
	switch n.State {
	case string(agentproto.Removed), string(agentproto.Contested):
		return n, fmt.Errorf("%w: node %s is %s; it serves nothing to drain", engine.ErrRefused, name, n.State)
	case string(agentproto.Draining):
		return n, fmt.Errorf("%w: node %s is draining already", engine.ErrRefused, name)
	}
	if n.Drained != "" {
		return n, fmt.Errorf("%w: node %s is drained already (%s)", engine.ErrRefused, name, n.Drained)
	}
	others := slices.ContainsFunc(e.state.Nodes, func(o engine.NodeView) bool {
		return o.Name != name && o.State == string(agentproto.Active) && o.Drained == ""
	})
	if !force && n.State == string(agentproto.Active) && !others {
		return n, fmt.Errorf("%w: node %s is the only active replica of the shared tunnel; draining it stops the tunnel's only connector, "+
			"which --force does all the same", engine.ErrRefused, name)
	}
	now := e.now()
	drained := e.editNode(i, func(n *engine.NodeView) {
		n.Drained = store.DrainAdmin
		if n.State == string(agentproto.Active) {
			n.State, n.StateSince, n.ReplicaReady = string(agentproto.Draining), now, false
		}
	})
	msg := fmt.Sprintf("node %s is drained by the admin: it leaves the shared tunnel's replicas; its pinned tunnel keeps the routes of its own guests", name)
	if drained.State != string(agentproto.Draining) {
		msg = fmt.Sprintf("node %s is to be drained by the admin; it is %s, and the drain applies once it is admitted", name, drained.State)
	}
	e.adminEvent(ctx, name, msg)
	return drained, nil
}

// UndrainNode ends an admin's drain, not one of Proxmox's maintenance mode.
func (e *Engine) UndrainNode(ctx context.Context, name string) (engine.NodeView, error) {
	if err := e.begin(ctx, "UndrainNode", nodeArgs{name}); err != nil {
		return engine.NodeView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i, err := e.nodeAt(name)
	if err != nil {
		return engine.NodeView{}, err
	}
	n := e.state.Nodes[i]
	switch n.Drained {
	case "":
		return n, fmt.Errorf("%w: node %s is not drained", engine.ErrRefused, name)
	case store.DrainMaintenance:
		return n, fmt.Errorf("%w: Proxmox has node %s in maintenance mode, which drains it; end it there (ha-manager crm-command node-maintenance disable %s)",
			engine.ErrRefused, name, name)
	}
	now := e.now()
	undrained := e.editNode(i, func(n *engine.NodeView) {
		n.Drained = ""
		if n.State == string(agentproto.Draining) {
			n.State, n.StateSince, n.ReplicaReady = string(agentproto.Active), now, true
		}
	})
	e.adminEvent(ctx, name, fmt.Sprintf("the drain of node %s is ended by the admin: it is a replica of the shared tunnel again", name))
	return undrained, nil
}

// ForgetNode takes a node out of the install. As compromised it is refused
// while Proxmox lists the node, as the daemon refuses it.
func (e *Engine) ForgetNode(ctx context.Context, name string, compromised bool) (engine.ForgetResult, error) {
	if err := e.begin(ctx, "ForgetNode", struct {
		Node        string `json:"node"`
		Compromised bool   `json:"compromised"`
	}{name, compromised}); err != nil {
		return engine.ForgetResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i, err := e.nodeAt(name)
	if err != nil {
		return engine.ForgetResult{}, err
	}
	n := e.state.Nodes[i]
	switch {
	case n.Controller || name == e.state.Node:
		return engine.ForgetResult{}, fmt.Errorf("%w: node %s is this node; pco uninstall takes it out of the install", engine.ErrRefused, name)
	case n.State == string(agentproto.Removed):
		return engine.ForgetResult{}, fmt.Errorf("%w: node %s was removed already", engine.ErrRefused, name)
	case compromised && e.state.Cluster != nil && slices.Contains(e.state.Cluster.Members, name):
		return engine.ForgetResult{}, fmt.Errorf("%w: run pvecm delnode %s first: a node still in the cluster would receive every new secret", engine.ErrRefused, name)
	}
	now := e.now()
	e.editNode(i, func(n *engine.NodeView) {
		n.State, n.StateSince, n.Drained, n.Controller = string(agentproto.Removed), now, "", false
		n.ReplicaReady, n.WatchRunning, n.Covered, n.JoinUses, n.Contested = false, false, false, nil, nil
	})
	if c := e.state.Cluster; c != nil {
		cluster := *c
		cluster.Members = slices.DeleteFunc(slices.Clone(c.Members), func(m string) bool { return m == name })
		e.state.Cluster = &cluster
	}
	res := engine.ForgetResult{Node: name, Compromised: compromised, Done: []string{
		"the entry of the node is removed: its agent is refused from its next request",
		"its connections to this controller are closed",
		"its pinned tunnels are deleted in the next cycle",
	}}
	what := ""
	if compromised {
		what = " as compromised"
		res.Pending = append(res.Pending, "every secret of the install is rotated step by step by the cycles that follow: the credentials, the Proxmox token, the tunnels, the CA")
		res.NotRotated = slices.Clone(notRotated)
	} else {
		res.Done = append(res.Done, "the secret of the shared tunnel is rotated: the connector of the node is cut off, and the others restart with the new token")
	}
	e.adminEvent(ctx, name, fmt.Sprintf("node %s is forgotten%s by the admin", name, what))
	e.changed()
	return res, nil
}

// SetNode keeps nothing of the patch: the state shows no field of it.
func (e *Engine) SetNode(ctx context.Context, name string, p engine.NodePatch) (engine.NodeView, error) {
	if err := e.begin(ctx, "SetNode", struct {
		Node  string           `json:"node"`
		Patch engine.NodePatch `json:"patch"`
	}{name, p}); err != nil {
		return engine.NodeView{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i, err := e.nodeAt(name)
	if err != nil {
		return engine.NodeView{}, err
	}
	return e.state.Nodes[i], nil
}

// AgentView is the agent of this node, as the state has it: on a cluster
// the controller's own agent, at the epoch of the lease.
func (e *Engine) AgentView() (agentproto.AgentView, error) {
	e.record(context.Background(), "AgentView", noArgs)
	e.mu.Lock()
	defer e.mu.Unlock()
	v := agentproto.AgentView{Node: e.state.Node, Kind: pki.KindHost, Controller: e.state.Node + " (this node)", Problems: []string{},
		Epoch: agentproto.Epoch{InstallID: "abc123"}, Connectors: len(e.state.Connectors)}
	if l := e.state.Lease; l != nil {
		v.Epoch.Generation, v.Epoch.Nonce = l.Generation, l.Nonce
		v.Seq, v.SeqAge, v.HeartbeatAge = 42, 3*time.Second, time.Second
	}
	if len(e.state.Nodes) > 0 {
		v.CertFingerprint = EnrolledFingerprint(e.state.Node)
		for _, n := range e.state.Nodes {
			if n.Name == e.state.Node {
				v.CertExpires = n.CertExpires
			}
		}
		for _, r := range e.state.Routes {
			if r.State == "active" {
				v.EgressTargets++
			}
		}
		ready := map[string]int{}
		for _, t := range e.state.Tunnels {
			if t.ID == "" || t.Kind == engine.TunnelPinned {
				continue
			}
			for _, n := range e.state.Nodes {
				if n.ReplicaReady {
					ready[t.ID]++
				}
			}
		}
		v.ReadyReplicas = ready
	}
	return v, nil
}

// ResetAgentEpoch has nothing kept to forget.
func (e *Engine) ResetAgentEpoch(ctx context.Context) error {
	return e.begin(ctx, "ResetAgentEpoch", noArgs)
}
