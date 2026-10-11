package pve

import (
	"context"
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// NodeFirewallOptions is the part of the firewall options of a node the
// managed network reads: whether the firewall is on, and whether it runs on
// the nftables backend, which installs no guest rules.
type NodeFirewallOptions struct {
	Enabled  bool
	NFTables bool
}

// ClusterFirewallOptions is the part of the firewall options of the
// datacenter the managed network reads: without it no guest rule is in
// force.
type ClusterFirewallOptions struct {
	Enabled bool
}

// GuestFirewallOptions is the part of the firewall options of a guest the
// managed network reads.
type GuestFirewallOptions struct {
	Enabled bool
}

type nodeFirewallWire struct {
	Enable   flexBool `json:"enable"`
	NFTables flexBool `json:"nftables"`
}

type serviceStateWire struct {
	State string `json:"state"`
}

// NodeFirewallOptions reads the firewall options of a node. An absent
// option is off: a firewall never configured, or the iptables backend.
func (c *Client) NodeFirewallOptions(ctx context.Context, node string) (NodeFirewallOptions, error) {
	endpoint, err := nodeEndpoint(node, "firewall/options")
	if err != nil {
		return NodeFirewallOptions{}, err
	}
	var w nodeFirewallWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		return NodeFirewallOptions{}, fmt.Errorf("fetching the firewall options of node %s: %w", node, err)
	}
	return NodeFirewallOptions{Enabled: bool(w.Enable), NFTables: bool(w.NFTables)}, nil
}

// ClusterFirewallOptions reads the firewall options of the datacenter.
func (c *Client) ClusterFirewallOptions(ctx context.Context) (ClusterFirewallOptions, error) {
	o, err := c.firewall(ctx, "cluster/firewall/options", "the datacenter")
	if err != nil {
		return ClusterFirewallOptions{}, err
	}
	return ClusterFirewallOptions{Enabled: o.Enabled}, nil
}

// GuestFirewallOptions reads the firewall options of a guest on node.
func (c *Client) GuestFirewallOptions(ctx context.Context, node string, ref model.GuestRef) (GuestFirewallOptions, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "firewall/options")
	if err != nil {
		return GuestFirewallOptions{}, err
	}
	o, err := c.firewall(ctx, endpoint, ref.String())
	if err != nil {
		return GuestFirewallOptions{}, err
	}
	return GuestFirewallOptions{Enabled: o.Enabled}, nil
}

// NodeServiceState reads the state of a service of a node as systemd
// reports it: "running", "stopped", and the others of systemctl.
func (c *Client) NodeServiceState(ctx context.Context, node, name string) (string, error) {
	if !validSegment(name) {
		return "", fmt.Errorf("invalid service name %q", name)
	}
	endpoint, err := nodeEndpoint(node, "services/"+name+"/state")
	if err != nil {
		return "", err
	}
	var w serviceStateWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		return "", fmt.Errorf("fetching the state of %s on node %s: %w", name, node, err)
	}
	if w.State == "" {
		return "", fmt.Errorf("fetching the state of %s on node %s: unexpected response: no state", name, node)
	}
	return w.State, nil
}
