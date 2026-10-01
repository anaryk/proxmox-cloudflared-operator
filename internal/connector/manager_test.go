package connector

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const (
	idD = "44444444-4444-4444-8444-444444444444"

	// idUpper is a valid id in the wrong case.
	idUpper = "CCCCCCCC-CCCC-4CCC-8CCC-CCCCCCCCCCCC"
)

var (
	unitA = UnitName(idA)
	unitB = UnitName(idB)
	unitC = UnitName(idC)

	errBoom = errors.New("boom")
)

func TestUnitName(t *testing.T) {
	require.Equal(t, "pco-cloudflared@"+idA+".service", UnitName(idA))
}

func TestEnsureWritesFilesAndEnablesTheUnit(t *testing.T) {
	m, sd, dir := newTestManager(t)

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, "token-1", readFile(t, dir, idA+".token"))
	require.Equal(t, "METRICS_ADDR=127.0.0.1:20300\n", readFile(t, dir, idA+".env"))
	require.Equal(t, os.FileMode(0o700), fileMode(t, dir))
	require.Equal(t, os.FileMode(0o600), fileMode(t, filepath.Join(dir, idA+".token")))
	require.Equal(t, os.FileMode(0o644), fileMode(t, filepath.Join(dir, idA+".env")))
	require.ElementsMatch(t, []string{idA + ".token", idA + ".env"}, listDir(t, dir), "no temporary file is left behind")
	require.Equal(t, []string{"IsActive " + unitA, "EnableNow " + unitA}, sd.calls)
}

func TestEnsureTwiceWithTheSameInputChangesNothing(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	tokenBefore := statFile(t, dir, idA+".token")
	envBefore := statFile(t, dir, idA+".env")
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Empty(t, sd.changes())
	require.True(t, os.SameFile(tokenBefore, statFile(t, dir, idA+".token")), "token file was rewritten")
	require.True(t, os.SameFile(envBefore, statFile(t, dir, idA+".env")), "env file was rewritten")
	require.Len(t, listDir(t, dir), 2)
}

func TestEnsureRestartsOnlyOnTokenChange(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a-rotated"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))

	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.Equal(t, "token-a-rotated", readFile(t, dir, idA+".token"))
	require.Equal(t, "token-b", readFile(t, dir, idB+".token"))
}

func TestEnsureOnlyStartsAnInactiveUnitWhoseTokenChanged(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	delete(sd.active, unitA)
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-2"))

	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes())
	require.Equal(t, "token-2", readFile(t, dir, idA+".token"))
}

func TestEnsureStartsAUnitThatStoppedWithoutAChange(t *testing.T) {
	m, sd, _ := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	delete(sd.active, unitA)
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes())
}

func TestEnsureTreatsAnUnreadableTokenFileAsChanged(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	path := filepath.Join(dir, idA+".token")
	require.NoError(t, os.Chmod(path, 0))
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.Equal(t, "token-1", readFile(t, dir, idA+".token"))
	require.Equal(t, os.FileMode(0o600), fileMode(t, path))
}

func TestEnsureRetriesARestartThatFailed(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	sd.fail["Restart "+unitA] = errBoom

	err := m.Ensure(t.Context(), idA, "token-2")
	require.ErrorIs(t, err, errBoom)
	require.ErrorContains(t, err, unitA)
	require.FileExists(t, filepath.Join(dir, pendingOf(idA)), "a restart that failed stays pending")

	delete(sd.fail, "Restart "+unitA)
	sd.reset()
	require.NoError(t, m.Ensure(t.Context(), idA, "token-2"), "the token on disk is already the new one")
	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.NoFileExists(t, filepath.Join(dir, pendingOf(idA)))

	sd.reset()
	require.NoError(t, m.Ensure(t.Context(), idA, "token-2"))
	require.Empty(t, sd.changes())
}

func TestEnsureRestartsAfterAFailedActivityCheck(t *testing.T) {
	m, sd, _ := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	sd.fail["IsActive "+unitA] = errBoom

	require.ErrorIs(t, m.Ensure(t.Context(), idA, "token-2"), errBoom)

	delete(sd.fail, "IsActive "+unitA)
	sd.reset()
	require.NoError(t, m.Ensure(t.Context(), idA, "token-2"))
	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
}

