package agent

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

func TestEveryTargetThisNodeResolvesIsResolvedWithItsStoredBinding(t *testing.T) {
	e := newEnv(t)
	st := e.apply(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))

	require.ElementsMatch(t, []string{"www.example.com", "api.example.com"}, e.res.calls())
	r := report(st, "www.example.com")
	require.True(t, r.OK)
	require.Equal(t, guestAddr, r.Addr)
	require.Equal(t, "port", r.Level)
	require.Equal(t, t0, r.VerifiedAt)
	require.True(t, r.InEgressSet)
	res, err := DecodeResult(r.Result)
	require.NoError(t, err)
	require.Equal(t, guestAddr, res.Target.Addr)
	require.Equal(t, "www.example.com", res.Binding.Hostname)

	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Len(t, bindings, 2)
	require.Equal(t, guestAddr, bindings["www.example.com"].Addr)

	e.res.stop("www.example.com", "guest is not running")
	e.clock.advance(10 * time.Second)
	st = e.apply(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))
	r = report(st, "www.example.com")
	require.False(t, r.OK)
	require.Equal(t, "guest is not running", r.Reason)
	bindings, err = e.store.Bindings()
	require.NoError(t, err)
	require.True(t, bindings["www.example.com"].Withdrawn, "the stored binding was handed to the resolver")
}

func TestTheBindingsOfRoutesThatAreGoneAreNotKept(t *testing.T) {
	e := newEnv(t)
	e.apply(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))

	e.apply(e.snapshot("www.example.com -> :8080"))

	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Len(t, bindings, 1)
}

func TestEachResolutionIsBoundedByTheResolveTimeout(t *testing.T) {
	e := newEnv(t)
	start := time.Now()

	e.apply(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))

	require.Len(t, e.res.deadlines, 2)
	for _, d := range e.res.deadlines {
		require.WithinDuration(t, start.Add(resolveTimeout), d, 5*time.Second)
	}
}

func TestTheRequiredLevelOfEachTargetGoesToTheResolver(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080", "api.example.com -> :9090")
	s.Targets[1].Required = "observed"

	e.apply(s)

	require.ElementsMatch(t, []resolve.Level{resolve.LevelPort, resolve.LevelObserved}, e.res.required)
}

