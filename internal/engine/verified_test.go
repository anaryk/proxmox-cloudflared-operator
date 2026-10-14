package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var pinnedName = func() string {
	name, err := planner.PinnedTunnelName(testInstall, "pve2")
	if err != nil {
		panic(err)
	}
	return name
}()

// twoTunnelPlan is a plan with the shared tunnel of the account and the one
// pinned to pve2, each with a rule of its own.
func twoTunnelPlan() planner.Plan {
	return planner.Plan{Tunnels: []planner.TunnelPlan{
		{AccountID: testAccount, Name: tunnelName, Rules: withSentinel(hostRule("www.example.com"))},
		{AccountID: testAccount, Name: pinnedName, Kind: planner.PlacementPinned, Node: "pve2", Rules: withSentinel(
			planner.IngressRule{Hostname: "db.example.com", Service: "tcp://10.0.0.12:5432"})},
	}}
}

// The configurations verified at Cloudflare are kept per tunnel: the
// pinned tunnel of an account is not taken for its shared one, and a tunnel
// that is gone forgets only its own.
func TestVerifiedConfigurationsAreKeptPerTunnel(t *testing.T) {
	e := newEnv(t)
	c := e.eng.newCycle(t.Context())
	c.plan = twoTunnelPlan()

	c.noteVerified([]reconcile.TunnelState{
		{AccountID: testAccount, Name: tunnelName, Exists: true, Verified: true},
		{AccountID: testAccount, Name: pinnedName, Kind: planner.PlacementPinned, Node: "pve2", Exists: true, Verified: true},
	})

	require.Equal(t, egressTargets("10.0.0.11:8080"), e.eng.verified[tunnelRef{testAccount, tunnelName}])
	require.Equal(t, egressTargets("10.0.0.12:5432"), e.eng.verified[tunnelRef{testAccount, pinnedName}])

	c.noteVerified([]reconcile.TunnelState{{AccountID: testAccount, Name: pinnedName, Kind: planner.PlacementPinned, Node: "pve2"}})

	require.Equal(t, egressTargets("10.0.0.11:8080"), e.eng.verified[tunnelRef{testAccount, tunnelName}])
	require.NotContains(t, e.eng.verified, tunnelRef{testAccount, pinnedName})
}

// The snapshot of a node names each tunnel with the targets of its own
// verified configuration, and keeps the other verified ones the node may
// run: the shared tunnel and the one pinned to it, never one pinned to
// another node.
func TestTheSnapshotKeepsTheTargetsOfEachTunnel(t *testing.T) {
	e := newEnv(t)
	e.eng.verified[tunnelRef{testAccount, tunnelName}] = egressTargets("10.0.0.11:8080")
	e.eng.verified[tunnelRef{testAccount, pinnedName}] = egressTargets("10.0.0.12:5432")
	e.eng.verified[tunnelRef{"acc2", tunnelName}] = egressTargets("10.0.0.13:80")
	c := e.eng.newCycle(t.Context())
	c.install = store.Install{ID: testInstall}
	shared := []agentproto.Tunnel{{AccountID: testAccount, TunnelID: "t1", Name: tunnelName, Kind: agentproto.TunnelShared}}

	got := c.withVerified(shared, "pve2", true)

	require.Len(t, got, 3)
	require.Equal(t, tunnelName, got[0].Name)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:8080")}, got[0].Verified)
	require.Equal(t, pinnedName, got[1].Name)
	require.Equal(t, agentproto.TunnelPinned, got[1].Kind)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.12:5432")}, got[1].Verified)
	require.Equal(t, "acc2", got[2].AccountID)

	for _, node := range []string{"pve1", "pve3"} {
		got = c.withVerified(shared, node, true)
		require.Len(t, got, 2, node)
		for _, tun := range got {
			require.NotEqual(t, pinnedName, tun.Name, node)
		}
	}
	require.Len(t, c.withVerified(shared, "", true), 1)

	got = c.withVerified(nil, "pve2", false)
	require.Len(t, got, 1, "a node that runs no shared connector keeps no shared tunnel")
	require.Equal(t, pinnedName, got[0].Name)
}