func TestEnsureAppliesAPendingTokenAfterTheDaemonRestarted(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	sd.fail["IsActive "+unitA] = errBoom
	require.ErrorIs(t, m.Ensure(t.Context(), idA, "token-2"), errBoom)
	marker := filepath.Join(dir, pendingOf(idA))
	require.Equal(t, "", readFile(t, dir, pendingOf(idA)))
	require.Equal(t, os.FileMode(0o600), fileMode(t, marker))
	delete(sd.fail, "IsActive "+unitA)
	sd.reset()

	restarted := NewManager(sd, dir, nil, zerolog.Nop())
	require.NoError(t, restarted.Ensure(t.Context(), idA, "token-2"))

	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.NoFileExists(t, marker)
	sd.reset()
	require.NoError(t, restarted.Ensure(t.Context(), idA, "token-2"))
	require.Empty(t, sd.changes())
}

func TestEnsureRestartsWhenAMarkerIsLeftBehind(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	writeFile(t, dir, pendingOf(idA), "")
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"), "whatever the bytes on disk say")

	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.NoFileExists(t, filepath.Join(dir, pendingOf(idA)))
}

func TestEnsureStartsAPendingUnitThatStoppedInsteadOfRestartingIt(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	writeFile(t, dir, pendingOf(idA), "")
	delete(sd.active, unitA)
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes())
	require.NoFileExists(t, filepath.Join(dir, pendingOf(idA)))
}

func TestEnsureMarksTheTokenPendingBeforeReplacingIt(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	// A non-empty directory in place of the token file makes the replacement
	// fail, as a crash in the middle of it would.
	token := filepath.Join(dir, idA+".token")
	require.NoError(t, os.Remove(token))
	require.NoError(t, os.MkdirAll(filepath.Join(token, "x"), 0o700))
	sd.reset()

	require.Error(t, m.Ensure(t.Context(), idA, "token-2"))

	require.FileExists(t, filepath.Join(dir, pendingOf(idA)))
	require.Empty(t, sd.calls)
}

func TestEnsureRetriesAnEnableThatFailed(t *testing.T) {
	m, sd, dir := newTestManager(t)
	sd.fail["EnableNow "+unitA] = errBoom

	err := m.Ensure(t.Context(), idA, "token-1")
	require.ErrorIs(t, err, errBoom)
	require.ErrorContains(t, err, unitA)
	require.FileExists(t, filepath.Join(dir, pendingOf(idA)))

	delete(sd.fail, "EnableNow "+unitA)
	sd.reset()
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes())
	require.NoFileExists(t, filepath.Join(dir, pendingOf(idA)))
}

func TestEnsureAllocatesStablePortsFromTheLowestFree(t *testing.T) {
	m, _, dir := newTestManager(t)

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	require.Equal(t, 20300, portOf(t, dir, idA))
	require.Equal(t, 20301, portOf(t, dir, idB))

	require.NoError(t, m.Ensure(t.Context(), idB, "token-b-rotated"))
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a-rotated"))
	require.NoError(t, m.Ensure(t.Context(), idC, "token-c"))
	require.Equal(t, 20300, portOf(t, dir, idA))
	require.Equal(t, 20301, portOf(t, dir, idB))
	require.Equal(t, 20302, portOf(t, dir, idC))

	require.NoError(t, m.Prune(t.Context(), []string{idA, idC}))
	require.NoError(t, m.Ensure(t.Context(), idD, "token-d"))
	require.Equal(t, 20301, portOf(t, dir, idD), "the port of a removed connector is free again")
}

func TestEnsureKeepsAnExistingPort(t *testing.T) {
	m, _, dir := newTestManager(t)
	writeFile(t, dir, idA+".env", "METRICS_ADDR=127.0.0.1:20450\n")
	before := statFile(t, dir, idA+".env")

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))

	require.Equal(t, 20450, portOf(t, dir, idA))
	require.True(t, os.SameFile(before, statFile(t, dir, idA+".env")), "env file was rewritten")
	require.Equal(t, 20300, portOf(t, dir, idB))
}

func TestEnsureKeepsThePortWhenOnlyTheAddressIsOdd(t *testing.T) {
	m, _, dir := newTestManager(t)
	writeFile(t, dir, idA+".env", "METRICS_ADDR=0.0.0.0:20450\n")

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))

	require.Equal(t, "METRICS_ADDR=127.0.0.1:20450\n", readFile(t, dir, idA+".env"))
}

