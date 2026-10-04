package engine

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const (
	testInstall = "abc123"
	testNode    = "pve1"
	testCred    = "cred1"
	testToken   = "cf-api-token-0123456789-do-not-leak"
	testAccount = "acc1"
	testZone    = "zone1"
	testMAC     = "bc:24:11:00:00:01"
)

var (
	testWriter = planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "n1"}
	tunnelName = planner.TunnelName(testInstall)
	marker     = planner.DNSMarker(testInstall)
	nodeAddr   = netip.MustParseAddr("10.0.0.2")
	guestAddr  = netip.MustParseAddr("10.0.0.11")
)

// clock is a time that only moves when a test moves it.
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

// fakeInventory returns the snapshot a test sets, or the ones it queued first.
type fakeInventory struct {
	mu          sync.Mutex
	snap        inventory.Snapshot
	queue       []inventory.Snapshot
	calls       int
	deadline    time.Time // of the last call
	hasDeadline bool
	onRefresh   func()
}

func (f *fakeInventory) Refresh(ctx context.Context) inventory.Snapshot {
	f.mu.Lock()
	hook := f.onRefresh
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.deadline, f.hasDeadline = ctx.Deadline()
	if len(f.queue) > 0 {
		s := f.queue[0]
		f.queue = f.queue[1:]
		return s
	}
	return f.snap
}

func (f *fakeInventory) hook(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRefresh = fn
}

func (f *fakeInventory) set(s inventory.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = s
}

func (f *fakeInventory) enqueue(s ...inventory.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, s...)
}

func (f *fakeInventory) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeResolver verifies every route on guestAddr, or on the address the route
// names, unless a test made its hostname unreachable. A guest's route is
// proven at port and bound, a route without a guest is manual and not bound,
// unless a test set another level for its hostname. A hostname whose guest a
// test stopped is answered as resolve answers for a guest it cannot check:
// its binding withdrawn, or nothing when it has none.
type fakeResolver struct {
	mu          sync.Mutex
	now         func() time.Time
	unreachable map[string]string        // hostname -> reason
	rejected    map[string]string        // hostname -> reason
	stopped     map[string]string        // hostname -> reason
	levels      map[string]resolve.Level // hostname -> level
	required    []resolve.Level          // what every call was told the minimum is
	bridge      string                   // where every binding's MACs were placed, by MAC
	placed      map[string]string
	calls       int
	denied      []netip.Addr // the node addresses the last denylist refused
	deadlines   []time.Time
	onResolve   func()
}

func (f *fakeResolver) Resolve(ctx context.Context, route model.Route, _ inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist, required resolve.Level, _ *resolve.Shared) resolve.Result {
	f.mu.Lock()
	f.calls++
	f.required = append(f.required, required)
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
	f.denied = f.denied[:0]
	for _, a := range []string{"10.0.0.2", "10.0.0.3", "10.0.0.4"} {
		if _, d := deny.Check(netip.MustParseAddr(a)); d {
			f.denied = append(f.denied, netip.MustParseAddr(a))
		}
	}
	hook := f.onResolve
	f.mu.Unlock()
	if hook != nil {
		hook()
	}

	switch {
	case rejected:
		return resolve.Result{
			Target:     planner.ResolvedTarget{Rejected: true, Reason: rejection},
			Candidates: []resolve.CandidateResult{{Addr: guestAddr, Source: resolve.FromStatic, Reason: rejection}},
		}
	case stopped && prev == nil:
		return resolve.Result{Target: planner.ResolvedTarget{Reason: stop}}
	case stopped:
		b := *prev
		b.Withdrawn = true
		return resolve.Result{Target: planner.ResolvedTarget{Addr: b.Addr, Withdrawn: true, Reason: stop, Owner: route.Owner()}, Binding: &b}
	}
	addr := guestAddr
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
			Addr: addr, MAC: testMAC, VerifiedAt: f.now(), Level: level,
		}
		if f.placed != nil {
			b := res.Binding
			b.Bridge, b.Port, b.Ports = f.bridge, f.placed[testMAC], maps.Clone(f.placed)
		}
	}
	return res
}

// minimums returns the minimum every call was told, in order.
func (f *fakeResolver) minimums() []resolve.Level {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.required)
}

