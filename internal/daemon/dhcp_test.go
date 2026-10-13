package daemon

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet/dhcp"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeDHCP is a DHCP server that listens once the test lets it, and ends
// with the error the test gives it.
type fakeDHCP struct {
	cfg     dhcp.Config
	bound   atomic.Bool
	unknown atomic.Uint64
	forged  atomic.Uint64
	listen  chan struct{} // closed: Run binds
	fail    chan error    // what Run ends with
	running chan struct{} // closed once Run runs
	// closeGate, when set, holds the return of a stopped Run, as the close
	// of the sockets does, until the test closes it.
	closeGate chan struct{}
	ctx       atomic.Pointer[context.Context] // the context Run ran under
}

func newFakeDHCP(cfg dhcp.Config) *fakeDHCP {
	return &fakeDHCP{cfg: cfg, listen: make(chan struct{}), fail: make(chan error, 1), running: make(chan struct{})}
}

func (f *fakeDHCP) Run(ctx context.Context) error {
	f.ctx.Store(&ctx)
	close(f.running)
	select {
	case <-f.listen:
		f.bound.Store(true)
	case err := <-f.fail:
		return err
	case <-ctx.Done():
		return f.stopped()
	}
	defer f.bound.Store(false)
	select {
	case err := <-f.fail:
		return err
	case <-ctx.Done():
		return f.stopped()
	}
}

func (f *fakeDHCP) stopped() error {
	if f.closeGate != nil {
		<-f.closeGate
	}
	return nil
}

func (f *fakeDHCP) Bound() bool { return f.bound.Load() }

func (f *fakeDHCP) Stats() dhcp.Stats {
	return dhcp.Stats{Unknown: f.unknown.Load(), Spoofed: f.forged.Load()}
}

// dhcpNotes records what the part told the engine.
type dhcpNotes struct {
	mu    sync.Mutex
	views []*engine.DHCPView
	errs  []error
}

func (n *dhcpNotes) note(v *engine.DHCPView, problem error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.views = append(n.views, v)
	n.errs = append(n.errs, problem)
}

func (n *dhcpNotes) last() (*engine.DHCPView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.views) == 0 {
		return nil, nil
	}
	return n.views[len(n.views)-1], n.errs[len(n.errs)-1]
}

// dhcpTest is a part over networks the test sets, with servers it makes as
// fakes and keeps.
type dhcpTest struct {
	p       *dhcpPart
	mu      sync.Mutex
	nets    map[string]store.Network
	netsErr error
	servers []*fakeDHCP
	notes   *dhcpNotes
	now     time.Time
}

func managedNet(mark int, legs map[string]netip.Addr) store.Network {
	return store.Network{
		ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge,
		Prefix: netip.MustParsePrefix("10.203.118.0/24"), Legs: legs, Mark: mark, LegMAC: store.LegMACFor(store.NetworkManaged, mark),
		Scope: store.ScopeNode, CreatedAt: t0,
	}
}

var managedLegs = map[string]netip.Addr{testNode: netip.MustParseAddr("10.203.118.1")}

func newDHCPTest(t *testing.T, nets ...store.Network) *dhcpTest {
	t.Helper()
	d := &dhcpTest{nets: map[string]store.Network{}, notes: &dhcpNotes{}, now: t0}
	for _, n := range nets {
		d.nets[n.ID] = n
	}
	d.p = &dhcpPart{
		networks: func() (map[string]store.Network, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			return d.nets, d.netsErr
		},
		node: testNode,
		newServer: func(cfg dhcp.Config) DHCPServer {
			s := newFakeDHCP(cfg)
			d.mu.Lock()
			d.servers = append(d.servers, s)
			d.mu.Unlock()
			return s
		},
		note:  d.notes.note,
		now:   func() time.Time { d.mu.Lock(); defer d.mu.Unlock(); return d.now },
		after: time.After,
		log:   zerolog.New(zerolog.NewTestWriter(t)),
		wait:  defaultDHCPReadyWait,
	}
	t.Cleanup(d.p.shutdown)
	return d
}

func (d *dhcpTest) server(i int) *fakeDHCP {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i >= len(d.servers) {
		return nil
	}
	return d.servers[i]
}

func (d *dhcpTest) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.servers)
}