func TestEnsureDoesNotTouchTheMalformedEnvFileOfAnotherTunnel(t *testing.T) {
	m, _, dir := newTestManager(t)
	writeFile(t, dir, idB+".env", "not an env file\nMETRICS_ADDR=nonsense\n")

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))

	require.Equal(t, 20300, portOf(t, dir, idA))
	require.Equal(t, "not an env file\nMETRICS_ADDR=nonsense\n", readFile(t, dir, idB+".env"))
	require.NoFileExists(t, filepath.Join(dir, idB+".token"))
}

func TestEnsureRepairsItsOwnMalformedEnvFile(t *testing.T) {
	m, _, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	writeFile(t, dir, idB+".env", "METRICS_ADDR=127.0.0.1:99999\n")

	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))

	require.Equal(t, 20301, portOf(t, dir, idB))
}

func TestEnsureReservesThePortsOfEveryEnvFile(t *testing.T) {
	m, _, dir := newTestManager(t)
	writeFile(t, dir, "other.env", "METRICS_ADDR=127.0.0.1:20300\n")
	writeFile(t, dir, idC+".env", "# set by hand\nMETRICS_ADDR=127.0.0.1:20301\n")

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))

	require.Equal(t, 20302, portOf(t, dir, idA))
}

func TestEnsureRejectsInvalidIDs(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"short", "abc"},
		{"one character short", idA[:35]},
		{"one character long", idA + "1"},
		{"upper case", idUpper},
		{"not hex", strings.Repeat("g", 36)},
		{"trailing newline", idA[:35] + "\n"},
		{"path traversal", strings.Repeat("../", 12)},
		{"dot dot", ".."},
		{"slash", idA[:35] + "/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, sd, dir := newTestManager(t)

			err := m.Ensure(t.Context(), tc.id, "token")

			require.ErrorContains(t, err, "invalid tunnel id")
			require.Empty(t, sd.calls)
			require.NoDirExists(t, dir)
		})
	}
}

func TestEnsureRejectsAnEmptyToken(t *testing.T) {
	m, sd, dir := newTestManager(t)

	require.ErrorContains(t, m.Ensure(t.Context(), idA, ""), "empty token")

	require.Empty(t, sd.calls)
	require.NoDirExists(t, dir)
}

func TestEnsureFailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	sd := newFakeSystemd()
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	m := NewManager(sd, filepath.Join(blocker, "tunnels"), nil, zerolog.Nop())

	require.Error(t, m.Ensure(t.Context(), idA, "token"))
	require.Empty(t, sd.calls)
}

func TestConcurrentEnsureAllocatesDistinctPorts(t *testing.T) {
	m, _, dir := newTestManager(t)
	const n = 12
	id := func(i int) string { return fmt.Sprintf("%08d-0000-4000-8000-000000000000", i) }
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = m.Ensure(t.Context(), id(i), "token") })
	}
	wg.Wait()

	seen := map[int]bool{}
	for i := range n {
		require.NoError(t, errs[i])
		port := portOf(t, dir, id(i))
		require.GreaterOrEqual(t, port, 20300)
		require.Less(t, port, 20300+n)
		seen[port] = true
	}
	require.Len(t, seen, n)
}

func TestPruneRemovesTheUnwantedConnectorAndKeepsTheWanted(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	sd.reset()

	require.NoError(t, m.Prune(t.Context(), []string{idA}))

	require.Equal(t, []string{"ListUnits pco-cloudflared@*.service", "DisableNow " + unitB}, sd.calls)
	require.ElementsMatch(t, []string{idA + ".token", idA + ".env"}, listDir(t, dir))
	require.True(t, sd.active[unitA])
	require.False(t, sd.active[unitB])
}

func TestPruneWithNothingToKeepRemovesEveryConnector(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	sd.reset()

	require.NoError(t, m.Prune(t.Context(), nil))

	require.Equal(t, []string{"DisableNow " + unitA, "DisableNow " + unitB}, sd.changes())
	require.Empty(t, listDir(t, dir))
}

