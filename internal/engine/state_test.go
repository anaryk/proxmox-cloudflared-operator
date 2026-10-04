package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestTokensNeverReachTheStateEventsOrLog(t *testing.T) {
	e := newEnv(t)
	var logged bytes.Buffer
	eng, err := New(Deps{
		Store: e.store, Inventory: e.inv, Resolver: e.res, Connectors: e.conn, Egress: e.egr, NewClient: e.newClient,
		Node: testNode, Now: e.clock.now, Log: zerolog.New(&logged).Level(zerolog.DebugLevel), LocalDir: e.paths.Local,
	})
	require.NoError(t, err)
	e.eng = eng
	e.enforce()
	// A connector that fails says why, and an error may well repeat its input.
	e.conn.ensureErr = func(token string) error { return fmt.Errorf("unit refused %s", token) }
	_, err = e.eng.CheckCredential(t.Context(), testCred, false)
	require.NoError(t, err)

	st := e.cycle()

	tunnelToken := cffake.RunToken(testAccount, e.tunnels()[0].ID)
	require.Contains(t, st.Problems, "tunnel pco-abc123 in account acc1: starting its connector: unit refused [redacted]")
	state, err := json.Marshal(st)
	require.NoError(t, err)
	events, err := json.Marshal(e.eng.Events(time.Time{}))
	require.NoError(t, err)
	file, err := os.ReadFile(filepath.Join(e.paths.Local, eventsFile))
	require.NoError(t, err)
	for name, out := range map[string]string{
		"state": string(state), "events": string(events), "event log": string(file), "log": logged.String(),
	} {
		require.NotContains(t, out, testToken, name)
		require.NotContains(t, out, tunnelToken, name)
	}
	require.Contains(t, string(state), `"credentials":[{"id":"cred1","label":"main","kind":"scoped"`)
}

func TestTwoCyclesOverTheSameWorldGiveEqualStates(t *testing.T) {
	for _, observe := range []bool{true, false} {
		t.Run(modeName(observe), func(t *testing.T) {
			e := newEnv(t)
			if !observe {
				e.enforce()
			}
			e.inv.set(snapshot(
				guest(103, "app", "b.example.com a.example.com -> :3000", "*.example.com -> :80"),
				guest(101, "web-1", "www.example.com -> :8080", "broken"),
				guest(102, "web-2", "www.example.com -> :8080"),
			))
			require.NoError(t, e.store.SaveManualRoute(model.Route{
				Hostname: "nas.example.com", ManualID: "nas", Source: model.SourceManual,
				Target: model.Target{Scheme: model.SchemeHTTP, Addr: netip.MustParseAddr("10.0.0.50"), Port: 5000},
			}))
			e.res.setUnreachable("a.example.com", "connection refused")
			e.cycle()

			first := e.cycle()
			second := e.cycle()

			require.Equal(t, first, second)
			a, err := json.Marshal(first)
			require.NoError(t, err)
			b, err := json.Marshal(second)
			require.NoError(t, err)
			require.JSONEq(t, string(a), string(b))
			require.Len(t, first.Routes, 6)
			require.Equal(t, "*.example.com", first.Routes[0].Hostname)
			require.Equal(t, "www.example.com", first.Routes[5].Hostname)
			require.Equal(t, "qemu/102", first.Routes[5].Owner)
		})
	}
}

