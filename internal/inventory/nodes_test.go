package inventory

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

func TestRefreshBuildsNodes(t *testing.T) {
	src := newFake()
	src.nodes = []pve.ClusterNode{
		{Name: "pve3", Addr: ip("10.20.0.4"), Online: false},
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true},
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Local: true},
	}
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []Node{
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Local: true, Ifaces: src.networks["pve1"]},
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true, Ifaces: src.networks["pve2"]},
		{Name: "pve3", Addr: ip("10.20.0.4")},
	}, snap.Nodes)
	require.Zero(t, src.count("network pve3"), "offline nodes are not asked")
	require.Equal(t, 1, src.count("network pve1"))
	require.Equal(t, 1, src.count("network pve2"))
}

func TestRefreshNodeFailuresMarkIncomplete(t *testing.T) {
	t.Run("cluster status fails", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.nodesErr = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Equal(t, first.Nodes, snap.Nodes)
		require.Len(t, snap.Guests, 4)
	})

	t.Run("network of one node fails", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		first := inv.Refresh(t.Context())
		src.networkErrs["pve2"] = serverErr()
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Problems, 1)
		require.Contains(t, snap.Problems[0], "pve2")
		require.Equal(t, first.Nodes, snap.Nodes, "the failing node keeps what was known")
	})

	t.Run("network fails on the first refresh", func(t *testing.T) {
		src := newFake()
		src.networkErrs["pve2"] = serverErr()
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Len(t, snap.Nodes, 2)
		require.Equal(t, "pve2", snap.Nodes[1].Name)
		require.Empty(t, snap.Nodes[1].Ifaces)
	})
}

func TestSnapshotNodeAddrs(t *testing.T) {
	snap := Snapshot{Nodes: []Node{
		{Name: "pve2", Addr: ip("10.20.0.3"), Online: true, Ifaces: []pve.NodeIface{
			{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.3/24")}},
			{Name: "vmbr1", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.50.3/24")}},
			{Name: "vmbr6", Addrs: []netip.Prefix{netip.MustParsePrefix("fd00::3/64")}},
			{Name: "eno1"},
		}},
		{Name: "pve1", Addr: ip("10.20.0.2"), Online: true, Ifaces: []pve.NodeIface{
			{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}},
		}},
		{Name: "pve3", Addr: ip("10.20.0.4")},
		{Name: "pve4"},
	}}

	require.Equal(t, []netip.Addr{
		ip("10.20.0.2"), ip("10.20.0.3"), ip("10.20.0.4"), ip("192.168.50.3"),
	}, snap.NodeAddrs())
	require.Empty(t, Snapshot{}.NodeAddrs())
}

func TestRefreshNodeAddrsCoverEveryNode(t *testing.T) {
	src := newFake()
	inv, _ := newInventory(src, Options{})

	snap := inv.Refresh(t.Context())

	require.Equal(t, []netip.Addr{ip("10.20.0.2"), ip("10.20.0.3"), ip("192.168.50.3")}, snap.NodeAddrs())
}
