package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// published is an engine that has published www.example.com in enforce mode.
func published(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	st := e.cycle()
	require.Empty(t, st.Problems)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
	return e
}

// frozen is what a holding cycle must leave alone.
type frozen struct {
	files   map[string]string
	calls   int
	ensures int
	prunes  int
	rules   []planner.IngressRule
	records []string
	routes  []RouteView
	tunnels int
}

func freeze(e *env) frozen {
	f := frozen{
		files:   e.files(),
		ensures: len(e.conn.ensures()),
		prunes:  len(e.conn.prunes()),
		rules:   e.rules(),
		records: e.recordNames(),
		routes:  e.eng.State().Routes,
		tunnels: len(e.tunnels()),
	}
	// Reading the rules is a call too.
	f.calls = len(e.cf.Calls())
	return f
}

// requireUnchanged checks that nothing on disk, at Cloudflare or on the
// connectors changed, and that the state kept its routes.
func (f *frozen) requireUnchanged(t *testing.T, e *env, st State) {
	t.Helper()
	require.Len(t, e.cf.Calls(), f.calls, "Cloudflare is not asked")
	require.Equal(t, f.files, e.files(), "nothing on disk changes")
	require.Len(t, e.conn.ensures(), f.ensures, "no connector is started")
	require.Len(t, e.conn.prunes(), f.prunes, "no connector is pruned")
	require.Equal(t, f.rules, e.rules(), "the tunnel keeps its rules")
	f.calls = len(e.cf.Calls())
	require.Equal(t, f.records, e.recordNames(), "no record is deleted")
	require.Len(t, e.tunnels(), f.tunnels)
	require.Equal(t, f.routes, st.Routes, "the state keeps the last routes")
	require.Empty(t, st.Actions)
}

// Review Focus 2.
func TestCycleIncompleteSnapshotHolds(t *testing.T) {
	e := published(t)
	before := freeze(e)
	e.inv.set(incomplete("cluster resources not listed: proxmox api: HTTP 500"))

	// Long past every grace: an incomplete inventory proves nothing gone.
	for range 3 {
		e.clock.advance(61 * time.Second)
		st := e.cycle()

		require.False(t, st.Complete)
		require.Contains(t, st.Problems, "cluster resources not listed: proxmox api: HTTP 500")
		require.Contains(t, st.Problems, problemIncomplete)
		before.requireUnchanged(t, e, st)
	}

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.True(t, st.Complete)
	require.Empty(t, st.Problems)
	require.Equal(t, []string{"www.example.com"}, e.recordNames())
}

func TestCycleHoldsWhileTheClusterFilesystemIsNotMounted(t *testing.T) {
	var markerFile string
	e := newEnvWith(t, func(base string, p *store.Paths) {
		markerFile = filepath.Join(base, ".version")
		p.MountCheck = markerFile
		require.NoError(t, os.WriteFile(markerFile, []byte("1"), 0o600))
	})
	e.enforce()
	e.cycle()
	before := freeze(e)
	refreshes := e.inv.refreshes()

	require.NoError(t, os.Remove(markerFile))
	e.clock.advance(61 * time.Second)
	st := e.cycle()

	require.Equal(t, []string{"cluster filesystem is not mounted"}, st.Problems)
	require.Equal(t, refreshes, e.inv.refreshes())
	before.requireUnchanged(t, e, st)

	require.NoError(t, os.WriteFile(markerFile, []byte("1"), 0o600))
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.Empty(t, st.Problems)
}

func TestCycleOnANodeThatIsNotSetUp(t *testing.T) {
	t.Run("no cluster root", func(t *testing.T) {
		e := published(t)
		require.NoError(t, os.RemoveAll(e.paths.Cluster))
		calls := len(e.cf.Calls())

		st := e.cycle()

		require.Equal(t, []string{"pco is not set up on this node; run pco setup"}, st.Problems)
		require.Len(t, e.cf.Calls(), calls)
		require.NoDirExists(t, e.paths.Cluster, "nothing is made where the store should be")
	})
	t.Run("no install", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "install.json")))

		st := e.cycle()

		require.Equal(t, []string{"pco is not set up on this node; run pco setup"}, st.Problems)
		require.Zero(t, e.inv.refreshes())
		require.Empty(t, e.cf.Calls())
	})
}

func TestCycleWithUnreadableSettingsHolds(t *testing.T) {
	e := published(t)
	before := freeze(e)
	path := filepath.Join(e.paths.Cluster, "meta", "settings.json")
	bad := `{"schemaVersion":1,"rev":9,"id":"settings","data":{"gateTag":"cf-tunnel","denyHosts":["bad host"],` +
		`"pollInterval":"10s","grace":"1m0s","admission":"tag","observeOnly":false}}`
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o600))
	before.files[path] = bad
	refreshes := e.inv.refreshes()

	e.clock.advance(61 * time.Second)
	st := e.cycle()

	require.Len(t, st.Problems, 1)
	require.Contains(t, st.Problems[0], "reading the settings: stored settings are invalid")
	require.Equal(t, refreshes, e.inv.refreshes())
	before.requireUnchanged(t, e, st)
}