func TestNodeAddressesAreRememberedAndNeverShrink(t *testing.T) {
	e := newEnv(t)
	two := snapshot(guest(101, "web-1", "www.example.com -> :8080"))
	two.Nodes = append(two.Nodes, inventory.Node{Name: "pve2", Addr: netip.MustParseAddr("10.0.0.3"), Online: true})
	e.inv.set(two)
	e.cycle()

	addrs := []netip.Addr{netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")}
	require.Equal(t, addrs, e.res.deniedNodes())
	saved, err := e.store.NodeAddrs()
	require.NoError(t, err)
	require.Equal(t, addrs, saved)

	// pve2 drops out of the listing, then the daemon restarts.
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.cycle()
	require.Equal(t, addrs, e.res.deniedNodes())
	e.eng = e.newEngine()
	e.cycle()
	require.Equal(t, addrs, e.res.deniedNodes(), "a restart still denies what was learned before")
}

func TestNodeAddressesThatCannotBeReadHoldTheCycle(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.paths.Local, "meta", "node-addrs.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))

	st := e.cycle()

	require.Len(t, st.Problems, 1)
	require.Contains(t, st.Problems[0], "reading the saved node addresses")
	require.Empty(t, e.cf.Calls())
}

func TestRolloutIsConfirmedOnlyAgainstAVerifiedVersion(t *testing.T) {
	e := newEnv(t)
	readBack := newFailingReadBack(e.cf)
	e.useAPI(testToken, readBack)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	rollouts := func() []Event {
		var out []Event
		for _, ev := range unnumbered(e.eng.Events(time.Time{})) {
			if ev.Kind == "rollout" {
				out = append(out, ev)
			}
		}
		return out
	}
	require.Empty(t, rollouts(), "no connector reported yet")

	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: "c1", ConfigVersion: 1}})
	e.clock.advance(rolloutAskEvery)
	e.cycle()
	require.Equal(t, []Event{{
		At: t0.Add(rolloutAskEvery), Level: "info", Kind: "rollout", Subject: tunnelName,
		Message: "configuration version 1 runs on 1 connectors in account acc1", Tunnel: tunnelName, Account: testAccount,
	}}, rollouts())

	// A write whose read-back fails is not verified, although its version is
	// newer than the one confirmed: whatever the connectors run, nothing is
	// confirmed against it.
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: "c1", ConfigVersion: 9}})
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))
	readBack.arm()
	e.clock.advance(rolloutAskEvery)
	st := e.cycle()
	require.False(t, st.Tunnels[0].Verified)
	require.Equal(t, 2, st.Tunnels[0].Version)
	require.Len(t, rollouts(), 1)

	e.clock.advance(rolloutAskEvery)
	st = e.cycle()
	require.True(t, st.Tunnels[0].Verified)
	require.Equal(t, 2, st.Tunnels[0].Version)
	require.Len(t, rollouts(), 2)
	require.Equal(t, "configuration version 2 runs on 1 connectors in account acc1", rollouts()[1].Message)
}

func TestRolloutIsAskedAtMostEveryThirtySeconds(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	asked := func() int {
		n := 0
		for _, c := range e.cf.Calls() {
			if strings.HasPrefix(c, "Connectors ") {
				n++
			}
		}
		return n
	}
	require.Equal(t, 1, asked())

	e.clock.advance(10 * time.Second)
	e.cycle()
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, 1, asked(), "no connector reports the version yet; Cloudflare is not asked every cycle")

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, 2, asked())
}

func TestTheStateHandedOutIsACopy(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "*.example.com -> :8080"), guest(102, "web-2", "www.example.com -> :8080")))
	_, err := e.eng.CheckCredential(t.Context(), testCred, false)
	require.NoError(t, err)

	st := e.cycle()
	before, err := json.Marshal(e.eng.State())
	require.NoError(t, err)
	require.NotEmpty(t, st.Routes[0].Warnings)
	require.NotEmpty(t, st.Routes[0].Candidates)
	require.NotNil(t, st.Routes[0].Rule)
	require.NotEmpty(t, st.Credentials[0].Report.Checks)

	for _, got := range []State{st, e.eng.State()} {
		got.Routes[0].Warnings[0] = "changed"
		got.Routes[0].Candidates[0].Reason = "changed"
		got.Routes[0].Rule.Service = "changed"
		got.Credentials[0].Report.Checks[0].Detail = "changed"
		got.Credentials[0].Report.Zones[0].Name = "changed"
	}

	after, err := json.Marshal(e.eng.State())
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))
}

// failingReadBack fails the read of a configuration right after a write,
// once it is armed.
type failingReadBack struct {
	cfapi.API
	mu    *sync.Mutex
	armed *bool
	wrote *bool
}

func newFailingReadBack(api cfapi.API) failingReadBack {
	return failingReadBack{API: api, mu: &sync.Mutex{}, armed: new(bool), wrote: new(bool)}
}

func (f failingReadBack) arm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.armed = true
}

func (f failingReadBack) PutTunnelConfig(ctx context.Context, account, id string, rules []planner.IngressRule) (int, error) {
	v, err := f.API.PutTunnelConfig(ctx, account, id, rules)
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.wrote = *f.armed
	return v, err
}

func (f failingReadBack) TunnelConfig(ctx context.Context, account, id string) (cfapi.TunnelConfig, error) {
	f.mu.Lock()
	fail := *f.wrote
	if fail {
		*f.wrote, *f.armed = false, false
	}
	f.mu.Unlock()
	if fail {
		return cfapi.TunnelConfig{}, errors.New("read back failed")
	}
	return f.API.TunnelConfig(ctx, account, id)
}

