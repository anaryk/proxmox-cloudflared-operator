package engine

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// timers replaces the timers of an engine: a test fires them.
type timers struct {
	mu      sync.Mutex
	waits   []time.Duration
	pending []chan time.Time
}

func (tm *timers) install(e *Engine) {
	e.after = func(d time.Duration) (<-chan time.Time, func() bool) {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		ch := make(chan time.Time, 1)
		tm.waits = append(tm.waits, d)
		tm.pending = append(tm.pending, ch)
		return ch, func() bool { return true }
	}
}

// fire fires the oldest timer that was started and not fired.
func (tm *timers) fire(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		return len(tm.pending) > 0
	}, 5*time.Second, time.Millisecond, "no timer was started")
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.pending[0] <- time.Time{}
	tm.pending = tm.pending[1:]
}

// await waits until a timer was started, and returns how long each was for.
func (tm *timers) await(t *testing.T) []time.Duration {
	t.Helper()
	require.Eventually(t, func() bool { return len(tm.started()) > 0 }, 5*time.Second, time.Millisecond)
	return tm.started()
}

func (tm *timers) started() []time.Duration {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return append([]time.Duration(nil), tm.waits...)
}

func (f *fakeResolver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// watching is a published env whose engine has its timers replaced.
func watching(t *testing.T) (*env, *timers) {
	t.Helper()
	e := published(t)
	tm := &timers{}
	tm.install(e.eng)
	return e, tm
}

func TestTheWatchIsGivenThePinsOfTheServedAddresses(t *testing.T) {
	e := newEnv(t)
	web := guest(101, "web-1", "www.example.com -> :8080")
	web.NICs = []model.NIC{{Index: 0, MAC: testMAC, Bridge: "vmbr0"}, {Index: 1, MAC: "bc:24:11:00:00:09", Bridge: "vmbr0"}}
	e.inv.set(snapshot(web, guest(102, "api", "api.example.com -> 10.0.0.20:8080")))
	e.res.setLevel("api.example.com", resolve.LevelObserved)

	e.cycle()

	hw, err := net.ParseMAC(testMAC)
	require.NoError(t, err)
	other, err := net.ParseMAC("bc:24:11:00:00:09")
	require.NoError(t, err)
	require.Equal(t, map[netip.Addr]egress.Pin{
		guestAddr: {MAC: hw, Own: []net.HardwareAddr{other}},
	}, e.eng.Bound(), "a route held back by the minimum is not served, and not watched")
}

func TestAnAddressWhoseMACMovedIsBlockedAtOnceAndComesBackOnceVerified(t *testing.T) {
	e, _ := watching(t)
	log := &callLog{}
	e.egr.log = log
	resolved := e.res.count()

	e.eng.Moved(t.Context(), guestAddr)
	e.eng.moving.Wait()

	require.Equal(t, []string{"egress remove 10.0.0.11", "egress set 10.0.0.11:8080"}, log.all())
	require.Equal(t, resolved+1, e.res.count(), "the route was resolved again")
	events := e.eng.Events(t0.Add(-time.Hour))
	require.Equal(t, "the MAC of 10.0.0.11 moved; the connectors do not reach it until it is verified again", events[len(events)-2].Message)
	require.Equal(t, "10.0.0.11 was verified again after its MAC moved; the connectors reach it again", events[len(events)-1].Message)
	require.Empty(t, e.eng.trigger, "nothing to withdraw")
}

func TestAnAddressThatFailsAfterItsMACMovedStaysOutAndItsRouteIsWithdrawn(t *testing.T) {
	e, _ := watching(t)
	log := &callLog{}
	e.egr.log = log
	e.res.stop("www.example.com", "10.0.0.11 answered by bc:24:11:ff:ff:01, which is not this guest")

	e.eng.Moved(t.Context(), guestAddr)
	e.eng.moving.Wait()

	require.Equal(t, []string{"egress remove 10.0.0.11"}, log.all(), "nothing lets it back")
	require.NotContains(t, e.eng.Bound(), guestAddr)
	events := e.eng.Events(t0.Add(-time.Hour))
	require.Equal(t, "10.0.0.11 did not pass its verification after its MAC moved "+
		"(10.0.0.11 answered by bc:24:11:ff:ff:01, which is not this guest); it stays out of the egress filter, and its routes are withdrawn",
		events[len(events)-1].Message)
	require.Len(t, e.eng.trigger, 1, "a cycle is asked for")
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.True(t, bindings["www.example.com"].Withdrawn, "the next cycle starts from what the verification found")

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, "withdrawn", string(route(st, "www.example.com").State))
	got, _ := e.egr.last()
	require.Empty(t, got)
}

