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
	SDNPending(ctx context.Context) (pve.SDN, error)
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

// managedVnet reads the vnet of the managed network as configured, with
// what is pending: the type of its zone and its subnets.
func managedVnet(ctx context.Context, src managedSource) (engine.ManagedVnet, error) {
	sdn, err := src.SDNPending(ctx)
	if err != nil {
		return engine.ManagedVnet{}, err
	}
	var v engine.ManagedVnet
	for _, vnet := range sdn.Vnets {
		if vnet.Name != gateway.ManagedBridge || vnet.State == pve.SDNDeleted {
			continue
		}
		v.Exists, v.IsolatePorts = true, vnet.IsolatePorts
		for _, z := range sdn.Zones {
			if z.Name == vnet.Zone && z.State != pve.SDNDeleted {
				v.ZoneType = z.Type
			}
		}
		for _, s := range sdn.Subnets {
			if s.Vnet == vnet.Name && s.State != pve.SDNDeleted {
				v.Subnets++
			}
		}
	}
	return v, nil
}
