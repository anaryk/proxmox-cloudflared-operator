package daemon

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var guestEntry = gateway.Sticky{MAC: net.HardwareAddr{0x06, 0x50, 0x43, 0x4f, 0x76, 0x02}, Bridge: gateway.ManagedBridge, Port: "veth101i1"}

// stickyRig is a gateway with one wanted sticky entry, whose port the test
// makes appear, and that records what ApplySticky is given.
type stickyRig struct {
	*gatewayRig
	mu      sync.Mutex
	ports   map[string]bool
	applied int
	err     error
}

func newStickyRig(t *testing.T, portThere bool) *stickyRig {
	t.Helper()
	r := &stickyRig{gatewayRig: newGatewayRig(t), ports: map[string]bool{"veth101i1": portThere}}
	r.g.portExists = func(name string) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.ports[name]
	}
	r.g.applySticky = func(_ context.Context, _ gateway.Backend, sticky []gateway.Sticky) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		require.Equal(t, []gateway.Sticky{guestEntry}, sticky)
		r.b.mu.Lock()
		r.b.record("apply sticky")
		r.b.mu.Unlock()
		if r.err != nil {
			return r.err
		}
		r.applied++
		return nil
	}
	_, err := r.g.Sync(t.Context(), testCache.Legs, testCache.Services, nil, []gateway.Sticky{guestEntry})
	require.NoError(t, err)
	r.b.mu.Lock()
	r.b.calls = nil
	r.b.mu.Unlock()
	return r
}

func (r *stickyRig) setPort(there bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ports["veth101i1"] = there
}

func (r *stickyRig) appliedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.applied
}

// A guest that starts after the last sync has its port given its entry by
// the keeper's next check, before the check compares: the port is no change
// of the gateway. A port that was there at the sync needs nothing.
func TestTheKeeperGivesAPortThatAppearedItsEntryBeforeItChecks(t *testing.T) {
	r := newStickyRig(t, false)
	require.Zero(t, r.k.checkGateway(t.Context()))
	require.Equal(t, []string{"verify"}, r.b.asked(), "no port, nothing to give")

	r.setPort(true)
	r.b.calls = nil
	require.Zero(t, r.k.checkGateway(t.Context()))
	require.Equal(t, []string{"apply sticky", "verify"}, r.b.asked())
	require.Equal(t, []engine.GatewayCheck{{}, {}}, r.notes)

	r.b.calls = nil
	require.Zero(t, r.k.checkGateway(t.Context()))
	require.Equal(t, []string{"verify"}, r.b.asked(), "given once")

	// The guest stops and starts: its port went and came back.
	r.setPort(false)
	require.Zero(t, r.k.checkGateway(t.Context()))
	r.setPort(true)
	r.b.calls = nil
	require.Zero(t, r.k.checkGateway(t.Context()))
	require.Equal(t, []string{"apply sticky", "verify"}, r.b.asked())
}

func TestAPortThatWasThereAtTheSyncNeedsNothing(t *testing.T) {
	r := newStickyRig(t, true)
	require.Zero(t, r.k.checkGateway(t.Context()))
	require.Equal(t, []string{"verify"}, r.b.asked())
}

// The port watch names a port that joined the bridge: it is given its entry
// though the keeper saw a port of that name before, as a guest that
// restarts between two checks makes it anew.
func TestAJoinedPortIsGivenItsEntryAtOnce(t *testing.T) {
	r := newStickyRig(t, true)
	require.NoError(t, r.g.keepSticky(t.Context(), "veth101i1"))
	require.Equal(t, 1, r.appliedCount())
	require.NoError(t, r.g.keepSticky(t.Context(), "tap999i0"), "a port of no entry")
	require.Equal(t, 1, r.appliedCount())
}

func TestAnEntryThatCouldNotBeGivenIsTriedAgain(t *testing.T) {
	r := newStickyRig(t, false)
	r.setPort(true)
	r.err = errors.New("netlink: no such device")
	require.Error(t, r.g.keepSticky(t.Context(), ""))
	r.err = nil
	require.NoError(t, r.g.keepSticky(t.Context(), ""))
	require.Equal(t, 1, r.appliedCount())
}

