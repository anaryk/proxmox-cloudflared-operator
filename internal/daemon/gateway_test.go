package daemon

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeBackend is a gateway backend that records what it is asked, in order.
type fakeBackend struct {
	mu        sync.Mutex
	calls     []string
	verify    []error // answered in turn; the last one stays
	upErr     error
	syncErr   error
	detachErr error
	down      map[string]bool // legs Sync reports not up
	synced    []gateway.Cache // what every Sync was given
}

func (f *fakeBackend) record(call string) {
	f.calls = append(f.calls, call)
}

func (f *fakeBackend) Up(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("up")
	return f.upErr
}

func (f *fakeBackend) Sync(_ context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("sync")
	f.synced = append(f.synced, gateway.Cache{Legs: legs, Services: services, Pins: pins, Sticky: sticky})
	if f.syncErr != nil {
		return nil, f.syncErr
	}
	var states []gateway.LegState
	for _, l := range legs {
		states = append(states, gateway.LegState{ID: l.ID, Iface: l.Iface(), Up: !f.down[l.ID]})
	}
	return states, nil
}

func (f *fakeBackend) Verify(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("verify")
	if len(f.verify) == 0 {
		return nil
	}
	err := f.verify[0]
	if len(f.verify) > 1 {
		f.verify = f.verify[1:]
	}
	return err
}

func (f *fakeBackend) Probe(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error) {
	return nil, nil
}

func (f *fakeBackend) Detach(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("detach " + id)
	return f.detachErr
}

func (f *fakeBackend) Down(context.Context) error { return nil }
func (f *fakeBackend) Netns() netns.NsHandle      { return netns.None() }

func (f *fakeBackend) Dial(context.Context, gateway.Leg, netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("no dial in the fake")
}

func (f *fakeBackend) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeBackend) lastSync() gateway.Cache {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced[len(f.synced)-1]
}

func testLeg(id string, mark int) gateway.Leg {
	return gateway.Leg{
		ID: id, Mark: mark, Bridge: id, Prefix: netip.MustParsePrefix("10.50.0.250/24"),
		MAC: net.HardwareAddr{0x02, 0x50, 0x43, 0x4f, 0x00, byte(mark)},
	}
}

var testCache = gateway.Cache{
	Legs: []gateway.Leg{testLeg("vmbr1", 1), testLeg("vmbr2", 2)},
	Services: []gateway.Service{{
		Leg: "vmbr1", Service: netip.MustParseAddr("198.18.1.0"), Addr: netip.MustParseAddr("10.50.0.10"), Ports: []uint16{8080},
	}},
}

var errNamespaceChanged = &gateway.ChangedError{Differences: []string{"leg1 is down"}}

func TestTheGatewayKeepsWhatItWasLastGiven(t *testing.T) {
	b := &fakeBackend{down: map[string]bool{"vmbr2": true}}
	g := newGatewayPart(b, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop())
	require.False(t, g.active(), "nothing was wanted yet")
	require.Empty(t, g.UpLegs())

	states, err := g.Sync(t.Context(), testCache.Legs, testCache.Services, nil, nil)

	require.NoError(t, err)
	require.Len(t, states, 2)
	require.True(t, g.active())
	require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, g.UpLegs(), "only the legs that are up are probed through")

	b.syncErr = errors.New("nft: busy")
	_, err = g.Sync(t.Context(), testCache.Legs[:1], nil, nil, nil)
	require.EqualError(t, err, "nft: busy")
	require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, g.UpLegs(), "a sync that failed says nothing of the legs")
	require.Equal(t, testCache.Legs[:1], g.wantedNow().Legs, "but is what is wanted now")
}

