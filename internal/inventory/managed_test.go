package inventory

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestManagedSubnetConflict(t *testing.T) {
	snap := Snapshot{
		Nodes: []Node{{Name: "pve1", Ifaces: []pve.NodeIface{{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.4.10/24")}}}}},
		Guests: []model.Guest{
			{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Name: " web-1 ", NICs: []model.NIC{{Static: []netip.Addr{netip.MustParseAddr("10.77.0.11")}}}},
			{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 102}, Reported: []model.ReportedAddr{{Addr: netip.MustParseAddr("10.77.1.12")}}},
		},
	}
	attached := []AttachedPrefix{{ID: "vmbr1", Prefix: netip.MustParsePrefix("10.60.0.0/23")}}
	for _, tt := range []struct{ subnet, want string }{
		{"192.168.0.0/16", "overlaps the route 192.168.4.0/24 of node pve1 on vmbr0"},
		{"10.77.0.0/24", "holds 10.77.0.11, an address of qemu/101 (web-1)"},
		{"10.77.1.0/28", "holds 10.77.1.12, an address of lxc/102"},
		{"10.60.1.0/24", "overlaps 10.60.0.0/23, the prefix of network vmbr1"},
		{"10.203.118.0/24", ""},
	} {
		require.Equal(t, tt.want, snap.ManagedSubnetConflict(netip.MustParsePrefix(tt.subnet), attached), tt.subnet)
	}
}
