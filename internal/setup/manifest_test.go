package setup

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNetworkGrantShapes(t *testing.T) {
	for _, tt := range []struct {
		grant NetworkGrant
		path  string
		err   string
	}{
		{grant: NetworkGrant{Zone: "localnetwork", VNet: "vmbr1"}, path: "/sdn/zones/localnetwork/vmbr1"},
		{grant: NetworkGrant{Zone: "localnetwork", VNet: "vmbr1", VLAN: 20}, path: "/sdn/zones/localnetwork/vmbr1/20"},
		{grant: NetworkGrant{Zone: "pco", VNet: "pconet", CreatedRoles: []string{RoleManaged, RoleSDN}}, path: "/sdn/zones/pco/pconet"},
		{grant: NetworkGrant{Zone: "pco"}, err: `vnet "": want the name of a bridge or vnet`},
		{grant: NetworkGrant{Zone: "../..", VNet: "x"}, err: `zone "../..": want the name of an SDN zone`},
		{grant: NetworkGrant{Zone: "z", VNet: "a/b"}, err: `vnet "a/b"`},
		{grant: NetworkGrant{Zone: "z", VNet: "v", VLAN: 4095}, err: "vlan 4095: want 1 to 4094, or none"},
		{grant: NetworkGrant{Zone: "z", VNet: "v", CreatedRoles: []string{"Administrator"}}, err: `role "Administrator": a network grant makes PCOManaged and PCOSDN only`},
	} {
		t.Run(tt.grant.Zone+"/"+tt.grant.VNet, func(t *testing.T) {
			err := tt.grant.Check()
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.path, tt.grant.Path())
		})
	}
}

func TestNetworkGrantsInTheManifestOfAnAppliance(t *testing.T) {
	local := t.TempDir()
	path := filepath.Join(local, manifestName)
	g := NetworkGrant{Zone: "localnetwork", VNet: "vmbr1", CreatedRoles: []string{RoleSDN}}

	require.ErrorContains(t, AddNetworkGrant(local, g), "is not the manifest of an appliance")
	require.NoError(t, writeManifest(path, Manifest{Node: "pve1", Appliance: &ApplianceManifest{VMID: 100, Node: "pve1", Pool: "pco"}}))

	require.NoError(t, AddNetworkGrant(local, g))
	require.NoError(t, AddNetworkGrant(local, NetworkGrant{Zone: "localnetwork", VNet: "vmbr1", CreatedRoles: []string{RoleManaged}}))
	require.NoError(t, AddNetworkGrant(local, NetworkGrant{Zone: "localnetwork", VNet: "vmbr1", VLAN: 20}))
	require.Error(t, AddNetworkGrant(local, NetworkGrant{Zone: "localnetwork"}))

	m, _, err := readManifest(path)
	require.NoError(t, err)
	require.Equal(t, []NetworkGrant{
		{Zone: "localnetwork", VNet: "vmbr1", CreatedRoles: []string{RoleSDN, RoleManaged}},
		{Zone: "localnetwork", VNet: "vmbr1", VLAN: 20},
	}, m.Appliance.Grants)

	require.NoError(t, RemoveNetworkGrant(local, g))
	m, _, err = readManifest(path)
	require.NoError(t, err)
	require.Equal(t, []NetworkGrant{{Zone: "localnetwork", VNet: "vmbr1", VLAN: 20}}, m.Appliance.Grants)
	require.Equal(t, 100, m.Appliance.VMID, "the rest is kept")
}