// The store refuses settings with a pattern that does not normalise, so the
// planner can only see one if the two ever disagree; the cycle holds anyway.
func TestCycleWithAnInvalidPolicyHolds(t *testing.T) {
	e := published(t)
	before := freeze(e)
	e.inv.set(snapshot())
	e.clock.advance(61 * time.Second)

	require.NoError(t, e.eng.acquire(t.Context()))
	c := e.eng.newCycle(t.Context())
	require.True(t, c.prepare())
	c.settings.DenyHosts = []string{"bad host"}
	require.False(t, c.inspect())
	st := c.st.normalized()
	e.eng.release()

	require.Contains(t, st.Problems, problemPolicyInvalid)
	require.Contains(t, st.Issues, planner.Issue{
		Msg: `invalid deny pattern "bad host": needs at least two labels; all hostnames are denied until it is fixed`,
	})
	before.requireUnchanged(t, e, st)

	unwanted, err := c.stillUnwanted(t.Context(), "www.example.com")
	require.False(t, unwanted)
	require.Error(t, err, "a delete is never confirmed under a policy that cannot be read")
}

func TestCycleWithoutAWriterKeepsCloudflareButShowsTheRoutes(t *testing.T) {
	for name, setup := range map[string]struct {
		change  func(t *testing.T, e *env)
		problem string
	}{
		"missing": {
			change: func(t *testing.T, e *env) {
				require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "leader.json")))
			},
			problem: "no writer identity; run pco setup",
		},
		"another install": {
			change: func(t *testing.T, e *env) {
				require.NoError(t, e.store.SaveWriter(planner.Writer{InstallID: "other1", Generation: 1, Nonce: "n1"}))
			},
			problem: "leader.json names install other1, but this is install abc123; run pco setup --recover",
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := published(t)
			setup.change(t, e)
			e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :3000")))
			writes, ensures, prunes := e.writes(), len(e.conn.ensures()), len(e.conn.prunes())
			calls := len(e.cf.Calls())

			e.clock.advance(20 * time.Second)
			st := e.cycle()

			require.Equal(t, []string{setup.problem}, st.Problems)
			require.Equal(t, planner.StateActive, route(st, "api.example.com").State, "the routes are worked out for display")
			require.Equal(t, writes, e.writes())
			require.Len(t, e.conn.ensures(), ensures)
			require.Len(t, e.conn.prunes(), prunes)
			require.Empty(t, st.Actions)
			for _, c := range e.cf.Calls()[calls:] {
				require.NotContains(t, c, "FindTunnel", "no reconciler runs")
				require.NotContains(t, c, "Records", "no reconciler runs")
			}
		})
	}
}

func TestCycleWhoseClaimsCannotBeSavedKeepsCloudflare(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	e := published(t)
	claims := filepath.Join(e.paths.Cluster, "claims")
	require.NoError(t, os.Chmod(claims, 0o500))
	t.Cleanup(func() { _ = os.Chmod(claims, 0o700) })
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :3000")))
	writes := e.writes()

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Len(t, st.Problems, 1)
	require.Contains(t, st.Problems[0], "saving the claims")
	require.Equal(t, writes, e.writes(), "a hostname whose claim is not saved is not published")

	require.NoError(t, os.Chmod(claims, 0o700))
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.Empty(t, st.Problems)
	require.Equal(t, []string{"api.example.com", "www.example.com"}, e.recordNames())
}

func TestCycleEndingDuringResolutionChangesNothing(t *testing.T) {
	e := published(t)
	before := freeze(e)
	ctx, cancel := context.WithCancel(t.Context())
	e.res.hook(cancel)

	e.clock.advance(61 * time.Second)
	st := e.eng.Cycle(ctx)

	require.Contains(t, st.Problems, "the cycle ended while addresses were resolved (context canceled); nothing is changed")
	before.requireUnchanged(t, e, st)
}

func TestCycleEndingDuringTheTunnelRunLeavesTheRestAlone(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	ctx, cancel := context.WithCancel(t.Context())
	e.useAPI(testToken, hookedAPI{API: e.cf, before: func(method string) {
		if method == "FindTunnel" {
			cancel()
		}
	}})

	st := e.eng.Cycle(ctx)

	require.Contains(t, st.Problems, "the cycle ended before the connectors (context canceled); the rest is left as it is")
	require.True(t, st.Tunnels[0].Unknown)
	require.Empty(t, e.writes())
	require.Empty(t, e.conn.ensures())
	require.Empty(t, e.conn.prunes())
	for _, c := range e.cf.Calls() {
		require.NotContains(t, c, "Records", "DNS is not looked at")
	}
}