// A detached network is no longer wanted: the keeper does not bring its leg
// back before the next cycle syncs the gateway without it, nor does a boot
// before that cycle.
func TestADetachedLegIsNoLongerWanted(t *testing.T) {
	b := &fakeBackend{}
	local := t.TempDir()
	g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())
	pins := []gateway.Pin{{Leg: "vmbr1", Addr: netip.MustParseAddr("10.50.0.10"), MAC: net.HardwareAddr{0xbc, 0x24, 0x11, 0, 0, 1}}}
	_, err := g.Sync(t.Context(), testCache.Legs, testCache.Services, pins, nil)
	require.NoError(t, err)

	require.NoError(t, g.Detach(t.Context(), "vmbr1"))

	require.Equal(t, []string{"sync", "detach vmbr1"}, b.asked())
	want := g.wantedNow()
	require.Equal(t, []gateway.Leg{testLeg("vmbr2", 2)}, want.Legs)
	require.Empty(t, want.Services)
	require.Empty(t, want.Pins)
	require.Equal(t, []gateway.Leg{testLeg("vmbr2", 2)}, g.UpLegs())
	cached, found, err := gateway.ReadCache(local)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want.Legs, cached.Legs)
	require.Empty(t, cached.Services)

	require.NoError(t, g.restore(t.Context()))
	require.Equal(t, []gateway.Leg{testLeg("vmbr2", 2)}, b.lastSync().Legs, "the keeper brings back what is wanted, without the leg")

	b.detachErr = errors.New("the leg is busy")
	require.EqualError(t, g.Detach(t.Context(), "vmbr2"), "the leg is busy")
	require.Equal(t, []gateway.Leg{testLeg("vmbr2", 2)}, g.wantedNow().Legs, "a detach that failed changes nothing of what is wanted")
}

// At its start the daemon takes the cache pco netns up restored the gateway
// from as what it expects, the legs of a gateway as the cache says as up, and
// restores one that differs from the cache, before the first cycle probes
// through its legs.
func TestTheGatewayStartsFromItsCache(t *testing.T) {
	pins := []gateway.Pin{{Leg: "vmbr1", Addr: netip.MustParseAddr("10.50.0.10"), MAC: net.HardwareAddr{0xbc, 0x24, 0x11, 0, 0, 1}}}
	t.Run("in place", func(t *testing.T) {
		local := t.TempDir()
		require.NoError(t, gateway.WriteCache(local, testCache))
		b := &fakeBackend{}
		g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())

		g.start(t.Context())

		require.True(t, g.active())
		require.Equal(t, testCache.Legs, g.UpLegs())
		require.Equal(t, []string{"verify"}, b.asked())
	})
	t.Run("changed", func(t *testing.T) {
		local := t.TempDir()
		c := testCache
		c.Pins, c.PinsAt = pins, t0.Add(-time.Minute)
		require.NoError(t, gateway.WriteCache(local, c))
		b := &fakeBackend{verify: []error{errNamespaceChanged}, down: map[string]bool{"vmbr2": true}}
		g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())

		g.start(t.Context())

		require.Equal(t, []string{"verify", "up", "sync"}, b.asked())
		require.Equal(t, pins, b.lastSync().Pins, "fresh pins go with it")
		require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, g.UpLegs())
	})
	t.Run("with stale pins", func(t *testing.T) {
		local := t.TempDir()
		c := testCache
		c.Pins, c.PinsAt = pins, t0.Add(-time.Hour)
		require.NoError(t, gateway.WriteCache(local, c))
		b := &fakeBackend{verify: []error{errNamespaceChanged}}
		g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())

		g.start(t.Context())

		require.Empty(t, b.lastSync().Pins, "a pin to a MAC a guest may no longer have would cut it off")
		require.Empty(t, g.wantedNow().Pins)
	})
	t.Run("not read", func(t *testing.T) {
		local := t.TempDir()
		require.NoError(t, gateway.WriteCache(local, testCache))
		b := &fakeBackend{verify: []error{errors.New("listing the table: nft: signal: killed")}}
		g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())

		g.start(t.Context())

		require.Equal(t, testCache.Legs, g.UpLegs(), "taken as the cache says: the first cycle's sync corrects it")
		require.Equal(t, []string{"verify"}, b.asked())
	})
	t.Run("gone", func(t *testing.T) {
		local := t.TempDir()
		require.NoError(t, gateway.WriteCache(local, testCache))
		b := &fakeBackend{verify: []error{errNamespaceChanged}, upErr: gateway.ErrNotPinned}
		g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())

		g.start(t.Context())

		require.True(t, g.active(), "the keeper says so until pco-netns runs again")
		require.Empty(t, g.UpLegs())
	})
	t.Run("no cache", func(t *testing.T) {
		b := &fakeBackend{}
		g := newGatewayPart(b, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop())

		g.start(t.Context())

		require.False(t, g.active(), "a node that never attached a network has no gateway to keep")
		require.Empty(t, b.asked())
	})
}

