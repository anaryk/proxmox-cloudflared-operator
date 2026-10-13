package agent

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// timers replaces the timers of an agent: a test fires them.
type timers struct {
	mu      sync.Mutex
	waits   []time.Duration
	pending []chan time.Time
}

func (tm *timers) install(a *Agent) {
	a.after = func(d time.Duration) (<-chan time.Time, func() bool) {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		ch := make(chan time.Time, 1)
		tm.waits = append(tm.waits, d)
		tm.pending = append(tm.pending, ch)
		return ch, func() bool { return true }
	}
}

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

// watching is an agent that serves www.example.com, with its timers
// replaced.
func watching(t *testing.T) (*env, *timers) {
	t.Helper()
	e := newEnv(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	tm := &timers{}
	tm.install(e.agent)
	return e, tm
}

func TestTheWatchIsGivenThePinsOfTheServedAddresses(t *testing.T) {
	const second, third = "bc:24:11:00:00:09", "bc:24:11:00:00:0a"
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080", "api.example.com -> :9090")
	s.Targets[0].Guest.NICs = []model.NIC{
		{Index: 0, MAC: testMAC, Bridge: "vmbr0"},
		{Index: 1, MAC: second, Bridge: "vmbr0"},
		{Index: 2, MAC: third, Bridge: "vmbr1"},
	}
	api := model.GuestRef{Kind: model.KindQEMU, VMID: 102}
	s.Targets[1].Route.Guest, s.Targets[1].Guest.Ref = &api, api
	e.res.observe("api.example.com")
	e.res.moveTo("api.example.com", netip.MustParseAddr("10.0.0.20"))

	e.apply(s)

	require.Equal(t, map[netip.Addr]egress.Pin{
		guestAddr: {MAC: mustMAC(testMAC), Bridge: "vmbr0", Port: "tap101i0", Own: []egress.OwnMAC{
			{MAC: mustMAC(second), Ports: []string{"tap101i1", "fwpr101p1", "veth101i1"}},
			{MAC: mustMAC(third), Ports: []string{"tap101i2", "fwpr101p2", "veth101i2"}},
		}},
	}, e.agent.Bound(), "a target held back by its minimum is not served, and not watched")

	t.Run("nor is a withdrawn one", func(t *testing.T) {
		e.res.stop("www.example.com", "guest is not running")
		e.apply(s)

		require.Empty(t, e.agent.Bound())
	})
}

func TestAnAddressWhoseMACMovedIsBlockedAtOnceAndComesBackOnceVerified(t *testing.T) {
	e, _ := watching(t)
	log := &callLog{}
	e.egr.log = log
	resolved := len(e.res.calls())

	e.agent.Moved(t.Context(), guestAddr)
	e.agent.Wait()

	require.Equal(t, []string{"egress remove 10.0.0.11", "egress set 10.0.0.11:8080"}, log.all())
	require.Len(t, e.res.calls(), resolved+1, "the route was resolved again")
	require.Equal(t, []string{
		"the MAC of 10.0.0.11 moved; the connectors do not reach it until it is verified again",
		"10.0.0.11 was verified again after its MAC moved; the connectors reach it again",
	}, e.events.messages())
	require.Zero(t, e.changed.Load(), "nothing to withdraw")
}

// While an address whose MAC moved waits for its verification it is out of
// the set kept on disk as well as of the filter: a daemon that restarts then
// does not let it back in on what it knew before the move. The verification
// waits for the agent's lock, which the test holds.
func TestAMovedAddressIsOutOfTheKeptSetUntilItIsVerified(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets = append(s.Targets, manualTarget("static.example.com", netip.MustParseAddrPort("10.0.0.30:443"), false))
	e.apply(verified(s, "10.0.0.11:8080", "10.0.0.30:443"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.agent.sem <- struct{}{}

	e.agent.Moved(ctx, guestAddr)

	kept, _, err := e.store.AgentEgress()
	require.NoError(t, err)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.30:443")}, kept)
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.30:443"), e.agent.EgressSet(), "the agent itself still knows it, for when it verifies")

	// The daemon restarts while the verification waits; the guest is then
	// proven below its minimum, which the set rule alone would let stay.
	old := e.agent
	cancel()
	old.Wait()
	e.egr.loaded(egressTargets("10.0.0.30:443"), nil)
	e.res.observe("www.example.com")
	e.agent = e.newAgent()
	require.NoError(t, e.agent.Boot(t.Context()))

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.30:443"), got, "it comes back only once it verifies")

	t.Run("and is kept again once verified", func(t *testing.T) {
		e, _ := watching(t)
		e.agent.sem <- struct{}{}
		e.agent.Moved(t.Context(), guestAddr)
		kept, _, err := e.store.AgentEgress()
		require.NoError(t, err)
		require.Empty(t, kept)

		<-e.agent.sem
		e.agent.Wait()

		kept, _, err = e.store.AgentEgress()
		require.NoError(t, err)
		require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:8080")}, kept)
	})
}

// The watch of the network reports moves beside the passes: the set they
// share is read and written under one lock.
func TestMovesBesideAPassRaceNothing(t *testing.T) {
	e := newEnv(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for range 20 {
				e.agent.Moved(t.Context(), guestAddr)
			}
		})
	}
	for range 20 {
		_, err := e.agent.Reverify(t.Context())
		require.NoError(t, err)
	}
	wg.Wait()
	e.agent.Wait()

	require.NoError(t, e.agent.Boot(t.Context()))
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

func TestAnAddressThatFailsAfterItsMACMovedStaysOut(t *testing.T) {
	e, _ := watching(t)
	log := &callLog{}
	e.egr.log = log
	e.res.stop("www.example.com", "10.0.0.11 answered by bc:24:11:ff:ff:01, which is not this guest")

	e.agent.Moved(t.Context(), guestAddr)
	e.agent.Wait()

	require.Equal(t, []string{"egress remove 10.0.0.11"}, log.all(), "nothing lets it back")
	require.NotContains(t, e.agent.Bound(), guestAddr)
	require.Equal(t, "10.0.0.11 did not pass its verification after its MAC moved "+
		"(10.0.0.11 answered by bc:24:11:ff:ff:01, which is not this guest); it stays out of the egress filter, and its routes are withdrawn",
		e.events.messages()[1])
	require.EqualValues(t, 1, e.changed.Load(), "the controller is told at once")
	require.Empty(t, e.agent.EgressSet())
	saved, _, err := e.store.AgentEgress()
	require.NoError(t, err)
	require.Empty(t, saved)
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.True(t, bindings["www.example.com"].Withdrawn, "the next pass starts from what the verification found")

	e.res.start()
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got, "a pass that verifies it lets it back")
}

// A target on a leg found at another address after its MAC moved stays out,
// and its binding keeps the service address it is published under: the
// controller then has the service address follow it, instead of taking the
// route for a new one.
func TestATargetOnALegFoundElsewhereAfterItsMACMovedKeepsItsServiceAddress(t *testing.T) {
	e := newEnv(t)
	e.res.onLeg("www.example.com", "lan")
	service := netip.MustParseAddr("10.203.118.5")
	s := verified(e.snapshot("www.example.com -> :8080"), "10.203.118.5:8080")
	s.Targets[0].Addr, s.Targets[0].Service = guestAddr, service
	e.apply(s)
	(&timers{}).install(e.agent)
	moved := netip.MustParseAddr("10.0.0.12")
	e.res.moveTo("www.example.com", moved)

	e.agent.Moved(t.Context(), service)
	e.agent.Wait()

	require.Equal(t, []netip.Addr{service}, e.egr.removes())
	require.Empty(t, e.agent.EgressSet())
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, moved, bindings["www.example.com"].Addr)
	require.Equal(t, service, bindings["www.example.com"].Service, "published under it still")
}