func TestCycleWithoutACredential(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	require.NoError(t, e.store.DeleteCredential(testCred))

	st := e.cycle()

	require.Equal(t, []string{"no Cloudflare credential; add one with pco credential add"}, st.Problems)
	require.Empty(t, e.cf.Calls(), "no Cloudflare call")
	require.Empty(t, e.made, "no client is built")
	require.Equal(t, planner.StateNoZone, route(st, "www.example.com").State)
	require.Empty(t, e.conn.ensures())
	require.Empty(t, e.conn.prunes(), "no connector is pruned for want of a credential")
}

func TestCycleWithACredentialWhoseZonesWereNeverListed(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cf.FailNext("zones", 1, errors.New("connection reset"))

	st := e.cycle()

	require.Equal(t, []string{
		"credential cred1: its zones are not listed yet (connection reset); nothing is changed at Cloudflare until they are",
	}, st.Problems)
	require.Equal(t, []string{"Zones"}, e.cf.Calls())
	require.Empty(t, e.conn.prunes())

	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.Empty(t, st.Problems, "a credential never listed is tried again in the next cycle")
	require.Equal(t, []string{"www.example.com"}, e.recordNames())

	// Once listed, a failing listing keeps the list it had.
	e.clock.advance(zoneRefreshEvery)
	e.cf.FailNext("zones", 1, errors.New("connection reset"))
	st = e.cycle()
	require.Equal(t, []string{
		"credential cred1: listing its zones failed (connection reset); using the list from 2026-10-01T12:00:10Z",
	}, st.Problems)
	require.Equal(t, planner.StateActive, route(st, "www.example.com").State)
}

func TestNoPruneWhileATunnelIsUnknown(t *testing.T) {
	e := published(t)
	require.Len(t, e.conn.prunes(), 1)

	e.cf.FailNext("tunnel.read", 1, errors.New("connection reset"))
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.True(t, st.Tunnels[0].Unknown)
	require.Contains(t, st.Problems, "pco-abc123 in account acc1: finding the tunnel: connection reset")
	require.Len(t, e.conn.prunes(), 1, "no prune while a tunnel is unknown")

	e.clock.advance(20 * time.Second)
	e.cycle()
	require.Len(t, e.conn.prunes(), 2)
}

// What cannot be read is never taken for nothing: a claim, binding, manual
// route or approval file that does not parse holds the cycle and is left as
// it is.
func TestCycleWithUnreadableStateHolds(t *testing.T) {
	for name, tc := range map[string]struct {
		root    func(e *env) string
		file    string
		setup   func(e *env)
		problem string
	}{
		"claims":        {root: func(e *env) string { return e.paths.Cluster }, file: "claims/x.example.com.json", problem: "reading the claims"},
		"bindings":      {root: func(e *env) string { return e.paths.Local }, file: "bindings/x.example.com.json", problem: "reading the bindings"},
		"manual routes": {root: func(e *env) string { return e.paths.Cluster }, file: "routes/nas.json", problem: "reading the manual routes"},
		"approvals": {
			root: func(e *env) string { return e.paths.Cluster }, file: "approvals/qemu_101.json", problem: "reading the approvals",
			setup: func(e *env) { e.settings(func(s *store.Settings) { s.Admission = "approve" }) },
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := published(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			path := filepath.Join(tc.root(e), tc.file)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte(`{"schemaVersion":1,"rev":1,"id":"x","data":{`), 0o600))
			before := freeze(e)
			e.inv.set(snapshot())

			e.clock.advance(61 * time.Second)
			st := e.cycle()

			require.Len(t, st.Problems, 1)
			require.Contains(t, st.Problems[0], tc.problem)
			before.requireUnchanged(t, e, st)
		})
	}
}

func TestCycleWithUnreadableCredentialsKeepsCloudflareAndTheRoutes(t *testing.T) {
	e := published(t)
	routes := e.eng.State().Routes
	path := filepath.Join(e.paths.Private, "credentials", "cred2.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"schemaVersion":1,"rev":1,"id":"cred2","data":{`), 0o600))
	e.inv.set(snapshot())
	calls := len(e.cf.Calls())

	e.clock.advance(61 * time.Second)
	st := e.cycle()

	require.Len(t, st.Problems, 1)
	require.Contains(t, st.Problems[0], "reading the credentials")
	require.NotContains(t, st.Problems[0], testToken)
	require.Len(t, e.cf.Calls(), calls)
	require.Equal(t, routes, st.Routes, "routes stay as they were shown")
	require.Len(t, st.Credentials, 1)
	require.Len(t, e.conn.prunes(), 1)
}
