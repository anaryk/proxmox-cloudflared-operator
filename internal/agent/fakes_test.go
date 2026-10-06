package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var t0 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

const (
	testInstall = "abc123"
	testNode    = "pve1"
	testMAC     = "bc:24:11:00:00:01"
	sharedID    = "11111111-1111-4111-8111-111111111111"
	pinnedID    = "22222222-2222-4222-8222-222222222222"
	otherID     = "33333333-3333-4333-8333-333333333333"
	sharedToken = "token-of-the-shared-tunnel"
	pinnedToken = "token-of-the-pinned-tunnel"
)

var (
	guestAddr = netip.MustParseAddr("10.0.0.11")
	nodeAddr  = netip.MustParseAddr("10.0.0.2")
	errBoom   = errors.New("boom")
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeResolver answers every route at the address of its guest, guestAddr
// unless a test moved it, or at the address the route names. A guest's route
// is proven at port and bound; a route without a guest is manual and not
// bound. A test can stop a hostname's guest (its binding is withdrawn, or it
// gets nothing without one), reject it, make it unreachable or set its level.
type fakeResolver struct {
	mu          sync.Mutex
	now         func() time.Time
	unreachable map[string]string
	rejected    map[string]string
	stopped     map[string]string
	levels      map[string]resolve.Level
	addrs       map[string]netip.Addr
	vias        map[string]string
	hosts       []string        // the hostname of every call, in order
	required    []resolve.Level // the minimum every call was told
	reused      []bool          // whether the share let the binding's proof stand, per call
	snaps       []inventory.Snapshot
	denies      []resolve.Denylist
	deadlines   []time.Time
	onResolve   func(host string)
}

func newFakeResolver(now func() time.Time) *fakeResolver {
	return &fakeResolver{
		now: now, unreachable: map[string]string{}, rejected: map[string]string{}, stopped: map[string]string{},
		levels: map[string]resolve.Level{}, addrs: map[string]netip.Addr{}, vias: map[string]string{},
	}
}

func (f *fakeResolver) Resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist, required resolve.Level, share *resolve.Shared) resolve.Result {
	f.mu.Lock()
	f.hosts = append(f.hosts, route.Hostname)
	f.required = append(f.required, required)
	f.reused = append(f.reused, prev != nil && share != nil && share.Reusable(*prev))
	f.snaps = append(f.snaps, snap)
	f.denies = append(f.denies, deny)
	if d, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, d)
	}
	reason, bad := f.unreachable[route.Hostname]
	rejection, rejected := f.rejected[route.Hostname]
	stop, stopped := f.stopped[route.Hostname]
	level, set := f.levels[route.Hostname]
	switch {
	case set:
	case route.Guest == nil:
		level = resolve.LevelManual
	default:
		level = resolve.LevelPort
	}
	addr, set := f.addrs[route.Hostname]
	if !set {
		addr = guestAddr
	}
	via := f.vias[route.Hostname]
	hook := f.onResolve
	f.mu.Unlock()
	if hook != nil {
		hook(route.Hostname)
	}

	switch {
	case rejected:
		return resolve.Result{
			Target:     planner.ResolvedTarget{Rejected: true, Reason: rejection},
			Candidates: []resolve.CandidateResult{{Addr: addr, Source: resolve.FromStatic, Reason: rejection}},
		}
	case stopped && prev == nil:
		return resolve.Result{Target: planner.ResolvedTarget{Reason: stop}}
	case stopped:
		b := *prev
		b.Withdrawn = true
		return resolve.Result{Target: planner.ResolvedTarget{Addr: b.Addr, Withdrawn: true, Reason: stop, Owner: route.Owner()}, Binding: &b}
	}
	if route.Target.Addr.IsValid() {
		addr = route.Target.Addr
	}
	res := resolve.Result{
		Target:     planner.ResolvedTarget{Addr: addr, Reachable: !bad, Reason: reason, Owner: route.Owner()},
		Candidates: []resolve.CandidateResult{{Addr: addr, Source: resolve.FromStatic, OK: !bad, Reason: reason}},
		Level:      level,
	}
	if route.Guest != nil {
		res.Binding = &resolve.Binding{
			Owner: route.Owner(), Hostname: route.Hostname, Guest: route.Guest.String(),
			Addr: addr, MAC: testMAC, VerifiedAt: f.now(), Level: level, Via: via,
			Bridge: "vmbr0", Port: "tap101i0",
		}
		if prev != nil && prev.Addr == addr && prev.Via == via {
			res.Binding.Since = prev.Since
		}
		res.SoftDenied, _ = deny.Soft(addr)
	}
	return res
}

