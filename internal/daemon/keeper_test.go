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
	require.Len(t, r.notes, 1)

	r.now = r.now.Add(wait)
	require.Zero(t, r.k.check(t.Context()))
	require.Equal(t, 2, r.table.loads())

	t.Run("also when the clock went back", func(t *testing.T) {
		r.now = t0.Add(-time.Hour)
		require.Zero(t, r.k.check(t.Context()))
		require.Equal(t, 3, r.table.loads())
	})
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