// place makes every binding say that its MACs were found on these ports of
// bridge.
func (f *fakeResolver) place(bridge string, ports map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bridge, f.placed = bridge, ports
}

// stop makes the guest of host one that cannot be checked, for reason.
func (f *fakeResolver) stop(host, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped[host] = reason
}

// start undoes stop.
func (f *fakeResolver) start(host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stopped, host)
}

// setLevel makes the routes of host proven at level.
func (f *fakeResolver) setLevel(host string, level resolve.Level) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.levels[host] = level
}

func (f *fakeResolver) hook(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onResolve = fn
}

func (f *fakeResolver) reject(host, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected[host] = reason
}

func (f *fakeResolver) setUnreachable(host, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreachable[host] = reason
}

func (f *fakeResolver) deniedNodes() []netip.Addr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.denied)
}

type ensureCall struct{ id, token string }

// fakeConnectors records what the engine asks of the connector manager.
type fakeConnectors struct {
	mu        sync.Mutex
	tokens    map[string]string // what the manager has on disk
	ensured   []ensureCall
	pruned    [][]string
	onEnsure  func()
	ensureErr func(token string) error
	notReady  map[string]bool   // tunnels whose connector is not ready
	refused   map[string]bool   // tunnels whose connector Cloudflare refuses: not ready, and its journal says so
	portHeld  map[string]bool   // tunnels whose metrics port another process holds: not ready, and its journal says so
	ids       map[string]string // the id /ready names, by tunnel; defaultConnectorID when not set
}

// defaultConnectorID is what the node's own connector of every tunnel calls
// itself, unless a test names another.
const defaultConnectorID = "c1"

func (f *fakeConnectors) Ensure(_ context.Context, install, id, token string) error {
	if install != testInstall {
		return fmt.Errorf("ensure for install %q", install)
	}
	f.mu.Lock()
	f.ensured = append(f.ensured, ensureCall{id, token})
	hook, fail := f.onEnsure, f.ensureErr
	if fail != nil {
		f.mu.Unlock()
		return fail(token)
	}
	f.tokens[id] = token
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (f *fakeConnectors) PruneInstall(_ context.Context, install string, keep []string) error {
	if install != testInstall {
		return fmt.Errorf("prune for install %q", install)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = append(f.pruned, slices.Clone(keep))
	return nil
}

// List names the connectors that were ensured: all of this install.
func (f *fakeConnectors) List(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.tokens)), nil
}

func (f *fakeConnectors) Status(_ context.Context, id string) (connector.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notReady[id] || f.refused[id] || f.portHeld[id] {
		return connector.Status{
			TunnelID: id, Active: true, TokenRefused: f.refused[id], MetricsPortHeld: f.portHeld[id],
			MetricsAddr: "127.0.0.1:20300", Install: testInstall,
		}, nil
	}
	cid, ok := f.ids[id]
	if !ok {
		cid = defaultConnectorID
	}
	return connector.Status{TunnelID: id, Active: true, Ready: true, Connections: 4, ConnectorID: cid, MetricsAddr: "127.0.0.1:20300", Install: testInstall}, nil
}

// setRefused makes Cloudflare refuse the token the connector of a tunnel runs
// with, or take it again.
func (f *fakeConnectors) setRefused(id string, refused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refused == nil {
		f.refused = map[string]bool{}
	}
	f.refused[id] = refused
}

// setPortHeld makes another process hold the metrics port of the connector of
// a tunnel.
func (f *fakeConnectors) setPortHeld(id string, held bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.portHeld == nil {
		f.portHeld = map[string]bool{}
	}
	f.portHeld[id] = held
}

// setConnectorID makes the node's connector of a tunnel name itself id.
func (f *fakeConnectors) setConnectorID(tunnel, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[string]string{}
	}
	f.ids[tunnel] = id
}

func (f *fakeConnectors) setReady(id string, ready bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notReady == nil {
		f.notReady = map[string]bool{}
	}
	f.notReady[id] = !ready
}

// lastPrune returns the keep list of the last prune, and false when there was
// none.
func (f *fakeConnectors) lastPrune() ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pruned) == 0 {
		return nil, false
	}
	return slices.Clone(f.pruned[len(f.pruned)-1]), true
}

func (f *fakeConnectors) Token(id string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tokens[id]
	return t, ok, nil
}

