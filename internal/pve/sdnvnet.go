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
// there is Found false and no error.
func (c *Client) SDNVnet(ctx context.Context, vnet string) (SDNVnetDetail, error) {
	if !validSegment(vnet) {
		return SDNVnetDetail{}, fmt.Errorf("invalid vnet name %q", vnet)
	}
	pending := url.Values{"pending": {"1"}}
	var raw json.RawMessage
	err := c.get(ctx, "cluster/sdn/vnets/"+vnet, pending, &raw)
	switch {
	case IsNotFound(err):
		return SDNVnetDetail{}, nil
	case err != nil:
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn vnet %s: %w", vnet, err)
	}
	vnets, err := DecodeSDNVnets(asList(raw))
	if err != nil || len(vnets) != 1 {
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn vnet %s: %w", vnet, cmpErr(err, "not one vnet"))
	}
	out := SDNVnetDetail{Vnet: vnets[0], Found: true}
	zone := out.Vnet.Zone
	err = c.get(ctx, "cluster/sdn/zones/"+zone, pending, &raw)
	switch {
	case IsNotFound(err):
	case err != nil:
		return SDNVnetDetail{}, fmt.Errorf("fetching sdn zone %s of vnet %s: %w", zone, vnet, err)
	default:
		zones, err := DecodeSDNZones(asList(raw))
		if err != nil || len(zones) != 1 {
			return SDNVnetDetail{}, fmt.Errorf("fetching sdn zone %s of vnet %s: %w", zone, vnet, cmpErr(err, "not one zone"))
		}
		out.Zone = zones[0]
	}
	if err := c.get(ctx, "cluster/sdn/vnets/"+vnet+"/subnets", pending, &raw); err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching the subnets of vnet %s: %w", vnet, err)
	}
	if out.Subnets, err = DecodeSDNSubnets(raw); err != nil {
		return SDNVnetDetail{}, fmt.Errorf("fetching the subnets of vnet %s: %w", vnet, err)
	}
	return out, nil
}

// asList is the JSON of one object as a list of it, which the decoders of
// the listings read.
func asList(raw json.RawMessage) []byte {
	return append(append([]byte("["), raw...), ']')
}

// cmpErr is err, or one that says what was wrong when the decoding went
// through and found the wrong number.
func cmpErr(err error, what string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected response: %s", what)
}