type gatewayRig struct {
	k     *keeper
	b     *fakeBackend
	g     *gatewayPart
	now   time.Time
	notes []engine.GatewayCheck
	idles int
}

func newGatewayRig(t *testing.T, verify ...error) *gatewayRig {
	t.Helper()
	r := &gatewayRig{b: &fakeBackend{verify: verify}, now: t0}
	r.g = newGatewayPart(r.b, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop())
	_, err := r.g.Sync(t.Context(), testCache.Legs, testCache.Services, nil, nil)
	require.NoError(t, err)
	r.b.calls = nil
	r.k = &keeper{
		gw:     r.g,
		noteGw: func(c engine.GatewayCheck) { r.notes = append(r.notes, c) },
		idle: func(_ context.Context, fn func()) error {
			r.idles++
			fn()
			return nil
		},
		now: func() time.Time { return r.now },
		log: zerolog.Nop(),
	}
	return r
}

func TestTheKeeperBringsTheGatewayBack(t *testing.T) {
	tests := []struct {
		name   string
		verify error
		upErr  error
		note   engine.GatewayCheck
		calls  []string
	}{
		{name: "in place", note: engine.GatewayCheck{}, calls: []string{"verify"}},
		{name: "changed and brought back", verify: errNamespaceChanged,
			note: engine.GatewayCheck{Restored: "leg1 is down"}, calls: []string{"verify", "up", "sync"}},
		{name: "gone", verify: &gateway.ChangedError{Differences: []string{"the namespace pco is not pinned"}}, upErr: gateway.ErrNotPinned,
			note: engine.GatewayCheck{NotKept: "the gateway namespace is not pinned; systemctl restart pco-netns"}, calls: []string{"verify", "up"}},
		{name: "not read", verify: errors.New("listing the table: nft: signal: killed"),
			note: engine.GatewayCheck{Failed: "listing the table: nft: signal: killed"}, calls: []string{"verify"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newGatewayRig(t, tc.verify)
			r.b.upErr = tc.upErr

			require.Zero(t, r.k.checkGateway(t.Context()))

			require.Equal(t, []engine.GatewayCheck{tc.note}, r.notes)
			require.Equal(t, tc.calls, r.b.asked())
			if slices.Contains(tc.calls, "sync") {
				require.Equal(t, testCache.Legs, r.b.lastSync().Legs, "synced with what was last wanted")
				require.Equal(t, testCache.Services, r.b.lastSync().Services)
				require.Equal(t, 1, r.idles, "while nothing probes through it")
			}
		})
	}
}

// An appliance that has not proved itself changes nothing of its gateway
// outside a cycle: a copy would add the original's cards to itself.
func TestAGatewayNotAllowedToChangeIsLeftToTheCycle(t *testing.T) {
	t.Run("at the start", func(t *testing.T) {
		local := t.TempDir()
		require.NoError(t, gateway.WriteCache(local, testCache))
		b := &fakeBackend{verify: []error{errNamespaceChanged}}
		g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())
		g.allowed = func() bool { return false }

		g.start(t.Context())

		require.Equal(t, []string{"verify"}, b.asked())
		require.Equal(t, testCache.Legs, g.UpLegs())
	})
	t.Run("in the keeper", func(t *testing.T) {
		r := newGatewayRig(t, errNamespaceChanged)
		r.g.allowed = func() bool { return false }

		require.Zero(t, r.k.checkGateway(t.Context()))

		require.Equal(t, []string{"verify"}, r.b.asked())
		require.Empty(t, r.notes)
	})
	t.Run("and the cycle brings it back", func(t *testing.T) {
		local := t.TempDir()
		require.NoError(t, gateway.WriteCache(local, testCache))
		b := &fakeBackend{verify: []error{errNamespaceChanged}, down: map[string]bool{"vmbr2": true}}
		g := newGatewayPart(b, local, func() time.Time { return t0 }, zerolog.Nop())
		g.allowed = func() bool { return false }
		g.start(t.Context())

		require.Equal(t, engine.GatewayCheck{Restored: "leg1 is down"}, g.Check(t.Context()))

		require.Equal(t, []string{"verify", "verify", "up", "sync"}, b.asked())
		require.Equal(t, testCache.Services, b.lastSync().Services, "with what the cache says")
		require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, g.UpLegs(), "and the legs are as the sync found them")
	})
}