func TestTheResolverSeesTheGuestsNodesAndDenylistOfTheSnapshot(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	clone := model.Guest{
		Ref: model.GuestRef{Kind: model.KindLXC, VMID: 202}, Node: "pve2", Running: true,
		NICs: []model.NIC{{Index: 0, MAC: testMAC, Bridge: "vmbr0"}},
	}
	s.Guests = []model.Guest{clone}
	s.Nodes = append(s.Nodes, agentproto.Node{Name: "pve2", Addr: netip.MustParseAddr("10.0.0.3"), Online: true, Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.3"), netip.MustParseAddr("172.16.0.3")}})
	s.Deny = agentproto.DenyList{
		Denied:  []netip.Addr{nodeAddr, netip.MustParseAddr("10.0.0.3")},
		Reasons: map[string]string{"10.0.0.1": "gateway of SDN subnet 10.0.0.0/24 of vnet lab"},
		Soft:    map[string]string{"10.0.0.254": "gateway of node pve1"},
	}

	e.apply(s)

	snap := e.res.lastSnap()
	require.True(t, snap.Complete)
	require.Len(t, snap.Guests, 2)
	require.Equal(t, model.KindQEMU, snap.Guests[0].Ref.Kind, "in the order of the inventory")
	require.Equal(t, clone, snap.Guests[1])
	g, ok := snap.Guest(model.GuestRef{Kind: model.KindQEMU, VMID: 101})
	require.True(t, ok)
	require.Equal(t, testNode, g.Node)
	require.Len(t, snap.Nodes, 2)
	require.Equal(t, []netip.Addr{nodeAddr, netip.MustParseAddr("10.0.0.3"), netip.MustParseAddr("172.16.0.3")}, snap.NodeAddrs())

	deny := e.res.lastDeny()
	why, denied := deny.Check(netip.MustParseAddr("10.0.0.1"))
	require.True(t, denied)
	require.Equal(t, "gateway of SDN subnet 10.0.0.0/24 of vnet lab", why)
	why, denied = deny.Check(netip.MustParseAddr("10.0.0.3"))
	require.True(t, denied)
	require.Equal(t, "address of a cluster node", why)
	why, soft := deny.Soft(netip.MustParseAddr("10.0.0.254"))
	require.True(t, soft)
	require.Equal(t, "gateway of node pve1", why)
}

// A manual route with allowNode reaches the addresses of the nodes and
// nothing else the denylist refuses: the resolver lifts only those, so they
// must be the denylist's node addresses.
func TestTheNodeAddressesOfTheDenylistAreTheOnesAllowNodeLifts(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Deny.Reasons = map[string]string{"10.0.0.1": "gateway of SDN subnet 10.0.0.0/24 of vnet lab"}

	e.apply(s)

	deny := e.res.lastDeny()
	_, denied := deny.Check(nodeAddr)
	require.True(t, denied)
	res := resolve.NewResolver(nil, resolve.Settings{}, nil).Resolve(t.Context(), model.Route{
		Hostname: "pve.example.com", Source: model.SourceManual, ManualID: "pve",
		Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: netip.MustParseAddr("10.0.0.1"), Port: 443},
		Options: model.RouteOptions{AllowNode: true},
	}, e.res.lastSnap(), nil, deny, resolve.LevelPort, nil)
	require.True(t, res.Target.Rejected, "allowNode does not lift the gateway of an SDN subnet")
}

func TestAnApplyWhoseContextEndsWhileResolvingChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	sets, calls := e.egr.setCount(), len(e.conn.changes())
	ctx, cancel := context.WithCancel(t.Context())
	e.res.onResolve = func(string) { cancel() }

	st, err := e.agent.Apply(ctx, e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))

	require.ErrorIs(t, err, context.Canceled)
	require.Contains(t, st.Problems, "the desired state was applied only in part: resolution ended (context canceled); the egress set and the connectors are left as they are")
	require.Equal(t, sets, e.egr.setCount())
	require.Len(t, e.conn.changes(), calls)
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Len(t, bindings, 1, "no binding of the cut resolution is saved")
}

func TestManyTargetsAreResolvedAtMostThirtyTwoAtATime(t *testing.T) {
	e := newEnv(t)
	routes := make([]string, 0, 80)
	for i := range 80 {
		routes = append(routes, "h"+string(rune('a'+i/26))+string(rune('a'+i%26))+".example.com -> :8080")
	}
	var mu sync.Mutex
	running, most := 0, 0
	release := make(chan struct{})
	e.res.onResolve = func(string) {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(20 * time.Millisecond):
		}
		mu.Lock()
		running--
		mu.Unlock()
	}

	st := e.apply(e.snapshot(routes...))

	close(release)
	require.Len(t, st.Targets, 80)
	require.LessOrEqual(t, most, resolveConcurrency)
	require.Greater(t, most, 1)
}

// The proofs of identity the watch of the network vouches for stand without
// being made again, as the engine's do: the watch ran before the proof, has
// the address pinned with the MAC of the proof since before it, and the guest
// ran unchanged since before it.
func TestAProofTheWatchVouchesForStandsUntilItComesDue(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	e.apply(s)
	e.agent.Watching(true)
	e.clock.advance(time.Second)
	e.apply(s)
	require.Equal(t, []bool{false, false}, e.res.reuses(), "the watch ran before neither proof")

	e.clock.advance(time.Second)
	e.apply(s)
	e.clock.advance(time.Second)
	e.apply(s)

	require.Equal(t, []bool{false, false, true, true}, e.res.reuses())
	pins := e.agent.Bound()
	require.Len(t, pins, 1)
	pin := pins[guestAddr]
	require.Equal(t, testMAC, pin.MAC.String())
	require.Equal(t, egress.Pin{MAC: pin.MAC, Bridge: "vmbr0", Port: "tap101i0"}, pin)

	e.agent.Watching(false)
	e.clock.advance(time.Second)
	e.apply(s)
	require.False(t, e.res.reuses()[4], "a watch that stopped vouches for nothing")
}
