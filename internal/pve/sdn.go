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

// The states of an object of the SDN whose change is not applied yet, as the
// pending view of Proxmox names them.
const (
	SDNNew     = "new"
	SDNChanged = "changed"
	SDNDeleted = "deleted"
)

// SDNZone is a zone of the SDN as configured, with the state of a change that
// is not applied yet.
type SDNZone struct {
	Name  string
	Type  string // simple, vlan, qinq, vxlan, evpn
	State string // SDNNew, SDNChanged or SDNDeleted while not applied; "" when applied
}

// SDNVnet is a vnet of the SDN as configured.
type SDNVnet struct {
	Name         string
	Zone         string
	IsolatePorts bool // its ports do not reach each other, only the bridge's uplink and the bridge itself
	VLANAware    bool
	State        string
}

// SDNSubnet is a subnet of a vnet as configured.
type SDNSubnet struct {
	Vnet  string
	CIDR  string // IPv4 or IPv6
	State string
}

// SDN is the configuration of the SDN with what is not applied yet.
type SDN struct {
	Zones   []SDNZone
	Vnets   []SDNVnet
	Subnets []SDNSubnet
}

// Pending names every object whose change is not applied, with its state.
func (s SDN) Pending() []string {
	var out []string
	for _, z := range s.Zones {
		if z.State != "" {
			out = append(out, fmt.Sprintf("zone %s (%s)", z.Name, z.State))
		}
	}
	for _, v := range s.Vnets {
		if v.State != "" {
			out = append(out, fmt.Sprintf("vnet %s (%s)", v.Name, v.State))
		}
	}
	for _, n := range s.Subnets {
		if n.State != "" {
			out = append(out, fmt.Sprintf("subnet %s of vnet %s (%s)", n.CIDR, n.Vnet, n.State))
		}
	}
	return out
}

// sdnRow is an object of the SDN as the API lists it: its applied values,
// and with pending=1 the state of its change and the values that change
// pending, one pending as "deleted" being removed.
type sdnRow struct {
	values  map[string]json.RawMessage
	pending map[string]json.RawMessage
}