func (f *fakeResolver) set(m map[string]string, host, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m[host] = v
}

// stop makes the guest of host one that cannot be checked, for reason.
func (f *fakeResolver) stop(host, reason string) { f.set(f.stopped, host, reason) }

// reject makes the address of host one that must never be served.
func (f *fakeResolver) reject(host string) { f.set(f.rejected, host, "address of a cluster node") }

// silence makes the port of host answer no more.
func (f *fakeResolver) silence(host string) {
	f.set(f.unreachable, host, "dial tcp 10.0.0.11:8080: connection refused")
}

// start undoes every stop.
func (f *fakeResolver) start() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.stopped)
}

// observe makes the address of host proven at observed only.
func (f *fakeResolver) observe(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.levels[host] = resolve.LevelObserved
}

func (f *fakeResolver) moveTo(host string, addr netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs[host] = addr
}

func (f *fakeResolver) onLeg(host, network string) { f.set(f.vias, host, network) }

func (f *fakeResolver) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hosts)
}

func (f *fakeResolver) lastSnap() inventory.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snaps[len(f.snaps)-1]
}

func (f *fakeResolver) lastDeny() resolve.Denylist {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.denies[len(f.denies)-1]
}

func (f *fakeResolver) reuses() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reused)
}

// fakeConnectors records what the agent asks of the connector manager. Its
// connectors run once ensured, until stopped.
type fakeConnectors struct {
	mu       sync.Mutex
	tokens   map[string]string // what the manager has on disk
	running  map[string]bool
	installs map[string]string // the install of each connector on the node
	calls    []string
	ready    map[string]bool // false makes a running connector not ready
	fail     map[string]error
}

func newFakeConnectors() *fakeConnectors {
	return &fakeConnectors{tokens: map[string]string{}, running: map[string]bool{}, installs: map[string]string{}, ready: map[string]bool{}, fail: map[string]error{}}
}

func (f *fakeConnectors) record(call string) error {
	f.calls = append(f.calls, call)
	return f.fail[strings.Fields(call)[0]]
}

func (f *fakeConnectors) Ensure(_ context.Context, install, id, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Ensure " + id + " " + token); err != nil {
		return fmt.Errorf("starting %s with %s: %w", id, token, err)
	}
	if install != testInstall {
		return fmt.Errorf("ensure for install %q", install)
	}
	f.tokens[id], f.running[id], f.installs[id] = token, true, install
	return nil
}

func (f *fakeConnectors) PruneInstall(_ context.Context, install string, keep []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("PruneInstall " + strings.Join(keep, ",")); err != nil {
		return err
	}
	for id, of := range f.installs {
		if of == install && !slices.Contains(keep, id) {
			delete(f.tokens, id)
			delete(f.running, id)
			delete(f.installs, id)
		}
	}
	return nil
}

func (f *fakeConnectors) List(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.installs)), nil
}

func (f *fakeConnectors) Status(_ context.Context, id string) (connector.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := connector.Status{TunnelID: id, Active: f.running[id], Install: f.installs[id]}
	if st.Active && f.ready[id] {
		st.Ready, st.Connections = true, 4
	}
	return st, nil
}

func (f *fakeConnectors) Token(id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	return t, ok, nil
}

func (f *fakeConnectors) StopAll(_ context.Context, install string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("StopAll"); err != nil {
		return err
	}
	for id, of := range f.installs {
		if of == install {
			f.running[id] = false
		}
	}
	return nil
}

func (f *fakeConnectors) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("Stop " + id); err != nil {
		return err
	}
	f.running[id] = false
	return nil
}

func (f *fakeConnectors) Disable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.record("Disable " + id)
}

// changes are the calls that act on a connector.
func (f *fakeConnectors) changes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeConnectors) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// put makes a connector the node has of install, running, with token on disk.
func (f *fakeConnectors) put(id, install, token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens[id], f.running[id], f.installs[id] = token, true, install
}

func (f *fakeConnectors) setReady(id string, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ready[id] = ready
}

func (f *fakeConnectors) isRunning(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[id]
}

// fakeEgress records what the agent gives the egress filter, and writes every
// call to the log too, when it has one.
type fakeEgress struct {
	mu      sync.Mutex
	sets    [][]egress.Target
	removed []netip.Addr
	setErr  error
	log     *callLog
}