// A MAC that moved to where the forwarding table no longer places it on the
// guest's port proves the address at observed at most: below the minimum, it
// does not come back.
func TestAnAddressProvenBelowTheMinimumAfterItsMACMovedStaysOut(t *testing.T) {
	e, _ := watching(t)
	e.res.setLevel("www.example.com", resolve.LevelObserved)

	e.eng.Moved(t.Context(), guestAddr)
	e.eng.moving.Wait()

	require.Equal(t, 1, e.egr.setCount(), "no Set after the one of the cycle")
	events := e.eng.Events(t0.Add(-time.Hour))
	require.Equal(t, "10.0.0.11 did not pass its verification after its MAC moved "+
		"(identity level observed is below the required port); it stays out of the egress filter, and its routes are withdrawn",
		events[len(events)-1].Message)
}

// A route that verifies again in the next cycle, where its MAC is where it
// was, comes back with that cycle: the verification after the move kept
// nothing out for good.
func TestAnAddressThatFailedComesBackWhenACycleVerifiesIt(t *testing.T) {
	e, _ := watching(t)
	e.res.stop("www.example.com", "10.0.0.11 answered by bc:24:11:ff:ff:01, which is not this guest")
	e.eng.Moved(t.Context(), guestAddr)
	e.eng.moving.Wait()
	e.res.start("www.example.com")

	e.clock.advance(20 * time.Second)
	e.cycle()

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// A cycle between a move and the verification that follows it keeps the
// address out: it was not verified after the move.
func TestACycleKeepsAMovedAddressOutUntilItIsVerified(t *testing.T) {
	e, tm := watching(t)
	e.eng.Moved(t.Context(), guestAddr)
	e.eng.moving.Wait()
	e.eng.Moved(t.Context(), guestAddr)
	require.Equal(t, []time.Duration{reverifyEvery}, tm.await(t), "the second waits out the rate limit")

	e.clock.advance(time.Second)
	e.cycle()
	got, _ := e.egr.last()
	require.Empty(t, got)

	tm.fire(t)
	e.eng.moving.Wait()
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// However many notifications come, an address is verified again at most once
// every reverifyEvery, and none of them waits for the cycle lock.
func TestAStormOfMovesIsOneVerificationAndBlocksNothing(t *testing.T) {
	e, tm := watching(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	e.res.hook(func() {
		select {
		case entered <- struct{}{}:
			<-release
		default:
		}
	})
	e.clock.advance(20 * time.Second)
	cycled := make(chan State, 1)
	go func() { cycled <- e.cycle() }()
	<-entered // the cycle holds the lock and resolves
	e.res.hook(nil)
	resolved := e.res.count()

	for range 1000 {
		e.eng.Moved(t.Context(), guestAddr)
	}

	require.Equal(t, []netip.Addr{guestAddr}, e.egr.removes()[:1], "blocked while the cycle runs")
	close(release)
	<-cycled
	e.eng.moving.Wait()
	require.Equal(t, resolved+1, e.res.count(), "one verification after the cycle")
	require.Empty(t, tm.started())

	for range 1000 {
		e.eng.Moved(t.Context(), guestAddr)
	}
	require.Equal(t, []time.Duration{reverifyEvery}, tm.await(t), "one waits out the rate limit")
	tm.fire(t)
	e.eng.moving.Wait()
	require.Equal(t, resolved+2, e.res.count())
}

// A move during the verification of an earlier one is verified as well
// before the address comes back.
func TestAMoveDuringTheVerificationIsVerifiedToo(t *testing.T) {
	e, tm := watching(t)
	log := &callLog{}
	e.egr.log = log
	var once sync.Once
	e.res.hook(func() { once.Do(func() { e.eng.Moved(t.Context(), guestAddr) }) })

	e.eng.Moved(t.Context(), guestAddr)
	tm.await(t)

	require.Equal(t, []string{"egress remove 10.0.0.11", "egress remove 10.0.0.11"}, log.all(), "not back yet")
	tm.fire(t)
	e.eng.moving.Wait()
	require.Equal(t, "egress set 10.0.0.11:8080", log.all()[len(log.all())-1])
}

// The engine stops what waits when its context ends.
func TestAVerificationThatWaitsEndsWithTheContext(t *testing.T) {
	e, tm := watching(t)
	e.eng.Moved(t.Context(), guestAddr)
	e.eng.moving.Wait()
	ctx, cancel := context.WithCancel(t.Context())

	e.eng.Moved(ctx, guestAddr)
	tm.await(t)
	cancel()

	e.eng.moving.Wait()
}