// An appliance that has not proved itself changes nothing outside a cycle.
func TestNoEntryIsGivenWhileTheGatewayMayNotChange(t *testing.T) {
	r := newStickyRig(t, false)
	r.g.allowed = func() bool { return false }
	r.setPort(true)
	require.NoError(t, r.g.keepSticky(t.Context(), "veth101i1"))
	require.Zero(t, r.appliedCount())
}

func TestThePortWatchGivesTheEntries(t *testing.T) {
	r := newStickyRig(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	watched := make(chan string, 1)
	watch := func(ctx context.Context, _ netns.NsHandle, bridge string, joined func(string)) error {
		watched <- bridge
		joined("veth101i1")
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.g.watchPorts(ctx, watch, sleepContext)
	}()
	require.Equal(t, gateway.ManagedBridge, <-watched)
	require.Eventually(t, func() bool { return r.appliedCount() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	<-done
}

// sourceRig runs the watch of the managed leg against a fake listener that
// hands the test the watch it was started with.
type sourceRig struct {
	s       *sourceWatch
	g       *gatewayPart
	b       *fakeBackend
	now     time.Time
	leases  map[string]store.Lease
	reads   int
	readErr error
	seen    []string
	started chan gateway.SourceWatch
	devs    chan string
	runErr  chan error
}

func newSourceRig(t *testing.T) *sourceRig {
	t.Helper()
	r := &sourceRig{b: &fakeBackend{}, now: t0, leases: map[string]store.Lease{},
		started: make(chan gateway.SourceWatch, 1), devs: make(chan string, 1), runErr: make(chan error)}
	r.g = newGatewayPart(r.b, t.TempDir(), func() time.Time { return t0 }, zerolog.Nop())
	r.s = &sourceWatch{
		gw: r.g,
		leases: func() (map[string]store.Lease, error) {
			r.reads++
			return r.leases, r.readErr
		},
		unknown: func(mac net.HardwareAddr, at time.Time) {
			r.seen = append(r.seen, mac.String()+" "+at.Format(time.TimeOnly))
		},
		run: func(ctx context.Context, w gateway.SourceWatch, _ netns.NsHandle, dev string) error {
			r.started <- w
			r.devs <- dev
			select {
			case err := <-r.runErr:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		now:   func() time.Time { return r.now },
		sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		log:   zerolog.Nop(),
	}
	return r
}

var managedTestLeg = gateway.Leg{ID: gateway.ManagedBridge, Mark: 3, Bridge: gateway.ManagedBridge,
	MAC: gateway.ManagedLegMAC, Managed: true}

func sourceFrame(src string) []byte {
	m, _ := net.ParseMAC(src)
	f := append([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, m...)
	return append(f, 0x08, 0x06)
}

// The watch listens on the managed leg once it is up, tells the engine of a
// MAC no lease knows, with the time, and starts again on the leg there when
// it ends.
func TestTheSourceWatchListensOnTheManagedLeg(t *testing.T) {
	r := newSourceRig(t)
	r.leases["06:50:43:4f:76:02"] = store.Lease{MAC: "06:50:43:4f:76:02"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.s.keep(ctx)
	}()
	r.g.setUp([]gateway.Leg{testLeg("vmbr1", 1), managedTestLeg})

	w := <-r.started
	require.Equal(t, "leg3", <-r.devs)
	require.Equal(t, []net.HardwareAddr{gateway.ManagedLegMAC}, w.Own)
	w.Frame(sourceFrame("06:50:43:4f:76:02"))
	w.Frame(sourceFrame("bc:24:11:00:99:99"))
	require.Equal(t, []string{"bc:24:11:00:99:99 12:00:00"}, r.seen)

	r.runErr <- errors.New("reading the frames on leg3: network is down")
	<-r.started
	require.Equal(t, "leg3", <-r.devs, "started again")
	cancel()
	<-done
}

// A MAC the watch does not know has the leases read again, at most once a
// second: a card attached this cycle speaks before the gateway knows it.
func TestTheSourceWatchReadsTheLeasesAgainForAMACItDoesNotKnow(t *testing.T) {
	r := newSourceRig(t)
	unknown := hw("06:50:43:4f:76:03")
	require.False(t, r.s.isLease(unknown))
	require.Equal(t, 1, r.reads)

	r.leases["06:50:43:4f:76:03"] = store.Lease{MAC: "06:50:43:4f:76:03"}
	require.False(t, r.s.isLease(unknown), "read again no sooner than a second after")
	require.Equal(t, 1, r.reads)
	r.now = r.now.Add(time.Second)
	require.True(t, r.s.isLease(unknown))
	require.Equal(t, 2, r.reads)
	require.True(t, r.s.isLease(unknown))
	require.Equal(t, 2, r.reads, "a known MAC reads nothing")
}

// Leases that were never read tell nothing: no MAC is reported then.
func TestTheSourceWatchReportsNothingItCannotTell(t *testing.T) {
	r := newSourceRig(t)
	r.readErr = errors.New("the cluster filesystem is not mounted")
	require.True(t, r.s.isLease(hw("bc:24:11:00:99:99")))

	r.readErr = nil
	r.now = r.now.Add(time.Second)
	require.False(t, r.s.isLease(hw("bc:24:11:00:99:99")))
	r.readErr = errors.New("the cluster filesystem is not mounted")
	r.now = r.now.Add(time.Second)
	require.False(t, r.s.isLease(hw("bc:24:11:00:99:99")), "the leases last read stand")
}

func TestTheSourceWatchWaitsForTheManagedLeg(t *testing.T) {
	r := newSourceRig(t)
	r.g.setUp([]gateway.Leg{testLeg("vmbr1", 1)})
	slept := 0
	ctx, cancel := context.WithCancel(t.Context())
	r.s.sleep = func(context.Context, time.Duration) error {
		slept++
		if slept == 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	r.s.keep(ctx)
	require.Empty(t, r.started, "no managed leg, nothing to listen on")
}

func hw(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return slices.Clone(m)
}

// A guest that sprays source MACs fills the watch's record; a spoof hidden
// in the flood still reaches the engine, which acts on it, within a second,
// and the watch keeps no more than its bound.
func TestAFloodOfMACsHidesNoSpoofFromTheEngine(t *testing.T) {
	r := newSourceRig(t)
	r.s.droppedAt = r.now // logged just now: the count is kept
	for i := range 1000 {
		r.s.report(net.HardwareAddr{0xbc, 0x24, 0x11, 0x00, byte(i >> 8), byte(i)})
	}
	require.Len(t, r.seen, maxReported+1, "the bound, and one past it")
	require.Len(t, r.s.reported, maxReported)
	require.Equal(t, 1000-maxReported-1, r.s.dropped, "counted")

	r.seen = nil
	r.now = r.now.Add(500 * time.Millisecond)
	for i := range 1000 {
		r.s.report(net.HardwareAddr{0xbc, 0x24, 0x11, 0x00, byte(i >> 8), byte(i)})
	}
	r.s.report(hw("06:50:43:4f:76:02"))
	require.Empty(t, r.seen, "all within a second of an action")

	r.now = r.now.Add(500 * time.Millisecond)
	r.s.report(hw("06:50:43:4f:76:02"))
	require.Equal(t, []string{"06:50:43:4f:76:02 12:00:01"}, r.seen, "the spoof reaches the engine")
	require.LessOrEqual(t, len(r.s.reported), maxReported)

	r.seen = nil
	r.s.report(hw("bc:24:11:00:00:05"))
	require.Equal(t, []string{"bc:24:11:00:00:05 12:00:01"}, r.seen, "a MAC of the record a second later")
}

// One MAC is passed on once a second, however often it speaks.
func TestOneMACIsPassedOnOnceASecond(t *testing.T) {
	r := newSourceRig(t)
	for range 100 {
		r.s.report(hw("bc:24:11:00:99:99"))
	}
	r.now = r.now.Add(time.Second)
	r.s.report(hw("bc:24:11:00:99:99"))
	require.Equal(t, []string{"bc:24:11:00:99:99 12:00:00", "bc:24:11:00:99:99 12:00:01"}, r.seen)
}

// The port watch's callback never waits for the entries to be given: a
// slow apply cannot hold up the reader of the links.
func TestASlowApplyDoesNotHoldUpThePortWatch(t *testing.T) {
	r := newStickyRig(t, true)
	release := make(chan struct{})
	r.g.applySticky = func(context.Context, gateway.Backend, []gateway.Sticky) error {
		<-release
		return nil
	}
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := make(chan struct{})
	watch := func(ctx context.Context, _ netns.NsHandle, _ string, joined func(string)) error {
		for range 10 * portJoinedQueue {
			joined("veth101i1")
		}
		close(called)
		<-ctx.Done()
		return ctx.Err()
	}
	go r.g.watchPorts(ctx, watch, sleepContext)
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("the callback waited for the apply")
	}
}

// The ports that joined meanwhile are given their entries in one turn, once
// each.
func TestThePortsWaitingAreTakenOnceEach(t *testing.T) {
	joined := make(chan string, portJoinedQueue)
	for _, p := range []string{"tap101i1", "veth102i1", "tap101i1"} {
		joined <- p
	}
	require.Equal(t, []string{"veth103i1", "tap101i1", "veth102i1"}, drainPorts(joined, "veth103i1"))
	require.Empty(t, joined)
}

// A port that went, as when its guest stops, has its entry parked on the
// leg at the next check, so that no other port takes the MAC over.
func TestAPortThatWentHasItsEntryParked(t *testing.T) {
	r := newStickyRig(t, true)
	r.setPort(false)
	require.NoError(t, r.g.keepSticky(t.Context()))
	require.Equal(t, 1, r.appliedCount())
	require.NoError(t, r.g.keepSticky(t.Context()))
	require.Equal(t, 1, r.appliedCount(), "once")
}

// A port the watch could not pass on, or a watch that failed, has the
// entries given at the next check whatever the ports look like.
func TestALostPortEventHasTheEntriesGivenAtTheNextCheck(t *testing.T) {
	r := newStickyRig(t, true)
	require.NoError(t, r.g.keepSticky(t.Context()))
	require.Zero(t, r.appliedCount())

	r.g.markDirty()
	require.NoError(t, r.g.keepSticky(t.Context()))
	require.Equal(t, 1, r.appliedCount())
	require.NoError(t, r.g.keepSticky(t.Context()))
	require.Equal(t, 1, r.appliedCount(), "clean again")

	r.g.markDirty()
	r.err = errors.New("netlink: no such device")
	require.Error(t, r.g.keepSticky(t.Context()))
	r.err = nil
	require.NoError(t, r.g.keepSticky(t.Context()))
	require.Equal(t, 2, r.appliedCount(), "dirty until it went through")
}

func TestAFullPortQueueMarksTheEntriesDirty(t *testing.T) {
	r := newStickyRig(t, true)
	block := make(chan struct{})
	applying := make(chan struct{}, 1)
	r.g.applySticky = func(context.Context, gateway.Backend, []gateway.Sticky) error {
		applying <- struct{}{}
		<-block
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := make(chan struct{})
	watch := func(ctx context.Context, _ netns.NsHandle, _ string, joined func(string)) error {
		// The first port is taken up and its apply holds; what the check
		// took as dirty is taken before that, so the flood that follows
		// is the only thing that can mark the entries dirty.
		joined("veth101i1")
		<-applying
		for range 10 * portJoinedQueue {
			joined("veth101i1")
		}
		close(called)
		<-ctx.Done()
		return ctx.Err()
	}
	go r.g.watchPorts(ctx, watch, sleepContext)
	<-called
	r.g.dirtyMu.Lock()
	dirty := r.g.dirty
	r.g.dirtyMu.Unlock()
	close(block)
	require.True(t, dirty, "a port the queue had no room for")
}

// The leases are read again every 30 seconds besides on a miss: a lease that
// went stops being known. They are read with no lock held.
func TestTheSourceWatchForgetsALeaseThatWent(t *testing.T) {
	r := newSourceRig(t)
	r.leases["06:50:43:4f:76:02"] = store.Lease{MAC: "06:50:43:4f:76:02"}
	read := r.s.leases
	r.s.leases = func() (map[string]store.Lease, error) {
		require.True(t, r.s.mu.TryLock(), "the store is read under the lock")
		r.s.mu.Unlock()
		return read()
	}
	require.True(t, r.s.isLease(hw("06:50:43:4f:76:02")))
	delete(r.leases, "06:50:43:4f:76:02")
	r.now = r.now.Add(10 * time.Second)
	require.True(t, r.s.isLease(hw("06:50:43:4f:76:02")), "read again no sooner than 30 seconds")
	r.now = r.now.Add(20 * time.Second)
	require.False(t, r.s.isLease(hw("06:50:43:4f:76:02")))
}