func (d *dhcpTest) advance(by time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.now = d.now.Add(by)
}

// bind lets server i listen and waits until it does.
func (d *dhcpTest) bind(t *testing.T, i int) {
	t.Helper()
	s := d.server(i)
	<-s.running
	close(s.listen)
	require.Eventually(t, s.Bound, time.Second, time.Millisecond)
}

// end has server i end with err and waits until the part sees it ended.
func (d *dhcpTest) end(t *testing.T, i int, err error) {
	t.Helper()
	s := d.server(i)
	<-s.running
	s.fail <- err
	require.Eventually(t, func() bool {
		d.p.mu.Lock()
		defer d.p.mu.Unlock()
		return d.p.cur != nil && d.p.cur.ended()
	}, time.Second, time.Millisecond)
}

func TestNoServerRunsWithoutAManagedLegOnTheNode(t *testing.T) {
	for name, nets := range map[string][]store.Network{
		"no network":                    nil,
		"an attached network":           {{ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Mark: 1, Legs: managedLegs}},
		"the managed leg on other node": {managedNet(2, map[string]netip.Addr{"pve2": netip.MustParseAddr("10.203.118.1")})},
	} {
		t.Run(name, func(t *testing.T) {
			d := newDHCPTest(t, nets...)
			d.p.check(t.Context())
			require.Zero(t, d.count())
			v, err := d.notes.last()
			require.Nil(t, v)
			require.NoError(t, err)
		})
	}
}

func TestTheServerRunsOnTheManagedLeg(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	d.p.check(t.Context())

	require.Equal(t, 1, d.count())
	require.Equal(t, dhcp.Config{
		Iface: "leg2", Mark: 2, Server: netip.MustParseAddr("10.203.118.1"), Subnet: netip.MustParsePrefix("10.203.118.0/24"),
		LeaseTime: 7 * 24 * time.Hour,
	}, d.server(0).cfg)
	v, err := d.notes.last()
	require.Equal(t, &engine.DHCPView{}, v, "not listening yet")
	require.NoError(t, err, "no problem while it starts")

	d.bind(t, 0)
	d.server(0).unknown.Store(4)
	d.server(0).forged.Store(2)
	d.advance(time.Second)
	d.p.check(t.Context())
	v, err = d.notes.last()
	require.Equal(t, &engine.DHCPView{Bound: true, Since: t0.Add(time.Second), Unknown: 4, Forged: 2}, v)
	require.NoError(t, err)
	require.Equal(t, 1, d.count(), "a server that listens is kept")
}

func TestAServerThatDoesNotListenInTimeIsAProblem(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	d.p.check(t.Context())
	d.advance(10 * time.Second)
	d.p.check(t.Context())

	v, err := d.notes.last()
	require.Equal(t, &engine.DHCPView{}, v)
	require.ErrorIs(t, err, errDHCPNotListening)
}

func TestAServerThatEndsRunsAgainAndSaysWhy(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	d.p.check(t.Context())
	d.bind(t, 0)
	d.server(0).unknown.Store(2)
	d.server(0).forged.Store(1)
	d.end(t, 0, errors.New("reading on leg2: network is down"))

	d.p.check(t.Context())
	require.Equal(t, 2, d.count(), "started again")
	require.Error(t, (*d.server(0).ctx.Load()).Err(), "the context of the run that ended is given back")
	v, err := d.notes.last()
	require.Equal(t, &engine.DHCPView{Unknown: 2, Forged: 1}, v, "the counts of the server before carry")
	require.EqualError(t, err, "reading on leg2: network is down", "until the new one listens")

	d.bind(t, 1)
	d.p.check(t.Context())
	v, err = d.notes.last()
	require.True(t, v.Bound)
	require.NoError(t, err)
}

func TestAChangedLegStartsTheServerAgainAndAGoneNetworkStopsIt(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	d.p.check(t.Context())
	d.bind(t, 0)

	d.mu.Lock()
	d.nets[gateway.ManagedBridge] = managedNet(3, managedLegs)
	d.mu.Unlock()
	d.p.check(t.Context())
	require.Equal(t, 2, d.count())
	require.False(t, d.server(0).Bound(), "the old one stopped")
	require.Equal(t, "leg3", d.server(1).cfg.Iface)
	_, err := d.notes.last()
	require.NoError(t, err, "a server stopped for a new leg is no problem")

	d.mu.Lock()
	delete(d.nets, gateway.ManagedBridge)
	d.mu.Unlock()
	d.p.check(t.Context())
	v, err := d.notes.last()
	require.Nil(t, v)
	require.NoError(t, err)
	d.p.mu.Lock()
	require.Nil(t, d.p.cur)
	d.p.mu.Unlock()
}

