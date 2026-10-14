package daemon

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// On a node whose controller runs elsewhere, the agent drives the gateway,
// and what it gives it is kept as the cache a boot restores the namespace
// from, as the engine keeps it where it runs.
func TestTheAgentKeepsTheCacheOfTheGatewayItDrives(t *testing.T) {
	b := &fakeBackend{}
	local := t.TempDir()
	g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())
	d := agentDeps(nil, nil, nil, g, nodeProbe{}, nil)
	require.NotNil(t, d.Runner, "the agent writes guests' cards with qm and pct")

	_, err := d.Gateway.Sync(t.Context(), testCache.Legs, testCache.Services, nil, nil)

	require.NoError(t, err)
	cached, found, err := gateway.ReadCache(local)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, testCache.Legs, cached.Legs)
	require.Equal(t, testCache.Services, cached.Services)
	require.Equal(t, testCache.Legs, g.wantedNow().Legs, "the keeper brings back what the agent gave")
}

// unpinnedBackend has no namespace until the unit started it, as before a
// node's first leg.
type unpinnedBackend struct {
	fakeBackend
	unit *startedUnit
}

func (b *unpinnedBackend) Probe(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error) {
	if !b.unit.started {
		return nil, gateway.ErrNotPinned
	}
	mac, _ := net.ParseMAC("bc:24:11:00:00:09")
	return []net.HardwareAddr{mac}, nil
}

type startedUnit struct{ started, enabled bool }

func (u *startedUnit) Start(context.Context) error   { u.started = true; return nil }
func (u *startedUnit) Enable(context.Context) error  { u.enabled = true; return nil }
func (u *startedUnit) Disable(context.Context) error { return nil }

type ifacesOnly struct{ resolve.Prober }

func (ifacesOnly) Interfaces(context.Context) ([]resolve.HostIface, error) {
	return []resolve.HostIface{{Name: "vmbr1", Segment: resolve.Segment{Bridge: "vmbr1"}}}, nil
}

// The agent of a node probes through the gateway of its node, which starts
// the namespace for it when the node has no leg yet, and reads the
// interfaces the prober sees.
func TestTheAgentProbesThroughTheGatewayOfItsNode(t *testing.T) {
	unit := &startedUnit{}
	g := newGatewayPart(&unpinnedBackend{unit: unit}, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop())
	d := agentDeps(nil, nil, nil, g, nodeProbe{gw: g, prober: ifacesOnly{}, unit: unit}, nil)

	macs, err := d.ProbeAddr(t.Context(), "vmbr1", 0, netip.MustParseAddr("10.77.0.250"))

	require.NoError(t, err)
	require.True(t, unit.started)
	require.Len(t, macs, 1)
	ifaces, err := d.Interfaces(t.Context())
	require.NoError(t, err)
	require.Equal(t, "vmbr1", ifaces[0].Name)

	t.Run("without a gateway it probes nothing", func(t *testing.T) {
		d := agentDeps(nil, nil, nil, nil, nodeProbe{prober: ifacesOnly{}}, nil)
		require.Nil(t, d.ProbeAddr)
		require.Nil(t, d.Interfaces)
	})
}

// A node given its first leg by the controller has the namespace unit
// enabled and started, so that the leg is raised now and after a boot.
func TestTheAgentEnablesTheNamespaceForTheFirstLeg(t *testing.T) {
	unit := &startedUnit{}
	b := &unpinnedSync{unit: unit}
	g := newGatewayPart(b, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop())
	d := agentDeps(nil, nil, nil, g, nodeProbe{gw: g, prober: ifacesOnly{}, unit: unit}, nil)

	_, err := d.Gateway.Sync(t.Context(), nil, nil, nil, nil)
	require.ErrorIs(t, err, gateway.ErrNotPinned, "no leg: nothing is started")
	require.False(t, unit.started)

	_, err = d.Gateway.Sync(t.Context(), testCache.Legs, nil, nil, nil)

	require.NoError(t, err)
	require.True(t, unit.started)
	require.True(t, unit.enabled)
}

// unpinnedSync syncs only once the unit started the namespace.
type unpinnedSync struct {
	fakeBackend
	unit *startedUnit
}

func (b *unpinnedSync) Sync(ctx context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error) {
	if !b.unit.started {
		return nil, gateway.ErrNotPinned
	}
	return b.fakeBackend.Sync(ctx, legs, services, pins, sticky)
}
