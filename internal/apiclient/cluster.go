package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// ClusterStatus returns the cluster as the controller's last cycle found it.
func (c *Client) ClusterStatus(ctx context.Context) (engine.ClusterStatus, error) {
	var v engine.ClusterStatus
	err := c.call(ctx, c.short, http.MethodGet, "/v1/cluster", nil, &v)
	return v, err
}

// ClusterStatusRaw is ClusterStatus as the daemon sent it.
func (c *Client) ClusterStatusRaw(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, c.short, "/v1/cluster")
}

// Nodes returns the nodes of the registry as the last cycle found them.
func (c *Client) Nodes(ctx context.Context) ([]engine.NodeView, error) {
	var nodes []engine.NodeView
	err := c.call(ctx, c.short, http.MethodGet, "/v1/nodes", nil, &nodes)
	return nodes, err
}

// NodesRaw is Nodes as the daemon sent it.
func (c *Client) NodesRaw(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, c.short, "/v1/nodes")
}

func nodePath(name, action string) string {
	p := "/v1/nodes/" + url.PathEscape(name)
	if action != "" {
		p += "/" + action
	}
	return p
}

// AdmitNode admits a node that joins: an appliance agent by the fingerprint
// of its certificate, a node that stays joining because too many routes
// would move. renew lets an appliance agent renew an expired certificate once.
func (c *Client) AdmitNode(ctx context.Context, name, fingerprint string, renew bool) (engine.NodeView, error) {
	body := struct {
		Fingerprint string `json:"fingerprint,omitempty"`
		Renew       bool   `json:"renew,omitempty"`
	}{fingerprint, renew}
	var v engine.NodeView
	err := c.call(ctx, c.long, http.MethodPost, nodePath(name, "admit"), body, &v)
	return v, err
}

// DrainNode takes a node out of the shared tunnel's replicas; force drains
// the last one.
func (c *Client) DrainNode(ctx context.Context, name string, force bool) (engine.NodeView, error) {
	body := struct {
		Force bool `json:"force,omitempty"`
	}{force}
	var v engine.NodeView
	err := c.call(ctx, c.long, http.MethodPost, nodePath(name, "drain"), body, &v)
	return v, err
}

// UndrainNode ends an admin's drain.
func (c *Client) UndrainNode(ctx context.Context, name string) (engine.NodeView, error) {
	var v engine.NodeView
	err := c.call(ctx, c.long, http.MethodPost, nodePath(name, "undrain"), nil, &v)
	return v, err
}

// ForgetNode removes a node that is gone from the install; compromised has
// every secret of the install rotated after it.
func (c *Client) ForgetNode(ctx context.Context, name string, compromised bool) (engine.ForgetResult, error) {
	body := struct {
		Compromised bool `json:"compromised,omitempty"`
	}{compromised}
	var res engine.ForgetResult
	err := c.call(ctx, c.long, http.MethodPost, nodePath(name, "forget"), body, &res)
	return res, err
}

// SetNode changes what the admin may of a node's entry.
func (c *Client) SetNode(ctx context.Context, name string, p engine.NodePatch) (engine.NodeView, error) {
	var v engine.NodeView
	err := c.call(ctx, c.long, http.MethodPut, nodePath(name, ""), p, &v)
	return v, err
}

// JoinToken makes the token an appliance agent of node enrols with. The
// answer is the one copy of it.
func (c *Client) JoinToken(ctx context.Context, node string) (engine.JoinTokenView, error) {
	body := struct {
		Node string `json:"node"`
	}{node}
	var v engine.JoinTokenView
	err := c.call(ctx, c.long, http.MethodPost, "/v1/cluster/join-tokens", body, &v)
	return v, err
}

// FinishUpgrade ends an upgrade from an earlier release.
func (c *Client) FinishUpgrade(ctx context.Context) (engine.FinishUpgradeResult, error) {
	var res engine.FinishUpgradeResult
	err := c.call(ctx, c.long, http.MethodPost, "/v1/cluster/finish-upgrade", nil, &res)
	return res, err
}

// Unblock ends the block of an address every node blocks.
func (c *Client) Unblock(ctx context.Context, addr netip.Addr) error {
	body := struct {
		Address string `json:"address"`
	}{addr.String()}
	return c.call(ctx, c.long, http.MethodPost, "/v1/cluster/unblock", body, nil)
}

// UninstallCluster takes every other node out of the install, for pco
// uninstall --cluster. Only root may ask.
func (c *Client) UninstallCluster(ctx context.Context, force bool) (engine.UninstallResult, error) {
	body := struct {
		Force bool `json:"force,omitempty"`
	}{force}
	var res engine.UninstallResult
	err := c.call(ctx, c.long, http.MethodPost, "/v1/cluster/uninstall", body, &res)
	return res, err
}

// AgentStatus returns the agent of this node as it sees itself, whatever
// the node's role.
func (c *Client) AgentStatus(ctx context.Context) (agentproto.AgentView, error) {
	var v agentproto.AgentView
	err := c.call(ctx, c.short, http.MethodGet, "/v1/local/agent", nil, &v)
	return v, err
}

// AgentStatusRaw is AgentStatus as the daemon sent it.
func (c *Client) AgentStatusRaw(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, c.short, "/v1/local/agent")
}

// ResetAgentEpoch has the agent of this node forget the epoch it remembers.
// Only root may ask.
func (c *Client) ResetAgentEpoch(ctx context.Context) error {
	return c.call(ctx, c.long, http.MethodPost, "/v1/local/agent/reset-epoch", nil, nil)
}