// A gateway namespace made again, as a restart of pco-netns makes it, has
// the server started again in it: its sockets were opened in the one before.
func TestAServerInANamespaceMadeAgainIsStartedAgain(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	var ns atomic.Value
	ns.Store("NS(4:4026531840)")
	d.p.nsID = func() string { return ns.Load().(string) }
	d.p.check(t.Context())
	d.bind(t, 0)

	d.p.check(t.Context())
	require.Equal(t, 1, d.count(), "the same namespace keeps the server")
	ns.Store("")
	d.p.check(t.Context())
	require.Equal(t, 1, d.count(), "a namespace that cannot be told keeps it too")

	ns.Store("NS(4:4026531999)")
	d.p.check(t.Context())
	require.Equal(t, 2, d.count())
	require.False(t, d.server(0).Bound(), "the old one stopped")
	_, err := d.notes.last()
	require.NoError(t, err, "a server stopped for the new namespace is no problem")
}

// A server is started again only once the one before has closed its
// sockets: port 67 and the leg's packet socket are held by one at a time.
func TestTheNextServerStartsOnceTheLastHasClosedItsSockets(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	d.p.check(t.Context())
	d.bind(t, 0)
	gate := make(chan struct{})
	d.server(0).closeGate = gate

	d.mu.Lock()
	d.nets[gateway.ManagedBridge] = managedNet(3, managedLegs)
	d.mu.Unlock()
	checked := make(chan struct{})
	go func() {
		d.p.check(t.Context())
		close(checked)
	}()
	require.Never(t, func() bool { return d.count() > 1 }, 50*time.Millisecond, 5*time.Millisecond, "started beside the old one")
	close(gate)
	<-checked
	require.Equal(t, 2, d.count())
	require.False(t, d.server(0).Bound())
}

// Checks run one at a time: the start's check and the timed one never start
// a server each for the same end.
func TestTwoChecksAtOnceStartOneServer(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	d.p.check(t.Context())
	for i := range 10 {
		d.end(t, i, errors.New("reading on leg2: network is down"))
		var wg sync.WaitGroup
		for range 2 {
			wg.Go(func() { d.p.check(t.Context()) })
		}
		wg.Wait()
		require.Equal(t, i+2, d.count(), "one server for one end")
	}
}

// Nothing starts a server after the stop: a check with the ended context,
// as the start's may run while the daemon stops, does nothing.
func TestNoServerStartsAfterTheStop(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	ctx, cancel := context.WithCancel(t.Context())
	d.p.check(ctx)
	d.bind(t, 0)

	cancel()
	d.p.shutdown()
	require.False(t, d.server(0).Bound())
	d.p.check(ctx)
	d.p.awaitBound(ctx)
	require.Equal(t, 1, d.count())
	d.p.mu.Lock()
	require.Nil(t, d.p.cur)
	d.p.mu.Unlock()
}

func TestAStoreThatCannotBeReadLeavesTheServerRunning(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	d.p.check(t.Context())
	d.bind(t, 0)
	d.mu.Lock()
	d.netsErr = errors.New("the store is gone")
	d.mu.Unlock()

	d.p.check(t.Context())
	require.Equal(t, 1, d.count())
	require.True(t, d.server(0).Bound())
}

func TestAnApplianceThatHasNotProvedItselfRunsNoServer(t *testing.T) {
	d := newDHCPTest(t, managedNet(2, managedLegs))
	var allowed atomic.Bool
	d.p.allowed = allowed.Load
	d.p.check(t.Context())
	require.Zero(t, d.count())

	allowed.Store(true)
	d.p.check(t.Context())
	require.Equal(t, 1, d.count())
}

