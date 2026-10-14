package appliance

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestAnAgentIsBoundToTheNodeOfItsInstall(t *testing.T) {
	ct := func(node, lock string) []pve.Resource {
		return []pve.Resource{
			{Kind: model.KindQEMU, VMID: 9301, Node: "pve3"},
			{Kind: model.KindLXC, VMID: 9301, Node: node, Lock: lock},
		}
	}
	ha := []pve.HAResource{{SID: "ct:9301", Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9301}}}
	for _, tt := range []struct {
		name      string
		resources []pve.Resource
		ha        []pve.HAResource
		want      Binding
	}{
		{"on its node", ct("pve2", ""), nil, Binding{Bound: true}},
		{"moved", ct("pve3", ""), nil, Binding{Why: "agent 9301 runs on pve3, enrolled for pve2; move it back or enrol it again"}},
		{"being migrated", ct("pve3", "migrate"), nil, Binding{Pending: true, Why: "agent 9301 is being migrated to pve3"}},
		{"backed up where it is", ct("pve2", "backup"), nil, Binding{Bound: true}},
		{"an HA resource", ct("pve2", ""), ha, Binding{Why: "agent 9301 is a Proxmox HA resource; agents are bound to their node"}},
		{"not listed", nil, nil, Binding{Pending: true, Why: "agent 9301 is not among the cluster's resources"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, BoundTo(9301, "pve2", tt.resources, tt.ha))
		})
	}
}
