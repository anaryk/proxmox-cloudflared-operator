package engine

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	// TrafficInterval is how often the daemon scrapes the connectors.
	TrafficInterval = 5 * time.Second
	// trafficSamples is how many samples a tunnel keeps: 15 minutes.
	trafficSamples = 180
	// staleAfter is how many scrapes in a row may fail before the figures of
	// a tunnel are stale.
	staleAfter = 3
)

// TrafficSample is the traffic of a tunnel's connector over one interval.
type TrafficSample struct {
	At           time.Time `json:"at"`
	RPS          float64   `json:"rps"`
	ErrorsPerSec float64   `json:"errorsPerSec"`
	Concurrent   float64   `json:"concurrent"`
}

// TunnelTraffic is the traffic of the connector of one tunnel on this node,
// and what its last scrape said of its connections.
type TunnelTraffic struct {
	TunnelID      string           `json:"tunnelId"`
	Node          string           `json:"node"`
	Cloudflared   string           `json:"cloudflared,omitempty"`
	ConfigVersion int              `json:"configVersion"`
	HAConnections int              `json:"haConnections"`
	Edges         []connector.Edge `json:"edges"`
	RTTMillis     []float64        `json:"rttMs"`
	Stale         bool             `json:"stale"`
	Samples       []TrafficSample  `json:"samples"` // oldest first, at most 180
}

// TrafficView is the traffic of the connectors on this node as the last
// round of scrapes left it, and the rate of new connections to the target of
// each route as the last read of the counters of the egress filter did.
type TrafficView struct {
	At          time.Time       `json:"at"`
	Interval    string          `json:"interval"`
	Tunnels     []TunnelTraffic `json:"tunnels"` // by tunnel id
	Routes      []RouteTraffic  `json:"routes"`  // by hostname; those with a target the read counted
	RoutesTotal int             `json:"routesTotal"`
	RoutesWhy   string          `json:"routesWhy,omitempty"` // why there are no routes
}

// TunnelNotice is the newest sample of a tunnel, zero before its first.
type TunnelNotice struct {
	TunnelID      string  `json:"tunnelId"`
	RPS           float64 `json:"rps"`
	ErrorsPerSec  float64 `json:"errorsPerSec"`
	Concurrent    float64 `json:"concurrent"`
	HAConnections int     `json:"haConnections"`
	Stale         bool    `json:"stale"`
}

// TrafficNotice is what the stream is told of a round of scrapes. Of the
// routes it carries at most 100: the busiest, then those that stopped since
// the last notice; RoutesTotal says how many have a figure.
type TrafficNotice struct {
	At          time.Time      `json:"at"`
	Tunnels     []TunnelNotice `json:"tunnels"` // by tunnel id
	Routes      []RouteTraffic `json:"routes,omitempty"`
	RoutesTotal int            `json:"routesTotal"`
	RoutesWhy   string         `json:"routesWhy,omitempty"`
}

// traffic holds the samples of every tunnel scraped in the last round. It is
// not part of the state: it changes every few seconds, and a state is
// fetched again whenever its digest changes.
type traffic struct {
	mu      sync.Mutex
	at      time.Time
	tunnels map[string]*tunnelSeries
	flows   flowTraffic
}

// tunnelSeries is what the scrapes of one tunnel gave so far.
type tunnelSeries struct {
	last    *connector.Metrics // of the last scrape that worked
	lastAt  time.Time
	misses  int // failed scrapes since the last that worked
	samples []TrafficSample
}