// A tunnel whose own configuration was never verified keeps no target in the
// snapshot, whatever another tunnel of its account had verified: one
// tunnel's configuration never stands in for another's.
func TestATunnelGetsNoTargetsOfAnotherTunnelOfItsAccount(t *testing.T) {
	e := newEnv(t)
	e.eng.verified[tunnelRef{testAccount, tunnelName}] = egressTargets("10.0.0.11:8080")
	c := e.eng.newCycle(t.Context())
	c.install = store.Install{ID: testInstall}

	got := c.withVerified([]agentproto.Tunnel{
		{AccountID: testAccount, TunnelID: "t2", Name: pinnedName, Kind: agentproto.TunnelPinned},
		{AccountID: "acc2", TunnelID: "t3", Name: tunnelName, Kind: agentproto.TunnelShared},
	}, "", false)

	require.Len(t, got, 2)
	for _, tun := range got {
		require.Empty(t, tun.Verified, tun.Name+" in "+tun.AccountID)
	}
}

// The memory keeps the verified configurations per tunnel; one saved before
// tunnels were told apart is the shared tunnel's, and the shared tunnel's
// entry comes last of its account, where an earlier release, which keeps the
// last one of an account, takes it.
func TestTheMemoryKeepsTheVerifiedConfigurationsPerTunnel(t *testing.T) {
	e := newEnv(t)
	e.eng.verified[tunnelRef{testAccount, tunnelName}] = egressTargets("10.0.0.11:8080")
	e.eng.verified[tunnelRef{testAccount, pinnedName}] = egressTargets("10.0.0.12:5432")
	m := store.EngineMemory{InstallID: testInstall}
	e.eng.egressMemory(&m)
	require.NoError(t, e.store.SaveEngineMemory(m))
	m, err := e.store.EngineMemory()
	require.NoError(t, err)

	require.Len(t, m.Verified, 2)
	require.Equal(t, pinnedName, m.Verified[0].Name)
	require.Empty(t, m.Verified[1].Name)

	again := newEnv(t).eng
	again.rememberEgress(m)
	require.Equal(t, e.eng.verified, again.verified)

	old := newEnv(t).eng
	old.rememberEgress(store.EngineMemory{InstallID: testInstall, Verified: []store.VerifiedTargets{{AccountID: testAccount, Targets: toAddrPorts(egressTargets("10.0.0.11:8080"))}}})
	require.Equal(t, egressTargets("10.0.0.11:8080"), old.verified[tunnelRef{testAccount, tunnelName}])
}

// What the inventory read of the watched guests goes into the memory of a
// cluster's controller, and a controller after it gives it to its inventory
// before its first refresh: a guest of a node that went down with the last
// controller is built from it, and no cycle holds for it. A node alone keeps
// no guests in its memory.
func TestTheMemoryCarriesTheGuestsOverToTheNextController(t *testing.T) {
	web := inventory.Remembered{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, Digest: "d1", Values: map[string]string{"name": "web", "description": "www.example.com -> :8080"}}

	alone := newEnv(t)
	alone.inv.kept = []inventory.Remembered{web}
	alone.enforce()
	alone.cycle()
	m, err := alone.store.EngineMemory()
	require.NoError(t, err)
	require.Empty(t, m.Guests)

	w := threeNodes(t)
	w.inv.kept = []inventory.Remembered{web}
	w.settle()
	m, err = w.store.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, []store.RememberedGuest{{Ref: web.Ref, Digest: web.Digest, Values: web.Values}}, m.Guests)

	w.inv.kept = nil
	w.eng = w.newEngine()
	w.watchHere(t)
	w.tick(time.Second)
	require.Equal(t, []inventory.Remembered{web}, w.inv.given)
}

// A tunnel whose account the run reported on is not taken for unreported
// because another tunnel of that account was reported.
func TestATunnelOfAReportedAccountIsUnreportedByItsName(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	c := e.eng.newCycle(t.Context())
	c.st.Tunnels = []TunnelView{
		{TunnelState: reconcile.TunnelState{AccountID: testAccount, Name: tunnelName, ID: "t1", Exists: true}},
		{TunnelState: reconcile.TunnelState{AccountID: testAccount, Name: pinnedName, ID: "t2", Exists: true}},
	}

	got := c.unreportedOf([]reconcile.TunnelState{{AccountID: testAccount, Name: tunnelName, ID: "t1", Exists: true}})

	require.Len(t, got, 1)
	require.Equal(t, pinnedName, got[0].Name)
}
