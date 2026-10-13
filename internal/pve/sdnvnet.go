package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// SDNVnetDetail is one vnet of the SDN with its zone and its subnets, each
// as configured with what is pending.
type SDNVnetDetail struct {
	Vnet    SDNVnet
	Zone    SDNZone
	Subnets []SDNSubnet
	// Found says that the SDN has the vnet, applied or not.
	Found bool
}

// SDNVnet reads one vnet, its zone and its subnets, with what is pending: a
// fixed three calls, however many vnets the cluster has. A vnet that is not
// there is Found false and no error; a zone that is not there, or that the
// token may not audit, is left zero.
//
// The vnet and its zone are picked from the listings, which take SDN.Audit
// on the object; the single zone takes SDN.Allocate on it, which a token
// that only reads the SDN does not hold.
func (c *Client) SDNVnet(ctx context.Context, vnet string) (SDNVnetDetail, error) {
	if !validSegment(vnet) {
		return SDNVnetDetail{}, fmt.Errorf("invalid vnet name %q", vnet)
	}
	pending := url.Values{"pending": {"1"}}
	var raw json.RawMessage
	if err := c.get(ctx, "cluster/sdn/vnets", pending, &raw); err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn vnet %s: %w", vnet, err)
	}
	vnets, err := DecodeSDNVnets(raw)
	if err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn vnet %s: %w", vnet, err)
	}
	v, n := pick(vnets, func(v SDNVnet) bool { return v.Name == vnet })
	switch {
	case n == 0:
		return SDNVnetDetail{}, nil
	case n > 1:
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn vnet %s: unexpected response: %d vnets of that name", vnet, n)
	}
	out := SDNVnetDetail{Vnet: v, Found: true}
	zone := v.Zone
	if err := c.get(ctx, "cluster/sdn/zones", pending, &raw); err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn zone %s of vnet %s: %w", zone, vnet, err)
	}
	zones, err := DecodeSDNZones(raw)
	if err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn zone %s of vnet %s: %w", zone, vnet, err)
	}
	z, n := pick(zones, func(z SDNZone) bool { return z.Name == zone })
	if n > 1 {
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn zone %s of vnet %s: unexpected response: %d zones of that name", zone, vnet, n)
	}
	out.Zone = z
	if err := c.get(ctx, "cluster/sdn/vnets/"+vnet+"/subnets", pending, &raw); err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching the subnets of vnet %s: %w", vnet, err)
	}
	if out.Subnets, err = DecodeSDNSubnets(raw); err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching the subnets of vnet %s: %w", vnet, err)
	}
	return out, nil
}

// pick is the one item that match picks, and how many it picks.
func pick[T any](items []T, match func(T) bool) (T, int) {
	var (
		found T
		n     int
	)
	for _, item := range items {
		if match(item) {
			found = item
			n++
		}
	}
	return found, n
}
