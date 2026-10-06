// Package apifake is a daemon to develop and test the web interface against,
// without a Proxmox VE node or Cloudflare: an api.Engine that answers from a
// scenario, served by the real api.Server, so that what it answers has the
// daemon's own types and wire format.
//
// A scenario is a directory of JSON files, each in the type the API answers
// with; the scenarios of the package are made from the goldens of the engine
// and the API (see scenarios_test.go). What an admin action changes is kept
// and shown, as far as the scenario has it: an approval takes the guest off
// the list of those that wait, a confirmation ends the offer. Nothing is
// planned, resolved or reconciled.
//
// The engine records every call, and Control serves what a test needs to
// steer it: another state, events, traffic, refusals and the streams.
package apifake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// keepSamples is how many samples of traffic a tunnel and a route keep,
	// 15 minutes of them; a route starts with routeHistory.
	keepSamples  = 180
	routeHistory = 36
	// ringEvents is how many events of the running boot a query without
	// history sees, as the daemon keeps them in memory.
	ringEvents        = 1000
	defaultEventLimit = 1000
	// maxAnnotation is how many characters of the route block of a guest an
	// annotation has at most.
	maxAnnotation = 8192
)

// Why a traffic view has no routes, as the daemon says it.
const (
	whyNotChecked = "the egress filter was not checked yet"
	whyOff        = "the egress filter is off: pco has no per-guest counters"
	whyNotLoaded  = "the egress filter is not loaded"
	whyChanged    = "the egress filter is not the one pco loads"
)

// Engine implements api.Engine from a scenario and records every call. It is
// safe for concurrent use. A list it holds is replaced when it changes, never
// changed in place, so that a state or a list it handed out stays as it was.
type Engine struct {
	now   func() time.Time
	dir   string       // of the store, removed by Close
	store *store.Store // the settings and the manual routes, with revisions

	mu           sync.Mutex
	state        engine.State
	cycle        time.Duration // how long a cycle takes, as the scenario's did
	boot         string
	seq          uint64         // of the last event of the boot
	events       []engine.Event // of every boot, oldest first
	traffic      engine.TrafficView
	series       map[string][]engine.RouteSample // by hostname
	moving       map[string]bool                 // the routes whose rate the last notice gave as not zero
	guests       []engine.GuestListView
	notes        map[string]string
	claims       []engine.ClaimView
	approvals    []engine.ApprovalView
	report       credentials.Report
	readAtStart  []string
	settingNotes []string
	started      store.Settings // what a change of a setting read at start is compared with
	refusals     map[string]error
	calls        []Call
	subs         map[*subscriber]bool
	resumed      chan struct{} // closed when the streams resume; nil while they run
}

// Call is a call of the engine: the method, its arguments as JSON and, for a
// request the API was told the actor of, who asked. Boot, RunsAs and
// PollInterval, which the API asks to describe the daemon in its answers, are
// not recorded; a token is recorded by its length.
type Call struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
	At     time.Time       `json:"at"`
	Actor  string          `json:"actor,omitempty"`
}

// Load reads the scenario of a directory. now is the clock of the engine; nil
// is the system's. Close removes what the engine keeps on disk.
func Load(dir string, now func() time.Time) (*Engine, error) {
	f, err := readFiles(os.DirFS(dir))
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", dir, err)
	}
	return newEngine(f, now)
}

// Scenario loads a scenario of the package by its name.
func Scenario(name string, now func() time.Time) (*Engine, error) {
	f, err := builtinFiles(name)
	if err != nil {
		return nil, fmt.Errorf("scenario %s: %w", name, err)
	}
	return newEngine(f, now)
}

func newEngine(f files, now func() time.Time) (*Engine, error) {
	if now == nil {
		now = time.Now
	}
	dir, err := os.MkdirTemp("", "apifake-")
	if err != nil {
		return nil, err
	}
	e := &Engine{
		// The times of the state are UTC, as the goldens have them.
		now: func() time.Time { return now().UTC() },
		dir: dir, refusals: map[string]error{}, subs: map[*subscriber]bool{}, moving: map[string]bool{},
	}
	if err := e.open(f); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return e, nil
}