func TestTheCycleChecksTheGateway(t *testing.T) {
	tests := []struct {
		name   string
		verify error
		upErr  error
		found  engine.GatewayCheck
		calls  []string
	}{
		{name: "in place", calls: []string{"verify"}},
		{name: "changed and brought back", verify: errNamespaceChanged,
			found: engine.GatewayCheck{Restored: "leg1 is down"}, calls: []string{"verify", "up", "sync"}},
		{name: "gone", verify: &gateway.ChangedError{Differences: []string{"the namespace pco is not pinned"}}, upErr: gateway.ErrNotPinned,
			found: engine.GatewayCheck{NotKept: "the gateway namespace is not pinned; systemctl restart pco-netns"}, calls: []string{"verify", "up"}},
		{name: "not read", verify: errors.New("listing the table: nft: signal: killed"),
			found: engine.GatewayCheck{Failed: "listing the table: nft: signal: killed"}, calls: []string{"verify"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newGatewayRig(t, tc.verify)
			r.b.upErr = tc.upErr

			require.Equal(t, tc.found, r.g.Check(t.Context()))

			require.Equal(t, tc.calls, r.b.asked())
		})
	}
	t.Run("nothing wanted", func(t *testing.T) {
		b := &fakeBackend{verify: []error{errNamespaceChanged}}
		g := newGatewayPart(b, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop())

		require.Equal(t, engine.GatewayCheck{}, g.Check(t.Context()))

		require.Empty(t, b.asked(), "the cycle's sync makes it")
	})
}

func TestTheKeeperBringsTheGatewayBackAtMostOnceEveryFiveSeconds(t *testing.T) {
	r := newGatewayRig(t, errNamespaceChanged)
	r.k.checkGateway(t.Context())

	r.now = t0.Add(time.Second)
	require.Equal(t, 4*time.Second, r.k.checkGateway(t.Context()))
	require.Equal(t, []string{"verify", "up", "sync", "verify"}, r.b.asked())

	r.now = t0.Add(5 * time.Second)
	r.k.checkGateway(t.Context())
	require.Equal(t, []string{"verify", "up", "sync", "verify", "verify", "up", "sync"}, r.b.asked())
}

func TestTheKeeperLeavesAGatewayNothingWasWantedOfAlone(t *testing.T) {
	b := &fakeBackend{}
	k := &keeper{gw: newGatewayPart(b, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop()), noteGw: func(engine.GatewayCheck) { t.Fatal("noted") },
		now: func() time.Time { return t0 }, log: zerolog.Nop()}

	k.checkGateway(t.Context())

	require.Empty(t, b.asked())
}

// The daemon takes the cache of the gateway at its start, the engine syncs
// the gateway with the legs of the networks of the node, and the cache is
// written again after the sync.
func TestTheDaemonDrivesTheGatewayOfItsNetworks(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, gateway.WriteCache(w.paths.Local, testCache))
	require.NoError(t, w.store.SaveNetwork(store.Network{
		ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.50.0.0/24"),
		Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.50.0.250")}, LegMAC: store.LegMACFor(store.NetworkAttach, 1),
		Mark: 1, CreatedAt: t0,
	}, 0))
	d := w.start()

	require.NoError(t, d.client.Sync(t.Context()))
	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })

	asked := w.gw.asked()
	require.Equal(t, "verify", asked[0], "the cache is checked at the start")
	require.Contains(t, asked, "sync")
	require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, w.gw.lastSync().Legs)
	c, found, err := gateway.ReadCache(w.paths.Local)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, c.Legs, "written again after the sync")
}

