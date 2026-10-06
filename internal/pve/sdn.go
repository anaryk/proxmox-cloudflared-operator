package pve

import (
	"context"
	"fmt"
	"net/netip"
)

// Subnet is an IPv4 subnet of an SDN vnet.
type Subnet struct {
	Vnet    string
	Prefix  netip.Prefix
	Gateway netip.Addr // zero when the subnet has none
}

// VNet is an SDN vnet and the zone it is in, which the path of its access
// control starts with: /sdn/zones/<zone>/<vnet>.
type VNet struct {
	Name string
	Zone string
}

type vnetWire struct {
	Vnet string `json:"vnet"`
	Zone string `json:"zone"`
}

// VNets lists the SDN vnets as configured, applied or not, with their zones.
func (c *Client) VNets(ctx context.Context) ([]VNet, error) {
	var rows []vnetWire
	if err := c.get(ctx, "cluster/sdn/vnets", nil, &rows); err != nil {
		return nil, fmt.Errorf("fetching sdn vnets: %w", err)
	}
	var out []VNet
	for _, row := range rows {
		if !validSegment(row.Vnet) {
			return nil, fmt.Errorf("fetching sdn vnets: invalid vnet name %q", row.Vnet)
		}
		if !validSegment(row.Zone) {
			return nil, fmt.Errorf("fetching sdn vnets: vnet %s has the invalid zone %q", row.Vnet, row.Zone)
		}
		out = append(out, VNet{Name: row.Vnet, Zone: row.Zone})
	}
	return out, nil
}

type subnetWire struct {
	CIDR    string `json:"cidr"`
	Gateway string `json:"gateway"`
}

// Subnets lists the IPv4 subnets of every SDN vnet as configured, applied or
// not. IPv6 subnets are left out.
func (c *Client) Subnets(ctx context.Context) ([]Subnet, error) {
	var vnets []vnetWire
	if err := c.get(ctx, "cluster/sdn/vnets", nil, &vnets); err != nil {
		return nil, fmt.Errorf("fetching sdn vnets: %w", err)
	}
	var out []Subnet
	for _, v := range vnets {
		if !validSegment(v.Vnet) {
			return nil, fmt.Errorf("fetching sdn vnets: invalid vnet name %q", v.Vnet)
		}
		var rows []subnetWire
		if err := c.get(ctx, "cluster/sdn/vnets/"+v.Vnet+"/subnets", nil, &rows); err != nil {
			return nil, fmt.Errorf("fetching the subnets of vnet %s: %w", v.Vnet, err)
		}
		for _, row := range rows {
			prefix, isV4, err := parsePrefix(row.CIDR)
			if err != nil {
				return nil, fmt.Errorf("subnet of vnet %s: invalid cidr %q", v.Vnet, row.CIDR)
			}
			if !isV4 {
				continue
			}
			subnet := Subnet{Vnet: v.Vnet, Prefix: prefix}
			if row.Gateway != "" {
				if subnet.Gateway, err = netip.ParseAddr(row.Gateway); err != nil || !subnet.Gateway.Is4() {
					return nil, fmt.Errorf("subnet %s of vnet %s: invalid gateway %q", row.CIDR, v.Vnet, row.Gateway)
				}
			}
			out = append(out, subnet)
		}
	}
	return out, nil
}
