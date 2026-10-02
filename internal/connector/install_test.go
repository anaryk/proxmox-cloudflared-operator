package connector

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const (
	testInstall  = "abc123"
	otherInstall = "def456"
)

// connectorOf writes the files of a connector as a manager of an earlier
// version, or of another install, left them, with its unit running.
func connectorOf(t *testing.T, sd *fakeSystemd, dir, id, env string) {
	t.Helper()
	writeFile(t, dir, id+".token", "token-"+id)
	writeFile(t, dir, id+".yml", configContent)
	writeFile(t, dir, id+".env", env)
	sd.active[UnitName(id)] = true
}

func TestEnsureNamesTheInstallInTheEnvFile(t *testing.T) {
	m, _, dir := newTestManager(t)

	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-1"))

	require.Equal(t, "METRICS_ADDR=127.0.0.1:20300\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n", readFile(t, dir, idA+".env"))
}

func TestEnsureRejectsAnInvalidInstallID(t *testing.T) {
	for _, bad := range []string{"", "ABC", "abc 123", "abc\nPCO_INSTALL=x", "abc-123"} {
		t.Run(bad, func(t *testing.T) {
			m, sd, dir := newTestManager(t)

			require.ErrorContains(t, m.Ensure(t.Context(), bad, idA, "token-1"), "install id")

			require.Empty(t, sd.calls)
			require.NoFileExists(t, filepath.Join(dir, idA+".env"))
		})
	}
}

func TestEnsureBringsAConnectorWithoutAnInstallInLineWithOneRestart(t *testing.T) {
	m, sd, dir := newTestManager(t)
	connectorOf(t, sd, dir, idA, "METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=auto\n")
	writeFile(t, dir, idA+".token", "token-1")

	// A prune that does not keep it leaves it alone: it names no install.
	require.NoError(t, m.PruneInstall(t.Context(), testInstall, nil))
	require.Empty(t, sd.changes())

	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-1"))

	require.Equal(t, "METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n", readFile(t, dir, idA+".env"), "the port stays")
	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.NoFileExists(t, filepath.Join(dir, pendingOf(idA)))

	sd.reset()
	require.NoError(t, m.PruneInstall(t.Context(), testInstall, []string{idA}))
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-1"))
	require.Empty(t, sd.changes(), "once")
}

func TestPruneInstallRemovesOnlyTheConnectorsOfTheInstall(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	require.NoError(t, m.Ensure(t.Context(), testInstall, idB, "token-b"))
	connectorOf(t, sd, dir, idC, "METRICS_ADDR=127.0.0.1:20500\nEDGE_IP_VERSION=auto\nPCO_INSTALL="+otherInstall+"\n")
	connectorOf(t, sd, dir, idD, "METRICS_ADDR=127.0.0.1:20501\nEDGE_IP_VERSION=auto\n")
	sd.active[UnitName(idE)] = true // a unit without files names no install either
	sd.reset()

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, []string{idB}))

	require.Equal(t, []string{"DisableNow " + unitA}, sd.changes())
	require.ElementsMatch(t, []string{
		idB + ".token", idB + ".env", idB + ".yml",
		idC + ".token", idC + ".env", idC + ".yml",
		idD + ".token", idD + ".env", idD + ".yml",
	}, listDir(t, dir))
}

func TestPruneInstallRefusesAnInvalidInstallID(t *testing.T) {
	m, sd, _ := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	sd.reset()

	require.ErrorContains(t, m.PruneInstall(t.Context(), "", nil), "install id")

	require.Empty(t, sd.calls)
}

func TestPruneInstallFinishesARemovalThatWasInterrupted(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	sd.fail["DisableNow "+unitA] = errBoom
	require.ErrorIs(t, m.PruneInstall(t.Context(), testInstall, nil), errBoom)
	delete(sd.fail, "DisableNow "+unitA)
	// What a removal that stopped after the token leaves: the env file,
	// which names the install, goes last.
	for _, name := range []string{idA + ".token", idA + ".yml"} {
		require.NoError(t, os.Remove(filepath.Join(dir, name)))
	}
	sd.reset()

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, nil))

	require.Equal(t, []string{"DisableNow " + unitA}, sd.changes())
	require.Empty(t, listDir(t, dir))
}

func TestPruneRemovesTheConnectorsOfEveryInstall(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	connectorOf(t, sd, dir, idB, "METRICS_ADDR=127.0.0.1:20500\nPCO_INSTALL="+otherInstall+"\n")
	connectorOf(t, sd, dir, idC, "METRICS_ADDR=127.0.0.1:20501\n")
	sd.reset()

	require.NoError(t, m.Prune(t.Context(), nil))

	require.ElementsMatch(t, []string{"DisableNow " + unitA, "DisableNow " + unitB, "DisableNow " + unitC}, sd.changes())
	require.Empty(t, listDir(t, dir))
}

func TestStatusSaysWhichInstallAConnectorIsOf(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	connectorOf(t, sd, dir, idB, "METRICS_ADDR=127.0.0.1:1\nPCO_INSTALL="+otherInstall+"\n")
	connectorOf(t, sd, dir, idC, "METRICS_ADDR=127.0.0.1:1\n")
	delete(sd.active, unitA)
	delete(sd.active, unitB)
	delete(sd.active, unitC)

	for id, want := range map[string]string{idA: testInstall, idB: otherInstall, idC: "", idD: ""} {
		st, err := m.Status(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, want, st.Install, id)
	}
}

func TestListNamesEveryConnectorOnTheNode(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	connectorOf(t, sd, dir, idB, "METRICS_ADDR=127.0.0.1:20500\nPCO_INSTALL="+otherInstall+"\n")
	sd.active[UnitName(idC)] = true
	writeFile(t, dir, pendingOf(idD), "")
	writeFile(t, dir, "not-a-tunnel.env", "")

	ids, err := m.List(t.Context())

	require.NoError(t, err, "a name that is no tunnel is prune's to report")
	require.Equal(t, []string{idA, idB, idC, idD}, ids)
}

func TestListSaysWhenTheUnitsCannotBeListed(t *testing.T) {
	m, sd, _ := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	sd.fail["ListUnits "+unitGlob] = errBoom

	ids, err := m.List(t.Context())

	require.ErrorIs(t, err, errBoom)
	require.Equal(t, []string{idA}, ids, "what the files show is there all the same")
}

func TestAStaleTemporaryFileSweepThatCannotListTheDirectorySaysSo(t *testing.T) {
	sd := newFakeSystemd()
	dir := filepath.Join(t.TempDir(), "tunnels")
	var logged bytes.Buffer
	m := NewManager(sd, dir, nil, zerolog.New(&logged))
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	m.readDir = func(string) ([]os.DirEntry, error) { return nil, errBoom }

	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	require.ErrorContains(t, m.PruneInstall(t.Context(), testInstall, []string{idA}), "listing "+dir+" for stale temporary files: boom")

	require.Contains(t, logged.String(), `"message":"could not list the directory of the connectors for stale temporary files"`)
	require.NotContains(t, logged.String(), "could not remove a stale temporary file")
}
