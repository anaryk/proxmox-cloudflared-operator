package pve

import (
	"context"
	"fmt"
	"net/netip"
)

// NodeDNS is the resolver configuration of a node.
type NodeDNS struct {
	Servers []netip.Addr // IPv4 only, in the order dns1 to dns3
}

// FirewallOptions is the part of the firewall options pco reads.
type FirewallOptions struct {
	Enabled bool
}

type nodeDNSWire struct {
	DNS1 string `json:"dns1"`
	DNS2 string `json:"dns2"`
	DNS3 string `json:"dns3"`
}

type firewallWire struct {
	Enable flexBool `json:"enable"`
}

// NodeDNS returns the resolvers a node is configured with.
func (c *Client) NodeDNS(ctx context.Context, node string) (NodeDNS, error) {
	endpoint, err := nodeEndpoint(node, "dns")
	if err != nil {
		return NodeDNS{}, err
	}
	var w nodeDNSWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		return NodeDNS{}, fmt.Errorf("fetching the dns of node %s: %w", node, err)
	}
	var out NodeDNS
	for i, server := range []string{w.DNS1, w.DNS2, w.DNS3} {
		if server == "" {
			continue
		}
		addr, isV4, err := parseAddr(server)
		if err != nil {
			return NodeDNS{}, fmt.Errorf("dns of node %s: invalid dns%d %q", node, i+1, server)
		}
		if isV4 {
			out.Servers = append(out.Servers, addr)
		}
	}
	return out, nil
}

// DatacenterFirewall returns the firewall options of the datacenter.
func (c *Client) DatacenterFirewall(ctx context.Context) (FirewallOptions, error) {
	return c.firewall(ctx, "cluster/firewall/options", "the datacenter")
}

// NodeFirewall returns the firewall options of a node.
func (c *Client) NodeFirewall(ctx context.Context, node string) (FirewallOptions, error) {
	endpoint, err := nodeEndpoint(node, "firewall/options")
	if err != nil {
		return FirewallOptions{}, err
	}
	return c.firewall(ctx, endpoint, "node "+node)
}

// firewall reads options in which an absent enable, a firewall never
// configured, means off.
func (c *Client) firewall(ctx context.Context, endpoint, of string) (FirewallOptions, error) {
	var w firewallWire
	if err := c.get(ctx, endpoint, nil, &w); err != nil {
		return FirewallOptions{}, fmt.Errorf("fetching the firewall options of %s: %w", of, err)
	}
	return FirewallOptions{Enabled: bool(w.Enable)}, nil
}