func TestPruneFindsConnectorsByFilesAndByUnits(t *testing.T) {
	m, sd, dir := newTestManager(t)
	writeFile(t, dir, idA+".token", "token-a")                      // token only
	writeFile(t, dir, idB+".env", "METRICS_ADDR=127.0.0.1:20300\n") // env only, as after an interrupted Ensure
	sd.loaded = []string{unitC}                                     // unit without files
	writeFile(t, dir, idD+".token", "token-d")
	writeFile(t, dir, idD+".env", "METRICS_ADDR=127.0.0.1:20301\n")

	require.NoError(t, m.Prune(t.Context(), []string{idD}))

	require.Equal(t, []string{
		"DisableNow " + unitA,
		"DisableNow " + unitB,
		"DisableNow " + unitC,
	}, sd.changes())
	require.ElementsMatch(t, []string{idD + ".token", idD + ".env"}, listDir(t, dir))
}

func TestPruneKeepsTheFilesOfAUnitThatWillNotStop(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	sd.fail["DisableNow "+unitA] = errBoom

	err := m.Prune(t.Context(), nil)

	require.ErrorIs(t, err, errBoom)
	require.ErrorContains(t, err, unitA)
	require.ElementsMatch(t, []string{idA + ".token", idA + ".env"}, listDir(t, dir), "the connector that still runs keeps its files")
	require.Contains(t, sd.calls, "DisableNow "+unitB, "one failure does not stop the others")

	delete(sd.fail, "DisableNow "+unitA)
	require.NoError(t, m.Prune(t.Context(), nil))
	require.Empty(t, listDir(t, dir))
}

func TestPruneContinuesPastARemovalThatFails(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	// A directory in place of a file cannot be removed with os.Remove once it
	// has content.
	require.NoError(t, os.Remove(filepath.Join(dir, idA+".env")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, idA+".env", "x"), 0o700))
	sd.reset()

	err := m.Prune(t.Context(), nil)

	require.Error(t, err)
	require.ErrorContains(t, err, idA+".env")
	require.NoFileExists(t, filepath.Join(dir, idA+".token"))
	require.NoFileExists(t, filepath.Join(dir, idB+".token"))
	require.NoFileExists(t, filepath.Join(dir, idB+".env"))
	require.Equal(t, []string{"DisableNow " + unitA, "DisableNow " + unitB}, sd.changes())
}

func TestPruneLeavesNamesThatAreNoTunnelAlone(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	writeFile(t, dir, "foo.token", "mine")
	writeFile(t, dir, "notes.env", "mine")
	writeFile(t, dir, "README", "mine")
	sd.loaded = []string{"pco-cloudflared@foo.service", "pco-cloudflared@" + idUpper + ".service", "other.service"}
	sd.reset()

	err := m.Prune(t.Context(), []string{idA})

	require.ErrorContains(t, err, "foo.token")
	require.ErrorContains(t, err, "notes.env")
	require.ErrorContains(t, err, "pco-cloudflared@foo.service")
	require.ErrorContains(t, err, "pco-cloudflared@"+idUpper+".service")
	require.ErrorContains(t, err, "other.service")
	require.NotContains(t, err.Error(), "README")
	require.Equal(t, []string{"DisableNow " + unitB}, sd.changes(), "only a real tunnel id reaches systemd")
	require.ElementsMatch(t, []string{
		idA + ".token", idA + ".env", "foo.token", "notes.env", "README",
	}, listDir(t, dir))
}

func TestPruneStillRemovesFilesWhenUnitsCannotBeListed(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	sd.fail["ListUnits pco-cloudflared@*.service"] = errBoom

	err := m.Prune(t.Context(), nil)

	require.ErrorIs(t, err, errBoom)
	require.Empty(t, listDir(t, dir))
}

func TestPruneWorksBeforeTheDirectoryExists(t *testing.T) {
	m, sd, dir := newTestManager(t)
	sd.loaded = []string{unitA}

	require.NoError(t, m.Prune(t.Context(), nil))

	require.Equal(t, []string{"DisableNow " + unitA}, sd.changes())
	require.NoDirExists(t, dir)
}

func TestEnsureAfterPruneStartsTheConnectorAgain(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Prune(t.Context(), nil))
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))

	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes())
	require.Equal(t, 20300, portOf(t, dir, idA))
}

func statFile(t *testing.T, dir, name string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name))
	require.NoError(t, err)
	return info
}