// The appliance runs the gateway as the host does: the cache at its start,
// the sync of every cycle with the legs of the networks of its node, and the
// keeper's check when a link or a route of the container changes.
func TestTheApplianceDrivesTheGatewayOfItsNetworks(t *testing.T) {
	a := newApplianceWorld(t)
	require.NoError(t, gateway.WriteCache(a.paths.Local, testCache))
	require.NoError(t, a.store.SaveNetwork(store.Network{
		ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.50.0.0/24"),
		Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.50.0.250")}, LegMAC: store.LegMACFor(store.NetworkAttach, 1),
		Mark: 1, CreatedAt: t0,
	}, 0))
	changedc := make(chan func(), 1)
	a.deps.WatchGateway = func(ctx context.Context, changed func()) error {
		changedc <- changed
		<-ctx.Done()
		return ctx.Err()
	}
	a.gw.verify = []error{errNamespaceChanged}
	d := a.start()
	verifies := func() int {
		n := 0
		for _, call := range a.gw.asked() {
			if call == "verify" {
				n++
			}
		}
		return n
	}

	d.await(func(st engine.State) bool { return st.Identity != nil && st.Identity.OK })
	d.await(func(st engine.State) bool {
		return slices.Contains(st.Problems, "the gateway namespace was changed outside pco and was brought back (leg1 is down)")
	})
	require.Equal(t, []string{"verify", "verify", "up", "sync"}, a.gw.asked()[:4],
		"the start left it as it was, since nothing said yet that this is not a copy; the first cycle brought it back")
	require.NoError(t, d.client.Sync(t.Context()))
	d.await(func(st engine.State) bool { return len(st.Routes) == 1 })

	require.Equal(t, "verify", a.gw.asked()[0], "the cache is checked at the start")
	require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, a.gw.lastSync().Legs)
	c, found, err := gateway.ReadCache(a.paths.Local)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []gateway.Leg{testLeg("vmbr1", 1)}, c.Legs)

	before := verifies()
	(<-changedc)()
	require.Eventually(t, func() bool { return verifies() > before }, 10*time.Second, 5*time.Millisecond, "the keeper checks it")
}

// A change of a link or a route of the node has the gateway checked, and
// nothing else.
func TestTheKeeperChecksTheGatewayOnLinkAndRouteChanges(t *testing.T) {
	r := newGatewayRig(t)
	table := &countingTable{}
	r.k.table, r.k.note = table, func(engine.EgressCheck) {}
	r.k.off = func() (time.Time, bool, error) { return time.Time{}, false, nil }
	var mu sync.Mutex
	verified := func() int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Index(r.b.asked(), "verify")
	}
	noted := r.k.noteGw
	r.k.noteGw = func(c engine.GatewayCheck) { mu.Lock(); defer mu.Unlock(); noted(c) }
	changedc := make(chan func(), 1)
	r.k.watchNet = func(ctx context.Context, changed func()) error {
		changedc <- changed
		<-ctx.Done()
		return ctx.Err()
	}
	ruleset := func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.k.keep(ctx, ruleset, time.Hour, sleepContext)
	}()

	change := <-changedc
	for range 10 {
		change()
	}
	require.Eventually(t, func() bool { return verified() >= 0 }, 5*time.Second, time.Millisecond)
	cancel()
	<-done
	require.Zero(t, table.verifies.Load(), "the egress table is not checked for it")
}

// countingTable is an egress table in place that counts its checks.
type countingTable struct{ verifies atomic.Int32 }

func (c *countingTable) Verify(context.Context) error {
	c.verifies.Add(1)
	return nil
}

func (c *countingTable) Reapply(context.Context) error { return nil }