func (f *fakeConnectors) ensures() []ensureCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ensured)
}

func (f *fakeConnectors) prunes() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.pruned)
}

// fakeEgress records what the engine gives the egress filter. With a log, it
// writes every call there too, in one order with the other calls a test
// records in it.
type fakeEgress struct {
	mu        sync.Mutex
	sets      [][]egress.Target
	removed   []netip.Addr
	setErr    error
	removeErr error
	log       *callLog
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
	return f.removeErr
}

// last returns the targets of the last Set, and false when there was none.
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

// callLog is one list of calls made to several fakes, in the order they came.
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

// index returns where the first call that begins with prefix is, at or after
// from, and -1 when there is none.
func (l *callLog) index(prefix string, from int) int {
	calls := l.all()
	for i := from; i < len(calls); i++ {
		if strings.HasPrefix(calls[i], prefix) {
			return i
		}
	}
	return -1
}

// hookedAPI calls before ahead of every call to the API it wraps.
type hookedAPI struct {
	cfapi.API
	before func(method string)
}

func (h hookedAPI) FindTunnel(ctx context.Context, account, name string) (cfapi.Tunnel, bool, error) {
	h.before("FindTunnel")
	return h.API.FindTunnel(ctx, account, name)
}

func (h hookedAPI) CreateTunnel(ctx context.Context, account, name string) (cfapi.Tunnel, error) {
	h.before("CreateTunnel")
	return h.API.CreateTunnel(ctx, account, name)
}

func (h hookedAPI) PutTunnelConfig(ctx context.Context, account, id string, rules []planner.IngressRule) (int, error) {
	h.before("PutTunnelConfig")
	return h.API.PutTunnelConfig(ctx, account, id, rules)
}

func (h hookedAPI) Records(ctx context.Context, zoneID string, f cfapi.RecordFilter) ([]cfapi.Record, error) {
	h.before("Records")
	return h.API.Records(ctx, zoneID, f)
}

// zoneView changes what Zones lists of the API it wraps: zones it hides are
// left out, and a zone can be given another status.
type zoneView struct {
	cfapi.API
	mu       *sync.Mutex
	hidden   map[string]bool   // by zone id
	status   map[string]string // by zone id
	accounts map[string]bool   // account ids Accounts leaves out
}

func newZoneView(api cfapi.API) zoneView {
	return zoneView{API: api, mu: &sync.Mutex{}, hidden: map[string]bool{}, status: map[string]string{}, accounts: map[string]bool{}}
}

func (z zoneView) Accounts(ctx context.Context) ([]cfapi.Account, error) {
	all, err := z.API.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	return slices.DeleteFunc(all, func(a cfapi.Account) bool { return z.accounts[a.ID] }), nil
}

func (z zoneView) hideAccount(id string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.accounts[id] = true
}

func (z zoneView) Zones(ctx context.Context) ([]cfapi.Zone, error) {
	zones, err := z.API.Zones(ctx)
	if err != nil {
		return nil, err
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	var out []cfapi.Zone
	for _, zone := range zones {
		if z.hidden[zone.ID] {
			continue
		}
		if st, ok := z.status[zone.ID]; ok {
			zone.Status = st
		}
		out = append(out, zone)
	}
	return out, nil
}

func (z zoneView) hide(id string, hidden bool) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.hidden[id] = hidden
}

func (z zoneView) setStatus(id, status string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.status[id] = status
}

// timeouts records the deadlines the engine asks for, by length.
type timeouts struct {
	mu   sync.Mutex
	seen []time.Duration
}

func (r *timeouts) withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	r.mu.Lock()
	r.seen = append(r.seen, d)
	r.mu.Unlock()
	return context.WithTimeout(ctx, d)
}

func (r *timeouts) count(d time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.seen {
		if x == d {
			n++
		}
	}
	return n
}