// portOf is the port in the env file of a tunnel, however it is written.
func portOf(t *testing.T, dir, id string) int {
	t.Helper()
	_, port, err := readMetricsAddr(filepath.Join(dir, id+".env"))
	require.NoError(t, err)
	return port
}

func TestEnsureRepairsModesWithoutRewritingOrRestarting(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	token, env := filepath.Join(dir, idA+".token"), filepath.Join(dir, idA+".env")
	tokenBefore, envBefore := statFile(t, dir, idA+".token"), statFile(t, dir, idA+".env")
	require.NoError(t, os.Chmod(token, 0o644))
	require.NoError(t, os.Chmod(env, 0o600))
	require.NoError(t, os.Chmod(dir, 0o755))
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, os.FileMode(0o600), fileMode(t, token))
	require.Equal(t, os.FileMode(0o644), fileMode(t, env))
	require.Equal(t, os.FileMode(0o700), fileMode(t, dir))
	require.True(t, os.SameFile(tokenBefore, statFile(t, dir, idA+".token")), "token file was rewritten")
	require.True(t, os.SameFile(envBefore, statFile(t, dir, idA+".env")), "env file was rewritten")
	require.Empty(t, sd.changes(), "a mode alone is no reason to restart")
	require.Len(t, listDir(t, dir), 2)
}

func TestEnsureRepairsTheModeOfADirectoryThatExists(t *testing.T) {
	m, _, dir := newTestManager(t)
	require.NoError(t, os.Mkdir(dir, 0o755))

	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, os.FileMode(0o700), fileMode(t, dir))
}

// stale returns the names of temporary files a crashed write leaves behind.
func stale(id string) []string {
	return []string{"." + id + ".token.123.tmp", ".." + id + ".pending.456.tmp", "." + id + ".env.789.tmp"}
}

func TestEnsureRemovesStaleTemporaryFiles(t *testing.T) {
	m, _, dir := newTestManager(t)
	for _, name := range stale(idB) {
		writeFile(t, dir, name, "a copy of a token")
	}
	writeFile(t, dir, "plain.tmp", "not hidden, not ours")
	writeFile(t, dir, ".hidden", "no tmp suffix")

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))

	require.ElementsMatch(t, []string{idA + ".token", idA + ".env", "plain.tmp", ".hidden"}, listDir(t, dir))
}

func TestPruneRemovesStaleTemporaryFiles(t *testing.T) {
	m, _, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	for _, name := range stale(idA) {
		writeFile(t, dir, name, "a copy of a token")
	}
	writeFile(t, dir, "plain.tmp", "not hidden, not ours")

	require.NoError(t, m.Prune(t.Context(), []string{idA}), "a kept tunnel's leftovers go too")

	require.ElementsMatch(t, []string{idA + ".token", idA + ".env", "plain.tmp"}, listDir(t, dir))
}

func TestPruneRejectsAnInvalidIDInKeepBeforeRemovingAnything(t *testing.T) {
	for name, bad := range map[string]string{
		"empty":      "",
		"upper case": idUpper,
		"padded":     " " + idA,
		"traversal":  "../" + idA[3:],
	} {
		t.Run(name, func(t *testing.T) {
			m, sd, dir := newTestManager(t)
			require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
			require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
			writeFile(t, dir, stale(idB)[0], "a copy of a token")
			sd.reset()

			err := m.Prune(t.Context(), []string{idA, bad})

			require.ErrorContains(t, err, "invalid tunnel id")
			require.Empty(t, sd.calls)
			require.Len(t, listDir(t, dir), 5)
		})
	}
}

func TestDotNamesAreInvisibleToPruneAndPortAllocation(t *testing.T) {
	m, sd, dir := newTestManager(t)
	// Each of these would name a port or a connector if the dot did not hide it.
	hidden := []string{"." + idC + ".env", "." + idC + ".token", ".stray.pending"}
	writeFile(t, dir, hidden[0], "METRICS_ADDR=127.0.0.1:20300\n")
	writeFile(t, dir, hidden[1], "x")
	writeFile(t, dir, hidden[2], "")

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.Equal(t, 20300, portOf(t, dir, idA), "a hidden env file reserves no port")
	sd.reset()
	require.NoError(t, m.Prune(t.Context(), []string{idA}), "hidden names are not reported")

	require.Empty(t, sd.changes())
	require.ElementsMatch(t, append([]string{idA + ".token", idA + ".env"}, hidden...), listDir(t, dir))
}

