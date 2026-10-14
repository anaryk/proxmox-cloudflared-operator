package appliance

import (
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// The lines of an agent container that does not serve where it is bound.
const (
	LineAgentMoved = "agent %d runs on %s, enrolled for %s; move it back or enrol it again"
	LineAgentHA    = "agent %d is a Proxmox HA resource; agents are bound to their node"
)

// Binding is whether an agent container serves the node it was installed on.
// An agent serves the guests, the legs and the pinned tunnel of one node: a
// container moved to another node, by an admin's pct migrate, a restore
// elsewhere or the HA manager, would serve that node's in its name. It stops
// as a copy does, until it is back or enrolled again.
type Binding struct {
	Bound bool
	// Pending says that nothing is decided yet: the container is moving, or
	// the cluster's resources do not list it; what was decided last stands.
	Pending bool
	Why     string
}

// BoundTo judges where the agent container vmid runs, by the cluster's
// resources and the HA resources, against node, the node of its install.
func BoundTo(vmid int, node string, resources []pve.Resource, ha []pve.HAResource) Binding {
	ref := model.GuestRef{Kind: model.KindLXC, VMID: vmid}
	for _, r := range ha {
		if r.Ref == ref {
			return Binding{Why: fmt.Sprintf(LineAgentHA, vmid)}
		}
	}
	for _, r := range resources {
		if r.Kind != model.KindLXC || r.VMID != vmid {
			continue
		}
		switch {
		case r.Lock == "migrate":
			return Binding{Pending: true, Why: fmt.Sprintf("agent %d is being migrated to %s", vmid, r.Node)}
		case r.Node != node:
			return Binding{Why: fmt.Sprintf(LineAgentMoved, vmid, r.Node, node)}
		}
		return Binding{Bound: true}
	}
	return Binding{Pending: true, Why: fmt.Sprintf("agent %d is not among the cluster's resources", vmid)}
}
