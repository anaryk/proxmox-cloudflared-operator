package daemon

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

// pingEvery is how often the watchdog of systemd is told that the engine
// makes progress. pco.service has WatchdogSec=90s: six pings fit in it, and
// a hung engine is restarted within 90 s of the last one.
const pingEvery = 15 * time.Second

// watchdog pings systemd while the engine makes progress, and only then. The
// keeper of the lease and the API run in goroutines of their own, so a hung
// engine would otherwise keep its lease, and every other node out, for good.
type watchdog struct {
	notify  Notifier
	healthy func(now time.Time) bool
	now     func() time.Time
	// ticker drives the pings; tests replace it.
	ticker func() (<-chan time.Time, func())
	log    zerolog.Logger

	// silent says that the last tick sent no ping, so that the change is
	// logged once; pinged that a ping was sent: before the loop of the
	// engine has begun, no ping is no news.
	silent bool
	pinged bool
}

func newWatchdog(n Notifier, healthy func(time.Time) bool, now func() time.Time, log zerolog.Logger) *watchdog {
	return &watchdog{
		notify: n, healthy: healthy, now: now, log: log,
		ticker: func() (<-chan time.Time, func()) {
			t := time.NewTicker(pingEvery)
			return t.C, t.Stop
		},
	}
}

// run pings at once and then every pingEvery, until ctx ends.
func (w *watchdog) run(ctx context.Context) {
	ticks, stop := w.ticker()
	defer stop()
	for {
		w.ping()
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// ping tells systemd that the engine makes progress, when it does.
func (w *watchdog) ping() {
	if !w.healthy(w.now()) {
		if !w.silent && w.pinged {
			w.log.Warn().Msg("the engine makes no progress; the watchdog is not told, and systemd restarts the daemon unless it does again")
		}
		w.silent = true
		return
	}
	if w.silent && w.pinged {
		w.log.Info().Msg("the engine makes progress again")
	}
	w.silent, w.pinged = false, true
	if err := w.notify.Watchdog(); err != nil {
		w.log.Debug().Err(err).Msg("pinging the watchdog of systemd failed")
	}
}