func (f *fakeEgress) Set(_ context.Context, targets []egress.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, slices.Clone(targets))
	f.log.add("egress set " + targetsText(targets))
	return f.setErr
}

func (f *fakeEgress) Remove(_ context.Context, addr netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, addr)
	f.log.add("egress remove " + addr.String())
	return nil
}

func (f *fakeEgress) FlowCounts(context.Context) (string, map[netip.AddrPort]uint64, error) {
	return "g1", map[netip.AddrPort]uint64{netip.MustParseAddrPort("10.0.0.11:8080"): 7}, nil
}

func (f *fakeEgress) last() ([]egress.Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sets) == 0 {
		return nil, false
	}
	return slices.Clone(f.sets[len(f.sets)-1]), true
}

func (f *fakeEgress) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sets)
}

func (f *fakeEgress) removes() []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

func (f *fakeEgress) failSet(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setErr = err
}

func targetsText(ts []egress.Target) string {
	if len(ts) == 0 {
		return "none"
	}
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = t.String()
	}
	return strings.Join(parts, " ")
}

// callLog is one list of the calls of several fakes, in the order they came.
// A nil log records nothing.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// fakeGateway records what the agent gives the gateway.
type fakeGateway struct {
	mu    sync.Mutex
	syncs []gatewaySync
	err   error
}

type gatewaySync struct {
	legs     []gateway.Leg
	services []gateway.Service
	pins     []gateway.Pin
	sticky   []gateway.Sticky
}

func (f *fakeGateway) Sync(_ context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, gatewaySync{legs, services, pins, sticky})
	states := make([]gateway.LegState, len(legs))
	for i, l := range legs {
		states[i] = gateway.LegState{ID: l.ID, Up: true, Iface: l.Iface()}
	}
	return states, f.err
}

func (f *fakeGateway) last() (gatewaySync, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.syncs) == 0 {
		return gatewaySync{}, false
	}
	return f.syncs[len(f.syncs)-1], true
}

// env is an agent over a real store in a temporary directory and fakes for
// everything else.
type env struct {
	t       *testing.T
	clock   *clock
	paths   store.Paths
	store   *store.Store
	res     *fakeResolver
	conn    *fakeConnectors
	egr     *fakeEgress
	gw      *fakeGateway
	blocked []netip.Addr
	runs    *runner
	events  *events
	kind    pki.Kind
	changed int

	agent *Agent
	seq   uint64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{
		t:     t,
		clock: &clock{t: t0},
		paths: store.Paths{
			Cluster: filepath.Join(base, "cluster"),
			Private: filepath.Join(base, "private"),
			Local:   filepath.Join(base, "local"),
		},
		conn:   newFakeConnectors(),
		egr:    &fakeEgress{},
		runs:   &runner{},
		events: &events{},
		kind:   pki.KindHost,
	}
	e.res = newFakeResolver(e.clock.now)
	s, err := store.Open(e.paths)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	e.store = s
	e.agent = e.newAgent()
	return e
}

func (e *env) deps() Deps {
	d := Deps{
		Node:       testNode,
		Kind:       e.kind,
		InstallID:  testInstall,
		Store:      e.store,
		Resolver:   e.res,
		Connectors: e.conn,
		Egress:     e.egr,
		Blocked:    func() ([]netip.Addr, error) { return slices.Clone(e.blocked), nil },
		Runner:     e.runs.run,
		Now:        e.clock.now,
		Log:        zerolog.Nop(),
		Changed:    func() { e.changed++ },
		Event:      e.events.add,
	}
	if e.gw != nil {
		d.Gateway = e.gw
	}
	return d
}

// newAgent makes the agent anew over the same store and fakes, as a restart
// of the daemon does.
func (e *env) newAgent() *Agent {
	e.t.Helper()
	a, err := New(e.deps())
	require.NoError(e.t, err)
	a.after = func(time.Duration) (<-chan time.Time, func() bool) {
		ch := make(chan time.Time, 1)
		ch <- e.clock.now()
		return ch, func() bool { return true }
	}
	return a
}

// apply hands the agent s as the controller would, and fails the test when
// the agent refuses it.
func (e *env) apply(s agentproto.Snapshot) agentproto.Status {
	e.t.Helper()
	st, err := e.agent.Apply(e.t.Context(), s)
	require.NoError(e.t, err)
	return st
}

var epoch1 = agentproto.Epoch{InstallID: testInstall, Generation: 1, Nonce: "n1"}

