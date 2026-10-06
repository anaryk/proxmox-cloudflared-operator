package daemon

import (
	"context"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// proxmoxNetworks is what the admin actions of the networks read of Proxmox.
type proxmoxNetworks interface {
	NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error)
	VNets(ctx context.Context) ([]pve.VNet, error)
}

// Netns is pco-netns.service as the daemon runs it: setup.NamespaceUnit.
type Netns interface {
	Start(ctx context.Context) error
	Enable(ctx context.Context) error
	Disable(ctx context.Context) error
}

// nodeNetworks is what the engine's admin actions of the networks read and
// run on a node: its network and the SDN vnets as Proxmox has them, its
// interfaces as the prober sees them, and the unit of the gateway namespace,
// which the appliance does not have.
type nodeNetworks struct {
	proxmox proxmoxNetworks
	prober  resolve.Prober
	unit    Netns // nil in the appliance
}

var _ engine.Networks = nodeNetworks{}

func (n nodeNetworks) NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error) {
	return n.proxmox.NodeNetwork(ctx, node)
}

func (n nodeNetworks) VNets(ctx context.Context) ([]pve.VNet, error) { return n.proxmox.VNets(ctx) }

func (n nodeNetworks) Interfaces(ctx context.Context) ([]resolve.HostIface, error) {
	return n.prober.Interfaces(ctx)
}

func (n nodeNetworks) StartNamespace(ctx context.Context) error {
	if n.unit == nil {
		return nil
	}
	return n.unit.Start(ctx)
}

func (n nodeNetworks) EnableNamespace(ctx context.Context) error {
	if n.unit == nil {
		return nil
	}
	return n.unit.Enable(ctx)
}

func (n nodeNetworks) DisableNamespace(ctx context.Context) error {
	if n.unit == nil {
		return nil
	}
	return n.unit.Disable(ctx)
}
