package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Version returns the Proxmox VE release.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var w versionWire
	if err := c.get(ctx, "version", nil, &w); err != nil {
		return Version{}, fmt.Errorf("fetching version: %w", err)
	}
	return parseVersion(w.Release, w.Version)
}

// Resources lists every VM and container in the cluster. Rows that are
// neither are skipped.
func (c *Client) Resources(ctx context.Context) ([]Resource, error) {
	var rows []resourceWire
	if err := c.get(ctx, "cluster/resources", url.Values{"type": {"vm"}}, &rows); err != nil {
		return nil, fmt.Errorf("fetching cluster resources: %w", err)
	}
	var out []Resource
	for _, row := range rows {
		kind := model.GuestKind(row.Type)
		if kind != model.KindQEMU && kind != model.KindLXC {
			continue
		}
		out = append(out, Resource{
			Kind:     kind,
			VMID:     row.VMID,
			Name:     row.Name,
			Node:     row.Node,
			Status:   row.Status,
			Template: bool(row.Template),
			Tags:     splitTags(row.Tags),
		})
	}
	return out, nil
}

// GuestConfig returns the configuration of a guest on node.
func (c *Client) GuestConfig(ctx context.Context, node string, ref model.GuestRef) (GuestConfig, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "config")
	if err != nil {
		return GuestConfig{}, err
	}
	var raw map[string]json.RawMessage
	if err := c.get(ctx, endpoint, nil, &raw); err != nil {
		return GuestConfig{}, fmt.Errorf("fetching config of %s: %w", ref, err)
	}
	cfg := GuestConfig{Values: make(map[string]string, len(raw))}
	for key, value := range raw {
		text, ok := stringify(value)
		if !ok {
			continue
		}
		if key == "digest" {
			cfg.Digest = text
			continue
		}
		cfg.Values[key] = text
	}
	return cfg, nil
}

// AgentInterfaces asks the QEMU guest agent of a VM for its interfaces. It
// returns ErrAgentUnavailable when the agent cannot answer.
func (c *Client) AgentInterfaces(ctx context.Context, node string, vmid int) ([]GuestIface, error) {
	endpoint, err := guestEndpoint(node, model.KindQEMU, vmid, "agent/network-get-interfaces")
	if err != nil {
		return nil, err
	}
	var w agentWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		if isAgentUnavailable(err) {
			return nil, fmt.Errorf("fetching agent interfaces of qemu/%d: %w: %w", vmid, ErrAgentUnavailable, err)
		}
		return nil, fmt.Errorf("fetching agent interfaces of qemu/%d: %w", vmid, err)
	}
	out := make([]GuestIface, 0, len(w.Result))
	for _, row := range w.Result {
		iface := GuestIface{Name: row.Name, MAC: lenientMAC(row.MAC)}
		for _, a := range row.IPAddresses {
			if a.Type != "ipv4" {
				continue
			}
			if addr, ok := parseIPv4(a.Address); ok {
				iface.Addrs = append(iface.Addrs, addr)
			}
		}
		out = append(out, iface)
	}
	return out, nil
}

// isAgentUnavailable recognises the 500 answers Proxmox gives when the guest
// agent is not configured, not running, or the VM is stopped.
func isAgentUnavailable(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		return false
	}
	return containsFold(apiErr.Message, "guest agent") || containsFold(apiErr.Message, "is not running")
}

// LXCInterfaces returns the interfaces of a running container.
func (c *Client) LXCInterfaces(ctx context.Context, node string, vmid int) ([]GuestIface, error) {
	endpoint, err := guestEndpoint(node, model.KindLXC, vmid, "interfaces")
	if err != nil {
		return nil, err
	}
	var rows []lxcIfaceWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching interfaces of lxc/%d: %w", vmid, err)
	}
	out := make([]GuestIface, 0, len(rows))
	for _, row := range rows {
		iface := GuestIface{Name: row.Name, MAC: lenientMAC(row.MAC)}
		if prefix, ok := parseIPv4Prefix(row.Inet); ok {
			iface.Addrs = []netip.Addr{prefix.Addr()}
		}
		out = append(out, iface)
	}
	return out, nil
}

// ClusterNodes lists the nodes of the cluster, or just the local node of a
// standalone host.
func (c *Client) ClusterNodes(ctx context.Context) ([]ClusterNode, error) {
	var rows []clusterEntryWire
	if err := c.get(ctx, "cluster/status", nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching cluster status: %w", err)
	}
	var out []ClusterNode
	for _, row := range rows {
		if row.Type != "node" {
			continue
		}
		node := ClusterNode{Name: row.Name, Online: bool(row.Online), Local: bool(row.Local)}
		if addr, ok := parseIPv4(row.IP); ok {
			node.Addr = addr
		}
		out = append(out, node)
	}
	return out, nil
}

// NodeNetwork lists the network interfaces of a node.
func (c *Client) NodeNetwork(ctx context.Context, node string) ([]NodeIface, error) {
	if !validNode(node) {
		return nil, fmt.Errorf("invalid node name %q", node)
	}
	var rows []nodeIfaceWire
	if err := c.get(ctx, "nodes/"+node+"/network", nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching network of node %s: %w", node, err)
	}
	out := make([]NodeIface, 0, len(rows))
	for _, row := range rows {
		iface := NodeIface{
			Name:   row.Name,
			Type:   row.Type,
			Active: bool(row.Active),
			Ports:  fields(row.BridgePorts),
		}
		if prefix, ok := parseIPv4Prefix(row.CIDR); ok {
			iface.Addrs = []netip.Prefix{prefix}
		}
		out = append(out, iface)
	}
	return out, nil
}

// guestEndpoint builds nodes/<node>/<kind>/<vmid>/<tail>. Node names come from
// API data and end up in a URL path, so they are checked first.
func guestEndpoint(node string, kind model.GuestKind, vmid int, tail string) (string, error) {
	if !validNode(node) {
		return "", fmt.Errorf("invalid node name %q", node)
	}
	if kind != model.KindQEMU && kind != model.KindLXC {
		return "", fmt.Errorf("invalid guest kind %q", kind)
	}
	if vmid < 1 {
		return "", fmt.Errorf("invalid vmid %d", vmid)
	}
	return fmt.Sprintf("nodes/%s/%s/%d/%s", node, kind, vmid, tail), nil
}

// validNode accepts host-name characters only, which keeps a node name from
// reaching outside its path segment.
func validNode(node string) bool {
	if node == "" || strings.Trim(node, ".") == "" {
		return false
	}
	for _, r := range node {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}
