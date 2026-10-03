package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	// checkEvery is how often the egress table is checked besides the
	// notifications of the ruleset.
	checkEvery = 30 * time.Second
	// settleFor lets a burst of ruleset notifications pass before the table
	// is checked.
	settleFor = 200 * time.Millisecond
	// reloadEvery is the least time between two loads of the table: a table
	// that is not as pco loads it right after pco loaded it is loaded again
	// no sooner, and not in a loop with the notifications of its own loads.
	reloadEvery = 5 * time.Second
)

// table is the part of the egress filter the keeper checks.
type table interface {
	Verify(ctx context.Context) error
	Reapply(ctx context.Context) error
}

// keeper keeps the egress table in place: it checks it, loads it again when
// it is gone, dormant or not the one pco applied, and tells the engine what
// it found.
type keeper struct {
	table table
	off   func() (since time.Time, off bool, err error)
	note  func(engine.EgressCheck)
	now   func() time.Time
	log   zerolog.Logger

	state      string    // as the last check that could tell found it
	lastReload time.Time // when the table was last loaded again
}

// check checks the table once. It returns how soon it has to check again,
// when that is sooner than the next timed check, and zero otherwise.
func (k *keeper) check(ctx context.Context) time.Duration {
	err := k.table.Verify(ctx)
	switch {
	case err == nil:
		if k.state == engine.EgressOff {
			// Switched on again: pco egress on loaded the table without the
			// targets, which come back at once rather than with the next
			// cycle.
			if rerr := k.table.Reapply(ctx); rerr != nil {
				k.log.Warn().Err(rerr).Msg("loading the egress table after it was switched on failed; the next cycle loads it")
			}
		}
		k.report(engine.EgressCheck{View: engine.EgressView{State: engine.EgressOn}})
	case errors.Is(err, egress.ErrOff):
		since, _, _ := k.off()
		k.report(engine.EgressCheck{View: engine.EgressView{State: engine.EgressOff, Since: since}})
	case errors.Is(err, egress.ErrChanged):
		if wait := k.lastReload.Add(reloadEvery).Sub(k.now()); wait > 0 && !k.lastReload.After(k.now()) {
			// Shown as it is until it is loaded again, which holds nothing.
			k.report(engine.EgressCheck{View: engine.EgressView{State: changedState(err)}})
			return wait
		}
		k.reload(ctx, err)
	case errors.Is(err, egress.ErrNoConnectorUser):
		// The cycle says so, as it cannot set the filter either.
	default:
		k.log.Warn().Err(err).Msg("checking the egress table failed")
		k.report(engine.EgressCheck{Failed: err.Error()})
	}
	return 0
}

// reload loads the table again after the check found it changed.
func (k *keeper) reload(ctx context.Context, changed error) {
	k.lastReload = k.now()
	what := strings.TrimPrefix(changed.Error(), egress.ErrChanged.Error()+": ")
	if err := k.table.Reapply(ctx); err != nil {
		state := changedState(changed)
		k.log.Error().Err(err).Str("found", what).Msg("the egress table was changed or removed outside pco and could not be loaded again")
		k.report(engine.EgressCheck{View: engine.EgressView{State: state}, Failed: err.Error(), Holds: true})
		return
	}
	k.log.Warn().Str("found", what).Msg("the egress table was changed or removed outside pco and was loaded again")
	k.report(engine.EgressCheck{View: engine.EgressView{State: engine.EgressOn}, Reloaded: what})
}

// changedState is the state of a table that is gone or not the one pco
// applied.
func changedState(changed error) string {
	if errors.Is(changed, egress.ErrNotLoaded) {
		return engine.EgressNotLoaded
	}
	return engine.EgressChanged
}

func (k *keeper) report(c engine.EgressCheck) {
	if c.View.State != "" {
		k.state = c.View.State
	}
	k.note(c)
}

// keep runs the keeper until ctx ends: a check every interval, and one after
// every burst of notifications that the ruleset changed. Before the first
// check it only reads the switch, so that a filter switched off shows as such
// from the start.
func (k *keeper) keep(ctx context.Context, watch func(context.Context, func()) error, every time.Duration, sleep func(context.Context, time.Duration) error) {
	changed := make(chan struct{}, 1)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		watchRuleset(ctx, watch, func() {
			select {
			case changed <- struct{}{}:
			default:
			}
		}, sleep, k.log)
	}()
	defer func() { <-watched }()

	if since, off, err := k.off(); err == nil && off {
		k.report(engine.EgressCheck{View: engine.EgressView{State: engine.EgressOff, Since: since}})
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	var soon <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-soon:
		case <-changed:
			if sleep(ctx, settleFor) != nil {
				return
			}
			select {
			case <-changed:
			default:
			}
		}
		soon = nil
		if wait := k.check(ctx); wait > 0 {
			soon = time.After(wait)
		}
	}
}

// watchRuleset runs the watch of the ruleset until ctx ends. A watch that
// fails is started again after a while; until then the table is checked on
// the timer only.
func watchRuleset(ctx context.Context, watch func(context.Context, func()) error, changed func(),
	sleep func(context.Context, time.Duration) error, log zerolog.Logger,
) {
	for {
		err := watch(ctx, changed)
		if ctx.Err() != nil {
			return
		}
		log.Warn().Err(err).Dur("again", watchAgainAfter).Msg("watching the nftables ruleset failed; " +
			"until it runs again, the egress table is checked on the timer only")
		if sleep(ctx, watchAgainAfter) != nil {
			return
		}
	}
}