func TestAnAddressProvenBelowItsMinimumAfterItsMACMovedStaysOut(t *testing.T) {
	e, _ := watching(t)
	e.res.observe("www.example.com")
	sets := e.egr.setCount()

	e.agent.Moved(t.Context(), guestAddr)
	e.agent.Wait()

	require.Equal(t, sets, e.egr.setCount())
	require.Equal(t, "10.0.0.11 did not pass its verification after its MAC moved "+
		"(identity level observed is below the required port); it stays out of the egress filter, and its routes are withdrawn",
		e.events.messages()[1])
}

// A pass between a move and the verification that follows it keeps the
// address out: it was not verified after the move.
func TestAPassKeepsAMovedAddressOutUntilItIsVerified(t *testing.T) {
	e, tm := watching(t)
	e.agent.Moved(t.Context(), guestAddr)
	e.agent.Wait()
	e.agent.Moved(t.Context(), guestAddr)
	require.Equal(t, []time.Duration{reverifyEvery}, tm.await(t), "the second waits out the rate limit")

	e.clock.advance(time.Second)
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	got, _ := e.egr.last()
	require.Empty(t, got)

	tm.fire(t)
	e.agent.Wait()
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// A move during the verification of an earlier one is verified as well
// before the address comes back.
func TestAMoveDuringTheVerificationIsVerifiedToo(t *testing.T) {
	e, tm := watching(t)
	log := &callLog{}
	e.egr.log = log
	var once sync.Once
	e.res.onResolve = func(string) { once.Do(func() { e.agent.Moved(t.Context(), guestAddr) }) }

	e.agent.Moved(t.Context(), guestAddr)
	tm.await(t)

	require.Equal(t, []string{"egress remove 10.0.0.11", "egress remove 10.0.0.11"}, log.all(), "not back yet")
	tm.fire(t)
	e.agent.Wait()
	require.Equal(t, "egress set 10.0.0.11:8080", log.all()[len(log.all())-1])
}

func TestAVerificationThatWaitsEndsWithTheContext(t *testing.T) {
	e, tm := watching(t)
	e.agent.Moved(t.Context(), guestAddr)
	e.agent.Wait()
	ctx, cancel := context.WithCancel(t.Context())

	e.agent.Moved(ctx, guestAddr)
	tm.await(t)
	cancel()

	e.agent.Wait()
}