// snapshot is a desired state of the next seq, with a shared tunnel whose
// configuration verified at Cloudflare names nothing yet, and a target for
// each route.
func (e *env) snapshot(routes ...string) agentproto.Snapshot {
	e.seq++
	s := agentproto.Snapshot{
		Protocol: agentproto.ProtocolMax,
		Epoch:    epoch1,
		Seq:      e.seq,
		Node:     testNode,
		State:    agentproto.Active,
		Settings: agentproto.Settings{PollInterval: 10 * time.Second, IdentityMinimum: "port", ReverifyInterval: time.Hour},
		Nodes:    []agentproto.Node{{Name: testNode, Addr: nodeAddr, Online: true, Local: true, Addrs: []netip.Addr{nodeAddr}}},
		Deny:     agentproto.DenyList{Denied: []netip.Addr{nodeAddr}},
		Tunnels: []agentproto.Tunnel{{
			AccountID: "acc1", TunnelID: sharedID, Name: "pco-" + testInstall, Kind: agentproto.TunnelShared,
			Run: true, Token: agentproto.NewSecret(sharedToken), Verified: []netip.AddrPort{},
		}},
	}
	for _, r := range routes {
		s.Targets = append(s.Targets, guestTarget(r))
	}
	return s
}

// guestTarget is the target of the route "<hostname> -> :<port>" of guest
// qemu/101 on this node, which this node resolves.
func guestTarget(route string) agentproto.Target {
	host, port, ok := strings.Cut(route, " -> :")
	if !ok {
		panic("route " + route)
	}
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	var p uint16
	_, _ = fmt.Sscan(port, &p)
	return agentproto.Target{
		Key:     host,
		Tunnels: []string{sharedID},
		Route: model.Route{
			Hostname: host, Source: model.SourceAnnotation, Guest: &ref,
			Target: model.Target{Scheme: model.SchemeHTTP, Port: p},
		},
		Guest: &model.Guest{
			Ref: ref, Name: "web-1", Node: testNode, Running: true, Identity: "id101", Digest: "d101",
			NICs: []model.NIC{{Index: 0, MAC: testMAC, Bridge: "vmbr0"}},
		},
		Port:     p,
		Required: "port",
		Resolve:  true,
	}
}

// manualTarget is a manual route to addr:port, with allowNode when asked.
func manualTarget(host string, ap netip.AddrPort, allowNode bool) agentproto.Target {
	return agentproto.Target{
		Key:     host,
		Tunnels: []string{sharedID},
		Route: model.Route{
			Hostname: host, Source: model.SourceManual, ManualID: host,
			Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: ap.Addr(), Port: ap.Port()},
			Options: model.RouteOptions{AllowNode: allowNode},
		},
		Port:      ap.Port(),
		Required:  "port",
		Resolve:   true,
		AllowNode: allowNode,
	}
}

// verified says that the shared tunnel's configuration verified at
// Cloudflare sends a connector to each of aps.
func verified(s agentproto.Snapshot, aps ...string) agentproto.Snapshot {
	out := []netip.AddrPort{}
	for _, ap := range aps {
		out = append(out, netip.MustParseAddrPort(ap))
	}
	s.Tunnels[0].Verified = out
	return s
}

func mustMAC(s string) net.HardwareAddr {
	mac, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return mac
}

func egressTargets(ss ...string) []egress.Target {
	out := []egress.Target{}
	for _, s := range ss {
		ap := netip.MustParseAddrPort(s)
		out = append(out, egress.Target{Addr: ap.Addr(), Port: ap.Port()})
	}
	return out
}

// runner records the argument vectors the agent runs and answers them.
type runner struct {
	mu    sync.Mutex
	argvs [][]string
	err   error
	ctxs  []context.Context
}

func (r *runner) run(ctx context.Context, argv ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.argvs = append(r.argvs, slices.Clone(argv))
	r.ctxs = append(r.ctxs, ctx)
	return "", r.err
}

func (r *runner) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.argvs)
}

// events records what the agent says.
type events struct {
	mu   sync.Mutex
	list []Event
}

func (l *events) add(ev Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, ev)
}

func (l *events) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.list))
	for i, ev := range l.list {
		out[i] = ev.Message
	}
	return out
}

func report(st agentproto.Status, key string) agentproto.TargetReport {
	for _, r := range st.Targets {
		if r.Key == key {
			return r
		}
	}
	return agentproto.TargetReport{}
}

// encoded is s as the controller sends it, tokens included.
func encoded(t *testing.T, s agentproto.Snapshot) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, agentproto.EncodeSnapshot(&buf, s))
	return buf.String()
}