// RecordTraffic adds one round of scrapes taken at t, nil for a tunnel whose
// scrape failed. A tunnel not in the round is forgotten. The rates are the
// growth of the counters over the time since the last scrape that worked, on
// the monotonic clock when t has it; an interval in which a counter went
// down, as when the connector started again, has none. The stream is told of
// every round.
func (e *Engine) RecordTraffic(t time.Time, scrapes map[string]*connector.Metrics) {
	tr := &e.traffic
	tr.mu.Lock()
	if tr.tunnels == nil {
		tr.tunnels = make(map[string]*tunnelSeries)
	}
	maps.DeleteFunc(tr.tunnels, func(id string, _ *tunnelSeries) bool {
		_, scraped := scrapes[id]
		return !scraped
	})
	for id, m := range scrapes {
		s := tr.tunnels[id]
		if s == nil {
			s = &tunnelSeries{}
			tr.tunnels[id] = s
		}
		s.add(t, m)
	}
	tr.at = t
	notice := tr.notice()
	tr.mu.Unlock()
	e.notify.send(Notice{Kind: NoticeTraffic, Traffic: &notice})
}

func (s *tunnelSeries) add(t time.Time, m *connector.Metrics) {
	if m == nil {
		s.misses++
		return
	}
	s.misses = 0
	if s.last != nil {
		elapsed := t.Sub(s.lastAt).Seconds()
		requests, failed := m.Requests-s.last.Requests, m.RequestErrors-s.last.RequestErrors
		if elapsed > 0 && requests >= 0 && failed >= 0 {
			s.samples = append(s.samples, TrafficSample{
				At: t, RPS: requests / elapsed, ErrorsPerSec: failed / elapsed, Concurrent: m.Concurrent,
			})
			if extra := len(s.samples) - trafficSamples; extra > 0 {
				s.samples = slices.Delete(s.samples, 0, extra)
			}
		}
	}
	s.last, s.lastAt = m, t
}

func (s *tunnelSeries) stale() bool { return s.misses >= staleAfter }

// notice is the newest sample of every tunnel, and the figures of the routes
// a notice carries. The caller holds mu.
func (tr *traffic) notice() TrafficNotice {
	n := TrafficNotice{At: tr.at, Tunnels: make([]TunnelNotice, 0, len(tr.tunnels))}
	for _, id := range slices.Sorted(maps.Keys(tr.tunnels)) {
		s := tr.tunnels[id]
		tn := TunnelNotice{TunnelID: id, Stale: s.stale()}
		if len(s.samples) > 0 {
			newest := s.samples[len(s.samples)-1]
			tn.RPS, tn.ErrorsPerSec, tn.Concurrent = newest.RPS, newest.ErrorsPerSec, newest.Concurrent
		}
		if s.last != nil {
			tn.HAConnections = s.last.HAConnections
		}
		n.Tunnels = append(n.Tunnels, tn)
	}
	n.Routes, n.RoutesTotal, n.RoutesWhy = tr.flows.notice()
	return n
}

// ConnectorStatuses returns the statuses of the connectors as the last cycle
// read them, without a copy of the rest of the state.
func (e *Engine) ConnectorStatuses() []connector.Status {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return slices.Clone(e.state.Connectors)
}

// Traffic returns the samples of the connectors on this node and the figures
// of every route with one, a copy.
func (e *Engine) Traffic() TrafficView {
	tr := &e.traffic
	tr.mu.Lock()
	defer tr.mu.Unlock()
	v := TrafficView{At: tr.at, Interval: TrafficInterval.String(), Tunnels: make([]TunnelTraffic, 0, len(tr.tunnels))}
	for _, id := range slices.Sorted(maps.Keys(tr.tunnels)) {
		s := tr.tunnels[id]
		tt := TunnelTraffic{
			TunnelID: id, Node: e.d.Node, Stale: s.stale(),
			Edges: []connector.Edge{}, RTTMillis: []float64{}, Samples: slices.Clone(nonNil(s.samples)),
		}
		if m := s.last; m != nil {
			tt.Cloudflared, tt.ConfigVersion, tt.HAConnections = m.Version, m.ConfigVersion, m.HAConnections
			tt.Edges = append(tt.Edges, m.Edges...)
			tt.RTTMillis = append(tt.RTTMillis, m.RTTMillis...)
		}
		v.Tunnels = append(v.Tunnels, tt)
	}
	v.Routes, v.RoutesWhy = tr.flows.all(), tr.flows.why
	v.RoutesTotal = len(v.Routes)
	return v
}
