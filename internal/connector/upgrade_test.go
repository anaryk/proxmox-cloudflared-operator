package connector

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// An upgrade finds the env files of every connector without the install. Only
// pco reads that line, so none of them is worth a restart, which would drop
// the connections of every tunnel at once.
func TestAnUpgradeRewritesTheEnvFilesWithoutTheInstallAndRestartsNone(t *testing.T) {
	m, sd, dir := newTestManager(t)
	ports := map[string]string{idA: "20450", idB: "20451", idC: "20452"}
	for id, port := range ports {
		connectorOf(t, sd, dir, id, "METRICS_ADDR=127.0.0.1:"+port+"\nEDGE_IP_VERSION=auto\n")
	}
	connectorOf(t, sd, dir, idD, "METRICS_ADDR=127.0.0.1:20453\nEDGE_IP_VERSION=auto\nPCO_INSTALL="+otherInstall+"\n")
	ports[idD] = "20453"
	for id := range ports {
		sd.fail["Restart "+UnitName(id)] = errBoom // a restart would be an error
	}
	sd.reset()

	for id := range ports {
		require.NoError(t, m.Ensure(t.Context(), testInstall, id, "token-"+id))
	}

	require.Empty(t, sd.changes(), "no connector restarts")
	for id, port := range ports {
		require.Equal(t, "METRICS_ADDR=127.0.0.1:"+port+"\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n", readFile(t, dir, id+".env"), "the port stays")
		require.Equal(t, envMode, fileMode(t, filepath.Join(dir, id+".env")))
		require.NoFileExists(t, filepath.Join(dir, pendingOf(id)))
	}
	require.ElementsMatch(t, []string{
		idA + ".env", idA + ".token", idA + ".yml",
		idB + ".env", idB + ".token", idB + ".yml",
		idC + ".env", idC + ".token", idC + ".yml",
		idD + ".env", idD + ".token", idD + ".yml",
	}, listDir(t, dir), "no marker and no temporary file is left")
}

func TestAMetricsOrEdgeChangeStillRestartsThroughTheMarker(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"no edge version", "METRICS_ADDR=127.0.0.1:20450\nPCO_INSTALL=abc123\n",
			"METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n"},
		{"another edge version", "METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=6\nPCO_INSTALL=abc123\n",
			"METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n"},
		{"a metrics address that is not the loopback", "METRICS_ADDR=0.0.0.0:20450\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n",
			"METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n"},
		{"no metrics address", "EDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n",
			"METRICS_ADDR=127.0.0.1:20300\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n"},
		{"neither the edge version nor the install", "METRICS_ADDR=127.0.0.1:20450\n",
			"METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=auto\nPCO_INSTALL=abc123\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, sd, dir := newTestManager(t)
			connectorOf(t, sd, dir, idA, tc.env)
			sd.fail["Restart "+unitA] = errBoom
			sd.reset()

			require.ErrorIs(t, m.Ensure(t.Context(), testInstall, idA, "token-"+idA), errBoom)

			require.Equal(t, []string{"Restart " + unitA}, sd.changes())
			require.Equal(t, tc.want, readFile(t, dir, idA+".env"))
			require.FileExists(t, filepath.Join(dir, pendingOf(idA)), "the marker stays until the restart is queued")

			delete(sd.fail, "Restart "+unitA)
			sd.reset()
			require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-"+idA))
			require.Equal(t, []string{"Restart " + unitA}, sd.changes(), "the marker restarts it once")
			require.NoFileExists(t, filepath.Join(dir, pendingOf(idA)))
		})
	}
}

func TestAnInstallChangeBesideATokenChangeIsOneRestart(t *testing.T) {
	m, sd, dir := newTestManager(t)
	connectorOf(t, sd, dir, idA, "METRICS_ADDR=127.0.0.1:20450\nEDGE_IP_VERSION=auto\n")
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "a-new-token"))

	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.Equal(t, "a-new-token", readFile(t, dir, idA+".token"))
	require.Contains(t, readFile(t, dir, idA+".env"), "PCO_INSTALL=abc123\n")
}
