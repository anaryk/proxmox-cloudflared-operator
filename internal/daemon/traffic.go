package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// scrapesAtOnce bounds the scrapes of a round: with each one bounded to 2 s
// by the connector manager, 30 tunnels take four of them, within the
// interval.
const scrapesAtOnce = 8

// sampler scrapes the metrics of the connectors on the node every
// engine.TrafficInterval, has the counters of the egress filter read, and
// hands each round to the engine. It reads, and never changes, anything: a
// scrape that fails is a missed sample, not a problem of the cycle.
type sampler struct {
	statuses func() []connector.Status
	scrape   func(ctx context.Context, tunnelID string) (connector.Metrics, error)
	record   func(at time.Time, scrapes map[string]*connector.Metrics)
	// targets reads the counters of the egress filter, while it is on, and
	// records them as of at.
	targets func(ctx context.Context, at time.Time)
	now     func() time.Time
	log     zerolog.Logger
}

// run samples until ctx ends. A round that takes longer than the interval is
// not overlapped: the next one begins as it ends.
func (s *sampler) run(ctx context.Context) {
	tick := time.NewTicker(engine.TrafficInterval)
	defer tick.Stop()
	for {
		s.round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// round scrapes the connectors of the last state's tunnels, at most
// scrapesAtOnce at a time, then has the counters of the egress filter read,
// and records both as of the time it began: the scrapes last, as their notice
// carries the counters too. A connector that does not run, has no metrics
// address or whose port another process holds is not asked: what answers
// there is not the connector, and would be shown as its traffic. A round the
// stop cut off is not recorded.
func (s *sampler) round(ctx context.Context) {
	at := s.now()
	statuses := s.statuses()
	got := make([]*connector.Metrics, len(statuses))
	slots := make(chan struct{}, scrapesAtOnce)
	var wg sync.WaitGroup
	for i, st := range statuses {
		if why := unscraped(st); why != "" {
			s.log.Debug().Str("tunnel", st.TunnelID).Msg("not scraping the metrics of a connector: " + why)
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			m, err := s.scrape(ctx, st.TunnelID)
			if err != nil {
				s.log.Debug().Err(err).Str("tunnel", st.TunnelID).Msg("scraping the metrics of a connector failed")
				return
			}
			got[i] = &m
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	s.targets(ctx, at)
	if ctx.Err() != nil {
		return
	}
	scrapes := make(map[string]*connector.Metrics, len(statuses))
	for i, st := range statuses {
		scrapes[st.TunnelID] = got[i]
	}
	s.record(at, scrapes)
}

// unscraped says why the metrics of a connector are not asked for, or "".
func unscraped(st connector.Status) string {
	switch {
	case !st.Active:
		return "it does not run"
	case st.MetricsPortHeld:
		return "another process holds its metrics port"
	case st.MetricsAddr == "":
		return "it has no metrics address"
	}
	return ""
}
