package engine

import (
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	// TrafficInterval is how often the daemon scrapes the connectors.
	TrafficInterval = 5 * time.Second
	// trafficSamples is how many samples a tunnel keeps: 15 minutes, as
	// many as a status may carry of one.
	trafficSamples = agentproto.MaxTrafficSamples
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

// TunnelNotice is the newest sample of a tunnel. Before its first the figures
// are zero and Sampled is left out of the JSON; At is the time of the
// sample, which for a tunnel another node reports is that node's.
type TunnelNotice struct {
	TunnelID      string    `json:"tunnelId"`
	At            time.Time `json:"at,omitzero"`
	RPS           float64   `json:"rps"`
	ErrorsPerSec  float64   `json:"errorsPerSec"`
	Concurrent    float64   `json:"concurrent"`
	HAConnections int       `json:"haConnections"`
	Stale         bool      `json:"stale"`
	Sampled       bool      `json:"sampled,omitempty"`
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

// traffic holds the samples of every tunnel scraped in the last round, and
// those the agents of the other nodes report of the tunnels pinned to them.
// It is not part of the state: it changes every few seconds, and a state is
// fetched again whenever its digest changes.
type traffic struct {
	mu     sync.Mutex
	book   TrafficBook
	flows  flowTraffic
	remote map[string][]TunnelTraffic // by node
	// noticed is, by tunnel id, the time of the newest reported sample the
	// last notice carried: a notice says a reported tunnel was sampled only
	// once per sample, as the reports come slower than the rounds.
	noticed map[string]time.Time
}

// TrafficBook keeps the samples of every tunnel scraped in the last round,
// for the connectors of one node: the engine's, or an agent container's,
// which reports them to the controller.
type TrafficBook struct {
	mu      sync.Mutex
	at      time.Time
	tunnels map[string]*tunnelSeries
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
// down or the process started again, or whose figures are no finite numbers,
// has none. The stream is told of every round.
func (e *Engine) RecordTraffic(t time.Time, scrapes map[string]*connector.Metrics) {
	tr := &e.traffic
	tr.book.Record(t, scrapes)
	tr.mu.Lock()
	notice := tr.notice()
	tr.mu.Unlock()
	e.notify.send(Notice{Kind: NoticeTraffic, Traffic: &notice})
}

// Record adds one round of scrapes taken at t, as RecordTraffic does.
func (b *TrafficBook) Record(t time.Time, scrapes map[string]*connector.Metrics) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tunnels == nil {
		b.tunnels = make(map[string]*tunnelSeries)
	}
	maps.DeleteFunc(b.tunnels, func(id string, _ *tunnelSeries) bool {
		_, scraped := scrapes[id]
		return !scraped
	})
	for id, m := range scrapes {
		s := b.tunnels[id]
		if s == nil {
			s = &tunnelSeries{}
			b.tunnels[id] = s
		}
		s.add(t, m)
	}
	b.at = t
}

// views are the tunnels of the book as the traffic view shows them, for
// node, a copy; the caller holds b.mu.
func (b *TrafficBook) views(node string) []TunnelTraffic {
	out := make([]TunnelTraffic, 0, len(b.tunnels))
	for _, id := range slices.Sorted(maps.Keys(b.tunnels)) {
		s := b.tunnels[id]
		tt := TunnelTraffic{
			TunnelID: id, Node: node, Stale: s.stale(),
			Edges: []connector.Edge{}, RTTMillis: []float64{}, Samples: slices.Clone(nonNil(s.samples)),
		}
		if m := s.last; m != nil {
			tt.Cloudflared, tt.ConfigVersion, tt.HAConnections = m.Version, m.ConfigVersion, m.HAConnections
			tt.Edges = append(tt.Edges, m.Edges...)
			tt.RTTMillis = append(tt.RTTMillis, m.RTTMillis...)
		}
		out = append(out, tt)
	}
	return out
}

// Report is the book as an agent reports it to the controller.
func (b *TrafficBook) Report() []agentproto.TunnelTraffic {
	b.mu.Lock()
	defer b.mu.Unlock()
	views := b.views("")
	out := make([]agentproto.TunnelTraffic, 0, len(views))
	for _, v := range views {
		out = append(out, agentproto.TunnelTraffic{
			TunnelID: v.TunnelID, Cloudflared: v.Cloudflared, ConfigVersion: v.ConfigVersion, HAConnections: v.HAConnections,
			Edges: v.Edges, RTTMillis: v.RTTMillis, Stale: v.Stale, Samples: samplesOf(v.Samples),
		})
	}
	return out
}

func samplesOf(in []TrafficSample) []agentproto.TrafficSample {
	out := make([]agentproto.TrafficSample, len(in))
	for i, s := range in {
		out[i] = agentproto.TrafficSample{At: s.At, RPS: s.RPS, ErrorsPerSec: s.ErrorsPerSec, Concurrent: s.Concurrent}
	}
	return out
}

// reported is a tunnel's traffic as the agent of node reported it, as the
// traffic view shows it.
func reported(node string, t agentproto.TunnelTraffic) TunnelTraffic {
	tt := TunnelTraffic{
		TunnelID: t.TunnelID, Node: node, Cloudflared: t.Cloudflared, ConfigVersion: t.ConfigVersion, HAConnections: t.HAConnections,
		Edges: append([]connector.Edge{}, t.Edges...), RTTMillis: append([]float64{}, t.RTTMillis...), Stale: t.Stale,
		Samples: make([]TrafficSample, 0, len(t.Samples)),
	}
	for _, s := range t.Samples {
		tt.Samples = append(tt.Samples, TrafficSample{At: s.At, RPS: s.RPS, ErrorsPerSec: s.ErrorsPerSec, Concurrent: s.Concurrent})
	}
	if len(tt.Samples) > trafficSamples {
		tt.Samples = tt.Samples[len(tt.Samples)-trafficSamples:]
	}
	return tt
}

// RecordRemoteTraffic keeps what the agents of the other nodes report of
// the connectors of the tunnels pinned to their nodes: by node, the
// traffic of the tunnels among pinned that the node reported. A node not
// in reports loses its entries.
func (e *Engine) RecordRemoteTraffic(reports map[string][]agentproto.TunnelTraffic, pinned map[string]string) {
	tr := &e.traffic
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.remote = make(map[string][]TunnelTraffic, len(reports))
	for node, list := range reports {
		for _, t := range list {
			if pinned[t.TunnelID] != node {
				continue
			}
			tr.remote[node] = append(tr.remote[node], reported(node, t))
		}
	}
}

// remoteViews are the reported tunnels, by node name and tunnel id. The
// caller holds mu.
func (tr *traffic) remoteViews() []TunnelTraffic {
	var out []TunnelTraffic
	for _, node := range slices.Sorted(maps.Keys(tr.remote)) {
		out = append(out, tr.remote[node]...)
	}
	slices.SortStableFunc(out, func(a, b TunnelTraffic) int { return strings.Compare(a.TunnelID, b.TunnelID) })
	return out
}

func (s *tunnelSeries) add(t time.Time, m *connector.Metrics) {
	if m == nil {
		s.misses++
		return
	}
	s.misses = 0
	if s.last != nil && !restarted(s.last, m) {
		elapsed := t.Sub(s.lastAt).Seconds()
		requests, failed := m.Requests-s.last.Requests, m.RequestErrors-s.last.RequestErrors
		if elapsed > 0 && requests >= 0 && failed >= 0 {
			sample := TrafficSample{At: t, RPS: requests / elapsed, ErrorsPerSec: failed / elapsed, Concurrent: m.Concurrent}
			// The figures come from a port another process could answer on.
			if finite(sample.RPS, sample.ErrorsPerSec, sample.Concurrent) {
				s.samples = append(s.samples, sample)
				if extra := len(s.samples) - trafficSamples; extra > 0 {
					s.samples = slices.Delete(s.samples, 0, extra)
				}
			}
		}
	}
	s.last, s.lastAt = m, t
}

// restarted is whether the process is another than the one scraped before, by
// its start time. A counter says so only when it fell below the old one.
func restarted(before, now *connector.Metrics) bool {
	return before.Started != 0 && now.Started != 0 && before.Started != now.Started
}

func finite(figures ...float64) bool {
	return !slices.ContainsFunc(figures, func(f float64) bool { return math.IsNaN(f) || math.IsInf(f, 0) })
}

func (s *tunnelSeries) stale() bool { return s.misses >= staleAfter }

// notice is the newest sample of every tunnel, this node's and those the
// other nodes reported of their pinned tunnels, and the figures of the
// routes a notice carries. The caller holds mu.
func (tr *traffic) notice() TrafficNotice {
	b := &tr.book
	b.mu.Lock()
	n := TrafficNotice{At: b.at, Tunnels: make([]TunnelNotice, 0, len(b.tunnels))}
	for _, id := range slices.Sorted(maps.Keys(b.tunnels)) {
		s := b.tunnels[id]
		tn := TunnelNotice{TunnelID: id, Stale: s.stale()}
		if len(s.samples) > 0 {
			newest := s.samples[len(s.samples)-1]
			tn.At, tn.RPS, tn.ErrorsPerSec, tn.Concurrent = newest.At, newest.RPS, newest.ErrorsPerSec, newest.Concurrent
			tn.Sampled = true
		}
		if s.last != nil {
			tn.HAConnections = s.last.HAConnections
		}
		n.Tunnels = append(n.Tunnels, tn)
	}
	b.mu.Unlock()
	if tr.noticed == nil {
		tr.noticed = map[string]time.Time{}
	}
	for _, tt := range tr.remoteViews() {
		tn := TunnelNotice{TunnelID: tt.TunnelID, Stale: tt.Stale, HAConnections: tt.HAConnections}
		if len(tt.Samples) > 0 {
			newest := tt.Samples[len(tt.Samples)-1]
			tn.At, tn.RPS, tn.ErrorsPerSec, tn.Concurrent = newest.At, newest.RPS, newest.ErrorsPerSec, newest.Concurrent
			if newest.At.After(tr.noticed[tt.TunnelID]) {
				tn.Sampled = true
				tr.noticed[tt.TunnelID] = newest.At
			}
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

// Traffic returns the samples of the connectors on this node, those the
// agents of the other nodes reported of the tunnels pinned to their nodes,
// and the figures of every route with one, a copy.
func (e *Engine) Traffic() TrafficView {
	tr := &e.traffic
	tr.mu.Lock()
	defer tr.mu.Unlock()
	b := &tr.book
	b.mu.Lock()
	v := TrafficView{At: b.at, Interval: TrafficInterval.String(), Tunnels: b.views(e.d.Node)}
	b.mu.Unlock()
	v.Tunnels = append(v.Tunnels, tr.remoteViews()...)
	v.Routes, v.RoutesWhy = tr.flows.all(), tr.flows.why
	v.RoutesTotal = len(v.Routes)
	return v
}
