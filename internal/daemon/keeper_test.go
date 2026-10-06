package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// fakeTable answers Verify from a queue and counts the loads.
type fakeTable struct {
	mu        sync.Mutex
	verify    []error // answered in turn; the last one stays
	reapplies int
	reapply   error
}

func (f *fakeTable) Verify(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.verify[0]
	if len(f.verify) > 1 {
		f.verify = f.verify[1:]
	}
	return err
}

func (f *fakeTable) Reapply(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reapplies++
	return f.reapply
}

func (f *fakeTable) loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reapplies
}

type keeperRig struct {
	k     *keeper
	table *fakeTable
	now   time.Time
	notes []engine.EgressCheck
	nets  []engine.NetCheck
}

func newKeeperRig(verify ...error) *keeperRig {
	r := &keeperRig{table: &fakeTable{verify: verify}, now: t0}
	r.k = &keeper{
		table: r.table,
		off:   func() (time.Time, bool, error) { return t0.Add(-time.Hour), true, nil },
		note:  func(c engine.EgressCheck) { r.notes = append(r.notes, c) },
		now:   func() time.Time { return r.now },
		log:   zerolog.Nop(),
	}
	return r
}

var (
	errGone    = fmt.Errorf("%w: %w", egress.ErrChanged, egress.ErrNotLoaded)
	errChanged = fmt.Errorf("%w: the table has flags dormant", egress.ErrChanged)
	on         = engine.EgressView{State: engine.EgressOn}
)

func TestTheKeeperReportsWhatItFinds(t *testing.T) {
	tests := []struct {
		name    string
		verify  error
		reapply error
		note    engine.EgressCheck
		loads   int
	}{
		{name: "in place", note: engine.EgressCheck{View: on}},
		{name: "switched off", verify: egress.ErrOff,
			note: engine.EgressCheck{View: engine.EgressView{State: engine.EgressOff, Since: t0.Add(-time.Hour)}}},
		{name: "gone and loaded again", verify: errGone, loads: 1,
			note: engine.EgressCheck{View: on, Reloaded: "the egress table is not loaded"}},
		{name: "dormant and loaded again", verify: errChanged, loads: 1,
			note: engine.EgressCheck{View: on, Reloaded: "the table has flags dormant"}},
		{name: "gone and not loaded again", verify: errGone, reapply: errors.New("nft -f -: exit status 1"), loads: 1,
			note: engine.EgressCheck{View: engine.EgressView{State: engine.EgressNotLoaded}, Failed: "nft -f -: exit status 1", Holds: true}},
		{name: "changed and not loaded again", verify: errChanged, reapply: errors.New("nft -f -: exit status 1"), loads: 1,
			note: engine.EgressCheck{View: engine.EgressView{State: engine.EgressChanged}, Failed: "nft -f -: exit status 1", Holds: true}},
		{name: "not listed", verify: errors.New("listing the egress table: nft: signal: killed"),
			note: engine.EgressCheck{Failed: "listing the egress table: nft: signal: killed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newKeeperRig(tc.verify)
			r.table.reapply = tc.reapply

			require.Zero(t, r.k.check(t.Context()))

			require.Equal(t, []engine.EgressCheck{tc.note}, r.notes)
			require.Equal(t, tc.loads, r.table.loads())
		})
	}
}

// The cycle says that the connector user is missing; the checks do not say
// it once more.
func TestTheKeeperLeavesAMissingUserToTheCycle(t *testing.T) {
	r := newKeeperRig(fmt.Errorf("pco-connector: %w", egress.ErrNoConnectorUser))

	r.k.check(t.Context())

	require.Empty(t, r.notes)
}

// pco egress on loads the table without targets: once the filter is on
// again, the keeper loads the whole table, which is no change made outside
// pco.
func TestTheKeeperLoadsTheTargetsOnceTheFilterIsOnAgain(t *testing.T) {
	r := newKeeperRig(egress.ErrOff, nil, nil)

	r.k.check(t.Context())
	r.k.check(t.Context())
	r.k.check(t.Context())

	require.Equal(t, 1, r.table.loads(), "once, as it came back")
	require.Equal(t, on, r.notes[1].View)
	require.Empty(t, r.notes[1].Reloaded)
}

// A table that is not as pco loads it right after pco loaded it is loaded
// again no sooner than reloadEvery: its own load must not start a loop with
// the notification it sends.
func TestTheKeeperLoadsTheTableAtMostOnceEveryFiveSeconds(t *testing.T) {
	r := newKeeperRig(errChanged)

	require.Zero(t, r.k.check(t.Context()))
	r.now = r.now.Add(time.Second)
	wait := r.k.check(t.Context())

	require.Equal(t, 4*time.Second, wait)
	require.Equal(t, 1, r.table.loads())
	require.Equal(t, []engine.EgressCheck{
		{View: on, Reloaded: "the table has flags dormant"},
		{View: engine.EgressView{State: engine.EgressChanged}},
	}, r.notes, "what the table is meanwhile is shown, and holds nothing")

	r.now = r.now.Add(wait)
	require.Zero(t, r.k.check(t.Context()))
	require.Equal(t, 2, r.table.loads())

	t.Run("also when the clock went back", func(t *testing.T) {
		r.now = t0.Add(-time.Hour)
		require.Zero(t, r.k.check(t.Context()))
		require.Equal(t, 3, r.table.loads())
	})
}

// fakeNet is the service-prefix route and table of an appliance: Verify
// answers from a queue, and the loads are counted.
type fakeNet struct {
	mu      sync.Mutex
	verify  []error // answered in turn; the last one stays
	loaded  int
	loadErr error
}

