package inventory

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
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

// addFarGuest puts a guest on pve3, which the test marks offline.
func addFarGuest(src *fakeSource, tags ...string) model.GuestRef {
	src.nodes = append(src.nodes, pve.ClusterNode{Name: "pve3", Addr: ip("10.20.0.4")})
	src.addGuest(
		pve.Resource{Kind: model.KindQEMU, VMID: 103, Name: "far-1", Node: "pve3", Status: "unknown", Tags: tags},
		map[string]string{"name": "far-1", "net0": "virtio=BC:24:11:00:AA:07,bridge=vmbr0"},
		nil,
	)
	return model.GuestRef{Kind: model.KindQEMU, VMID: 103}
}

func TestRefreshOfflineNodeServesCachedGuests(t *testing.T) {
	src := newFake()
	inv, clk := newInventory(src, Options{})
	first := inv.Refresh(t.Context())
	require.True(t, first.Complete)
	src.setOnline("pve2", false)
	src.setStatus(refDB, "unknown")
	clk.advance(6 * time.Minute) // sweep and reported answers are due

	snap := inv.Refresh(t.Context())

	require.True(t, snap.Complete)
	require.Equal(t, []string{
		"node pve2 is offline; using cached data for its guests",
		"status of 1 guests is unknown; using last known state",
	}, snap.Problems)
	require.Len(t, snap.Guests, 4)
	require.Equal(t, 1, src.count("config "+refDB.String()))
	require.Equal(t, 1, src.count("config "+refApp.String()))
	require.Equal(t, 1, src.count("iface "+refApp.String()))
	require.Equal(t, guestOf(t, first, refDB), guestOf(t, snap, refDB), "an unknown status keeps the last known state")
	require.False(t, guestOf(t, snap, refDB).StatusUnknown)
	require.Equal(t, guestOf(t, first, refApp), guestOf(t, snap, refApp))

	pve2 := snap.Nodes[1]
	require.Equal(t, "pve2", pve2.Name)
	require.False(t, pve2.Online)
	require.Equal(t, first.Nodes[1].Ifaces, pve2.Ifaces, "an offline node keeps its last known interfaces")
	require.Equal(t, 1, src.count("network pve2"))
	require.Equal(t, first.NodeAddrs(), snap.NodeAddrs())

	src.setOnline("pve2", true)
	src.setStatus(refDB, "running")
	clk.advance(10 * time.Second)
	snap = inv.Refresh(t.Context())
	require.True(t, snap.Complete)
	require.Empty(t, snap.Problems)
	require.Equal(t, 2, src.count("config "+refDB.String()))
	require.Equal(t, 2, src.count("config "+refApp.String()))
	require.Equal(t, 2, src.count("iface "+refApp.String()))
}

func TestRefreshOfflineNodeWithUncachedGuest(t *testing.T) {
	t.Run("watched guest keeps the snapshot incomplete", func(t *testing.T) {
		src := newFake()
		ref := addFarGuest(src, "cf-tunnel")
		inv, clk := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.False(t, snap.Complete)
		require.Equal(t, []string{
			"guest qemu/103 (far-1): node pve3 is offline and the guest is not cached",
			"node pve3 is offline; using cached data for its guests",
		}, snap.Problems)
		require.Equal(t, []model.GuestRef{refDB, refApp, refWeb, refBatch}, refs(snap))
		require.Zero(t, src.count("config "+ref.String()))

		src.setOnline("pve3", true)
		src.setStatus(ref, "running")
		clk.advance(10 * time.Second)
		snap = inv.Refresh(t.Context())
		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
		require.Len(t, snap.Guests, 5)
		require.Equal(t, 1, src.count("config "+ref.String()))
		require.True(t, guestOf(t, snap, ref).Running)
	})

	t.Run("other guest is left out quietly", func(t *testing.T) {
		src := newFake()
		ref := addFarGuest(src, "prod")
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.True(t, snap.Complete)
		require.Equal(t, []string{"node pve3 is offline; using cached data for its guests"}, snap.Problems)
		require.Len(t, snap.Guests, 4)
		require.Zero(t, src.count("config "+ref.String()))
	})

	t.Run("cached guest is served whatever its row says", func(t *testing.T) {
		src := newFake()
		inv, clk := newInventory(src, Options{})
		inv.Refresh(t.Context())
		src.setOnline("pve2", false)
		src.update(refApp, func(row *pve.Resource) { row.Tags = nil })
		clk.advance(10 * time.Second)

		snap := inv.Refresh(t.Context())

		require.True(t, snap.Complete, "cached guests are served whatever their tags")
		require.Len(t, snap.Guests, 4)
	})

	t.Run("offline node without guests says nothing", func(t *testing.T) {
		src := newFake()
		src.nodes = append(src.nodes, pve.ClusterNode{Name: "pve3", Addr: ip("10.20.0.4")})
		inv, _ := newInventory(src, Options{})

		snap := inv.Refresh(t.Context())

		require.True(t, snap.Complete)
		require.Empty(t, snap.Problems)
		require.Len(t, snap.Nodes, 3)
	})
}

func TestRefreshClusterNodesFailureTreatsEveryNodeAsOnline(t *testing.T) {
	src := newFake()
	ref := addFarGuest(src, "prod")
	inv, clk := newInventory(src, Options{})
	first := inv.Refresh(t.Context())
	require.Zero(t, src.count("config "+ref.String()))
	src.nodesErr = serverErr()
	clk.advance(10 * time.Second)

	snap := inv.Refresh(t.Context())

	require.False(t, snap.Complete)
	require.Len(t, snap.Problems, 2)
	require.Contains(t, snap.Problems[0], "cluster nodes not listed")
	require.Equal(t, "status of 1 guests is unknown; using last known state", snap.Problems[1])
	require.Equal(t, 1, src.count("config "+ref.String()), "the config call decides")
	require.Len(t, snap.Guests, 5)
	require.False(t, guestOf(t, snap, ref).Running, "never seen running")
	require.True(t, guestOf(t, snap, ref).StatusUnknown)
	require.Equal(t, first.Nodes, snap.Nodes)
	require.Equal(t, 1, src.count("network pve1"))
}