func TestPruneRemovesThePendingMarker(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	sd.fail["Restart "+unitB] = errBoom
	require.Error(t, m.Ensure(t.Context(), idB, "token-b-rotated"))
	require.FileExists(t, filepath.Join(dir, pendingOf(idB)))

	require.NoError(t, m.Prune(t.Context(), []string{idA}))

	require.ElementsMatch(t, []string{idA + ".token", idA + ".env"}, listDir(t, dir))
}

func TestEnsureFailsWhenNoPortIsLeft(t *testing.T) {
	m, sd, dir := newTestManager(t)
	m.firstPort = 65535

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.Equal(t, 65535, portOf(t, dir, idA))
	sd.reset()
	err := m.Ensure(t.Context(), idB, "token-b")

	require.ErrorContains(t, err, "no free metrics port")
	require.Empty(t, sd.calls)
	require.ElementsMatch(t, []string{idA + ".token", idA + ".env"}, listDir(t, dir))
}

func TestEnsureReadsQuotedAddressesLikeSystemdDoes(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantPort int
		rewrite  bool
	}{
		{"double quotes", `METRICS_ADDR="127.0.0.1:20450"` + "\n", 20450, false},
		{"single quotes", `METRICS_ADDR='127.0.0.1:20450'` + "\n", 20450, false},
		{"quotes and a comment", "# hand made\nMETRICS_ADDR=\"127.0.0.1:20450\"\n", 20450, false},
		{"quoted other host", `METRICS_ADDR="0.0.0.0:20450"` + "\n", 20450, true},
		{"quotes that do not match", `METRICS_ADDR="127.0.0.1:20450'` + "\n", 20300, true},
		{"quote only at the start", `METRICS_ADDR="127.0.0.1:20450` + "\n", 20300, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, sd, dir := newTestManager(t)
			require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
			writeFile(t, dir, idA+".env", tc.content)
			before := statFile(t, dir, idA+".env")
			sd.reset()

			require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))

			require.Equal(t, tc.wantPort, portOf(t, dir, idA))
			require.Equal(t, !tc.rewrite, os.SameFile(before, statFile(t, dir, idA+".env")))
			if tc.rewrite {
				require.Equal(t, []string{"Restart " + unitA}, sd.changes())
			} else {
				require.Empty(t, sd.changes(), "the same address in other words is no change")
			}
		})
	}
}

func TestEnsureRestartsAnActiveUnitWhoseEnvFileIsRewritten(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	writeFile(t, dir, idA+".env", "METRICS_ADDR=127.0.0.1:20450\n")
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.Empty(t, sd.changes(), "an address the manager would have written is left alone")

	writeFile(t, dir, idA+".env", "METRICS_ADDR=10.0.0.1:20450\n")
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.Equal(t, "METRICS_ADDR=127.0.0.1:20450\n", readFile(t, dir, idA+".env"))
	require.Equal(t, []string{"Restart " + unitA}, sd.changes(), "the process still holds the old address")
	require.NoFileExists(t, filepath.Join(dir, pendingOf(idA)))

	sd.reset()
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.Empty(t, sd.changes())
}

func pendingOf(id string) string { return "." + id + ".pending" }

// undeletable puts a directory with content in place of the marker of tunnel A:
// it can be seen, and removing it fails on every platform.
func undeletable(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, pendingOf(idA))
	require.NoError(t, os.RemoveAll(path))
	require.NoError(t, os.MkdirAll(filepath.Join(path, "x"), 0o700))
}

func TestEnsureRestartsOnceForAMarkerItCannotClear(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	undeletable(t, dir)
	sd.reset()

	for range 3 {
		err := m.Ensure(t.Context(), idA, "token-1")
		require.ErrorContains(t, err, "clearing the pending marker", "the problem stays visible")
		require.ErrorContains(t, err, pendingOf(idA))
	}
	require.Equal(t, []string{"Restart " + unitA}, sd.changes(), "a healthy connector is not restarted every poll")

	sd.reset()
	require.ErrorContains(t, m.Ensure(t.Context(), idA, "token-2"), "clearing the pending marker")
	require.Equal(t, []string{"Restart " + unitA}, sd.changes(), "a new token is applied whatever the marker says")
	require.Equal(t, "token-2", readFile(t, dir, idA+".token"))

	sd.reset()
	require.Error(t, m.Ensure(t.Context(), idA, "token-2"))
	require.Empty(t, sd.changes())
}

