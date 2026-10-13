package agent

import (
	"context"
	"encoding/json"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// fakeIsolation is what an appliance agent reads of its node for the
// filtered level: a test sets what it finds.
type fakeIsolation struct {
	mu     sync.Mutex
	in     managednet.FilteredInput
	sinces []time.Time
}

func (f *fakeIsolation) read(_ context.Context, since time.Time) managednet.FilteredInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinces = append(f.sinces, since)
	return f.in
}

func (f *fakeIsolation) set(in managednet.FilteredInput) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in = in
}

func (f *fakeIsolation) asked() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.sinces...)
}

// isolated is what the predicate reads of a node whose vnet holds.
func isolated() managednet.FilteredInput {
	return managednet.FilteredInput{
		Node: testNode, NodeFirewall: pve.NodeFirewallOptions{Enabled: true}, ClusterFirewall: pve.ClusterFirewallOptions{Enabled: true}, Service: "running",
	}
}

// onManaged is an appliance agent with a route served through the managed
// network at observed, and what it reads of its node.
func onManaged(t *testing.T, required string) (*env, *fakeIsolation, agentproto.Snapshot) {
	t.Helper()
	e := newEnv(t)
	e.kind = pki.KindAppliance
	e.gw = &fakeGateway{}
	iso := &fakeIsolation{in: isolated()}
	e.isolation = iso.read
	e.agent = e.newAgent()
	e.res.onLeg("www.example.com", gateway.ManagedBridge)
	e.res.observe("www.example.com")
	legs, err := json.Marshal(gateway.Cache{Legs: []gateway.Leg{{
		ID: gateway.ManagedBridge, Mark: 1, Bridge: gateway.ManagedBridge, Prefix: netip.MustParsePrefix("10.203.118.1/24"),
		MAC: mustMAC("02:50:43:4f:00:01"), Managed: true,
	}}})
	require.NoError(t, err)
	s := e.snapshot("www.example.com -> :8080")
	s.Networks = legs
	s.Targets[0].Addr, s.Targets[0].Service, s.Targets[0].Required = guestAddr, netip.MustParseAddr("10.203.118.5"), required
	return e, iso, s
}

func TestAnApplianceAgentRaisesTheManagedNetworkToFilteredWhileItsVnetIsIsolated(t *testing.T) {
	e, iso, s := onManaged(t, "observed")

	st := e.apply(s)

	r := report(st, "www.example.com")
	require.Equal(t, "filtered", r.Level)
	require.True(t, r.OK)
	require.Equal(t, []time.Time{{}}, iso.asked(), "the first judgment reads everything")
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, resolve.LevelFiltered, bindings["www.example.com"].Level)
	require.Empty(t, e.events.messages())

	t.Run("and takes it back when it is not", func(t *testing.T) {
		in := isolated()
		in.Service = "stopped"
		iso.set(in)
		e.clock.advance(10 * time.Second)

		st := e.apply(s)

		require.Equal(t, "observed", report(st, "www.example.com").Level)
		require.Equal(t, t0, iso.asked()[1], "unknown MACs since the last judgment")
		require.Contains(t, st.Problems, "filtered lost: pve-firewall is stopped on node pve1; the routes on pconet are served at observed")
		bindings, err := e.store.Bindings()
		require.NoError(t, err)
		require.Equal(t, resolve.LevelObserved, bindings["www.example.com"].Level)
		require.Equal(t, []string{"filtered lost: pve-firewall is stopped on node pve1; the routes on pconet are served at observed"}, e.events.messages())

		e.apply(s)
		require.Len(t, e.events.messages(), 1, "said once while the reason stays")
	})

	t.Run("and raises it again once it holds", func(t *testing.T) {
		iso.set(isolated())

		st := e.apply(s)

		require.Equal(t, "filtered", report(st, "www.example.com").Level)
		require.Equal(t, "filtered holds on pconet again: every guest on the vnet is isolated by its firewall", e.events.messages()[1])
	})
}

func TestARouteThatRequiresFilteredIsHeldBackWhileTheVnetIsNotIsolated(t *testing.T) {
	e, iso, s := onManaged(t, "filtered")
	in := isolated()
	in.Unknown = []string{"bc:24:11:00:00:ff"}
	iso.set(in)

	st := e.apply(s)

	r := report(st, "www.example.com")
	require.False(t, r.OK)
	require.False(t, r.InEgressSet)
	require.Equal(t, "identity level observed is below the required filtered", r.Reason)
	require.Contains(t, st.Problems, "filtered lost: a frame from bc:24:11:00:00:ff, a MAC no lease knows, was seen on the vnet; the routes on pconet are served at observed")

	// The reader is asked for the frames since the last judgment; a MAC it
	// still reports fails every judgment, not only the first.
	t.Run("and held while the MAC is still unknown", func(t *testing.T) {
		e.clock.advance(10 * time.Second)
		st := e.apply(s)
		require.Equal(t, []time.Time{{}, t0}, iso.asked())
		require.False(t, report(st, "www.example.com").InEgressSet)
		require.Contains(t, st.Problems, "filtered lost: a frame from bc:24:11:00:00:ff, a MAC no lease knows, was seen on the vnet; the routes on pconet are served at observed")
	})

	t.Run("and served once it is", func(t *testing.T) {
		iso.set(isolated())
		st := e.apply(s)
		require.True(t, report(st, "www.example.com").InEgressSet)
	})
}

func TestTheFilteredLevelIsJudgedOnlyOnAnApplianceWithTheManagedLeg(t *testing.T) {
	t.Run("a host agent judges nothing", func(t *testing.T) {
		e, iso, s := onManaged(t, "observed")
		e.kind = pki.KindHost
		e.agent = e.newAgent()

		st := e.apply(s)

		require.Equal(t, "observed", report(st, "www.example.com").Level)
		require.Empty(t, iso.asked())
	})

	t.Run("nor an appliance without the leg", func(t *testing.T) {
		e, iso, s := onManaged(t, "observed")
		s.Networks = nil

		st := e.apply(s)

		require.Equal(t, "observed", report(st, "www.example.com").Level)
		require.Empty(t, iso.asked())
	})

	t.Run("nor one that reads nothing of its node", func(t *testing.T) {
		e, _, s := onManaged(t, "observed")
		e.isolation = nil
		e.agent = e.newAgent()

		st := e.apply(s)

		require.Equal(t, "observed", report(st, "www.example.com").Level)
	})
}