func (f *fakeNet) Verify(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.verify[0]
	if len(f.verify) > 1 {
		f.verify = f.verify[1:]
	}
	return err
}

func (f *fakeNet) Load(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded++
	return f.loadErr
}

func (f *fakeNet) loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded
}

// withNet makes the rig an appliance's: the keeper checks the service-prefix
// route and table beside the egress table.
func (r *keeperRig) withNet(verify ...error) *fakeNet {
	n := &fakeNet{verify: verify}
	r.k.net = n
	r.k.noteNet = func(c engine.NetCheck) { r.nets = append(r.nets, c) }
	return n
}

var errRouteGone = &appnet.ChangedError{Differences: []string{"the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing"}}

func TestTheKeeperLoadsTheServicePrefixAgain(t *testing.T) {
	tests := []struct {
		name    string
		verify  error
		loadErr error
		note    engine.NetCheck
		loads   int
	}{
		{name: "in place"},
		{name: "changed and loaded again", verify: errRouteGone, loads: 1,
			note: engine.NetCheck{Reloaded: "the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing"}},
		{name: "changed and not loaded again", verify: errRouteGone, loadErr: errors.New("adding the dummy device pco0: operation not permitted"),
			loads: 1, note: engine.NetCheck{NotKept: "adding the dummy device pco0: operation not permitted"}},
		{name: "not read", verify: errors.New("listing the table inet pco_net: signal: killed"),
			note: engine.NetCheck{Failed: "listing the table inet pco_net: signal: killed"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newKeeperRig(nil)
			n := r.withNet(tc.verify)
			n.loadErr = tc.loadErr

			require.Zero(t, r.k.check(t.Context()))

			require.Equal(t, []engine.NetCheck{tc.note}, r.nets)
			require.Equal(t, tc.loads, n.loads())
			require.Equal(t, []engine.EgressCheck{{View: on}}, r.notes, "the egress table is checked as before")
			require.Zero(t, r.table.loads())
		})
	}
}

// The service prefix is loaded again no sooner than reloadEvery either, and
// its wait does not hold the egress table's.
func TestTheKeeperLoadsTheServicePrefixAtMostOnceEveryFiveSeconds(t *testing.T) {
	r := newKeeperRig(nil)
	n := r.withNet(errRouteGone)

	require.Zero(t, r.k.check(t.Context()))
	r.now = r.now.Add(time.Second)

	require.Equal(t, 4*time.Second, r.k.check(t.Context()))
	require.Equal(t, 1, n.loads())
	require.Len(t, r.nets, 1, "nothing new to say while it waits")
	r.now = r.now.Add(4 * time.Second)
	require.Zero(t, r.k.check(t.Context()))
	require.Equal(t, 2, n.loads())
}

// nft flush ruleset takes both tables in one transaction, which the ruleset
// watch tells in a burst of notifications: each is loaded once, and each
// load is one event.
func TestOneBurstLoadsBothTablesOnce(t *testing.T) {
	r := newKeeperRig(errGone, nil)
	n := r.withNet(errRouteGone, nil)
	var mu sync.Mutex
	notes, nets := r.k.note, r.k.noteNet
	r.k.note = func(c engine.EgressCheck) { mu.Lock(); defer mu.Unlock(); notes(c) }
	r.k.noteNet = func(c engine.NetCheck) { mu.Lock(); defer mu.Unlock(); nets(c) }
	r.k.off = func() (time.Time, bool, error) { return time.Time{}, false, nil }
	reloaded := func() (egress, net []string) {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range r.notes {
			if c.Reloaded != "" {
				egress = append(egress, c.Reloaded)
			}
		}
		for _, c := range r.nets {
			if c.Reloaded != "" {
				net = append(net, c.Reloaded)
			}
		}
		return egress, net
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	changedc := make(chan func(), 1)
	watch := func(ctx context.Context, changed func()) error {
		changedc <- changed
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.k.keep(ctx, watch, time.Hour, sleepContext)
	}()

	change := <-changedc
	for range 20 {
		change()
	}
	require.Eventually(t, func() bool { e, n := reloaded(); return len(e) == 1 && len(n) == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	<-done

	e, nt := reloaded()
	require.Equal(t, []string{"the egress table is not loaded"}, e)
	require.Equal(t, []string{"the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing"}, nt)
	require.Equal(t, 1, r.table.loads())
	require.Equal(t, 1, n.loads())
}

// The keeper checks on its timer and after every burst of notifications, and
// shows a filter that is off from the start.
func TestTheKeeperChecksOnTheTimerAndOnNotifications(t *testing.T) {
	r := newKeeperRig(egress.ErrOff)
	var mu sync.Mutex
	r.k.note = func(c engine.EgressCheck) {
		mu.Lock()
		defer mu.Unlock()
		r.notes = append(r.notes, c)
	}
	notes := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(r.notes)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	changedc := make(chan func(), 1)
	watch := func(ctx context.Context, changed func()) error {
		changedc <- changed
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.k.keep(ctx, watch, time.Hour, sleepContext)
	}()

	require.Eventually(t, func() bool { return notes() == 1 }, 5*time.Second, time.Millisecond, "the switch is read at once")
	change := <-changedc
	for range 10 {
		change()
	}
	require.Eventually(t, func() bool { return notes() >= 2 }, 5*time.Second, time.Millisecond)

	cancel()
	<-done
}