// open takes the scenario in. The last cycle of its state finished just now:
// the times of the cycle, of the events and of the traffic move with it.
func (e *Engine) open(f files) error {
	paths := store.Paths{Cluster: filepath.Join(e.dir, "cluster"), Private: filepath.Join(e.dir, "private"), Local: filepath.Join(e.dir, "local")}
	for _, p := range []string{paths.Cluster, paths.Private} {
		if err := os.Mkdir(p, 0o700); err != nil {
			return err
		}
	}
	st, err := store.Open(paths)
	if err != nil {
		return err
	}
	e.store = st
	settings := store.DefaultSettings()
	if v := f.Settings; v != nil {
		settings, e.readAtStart, e.settingNotes = v.Settings, slices.Sorted(slices.Values(v.ReadAtStart)), v.Notes
	}
	if err := st.SaveSettings(settings); err != nil {
		return fmt.Errorf("%s: %w", fileSettings, err)
	}
	if e.started, err = st.Settings(); err != nil {
		return err
	}
	for _, v := range f.Manual {
		r, err := manualRoute(v, e.started)
		if err == nil {
			_, err = st.SaveManualRouteIf(0, r)
		}
		if err != nil {
			return fmt.Errorf("%s: %s: %w", fileManual, v.ID, err)
		}
	}

	now := e.now()
	var shift time.Duration
	e.state = f.State
	sortState(&e.state)
	if !e.state.At.IsZero() {
		shift = now.Sub(e.state.FinishedAt)
		e.cycle = e.state.FinishedAt.Sub(e.state.At)
		e.state.At, e.state.FinishedAt = e.state.At.Add(shift), now
	}
	e.state.Digest = digestOf(e.state)
	e.boot = newBoot()
	for _, ev := range f.Events {
		if !ev.At.IsZero() {
			ev.At = ev.At.Add(shift)
		}
		e.seq++
		ev.Seq, ev.Boot = e.seq, e.boot
		e.events = append(e.events, ev)
	}
	e.openTraffic(f.Traffic, now)
	e.guests, e.notes, e.claims, e.approvals = nonNil(f.Guests), f.Notes, nonNil(f.Claims), nonNil(f.Approvals)
	e.report = e.reportOf(f.Report)
	return nil
}

// openTraffic takes the traffic of the scenario, or makes it from the state,
// with its newest sample now.
func (e *Engine) openTraffic(tv *engine.TrafficView, now time.Time) {
	if tv == nil {
		tv = &engine.TrafficView{Tunnels: []engine.TunnelTraffic{}}
		for _, c := range e.state.Connectors {
			tv.Tunnels = append(tv.Tunnels, engine.TunnelTraffic{
				TunnelID: c.TunnelID, Node: e.state.Node, Edges: []connector.Edge{},
				RTTMillis: []float64{}, Samples: []engine.TrafficSample{},
			})
		}
		if tv.RoutesWhy = whyOf(e.state.Egress.State); tv.RoutesWhy == "" {
			tv.Routes = routeTraffic(e.state.Routes)
		}
	}
	e.traffic = *tv
	e.traffic.At, e.traffic.Interval = now, engine.TrafficInterval.String()
	e.traffic.Routes = nonNil(e.traffic.Routes)
	e.traffic.RoutesTotal = len(e.traffic.Routes)
	for i := range e.traffic.Tunnels {
		s := e.traffic.Tunnels[i].Samples
		for j := range s {
			s[j].At = now.Add(-time.Duration(len(s)-1-j) * engine.TrafficInterval)
		}
	}
	e.series = map[string][]engine.RouteSample{}
	for _, r := range e.traffic.Routes {
		s := make([]engine.RouteSample, routeHistory)
		for j := range s {
			s[j] = engine.RouteSample{At: now.Add(-time.Duration(routeHistory-1-j) * engine.TrafficInterval), FlowsPerSec: r.FlowsPerSec}
		}
		e.series[r.Hostname] = s
	}
}