func TestTheStartWaitsForTheServerAtMostItsWait(t *testing.T) {
	t.Run("until it listens", func(t *testing.T) {
		d := newDHCPTest(t, managedNet(2, managedLegs))
		d.p.check(t.Context())
		s := d.server(0)
		go func() {
			<-s.running
			close(s.listen)
		}()
		d.p.awaitBound(t.Context())
		require.True(t, d.server(0).Bound())
		v, _ := d.notes.last()
		require.True(t, v.Bound, "the engine is told at once")
	})
	t.Run("not past the wait", func(t *testing.T) {
		d := newDHCPTest(t, managedNet(2, managedLegs))
		expired := make(chan time.Time)
		close(expired)
		d.p.after = func(time.Duration) <-chan time.Time { return expired }
		d.p.check(t.Context())
		d.advance(10 * time.Second)
		d.p.awaitBound(t.Context())
		_, err := d.notes.last()
		require.ErrorIs(t, err, errDHCPNotListening)
	})
	t.Run("not for a server that ended", func(t *testing.T) {
		d := newDHCPTest(t, managedNet(2, managedLegs))
		d.p.after = func(time.Duration) <-chan time.Time { return nil } // never
		d.p.ctx = t.Context()
		d.p.check(t.Context())
		d.end(t, 0, errors.New("opening the sockets on leg2: the gateway namespace is not open"))
		startCtx, cancelStart := context.WithCancel(t.Context())
		returned := make(chan struct{})
		go func() {
			d.p.awaitBound(startCtx)
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(5 * time.Second):
			t.Fatal("the start waited for a server that ended")
		}
		require.Equal(t, 2, d.count(), "the check started it again")
		_, err := d.notes.last()
		require.EqualError(t, err, "opening the sockets on leg2: the gateway namespace is not open")

		cancelStart()
		<-d.server(1).running
		require.NoError(t, (*d.server(1).ctx.Load()).Err(), "the server runs under the part's context, not the start's")
		d.p.check(t.Context())
		require.Equal(t, 2, d.count(), "and is kept")
	})
	t.Run("not at all without a managed leg", func(t *testing.T) {
		d := newDHCPTest(t)
		d.p.after = func(time.Duration) <-chan time.Time { t.Fatal("waited"); return nil }
		d.p.awaitBound(t.Context())
	})
}

// The daemon tells systemd that it is ready once the DHCP server of the
// managed network listens: the socket answers before, and READY=1 waits.
func TestReadyWaitsForTheDHCPServer(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, w.store.SaveNetwork(managedNet(2, managedLegs), 0))
	made := make(chan *fakeDHCP, 4)
	w.deps.NewDHCP = func(cfg dhcp.Config, _ dhcp.Leases) DHCPServer {
		s := newFakeDHCP(cfg)
		made <- s
		return s
	}
	var boundAtReady, readyBeforeBound atomic.Bool
	var first atomic.Pointer[fakeDHCP]
	w.notify.onReady = func() { boundAtReady.Store(first.Load() != nil && first.Load().Bound()) }
	go func() {
		s := <-made
		first.Store(s)
		<-s.running
		client := apiclient.New(w.cfg.SocketPath)
		for {
			if _, err := client.Version(context.Background()); err == nil {
				break
			}
			time.Sleep(time.Millisecond)
		}
		readyBeforeBound.Store(len(w.notify.sent()) > 0)
		close(s.listen)
	}()

	d := w.start()

	require.False(t, readyBeforeBound.Load(), "the socket answered and READY=1 was not sent yet")
	require.True(t, boundAtReady.Load(), "READY=1 came after the server listened")
	d.await(func(st engine.State) bool { return st.Managed.DHCP != nil && st.Managed.DHCP.Bound })
}

// A server that does not listen holds the start for no longer than the
// wait, and says so as a problem line.
func TestReadyComesWithAProblemWhenTheDHCPServerDoesNotListen(t *testing.T) {
	w := newWorld(t)
	require.NoError(t, w.store.SaveNetwork(managedNet(2, managedLegs), 0))
	w.deps.NewDHCP = func(cfg dhcp.Config, _ dhcp.Leases) DHCPServer { return newFakeDHCP(cfg) }
	w.deps.DHCPReadyWait = 50 * time.Millisecond

	d := w.start()

	require.Equal(t, []string{"READY=1"}, w.notify.sent())
	d.await(func(st engine.State) bool {
		return slices.Contains(st.Problems, "the DHCP server is not listening on the managed leg: its sockets are not open")
	})
}