func TestEnsureStartsOnceForAMarkerItCannotClear(t *testing.T) {
	m, sd, dir := newTestManager(t)
	undeletable(t, dir)

	for range 3 {
		require.ErrorContains(t, m.Ensure(t.Context(), idA, "token-1"), "clearing the pending marker")
	}

	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes(), "the unit that was started is not restarted")
}

func TestEnsureStartsAStoppedUnitWhoseMarkerCannotBeCleared(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	undeletable(t, dir)
	require.Error(t, m.Ensure(t.Context(), idA, "token-1"))
	delete(sd.active, unitA)
	sd.reset()

	require.Error(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes())
}

func TestEnsureRestartsOnceForAMarkerItCannotStat(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-1"))
	undeletable(t, dir) // so that clearing it fails as well
	m.lstat = func(name string) (fs.FileInfo, error) {
		if filepath.Base(name) == pendingOf(idA) {
			return nil, errors.New("input/output error")
		}
		return os.Lstat(name)
	}
	sd.reset()

	for range 3 {
		require.Error(t, m.Ensure(t.Context(), idA, "token-1"))
	}

	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
}

func TestEnsureCarriesOnWhenAStaleTemporaryFileCannotBeRemoved(t *testing.T) {
	sd := newFakeSystemd()
	dir := filepath.Join(t.TempDir(), "tunnels")
	var logged bytes.Buffer
	m := NewManager(sd, dir, nil, zerolog.New(&logged))
	// A non-empty directory cannot be removed with os.Remove.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".stuck.tmp", "x"), 0o700))
	writeFile(t, dir, "."+idB+".token.1.tmp", "a copy of a token")

	require.NoError(t, m.Ensure(t.Context(), idA, "token-secret"))

	require.Equal(t, []string{"EnableNow " + unitA}, sd.changes())
	require.NoFileExists(t, filepath.Join(dir, "."+idB+".token.1.tmp"), "the others are still removed")
	require.Contains(t, logged.String(), `"level":"warn"`)
	require.Contains(t, logged.String(), ".stuck.tmp")
	require.NotContains(t, logged.String(), "token-secret")
	require.NotContains(t, logged.String(), "a copy of a token")
}

func TestPruneReportsAStaleTemporaryFileItCannotRemove(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), idB, "token-b"))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".stuck.tmp", "x"), 0o700))
	sd.reset()

	err := m.Prune(t.Context(), []string{idA})

	require.ErrorContains(t, err, ".stuck.tmp")
	require.Equal(t, []string{"DisableNow " + unitB}, sd.changes(), "the rest of the work is done")
}

func TestPruneRemovesAnOrphanMarker(t *testing.T) {
	m, sd, dir := newTestManager(t)
	// An empty directory in place of the env file makes its replacement fail.
	// The marker is written before that, so it is what a failed first Ensure
	// leaves of a tunnel that never got a token or a unit.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, idA+".env"), 0o700))

	require.Error(t, m.Ensure(t.Context(), idA, "token-a"))

	require.FileExists(t, filepath.Join(dir, pendingOf(idA)), "the marker comes before the first file is replaced")
	require.NoFileExists(t, filepath.Join(dir, idA+".token"))
	require.Empty(t, sd.calls)

	require.NoError(t, m.Prune(t.Context(), nil))

	require.Empty(t, listDir(t, dir))
}

func TestPruneKeepsTheMarkerOfAKeptTunnel(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-a"))
	sd.fail["Restart "+unitA] = errBoom
	require.Error(t, m.Ensure(t.Context(), idA, "token-a-rotated"))
	writeFile(t, dir, pendingOf(idB), "")
	sd.reset()

	require.NoError(t, m.Prune(t.Context(), []string{idA}))

	require.Equal(t, []string{"DisableNow " + unitB}, sd.changes())
	require.ElementsMatch(t, []string{idA + ".token", idA + ".env", pendingOf(idA)}, listDir(t, dir))
}
