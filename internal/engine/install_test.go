package engine

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	oldInstall = "old999"
	oldTunnel  = "99999999-9999-4999-8999-999999999999"
)

// units is a systemd that keeps which units run, for the real connector
// manager.
type units struct {
	mu      sync.Mutex
	running map[string]bool
	calls   []string
}

func (u *units) record(call string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, call)
}

func (u *units) set(unit string, running bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.running[unit] = running
}

func (u *units) EnableNow(_ context.Context, unit string) error {
	u.record("EnableNow " + unit)
	u.set(unit, true)
	return nil
}

func (u *units) DisableNow(_ context.Context, unit string) error {
	u.record("DisableNow " + unit)
	u.set(unit, false)
	return nil
}

func (u *units) Restart(_ context.Context, unit string) error {
	u.record("Restart " + unit)
	u.set(unit, true)
	return nil
}

func (u *units) IsActive(_ context.Context, unit string) (bool, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running[unit], nil
}

func (u *units) ListUnits(context.Context, string) ([]string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for unit, running := range u.running {
		if running {
			out = append(out, unit)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (u *units) isRunning(unit string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running[unit]
}

// changes returns the calls that changed a unit since the last of them, and
// forgets them.
func (u *units) changes() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.calls
	u.calls = nil
	return out
}

// withRealConnectors makes the engine of e use the connector manager over a
// directory of its own, with units it can look at, and returns them.
func withRealConnectors(e *env) (*connector.Manager, *units, string) {
	e.t.Helper()
	sd := &units{running: map[string]bool{}}
	dir := filepath.Join(e.t.TempDir(), "tunnels")
	// No connector answers on its metrics address in a test.
	httpc := &http.Client{Transport: refusing{}}
	m := connector.NewManager(sd, dir, httpc, zerolog.Nop())
	e.eng = e.newEngineWith(m)
	return m, sd, dir
}

type refusing struct{}

func (refusing) RoundTrip(*http.Request) (*http.Response, error) { return nil, os.ErrClosed }

func foreignLine(id, install string) string {
	return "connector for tunnel " + id + " belongs to install " + install +
		"; pco setup --recover adopts that install, pco uninstall on this node removes it"
}

func noInstallLine(id string) string {
	return "connector for tunnel " + id + " names no install" +
		"; pco setup --recover adopts the install it was made for, pco uninstall on this node removes it"
}

// The store was lost and pco set up anew: the connector of the old install
// still runs and serves the old hostnames, and the new install must leave it
// alone however sure it is of its own tunnels.
func TestAConnectorOfAnotherInstallIsNeverPruned(t *testing.T) {
	e := newEnv(t)
	m, sd, dir := withRealConnectors(e)
	require.NoError(t, m.Ensure(t.Context(), oldInstall, oldTunnel, "old-token"))
	sd.changes()

	st := e.cycle()
	require.Contains(t, st.Problems, foreignLine(oldTunnel, oldInstall), "observe-only says so before pco apply")

	e.enforce()
	for range 2 {
		st = e.cycle()
	}

	require.Len(t, e.tunnels(), 1)
	require.True(t, sd.isRunning(connector.UnitName(e.tunnels()[0].ID)), "the connector of this install runs")
	require.True(t, sd.isRunning(connector.UnitName(oldTunnel)), "the old one runs on")
	require.NotContains(t, sd.changes(), "DisableNow "+connector.UnitName(oldTunnel))
	require.FileExists(t, filepath.Join(dir, oldTunnel+".token"))
	require.Contains(t, st.Problems, foreignLine(oldTunnel, oldInstall))
	require.False(t, hasProblem(st, "connectors are not pruned"), "the prune ran: %v", st.Problems)
}

func TestAConnectorThatNamesNoInstallIsNeverPruned(t *testing.T) {
	e := newEnv(t)
	_, sd, dir := withRealConnectors(e)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, oldTunnel+".env"), []byte("METRICS_ADDR=127.0.0.1:20400\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, oldTunnel+".token"), []byte("old-token"), 0o600))
	sd.set(connector.UnitName(oldTunnel), true)
	e.enforce()

	st := e.cycle()

	require.True(t, sd.isRunning(connector.UnitName(oldTunnel)))
	require.Contains(t, st.Problems, noInstallLine(oldTunnel))
	require.False(t, hasProblem(st, "install unknown"), "%v", st.Problems)
}

// A unit that is loaded and has no files at all serves nothing and is nobody's:
// it is reported until a prune stops it.
func TestAUnitWithNoFilesIsReportedAndThenStoppedByAPrune(t *testing.T) {
	e := newEnv(t)
	_, sd, _ := withRealConnectors(e)
	sd.set(connector.UnitName(oldTunnel), true)

	st := e.cycle()

	require.Contains(t, st.Problems, noInstallLine(oldTunnel), "observe-only says so")
	require.True(t, sd.isRunning(connector.UnitName(oldTunnel)))

	e.enforce()
	st = e.cycle()

	require.False(t, sd.isRunning(connector.UnitName(oldTunnel)))
	require.False(t, hasProblem(st, oldTunnel), "%v", st.Problems)
}

func TestAConnectorOfThisInstallWhoseTunnelIsGoneIsPruned(t *testing.T) {
	e := newEnv(t)
	m, sd, dir := withRealConnectors(e)
	require.NoError(t, m.Ensure(t.Context(), testInstall, oldTunnel, "gone-token"))
	e.enforce()

	st := e.cycle()

	require.False(t, sd.isRunning(connector.UnitName(oldTunnel)))
	require.NoFileExists(t, filepath.Join(dir, oldTunnel+".env"))
	require.False(t, hasProblem(st, "belongs to install"), "%v", st.Problems)
}

// A connector of this install written before connectors named their install
// is brought in line without a restart, since cloudflared does not read the
// install, and is not taken for a foreign one or pruned on the way.
func TestAConnectorOfThisInstallWithoutTheInstallIsBroughtInLineWithoutARestart(t *testing.T) {
	e := newEnv(t)
	_, sd, dir := withRealConnectors(e)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	envPath := filepath.Join(dir, id+".env")
	old, err := os.ReadFile(envPath)
	require.NoError(t, err)
	before, _, found := strings.Cut(string(old), "PCO_INSTALL=")
	require.True(t, found)
	require.NoError(t, os.WriteFile(envPath, []byte(before), 0o644))
	sd.changes()

	st := e.cycle()

	require.Empty(t, sd.changes())
	require.Contains(t, readText(t, envPath), "PCO_INSTALL="+testInstall+"\n")
	require.False(t, hasProblem(st, "belongs to install"), "%v", st.Problems)

	e.cycle()
	require.Empty(t, sd.changes())
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}