func TestEventLogIsRotated(t *testing.T) {
	dir := t.TempDir()
	l := newEventLog(dir, zerolog.Nop())
	path := filepath.Join(dir, eventsFile)
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), maxEventsLog-10), 0o600))

	l.add(Event{At: t0, Level: "info", Kind: "admin", Message: "after the limit"})

	old, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	require.Len(t, old, maxEventsLog-10)
	now, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(now), "after the limit")
}

func TestEventsAreNumbered(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	e.clock.advance(time.Second)
	e.res.setUnreachable("www.example.com", "connection refused")
	e.cycle()

	events := e.eng.Events(time.Time{})
	require.Greater(t, len(events), 2)
	for i, ev := range events {
		require.Equal(t, uint64(i+1), ev.Seq, "a number that only grows, from 1")
	}
	logged := readEventLog(t, e)
	require.Equal(t, events, logged)
}

func TestEventsKeepTheLastThousand(t *testing.T) {
	l := newEventLog("", zerolog.Nop())
	for i := range maxEvents + 5 {
		l.add(Event{At: t0.Add(time.Duration(i) * time.Second), Message: fmt.Sprint(i)})
	}

	all := l.since(time.Time{})

	require.Len(t, all, maxEvents)
	require.Equal(t, "5", all[0].Message)
	require.Len(t, l.since(t0.Add(time.Duration(maxEvents+3)*time.Second)), 1)
}

// fakeTimer replaces the wait between the cycles of Run.
type fakeTimer struct {
	waits chan time.Duration
	fire  chan time.Time
}

func (f *fakeTimer) after(d time.Duration) (<-chan time.Time, func() bool) {
	f.waits <- d
	return f.fire, func() bool { return true }
}

func TestRunCyclesOnThePollIntervalAndOnTrigger(t *testing.T) {
	e := newEnv(t)
	timer := &fakeTimer{waits: make(chan time.Duration, 8), fire: make(chan time.Time)}
	e.eng.after = timer.after
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- e.eng.Run(ctx) }()

	require.Equal(t, 10*time.Second, <-timer.waits, "the default poll interval")
	require.Equal(t, 1, e.inv.refreshes())

	// The second cycle is held in the resolver while two triggers arrive.
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.res.hook(func() { once.Do(func() { close(held); <-release }) })
	e.settings(func(s *store.Settings) { s.PollInterval = store.Duration(30 * time.Second) })
	timer.fire <- t0
	<-held
	e.eng.Trigger()
	e.eng.Trigger()
	close(release)

	require.Equal(t, 30*time.Second, <-timer.waits, "the interval comes from the settings")
	require.Equal(t, 30*time.Second, <-timer.waits)
	require.Equal(t, 3, e.inv.refreshes(), "two triggers during a cycle are one cycle after it")

	cancel()
	require.NoError(t, <-done)
	require.Equal(t, 3, e.inv.refreshes())
	require.Empty(t, timer.waits)
}

func TestConcurrentApplyAndCycle(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))
	e.cf.SeedRecord(testZone, cfapi.Record{Type: "A", Name: "api.example.com", Content: "192.0.2.10", Comment: "by hand"})
	e.enforce()
	e.cycle()
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { e.eng.Cycle(t.Context()) })
		wg.Go(func() {
			_, err := e.eng.Apply(t.Context(), i%2 == 0, "")
			errs <- err
		})
		wg.Go(func() {
			// Once another adoption went through, the name is in nobody's way.
			if err := e.eng.Adopt(t.Context(), "api.example.com"); !errors.Is(err, ErrNotFound) {
				errs <- err
			}
		})
		wg.Go(func() {
			_ = e.eng.State()
			_ = e.eng.Events(time.Time{})
			e.eng.Trigger()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	st := e.cycle()
	again := e.cycle()

	require.Equal(t, "enforce", st.Mode)
	require.Empty(t, st.Problems)
	require.Len(t, e.tunnels(), 1, "one tunnel whatever the order")
	require.Equal(t, []string{"api.example.com", "www.example.com"}, e.recordNames())
	for _, r := range e.records() {
		require.Equal(t, "CNAME", r.Type, "adopted")
	}
	require.Equal(t, st, again)
}

func TestCallsWaitForTheCycleLockUntilTheirContextEnds(t *testing.T) {
	e, _ := conflicted(t)
	first := e.eng.State()
	require.NoError(t, e.eng.acquire(t.Context()))
	defer e.eng.release()
	before := e.files()

	for name, call := range map[string]func(ctx context.Context) error{
		"Cycle": func(ctx context.Context) error {
			if st := e.eng.Cycle(ctx); !assertEqualState(first, st) {
				return errors.New("a cycle ran")
			}
			return ctx.Err()
		},
		"Apply": func(ctx context.Context) error {
			_, err := e.eng.Apply(ctx, true, "")
			return err
		},
		"Adopt":            func(ctx context.Context) error { return e.eng.Adopt(ctx, "www.example.com") },
		"RemoveCredential": func(ctx context.Context) error { return e.eng.RemoveCredential(ctx, testCred) },
	} {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- call(ctx) }()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled, name)
	}

	require.Equal(t, before, e.files(), "nothing was done while the lock was held")
	require.Equal(t, 1, e.inv.refreshes())
	require.Nil(t, e.eng.confirm)
	require.Empty(t, e.eng.adopt)
}

