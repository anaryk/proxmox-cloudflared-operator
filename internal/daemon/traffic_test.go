package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// fakeScrapes answers a scrape after took, on the clock of the bubble, and
// keeps what it was asked.
type fakeScrapes struct {
	took time.Duration
	fail map[string]error

	mu            sync.Mutex
	asked         []string
	running, most int
}

func (f *fakeScrapes) scrape(ctx context.Context, id string) (connector.Metrics, error) {
	f.mu.Lock()
	f.asked = append(f.asked, id)
	f.running++
	f.most = max(f.most, f.running)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.running--
		f.mu.Unlock()
	}()
	select {
	case <-time.After(f.took):
	case <-ctx.Done():
		return connector.Metrics{}, ctx.Err()
	}
	if err := f.fail[id]; err != nil {
		return connector.Metrics{}, err
	}
	return connector.Metrics{Requests: 1, HAConnections: 4}, nil
}

func (f *fakeScrapes) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(slices.Values(f.asked))
}

// round is one call of record.
type round struct {
	at      time.Time
	scrapes map[string]*connector.Metrics
}

type samplerRig struct {
	s       *sampler
	scrapes *fakeScrapes

	mu     sync.Mutex
	rounds []round
	// reads are the reads of the counters of the egress filter: the time of
	// the round each was for, and how long after it began each was made.
	reads []counterRead
	calls []string
}

type counterRead struct {
	at    time.Time
	after time.Duration
}

func newSamplerRig(statuses []connector.Status, took time.Duration) *samplerRig {
	r := &samplerRig{scrapes: &fakeScrapes{took: took}}
	r.s = &sampler{
		statuses: func() []connector.Status { return slices.Clone(statuses) },
		scrape:   r.scrapes.scrape,
		record: func(at time.Time, scrapes map[string]*connector.Metrics) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.rounds = append(r.rounds, round{at, scrapes})
			r.calls = append(r.calls, "record")
		},
		targets: func(_ context.Context, at time.Time) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.reads = append(r.reads, counterRead{at, time.Since(at)})
			r.calls = append(r.calls, "targets")
		},
		now: time.Now,
		log: zerolog.Nop(),
	}
	return r
}

func (r *samplerRig) recorded() []round {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.rounds)
}

func (r *samplerRig) counterReads() ([]counterRead, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads), slices.Clone(r.calls)
}

func tunnelNumbered(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func scrapable(id string) connector.Status {
	return connector.Status{TunnelID: id, Active: true, Ready: true, Connections: 4, MetricsAddr: "127.0.0.1:20300"}
}

// Thirty scrapes that each take the most the manager allows go in four turns
// of eight, which is longer than the interval: the next round begins as this
// one ends.
func TestARoundOfThirtySlowScrapesTakesFourTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var statuses []connector.Status
		for i := range 30 {
			statuses = append(statuses, scrapable(tunnelNumbered(i)))
		}
		r := newSamplerRig(statuses, 2*time.Second)

		start := time.Now()
		r.s.round(t.Context())
		took := time.Since(start)

		require.Equal(t, 8*time.Second, took)
		require.Greater(t, took, engine.TrafficInterval)
		require.Equal(t, 8, r.scrapes.most, "at most 8 at once")
		got := r.recorded()
		require.Len(t, got, 1)
		require.Equal(t, start, got[0].at)
		require.Len(t, got[0].scrapes, 30)
		for id, m := range got[0].scrapes {
			require.NotNil(t, m, id)
		}
	})
}

// What answers on the port of a connector that does not run, or whose port
// another process holds, is not the connector: it is not asked, and its
// round counts as a missed scrape, as one that failed does.
func TestOnlyConnectorsThatRunOnTheirOwnPortAreScraped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		held := scrapable(tunnelNumbered(2))
		held.Ready, held.MetricsPortHeld = false, true
		stopped := scrapable(tunnelNumbered(3))
		stopped.Active, stopped.Ready = false, false
		noAddress := scrapable(tunnelNumbered(4))
		noAddress.MetricsAddr = ""
		r := newSamplerRig([]connector.Status{scrapable(tunnelNumbered(1)), held, stopped, noAddress, scrapable(tunnelNumbered(5))}, time.Second)
		r.scrapes.fail = map[string]error{tunnelNumbered(5): errors.New("connection refused")}

		r.s.round(t.Context())

		require.Equal(t, []string{tunnelNumbered(1), tunnelNumbered(5)}, r.scrapes.seen())
		got := r.recorded()
		require.Len(t, got, 1)
		require.Equal(t, map[string]*connector.Metrics{
			tunnelNumbered(1): {Requests: 1, HAConnections: 4},
			tunnelNumbered(2): nil, tunnelNumbered(3): nil, tunnelNumbered(4): nil, tunnelNumbered(5): nil,
		}, got[0].scrapes)
	})
}

// The rounds come every interval; one that takes longer is not overlapped,
// and the next begins as it ends.
func TestTheRoundsComeOneAfterTheOther(t *testing.T) {
	for _, tt := range []struct {
		name string
		took time.Duration
		at   []time.Duration
	}{
		{"within the interval", time.Second, []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second, 25 * time.Second}},
		{"longer than the interval", 7 * time.Second, []time.Duration{0, 7 * time.Second, 14 * time.Second}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newSamplerRig([]connector.Status{scrapable(tunnelNumbered(1))}, tt.took)
				ctx, cancel := context.WithCancel(t.Context())
				start := time.Now()
				done := make(chan struct{})
				go func() {
					defer close(done)
					r.s.run(ctx)
				}()

				time.Sleep(27 * time.Second)
				cancel()
				<-done

				var at []time.Duration
				for _, rd := range r.recorded() {
					at = append(at, rd.at.Sub(start))
				}
				require.Equal(t, tt.at, at, "a round cut off by the stop is not recorded")
				require.Equal(t, 1, r.scrapes.most)
			})
		})
	}
}

// The counters of the egress filter are read in the same round, once the
// scrapes are in, and recorded before the round is: the notice of the round
// carries both.
func TestTheCountersAreReadInTheRoundAfterTheScrapes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newSamplerRig([]connector.Status{scrapable(tunnelNumbered(1))}, 1500*time.Millisecond)
		start := time.Now()

		r.s.round(t.Context())

		reads, calls := r.counterReads()
		require.Equal(t, []counterRead{{at: start, after: 1500 * time.Millisecond}}, reads)
		require.Equal(t, []string{"targets", "record"}, calls)
	})
}

func TestARoundCutOffBeforeTheReadReadsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newSamplerRig([]connector.Status{scrapable(tunnelNumbered(1))}, time.Minute)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()

		r.s.round(ctx)

		reads, calls := r.counterReads()
		require.Empty(t, reads)
		require.Empty(t, calls)
	})
}