// reportOf is what the check of a credential added finds: the scenario's
// report, or else that of a credential of the state.
func (e *Engine) reportOf(r *credentials.Report) credentials.Report {
	if r != nil {
		return *r
	}
	for _, c := range e.state.Credentials {
		if c.Checked {
			return c.Report
		}
	}
	return credentials.Report{
		Token: cfapi.TokenStatus{ID: "token", Status: "active"}, Accounts: []cfapi.Account{}, Zones: []cfapi.Zone{},
		Checks: []credentials.Check{}, Excluded: []credentials.Exclusion{}, Leftovers: []string{}, Usable: true,
	}
}

// Close removes what the engine keeps on disk.
func (e *Engine) Close() error { return os.RemoveAll(e.dir) }

func newBoot() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// whyOf is why there are no figures of the routes while the egress filter is
// in state; nothing when it is on.
func whyOf(state string) string {
	switch state {
	case engine.EgressOn:
		return ""
	case engine.EgressOff:
		return whyOff
	case engine.EgressNotLoaded:
		return whyNotLoaded
	case engine.EgressChanged:
		return whyChanged
	}
	return whyNotChecked
}

// Calls returns the calls the engine had, oldest first.
func (e *Engine) Calls() []Call {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

// record notes a call. The caller does not hold mu.
func (e *Engine) record(ctx context.Context, method string, args any) {
	data, err := json.Marshal(args)
	if err != nil {
		data = []byte("null")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, Call{Method: method, Args: data, At: e.now(), Actor: engine.ActorOf(ctx)})
}

// refused is the failure a control asked the next call of method to have,
// which it uses up, or nil.
func (e *Engine) refused(method string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.refusals[method]
	delete(e.refusals, method)
	return err
}

// begin records a call and returns the failure asked for it, if any.
func (e *Engine) begin(ctx context.Context, method string, args any) error {
	e.record(ctx, method, args)
	return e.refused(method)
}

// noArgs are the arguments of a call that has none.
var noArgs = struct{}{}

// State returns the state of the scenario, as the last cycle left it.
func (e *Engine) State() engine.State {
	e.record(context.Background(), "State", noArgs)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// Boot names this process of the daemon; a control starts a new one.
func (e *Engine) Boot() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.boot
}

// RunsAs is the profile and the node of the state.
func (e *Engine) RunsAs() (profile, node string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Profile, e.state.Node
}

// PollInterval is the poll interval of the settings.
func (e *Engine) PollInterval() time.Duration {
	s, err := e.store.Settings()
	if err != nil {
		return time.Duration(store.DefaultSettings().PollInterval)
	}
	return time.Duration(s.PollInterval)
}

// QueryEvents answers from the events of the scenario and those added since,
// as the daemon does from its ring, and with History from those of earlier
// boots too.
func (e *Engine) QueryEvents(q engine.EventQuery) ([]engine.Event, error) {
	if err := e.begin(context.Background(), "QueryEvents", q); err != nil {
		return nil, err
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultEventLimit
	case limit > engine.MaxEventLimit:
		limit = engine.MaxEventLimit
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	after := q.After
	if q.Boot != "" && q.Boot != e.boot {
		after = 0
	}
	from := e.events
	if !q.History {
		from = e.ring()
	}
	out := []engine.Event{}
	for _, ev := range from {
		if q.Match(ev) && (after == 0 || ev.Boot == e.boot && ev.Seq > after) {
			out = append(out, ev)
		}
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return slices.Clone(out), nil
}

// ring is the newest events of the boot. The caller holds mu.
func (e *Engine) ring() []engine.Event {
	i := slices.IndexFunc(e.events, func(ev engine.Event) bool { return ev.Boot == e.boot })
	if i < 0 {
		return nil
	}
	ring := e.events[i:]
	return ring[max(0, len(ring)-ringEvents):]
}

// Trigger runs a cycle now.
func (e *Engine) Trigger() {
	e.record(context.Background(), "Trigger", noArgs)
	e.Cycle()
}

// Diagnose walks the chain of a route as the daemon does, with the state of
// the scenario, but asks no target: a route whose target the daemon would ask
// gets the answer 200 OK.
func (e *Engine) Diagnose(ctx context.Context, host string) ([]doctor.Step, error) {
	if err := e.begin(ctx, "Diagnose", struct {
		Hostname string `json:"hostname"`
	}{host}); err != nil {
		return nil, err
	}
	e.mu.Lock()
	st := e.state
	e.mu.Unlock()
	// The request to the target is the one thing that needs the context: a
	// context that ended makes none.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	steps, err := doctor.DiagnoseRoute(ended, st, host, nil)
	if err != nil {
		return nil, err
	}
	if n := len(steps); n > 0 && steps[n-1].Name == "http" && steps[n-1].Level == doctor.LevelFail && steps[n-1].Detail == "the request failed" {
		steps[n-1].Level, steps[n-1].Detail = doctor.LevelOK, "the origin answered 200 OK"
	}
	return steps, nil
}

// Doctor runs the checks of the daemon on the state of the scenario, on a
// host where everything else is as it should be.
func (e *Engine) Doctor(ctx context.Context) []doctor.Finding {
	e.record(ctx, "Doctor", noArgs)
	e.mu.Lock()
	st := e.state
	e.mu.Unlock()
	return doctor.Run(ctx, st, healthyHost{now: e.now(), poll: e.PollInterval()})
}

// Traffic returns the traffic of the scenario, as it was sampled since.
func (e *Engine) Traffic() engine.TrafficView {
	e.record(context.Background(), "Traffic", noArgs)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.trafficView()
}

// trafficView is a copy of the traffic that shares nothing that changes. The
// caller holds mu.
func (e *Engine) trafficView() engine.TrafficView {
	v := e.traffic
	v.Tunnels = slices.Clone(v.Tunnels)
	v.Routes = slices.Clone(v.Routes)
	v.RoutesTotal = len(v.Routes)
	return v
}

// RouteSeries returns the samples of the route of a hostname.
func (e *Engine) RouteSeries(host string) (engine.RouteSeries, error) {
	if err := e.begin(context.Background(), "RouteSeries", struct {
		Hostname string `json:"hostname"`
	}{host}); err != nil {
		return engine.RouteSeries{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	i := slices.IndexFunc(e.traffic.Routes, func(r engine.RouteTraffic) bool { return r.Hostname == host })
	if i < 0 {
		return engine.RouteSeries{}, fmt.Errorf("%w: %s has no target", engine.ErrNotFound, host)
	}
	r := e.traffic.Routes[i]
	return engine.RouteSeries{Hostname: host, Target: r.Target, Shared: r.Shared, Samples: nonNil(slices.Clone(e.series[host]))}, nil
}

// its digest did. The caller holds mu.
func (e *Engine) changed() {
	d := digestOf(e.state)
	if d == e.state.Digest {
		return
	}
	e.state.Digest = d
	e.deliver(stateNotice(e.state))
}

func stateNotice(st engine.State) engine.Notice {
	return engine.Notice{Kind: engine.NoticeState, State: &engine.StateNotice{At: st.At, FinishedAt: st.FinishedAt, Digest: st.Digest}}
}

// healthyHost is a host on which every check that is not of the state
// passes.
type healthyHost struct {
	now  time.Time
	poll time.Duration
}

func (h healthyHost) CloudflaredVersion(context.Context) (string, error) {
	return fmt.Sprintf("cloudflared version %d.%d.0 (built %s)", h.now.Year(), int(h.now.Month()), h.now.Format("2006-01-02")), nil
}
func (healthyHost) UnitActive(context.Context, string) (bool, error)  { return true, nil }
func (healthyHost) UnitEnabled(context.Context, string) (bool, error) { return true, nil }
func (healthyHost) Store(context.Context) error                       { return nil }
func (h healthyHost) Now() time.Time                                  { return h.now }
func (healthyHost) CanDial(context.Context, string, string) error     { return nil }
func (healthyHost) PVEVersion(context.Context) (string, error)        { return "8.4.1", nil }
func (healthyHost) NodeLock(context.Context) error                    { return nil }
func (h healthyHost) PollInterval() time.Duration                     { return h.poll }
func (healthyHost) WebCert(context.Context) (doctor.WebCert, bool)    { return doctor.WebCert{}, false }
