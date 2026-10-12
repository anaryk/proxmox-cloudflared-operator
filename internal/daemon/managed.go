package daemon

import (
	"context"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// managedSource is what the step of the managed network reads of Proxmox;
// *pve.Client is one.
type managedSource interface {
	ClusterOptions(ctx context.Context) (pve.ClusterOptions, error)
	vnetSource
	NodeFirewallOptions(ctx context.Context, node string) (pve.NodeFirewallOptions, error)
	ClusterFirewallOptions(ctx context.Context) (pve.ClusterFirewallOptions, error)
	NodeServiceState(ctx context.Context, node, name string) (string, error)
	GuestFirewallOptions(ctx context.Context, node string, ref model.GuestRef) (pve.GuestFirewallOptions, error)
}

var _ managedSource = (*pve.Client)(nil)

// managedReaders fills what the engine reads of Proxmox for the managed
// network on node.
func managedReaders(d *engine.Deps, src managedSource, node string) {
	d.RegisteredTags = func(ctx context.Context) ([]string, error) {
		o, err := src.ClusterOptions(ctx)
		if err != nil {
			return nil, err
		}
		if o.RegisteredTags == nil {
			return []string{}, nil
		}
		return o.RegisteredTags, nil
	}
	d.Vnet = func(ctx context.Context) (engine.ManagedVnet, error) { return managedVnet(ctx, src) }
	d.NodeFirewall = func(ctx context.Context) (pve.NodeFirewallOptions, error) { return src.NodeFirewallOptions(ctx, node) }
	d.ClusterFirewall = src.ClusterFirewallOptions
	d.NodeService = func(ctx context.Context, name string) (string, error) { return src.NodeServiceState(ctx, node, name) }
	d.GuestFirewall = func(ctx context.Context, g model.Guest) (pve.GuestFirewallOptions, error) {
		return src.GuestFirewallOptions(ctx, g.Node, g.Ref)
	}
}

// vnetSource reads one vnet of the SDN with its zone and its subnets;
// *pve.Client is one.
type vnetSource interface {
	SDNVnet(ctx context.Context, vnet string) (pve.SDNVnetDetail, error)
}

// managedVnet reads the vnet of the managed network as configured, with what
// is pending, in a fixed three calls: a vnet that is made and not applied yet
// does not exist, nor one about to go, and a zone or a subnet about to go is
// gone; a subnet not applied yet counts, as it is on its way.
func managedVnet(ctx context.Context, src vnetSource) (engine.ManagedVnet, error) {
	d, err := src.SDNVnet(ctx, gateway.ManagedBridge)
	if err != nil {
		return engine.ManagedVnet{}, err
	}
	switch {
	case !d.Found, d.Vnet.State == pve.SDNNew, d.Vnet.State == pve.SDNDeleted:
		return engine.ManagedVnet{}, nil
	}
	v := engine.ManagedVnet{Exists: true, IsolatePorts: d.Vnet.IsolatePorts}
	if d.Zone.Name == d.Vnet.Zone && d.Zone.State != pve.SDNDeleted {
		v.ZoneType = d.Zone.Type
	}
	for _, s := range d.Subnets {
		if s.Vnet == d.Vnet.Name && s.State != pve.SDNDeleted {
			v.Subnets++
		}
	}
	return v, nil
}