// env is an engine over a real store in a temporary directory, the fake
// Cloudflare and fakes for everything else.
type env struct {
	t     *testing.T
	clock *clock
	paths store.Paths
	store *store.Store
	inv   *fakeInventory
	res   *fakeResolver
	conn  *fakeConnectors
	egr   *fakeEgress
	cf    *cffake.Fake

	mu   sync.Mutex
	apis map[string]cfapi.API // by token: what NewClient hands out; the fake by default
	made []string             // credential ids NewClient was called for

	eng *Engine
	// startOnly is what the engine is told of the settings read only at the
	// start of the daemon.
	startOnly func(store.Settings) []string
	// problems are the lines the engine is told every cycle reports.
	problems []string
	log      zerolog.Logger
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith is newEnv with a chance to change the paths before the store is
// opened.
func newEnvWith(t *testing.T, paths func(base string, p *store.Paths)) *env {
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
		inv:  &fakeInventory{},
		conn: &fakeConnectors{tokens: map[string]string{}},
		egr:  &fakeEgress{},
		cf:   cffake.New(),
		apis: map[string]cfapi.API{},
		log:  zerolog.Nop(),
	}
	e.res = &fakeResolver{
		now: e.clock.now, unreachable: map[string]string{}, rejected: map[string]string{},
		stopped: map[string]string{}, levels: map[string]resolve.Level{},
	}
	if paths != nil {
		paths(base, &e.paths)
	}
	s, err := store.Open(e.paths)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	e.store = s
	require.NoError(t, s.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0}))
	require.NoError(t, s.SaveNode(store.NodeEntry{Name: testNode, Since: t0}))
	require.NoError(t, s.SaveWriter(testWriter))
	require.NoError(t, s.SaveCredential(store.Credential{ID: testCred, Label: "main", Kind: "scoped", Token: store.NewSecret(testToken), AddedAt: t0}))

	e.cf.SetNow(e.clock.now)
	e.cf.AddAccount(testAccount, "Main")
	e.cf.AddZone(testZone, "example.com", testAccount)

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.eng = e.newEngine()
	return e
}

func (e *env) newEngine() *Engine { return e.newEngineWith(e.conn) }

func (e *env) newEngineWith(conns Connectors) *Engine {
	e.t.Helper()
	eng, err := New(Deps{
		Store:      e.store,
		Inventory:  e.inv,
		Resolver:   e.res,
		Connectors: conns,
		Egress:     e.egr,
		StartOnly:  e.startOnly,
		Problems:   e.problems,
		NewClient:  e.newClient,
		Node:       testNode,
		Now:        e.clock.now,
		Log:        e.log,
		LocalDir:   e.paths.Local,
	})
	require.NoError(e.t, err)
	return eng
}

func (e *env) newClient(c store.Credential) (cfapi.API, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.made = append(e.made, c.ID)
	if api, ok := e.apis[c.Token.Reveal()]; ok {
		return api, nil
	}
	return e.cf, nil
}

// useAPI makes NewClient hand out api for a token.
func (e *env) useAPI(token string, api cfapi.API) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.apis[token] = api
}

func (e *env) cycle() State {
	e.t.Helper()
	return e.eng.Cycle(e.t.Context())
}

// apply asks for what pco apply does: with confirm, a confirmation of what
// the last state showed.
func (e *env) apply(confirm bool) ApplyResult {
	e.t.Helper()
	offer := ""
	if confirm {
		offer = e.eng.State().Offer
	}
	res, err := e.eng.Apply(e.t.Context(), confirm, offer)
	require.NoError(e.t, err)
	return res
}

// enforce leaves observe-only mode without a cycle.
func (e *env) enforce() {
	e.t.Helper()
	s, err := e.store.Settings()
	require.NoError(e.t, err)
	s.ObserveOnly = false
	require.NoError(e.t, e.store.SaveSettings(s))
}

func (e *env) settings(change func(*store.Settings)) {
	e.t.Helper()
	s, err := e.store.Settings()
	require.NoError(e.t, err)
	change(&s)
	require.NoError(e.t, e.store.SaveSettings(s))
}

// writes returns the calls made to the fake that change something.
func (e *env) writes() []string {
	var out []string
	for _, c := range e.cf.Calls() {
		switch strings.Fields(c)[0] {
		case "CreateTunnel", "DeleteTunnel", "PutTunnelConfig", "CreateRecord", "UpdateRecord", "DeleteRecord":
			out = append(out, c)
		}
	}
	return out
}

func (e *env) tunnels() []cfapi.Tunnel { return e.cf.TunnelsIn(testAccount) }

