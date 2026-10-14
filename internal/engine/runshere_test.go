package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The connector of a tunnel runs on this node only when the tunnel is the
// install's shared one or the one pinned to this node, by its name: never
// one pinned to another node, one of another install, or a name the install
// does not make, whatever the state says of its kind and node.
func TestOnlyTheSharedTunnelAndThisNodesPinnedOneRunHere(t *testing.T) {
	e := newEnv(t)
	c := e.eng.newCycle(t.Context())
	c.install = store.Install{ID: testInstall}
	for name, want := range map[string]bool{
		tunnelName:              true,
		tunnelName + "-pve1":    true,
		tunnelName + "-pve2":    false,
		tunnelName + "-PVE1":    false,
		"pco-other1-pve1":       false,
		"pco-other1":            false,
		tunnelName + "_probe_x": false,
		"":                      false,
	} {
		got := c.runsHere(reconcile.TunnelState{AccountID: testAccount, Name: name, Kind: "pinned", Node: testNode})
		require.Equal(t, want, got, name)
	}
}

// A cycle that knows no install runs no connector of a pinned tunnel.
func TestWithoutAnInstallNoPinnedTunnelRunsHere(t *testing.T) {
	e := newEnv(t)
	c := e.eng.newCycle(t.Context())
	require.False(t, c.runsHere(reconcile.TunnelState{Name: tunnelName + "-pve1"}))
	require.False(t, c.runsHere(reconcile.TunnelState{Name: "pco--pve1"}))
}

// The snapshots served to the other nodes carry no run token of any tunnel
// and run none: no node gets a tunnel's token through a snapshot of the
// controller unless placement gives it that tunnel.
func TestNoOtherNodeIsServedARunToken(t *testing.T) {
	one := 1
	e, r, _ := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	r.report("pve2", e.clock.now(), t0.Add(-1))
	e.cycle()

	snaps := r.last()
	require.Contains(t, snaps, "pve2")
	for _, tun := range snaps["pve2"].Tunnels {
		require.False(t, tun.Run, tun.Name)
		require.Empty(t, tun.Token.Reveal(), tun.Name)
	}
}
