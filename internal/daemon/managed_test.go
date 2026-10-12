package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// fakeVnetSource answers the read of one vnet as a test sets it.
type fakeVnetSource struct {
	detail pve.SDNVnetDetail
	err    error
	asked  []string
}

func (v *fakeVnetSource) SDNVnet(_ context.Context, vnet string) (pve.SDNVnetDetail, error) {
	v.asked = append(v.asked, vnet)
	return v.detail, v.err
}

func TestTheManagedVnetAsTheEngineReadsIt(t *testing.T) {
	made := pve.SDNVnetDetail{
		Vnet: pve.SDNVnet{Name: "pconet", Zone: "pco", IsolatePorts: true}, Zone: pve.SDNZone{Name: "pco", Type: "simple"}, Found: true,
	}
	for _, tt := range []struct {
		name   string
		change func(d *pve.SDNVnetDetail)
		want   engine.ManagedVnet
	}{
		{"as made", func(*pve.SDNVnetDetail) {}, engine.ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true}},
		{"not there", func(d *pve.SDNVnetDetail) { *d = pve.SDNVnetDetail{} }, engine.ManagedVnet{}},
		{"made and not applied", func(d *pve.SDNVnetDetail) { d.Vnet.State = pve.SDNNew }, engine.ManagedVnet{}},
		{"about to go", func(d *pve.SDNVnetDetail) { d.Vnet.State = pve.SDNDeleted }, engine.ManagedVnet{}},
		{"a zone about to go", func(d *pve.SDNVnetDetail) { d.Zone.State = pve.SDNDeleted },
			engine.ManagedVnet{Exists: true, IsolatePorts: true}},
		{"a subnet, and one about to go", func(d *pve.SDNVnetDetail) {
			d.Subnets = []pve.SDNSubnet{{Vnet: "pconet", CIDR: "10.9.0.0/24"}, {Vnet: "pconet", CIDR: "10.9.1.0/24", State: pve.SDNDeleted}}
		}, engine.ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true, Subnets: 1}},
		{"a subnet not applied yet", func(d *pve.SDNVnetDetail) {
			d.Subnets = []pve.SDNSubnet{{Vnet: "pconet", CIDR: "10.9.0.0/24", State: pve.SDNNew}}
		}, engine.ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true, Subnets: 1}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := made
			tt.change(&d)
			src := &fakeVnetSource{detail: d}

			got, err := managedVnet(t.Context(), src)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
			require.Equal(t, []string{"pconet"}, src.asked)
		})
	}

	_, err := managedVnet(t.Context(), &fakeVnetSource{err: errors.New("503")})
	require.EqualError(t, err, "503")
}