// rules returns the ingress of the one tunnel of the account.
func (e *env) rules() []planner.IngressRule {
	e.t.Helper()
	ts := e.tunnels()
	require.Len(e.t, ts, 1)
	cfg, err := e.cf.TunnelConfig(e.t.Context(), testAccount, ts[0].ID)
	require.NoError(e.t, err)
	return cfg.Ingress
}

func (e *env) records() []cfapi.Record { return e.cf.RecordsIn(testZone) }

func (e *env) recordNames() []string {
	var out []string
	for _, r := range e.records() {
		out = append(out, r.Name)
	}
	slices.Sort(out)
	return out
}

// files returns every file below the store roots with its content, leaving
// out the event log, which is not state.
func (e *env) files() map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, root := range []string{e.paths.Cluster, e.paths.Private, e.paths.Local} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), eventsFile) {
				return err
			}
			b, err := os.ReadFile(path)
			out[path] = string(b)
			return err
		})
		require.NoError(e.t, err)
	}
	return out
}

func guest(vmid int, name string, routes ...string) model.Guest {
	return model.Guest{
		Ref:         model.GuestRef{Kind: model.KindQEMU, VMID: vmid},
		Name:        name,
		Node:        testNode,
		Running:     true,
		Tags:        []string{"cf-tunnel"},
		Description: "```cf-tunnel\n" + strings.Join(routes, "\n") + "\n```",
		Identity:    fmt.Sprintf("uuid:%d", vmid),
	}
}

// many returns n tagged guests, from qemu/101 on, each publishing
// g<vmid>.example.com.
func many(n int) []model.Guest {
	out := make([]model.Guest, n)
	for i := range out {
		vmid := 101 + i
		out[i] = guest(vmid, fmt.Sprintf("vm-%d", vmid), fmt.Sprintf("g%d.example.com -> :8080", vmid))
	}
	return out
}

// untagged is a guest that lost the gate tag: Proxmox still lists it.
func untagged(g model.Guest) model.Guest {
	g.Tags = nil
	return g
}

func snapshot(guests ...model.Guest) inventory.Snapshot {
	guests = slices.Clone(guests)
	slices.SortFunc(guests, func(a, b model.Guest) int { return a.Ref.VMID - b.Ref.VMID })
	return inventory.Snapshot{
		Guests:   guests,
		Nodes:    []inventory.Node{{Name: testNode, Addr: nodeAddr, Online: true, Local: true}},
		Complete: true,
	}
}

func incomplete(problem string, guests ...model.Guest) inventory.Snapshot {
	s := snapshot(guests...)
	s.Complete = false
	s.Problems = []string{problem}
	return s
}

func route(st State, host string) RouteView {
	for _, r := range st.Routes {
		if r.Hostname == host {
			return r
		}
	}
	return RouteView{}
}

// hostRule is the rule that serves host on the guest's port 8080.
func hostRule(host string) planner.IngressRule {
	return planner.IngressRule{Hostname: host, Service: "http://10.0.0.11:8080"}
}

func withSentinel(rules ...planner.IngressRule) []planner.IngressRule {
	return append(rules,
		planner.IngressRule{Hostname: planner.SentinelHostname(testWriter), Service: "http_status:404"},
		planner.IngressRule{Service: "http_status:404"},
	)
}

func actionKinds(st State) []string {
	var out []string
	for _, a := range st.Actions {
		s := string(a.Kind) + " " + a.Target
		if !a.Applied {
			s += " held: " + a.Held
		}
		out = append(out, s)
	}
	return out
}

// addSecondCredential stores credential cred2, whose token NewClient answers
// with api, or with the fake when api is nil.
func (e *env) addSecondCredential(token string, api cfapi.API) {
	e.t.Helper()
	if api != nil {
		e.useAPI(token, api)
	}
	require.NoError(e.t, e.store.SaveCredential(store.Credential{ID: "cred2", Label: "label-cred2", Kind: "scoped", Token: store.NewSecret(token), AddedAt: t0}))
}

// callsSince returns the calls made to the fake after the first n.
func (e *env) callsSince(n int) []string { return e.cf.Calls()[n:] }

// unnumbered returns events without their sequence numbers, for comparing
// them with expected ones.
func unnumbered(events []Event) []Event {
	out := slices.Clone(events)
	for i := range out {
		out[i].Seq = 0
	}
	return out
}

func hasProblem(st State, part string) bool {
	return slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, part) })
}