func assertEqualState(a, b State) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestResolutionRunsAFewAtATimeEachWithItsOwnDeadline(t *testing.T) {
	e := newEnv(t)
	deadlines := &timeouts{}
	e.eng.timeout = deadlines.withTimeout
	routes := make([]string, resolveConcurrency+4)
	for i := range routes {
		routes[i] = fmt.Sprintf("h%02d.example.com -> :80", i)
	}
	e.inv.set(snapshot(guest(101, "web-1", routes...)))
	var mu sync.Mutex
	inflight, most := 0, 0
	started := make(chan struct{}, len(routes))
	release := make(chan struct{})
	e.res.hook(func() {
		mu.Lock()
		inflight++
		most = max(most, inflight)
		mu.Unlock()
		started <- struct{}{}
		<-release
		mu.Lock()
		inflight--
		mu.Unlock()
	})
	done := make(chan State)
	go func() { done <- e.cycle() }()

	for range resolveConcurrency {
		<-started
	}
	close(release)
	st := <-done

	require.Len(t, st.Routes, len(routes))
	require.Equal(t, resolveConcurrency, most, "so many at a time, never more")
	require.Len(t, e.res.deadlines, len(routes), "each call has a deadline of its own")
	require.Equal(t, len(routes), deadlines.count(15*time.Second))
	require.Equal(t, 1, deadlines.count(60*time.Second), "the refresh has one too")
}

// The DNS reconciler remembers names it saw wanted until its tombstones are
// saved; a new one starts without that memory, so it is made anew only when
// its settings change.
func TestTheDNSReconcilerIsKeptBetweenCycles(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	first := e.eng.dns
	require.NotNil(t, first)

	e.cycle()
	require.Same(t, first, e.eng.dns)

	e.settings(func(s *store.Settings) { s.Grace = store.Duration(2 * time.Minute) })
	e.cycle()
	require.NotSame(t, first, e.eng.dns)
	require.Equal(t, 2*time.Minute, e.eng.dnsSet.Grace)
}

func TestDNSSettingsFollowTheSettings(t *testing.T) {
	for poll, gap := range map[time.Duration]time.Duration{
		10 * time.Second: 2 * time.Minute,
		30 * time.Second: 3 * time.Minute,
	} {
		c := &cycleRun{install: store.Install{ID: testInstall}, settings: store.DefaultSettings()}
		c.settings.PollInterval = store.Duration(poll)
		c.settings.Grace = store.Duration(90 * time.Second)
		require.Equal(t, reconcile.DNSSettings{InstallID: testInstall, Grace: 90 * time.Second, MaxGap: gap}, c.dnsSettings())
	}
}

func TestClientsAreRebuiltOnlyWhenTheTokenChanges(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	e.cycle()
	require.Equal(t, []string{testCred}, e.made, "an unchanged token keeps its client")

	require.NoError(t, e.store.SaveCredential(store.Credential{ID: testCred, Label: "main", Kind: "scoped", Token: store.NewSecret("rotated-token"), AddedAt: t0}))
	// The first listing with the new token fails: the zones of the old one
	// still stand, as for any listing that fails.
	e.cf.FailNext("zones", 1, errors.New("connection reset"))
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Equal(t, []string{testCred, testCred}, e.made)
	require.Equal(t, []string{
		"credential cred1: listing its zones failed (connection reset); using the list from 2026-10-01T12:00:00Z",
	}, st.Problems)
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
}

func TestTheStateSaysWhenTheCycleEnded(t *testing.T) {
	e := newEnv(t)
	require.True(t, e.eng.State().FinishedAt.IsZero())
	e.inv.hook(func() { e.clock.advance(3 * time.Second) })

	st := e.cycle()

	require.Equal(t, t0, st.At)
	require.Equal(t, t0.Add(3*time.Second), st.FinishedAt)
}