func decodeSDNRows(data []byte, of string) ([]sdnRow, error) {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decoding the sdn %s: %w", of, err)
	}
	rows := make([]sdnRow, 0, len(raw))
	for _, values := range raw {
		row := sdnRow{values: values}
		if p, ok := values["pending"]; ok && string(p) != "null" {
			if err := json.Unmarshal(p, &row.pending); err != nil {
				return nil, fmt.Errorf("decoding the sdn %s: pending: %w", of, err)
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// text is the value of key as it will be once applied.
func (r sdnRow) text(key string) string {
	if v, ok := r.pending[key]; ok {
		s, _ := stringify(v)
		if s == SDNDeleted {
			return ""
		}
		return s
	}
	s, _ := stringify(r.values[key])
	return s
}

func (r sdnRow) flag(key string) bool {
	switch r.text(key) {
	case "1", "true":
		return true
	}
	return false
}

// state is the state of the row's change, which is never pending itself.
func (r sdnRow) state() (string, error) {
	s, _ := stringify(r.values["state"])
	switch s {
	case "", SDNNew, SDNChanged, SDNDeleted:
		return s, nil
	}
	return "", fmt.Errorf("state %q: want %s, %s or %s", s, SDNNew, SDNChanged, SDNDeleted)
}

// name is the value of key, which names an object and goes into a path.
func (r sdnRow) name(key string) (string, error) {
	s := r.text(key)
	if !validSegment(s) {
		return "", fmt.Errorf("%s %q is no name", key, s)
	}
	return s, nil
}

// DecodeSDNZones reads the zones as /cluster/sdn/zones lists them, with or
// without pending=1, without the envelope of the API.
func DecodeSDNZones(data []byte) ([]SDNZone, error) {
	rows, err := decodeSDNRows(data, "zones")
	if err != nil {
		return nil, err
	}
	out := make([]SDNZone, 0, len(rows))
	for i, row := range rows {
		name, err := row.name("zone")
		if err != nil {
			return nil, fmt.Errorf("sdn zone %d: %w", i, err)
		}
		state, err := row.state()
		if err != nil {
			return nil, fmt.Errorf("sdn zone %s: %w", name, err)
		}
		out = append(out, SDNZone{Name: name, Type: row.text("type"), State: state})
	}
	return out, nil
}

// DecodeSDNVnets reads the vnets as /cluster/sdn/vnets lists them, with or
// without pending=1, without the envelope of the API.
func DecodeSDNVnets(data []byte) ([]SDNVnet, error) {
	rows, err := decodeSDNRows(data, "vnets")
	if err != nil {
		return nil, err
	}
	out := make([]SDNVnet, 0, len(rows))
	for i, row := range rows {
		name, err := row.name("vnet")
		if err != nil {
			return nil, fmt.Errorf("sdn vnet %d: %w", i, err)
		}
		zone, err := row.name("zone")
		if err != nil {
			return nil, fmt.Errorf("sdn vnet %s: %w", name, err)
		}
		state, err := row.state()
		if err != nil {
			return nil, fmt.Errorf("sdn vnet %s: %w", name, err)
		}
		out = append(out, SDNVnet{Name: name, Zone: zone, IsolatePorts: row.flag("isolate-ports"), VLANAware: row.flag("vlanaware"), State: state})
	}
	return out, nil
}

// DecodeSDNSubnets reads the subnets of a vnet as
// /cluster/sdn/vnets/<vnet>/subnets lists them, with or without pending=1,
// without the envelope of the API.
func DecodeSDNSubnets(data []byte) ([]SDNSubnet, error) {
	rows, err := decodeSDNRows(data, "subnets")
	if err != nil {
		return nil, err
	}
	out := make([]SDNSubnet, 0, len(rows))
	for i, row := range rows {
		vnet, err := row.name("vnet")
		if err != nil {
			return nil, fmt.Errorf("sdn subnet %d: %w", i, err)
		}
		cidr := row.text("cidr")
		if _, err := netip.ParsePrefix(cidr); err != nil {
			return nil, fmt.Errorf("sdn subnet %d of vnet %s: cidr %q", i, vnet, cidr)
		}
		state, err := row.state()
		if err != nil {
			return nil, fmt.Errorf("sdn subnet %s of vnet %s: %w", cidr, vnet, err)
		}
		out = append(out, SDNSubnet{Vnet: vnet, CIDR: cidr, State: state})
	}
	return out, nil
}

// SDNVnets lists the vnets as configured, applied or not.
func (c *Client) SDNVnets(ctx context.Context) ([]SDNVnet, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "cluster/sdn/vnets", nil, &raw); err != nil {
		return nil, fmt.Errorf("fetching sdn vnets: %w", err)
	}
	vnets, err := DecodeSDNVnets(raw)
	if err != nil {
		return nil, fmt.Errorf("fetching sdn vnets: %w", err)
	}
	return vnets, nil
}

// SDNPending reads the zones, the vnets and the subnets of every vnet as
// configured, each with the state of a change that is not applied yet.
func (c *Client) SDNPending(ctx context.Context) (SDN, error) {
	pending := url.Values{"pending": {"1"}}
	var s SDN
	var raw json.RawMessage
	if err := c.get(ctx, "cluster/sdn/zones", pending, &raw); err != nil {
		return SDN{}, fmt.Errorf("fetching sdn zones: %w", err)
	}
	var err error
	if s.Zones, err = DecodeSDNZones(raw); err != nil {
		return SDN{}, fmt.Errorf("fetching sdn zones: %w", err)
	}
	if err := c.get(ctx, "cluster/sdn/vnets", pending, &raw); err != nil {
		return SDN{}, fmt.Errorf("fetching sdn vnets: %w", err)
	}
	if s.Vnets, err = DecodeSDNVnets(raw); err != nil {
		return SDN{}, fmt.Errorf("fetching sdn vnets: %w", err)
	}
	for _, v := range s.Vnets {
		if err := c.get(ctx, "cluster/sdn/vnets/"+v.Name+"/subnets", pending, &raw); err != nil {
			return SDN{}, fmt.Errorf("fetching the subnets of vnet %s: %w", v.Name, err)
		}
		subnets, err := DecodeSDNSubnets(raw)
		if err != nil {
			return SDN{}, fmt.Errorf("fetching the subnets of vnet %s: %w", v.Name, err)
		}
		s.Subnets = append(s.Subnets, subnets...)
	}
	return s, nil
}

// SDNLock takes the lock of the SDN configuration, also while changes are
// pending, and returns its token, which every change and the apply made under
// it carry. It takes SDN.Allocate on /sdn; Proxmox VE 8 has no such lock.
func (c *Client) SDNLock(ctx context.Context) (string, error) {
	var token string
	if err := c.send(ctx, http.MethodPost, "cluster/sdn/lock", url.Values{"allow-pending": {"1"}}, &token); err != nil {
		return "", fmt.Errorf("taking the sdn lock: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", errors.New("taking the sdn lock: Proxmox answered no lock token")
	}
	return token, nil
}

// SDNUnlock releases the lock of the SDN configuration a token holds.
func (c *Client) SDNUnlock(ctx context.Context, token string) error {
	if token == "" {
		return errors.New("releasing the sdn lock: no lock token")
	}
	if err := c.send(ctx, http.MethodDelete, "cluster/sdn/lock", url.Values{"lock-token": {token}}, nil); err != nil {
		return fmt.Errorf("releasing the sdn lock: %w", err)
	}
	return nil
}
