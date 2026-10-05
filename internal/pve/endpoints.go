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
	rows, err := c.guestRows(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(rows))
	for _, row := range rows {
		out = append(out, Resource{
			Kind:     model.GuestKind(row.Type),
			VMID:     row.VMID,
			Name:     row.Name,
			Node:     row.Node,
			Status:   row.Status,
			Template: bool(row.Template),
			Tags:     SplitTags(row.Tags),
			Pool:     row.Pool,
		})
	}
	return out, nil
}

// guestRows reads the guest rows of the cluster resources, each with a valid
// vmid and node.
func (c *Client) guestRows(ctx context.Context) ([]resourceWire, error) {
	var rows []resourceWire
	if err := c.get(ctx, "cluster/resources", url.Values{"type": {"vm"}}, &rows); err != nil {
		return nil, fmt.Errorf("fetching cluster resources: %w", err)
	}
	var out []resourceWire
	for i, row := range rows {
		kind := model.GuestKind(row.Type)
		if kind != model.KindQEMU && kind != model.KindLXC {
			continue
		}
		if row.VMID < 1 || row.Node == "" {
			return nil, fmt.Errorf("fetching cluster resources: row %d (%s) has no valid vmid or node", i, row.Type)
		}
		out = append(out, row)
	}
	return out, nil
}

// GuestConfig returns the configuration of a guest on node: the values it
// runs with, not the pending view the endpoint answers with by default, in
// which a NIC whose removal waits for the next start is already gone.
func (c *Client) GuestConfig(ctx context.Context, node string, ref model.GuestRef) (GuestConfig, error) {
	endpoint, err := guestEndpoint(node, ref.Kind, ref.VMID, "config")
	if err != nil {
		return GuestConfig{}, err
	}
	var raw map[string]json.RawMessage
	if err := c.get(ctx, endpoint, url.Values{"current": {"1"}}, &raw); err != nil {
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
	if len(cfg.Values) == 0 && cfg.Digest == "" {
		return GuestConfig{}, fmt.Errorf("fetching config of %s: unexpected response: empty configuration", ref)
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
	if w.Result == nil {
		return nil, fmt.Errorf("fetching agent interfaces of qemu/%d: unexpected response: no result", vmid)
	}
	out := make([]GuestIface, 0, len(w.Result))
	for _, row := range w.Result {
		iface := GuestIface{Name: row.Name, MAC: lenientMAC(row.MAC)}
		for _, a := range row.IPAddresses {
			if a.Type != "ipv4" {
				continue
			}
			addr, isV4, err := parseAddr(a.Address)
			if err != nil || !isV4 {
				return nil, fmt.Errorf("agent interface %q of qemu/%d: invalid ipv4 address %q", row.Name, vmid, a.Address)
			}
			iface.Addrs = append(iface.Addrs, addr)
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
		if row.Inet != "" {
			prefix, isV4, err := parsePrefix(row.Inet)
			if err != nil {
				return nil, fmt.Errorf("interface %q of lxc/%d: invalid inet %q", row.Name, vmid, row.Inet)
			}
			if isV4 {
				iface.Addrs = []netip.Addr{prefix.Addr()}
			}
		}
		out = append(out, iface)
	}
	return out, nil
}

// ClusterNodes lists the nodes of the cluster, or just the local node of a
// standalone host.
func (c *Client) ClusterNodes(ctx context.Context) ([]ClusterNode, error) {
	status, err := c.ClusterStatus(ctx)
	if err != nil {
		return nil, err
	}
	return status.Nodes, nil
}

// ClusterStatus returns the nodes of the cluster and whether it is quorate.
// The quorum is the one of the entry of type cluster; a standalone node has
// no such entry and is quorate.
func (c *Client) ClusterStatus(ctx context.Context) (ClusterStatus, error) {
	var rows []clusterEntryWire
	if err := c.get(ctx, "cluster/status", nil, &rows); err != nil {
		return ClusterStatus{}, fmt.Errorf("fetching cluster status: %w", err)
	}
	status := ClusterStatus{Quorate: true}
	for _, row := range rows {
		switch row.Type {
		case "cluster":
			status.Quorate = bool(row.Quorate)
		case "node":
			node := ClusterNode{Name: row.Name, Online: bool(row.Online), Local: bool(row.Local)}
			if row.IP != "" {
				addr, isV4, err := parseAddr(row.IP)
				if err != nil {
					return ClusterStatus{}, fmt.Errorf("cluster node %q: invalid ip %q", row.Name, row.IP)
				}
				if isV4 {
					node.Addr = addr
				}
			}
			status.Nodes = append(status.Nodes, node)
		}
	}
	if len(status.Nodes) == 0 {
		return ClusterStatus{}, errors.New("fetching cluster status: unexpected response: no nodes")
	}
	return status, nil
}

// NodeNetwork lists the network interfaces of a node.
func (c *Client) NodeNetwork(ctx context.Context, node string) ([]NodeIface, error) {
	endpoint, err := nodeEndpoint(node, "network")
	if err != nil {
		return nil, err
	}
	var rows []nodeIfaceWire
	if err := c.get(ctx, endpoint, nil, &rows); err != nil {
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
		if row.CIDR != "" {
			prefix, isV4, err := parsePrefix(row.CIDR)
			if err != nil {
				return nil, fmt.Errorf("interface %q of node %s: invalid cidr %q", row.Name, node, row.CIDR)
			}
			if isV4 {
				iface.Addrs = []netip.Prefix{prefix}
			}
		}
		if row.Gateway != "" {
			addr, isV4, err := parseAddr(row.Gateway)
			if err != nil {
				return nil, fmt.Errorf("interface %q of node %s: invalid gateway %q", row.Name, node, row.Gateway)
			}
			if isV4 {
				iface.Gateway = addr
			}
		}
		out = append(out, iface)
	}
	return out, nil
}

// guestEndpoint builds nodes/<node>/<kind>/<vmid>/<tail>.
func guestEndpoint(node string, kind model.GuestKind, vmid int, tail string) (string, error) {
	if kind != model.KindQEMU && kind != model.KindLXC {
		return "", fmt.Errorf("invalid guest kind %q", kind)
	}
	if vmid < 1 {
		return "", fmt.Errorf("invalid vmid %d", vmid)
	}
	return nodeEndpoint(node, fmt.Sprintf("%s/%d/%s", kind, vmid, tail))
}

// nodeEndpoint builds nodes/<node>/<tail>. Node names come from API data and
// end up in a URL path, so they are checked first.
func nodeEndpoint(node, tail string) (string, error) {
	if !validSegment(node) {
		return "", fmt.Errorf("invalid node name %q", node)
	}
	return "nodes/" + node + "/" + tail, nil
}

// validSegment accepts host-name characters only, which keeps a name from API
// data (a node, a group, a vnet) from reaching outside its path segment.
func validSegment(name string) bool {
	if name == "" || strings.Trim(name, ".") == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return false
		}
	}
	return true
}
