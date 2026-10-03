package connector

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A unit with neither an env file nor a token file cannot serve a tunnel and
// names no install: it is nobody's, and a prune of the install clears it.
func TestPruneInstallStopsAUnitThatHasNoEnvFileAndNoTokenFile(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	sd.active[UnitName(idE)] = true
	sd.reset()

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, []string{idA}))

	require.Equal(t, []string{"DisableNow " + UnitName(idE), "ResetFailed " + UnitName(idE)}, sd.changes())
	require.NotContains(t, sd.active, UnitName(idE))
	require.ElementsMatch(t, []string{idA + ".env", idA + ".token", idA + ".yml"}, listDir(t, dir))
}

func TestPruneInstallResetsAFailedUnitThatIsNobodys(t *testing.T) {
	m, sd, _ := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	sd.failed[UnitName(idE)] = true
	sd.reset()

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, []string{idA}))

	require.Equal(t, []string{"DisableNow " + UnitName(idE), "ResetFailed " + UnitName(idE)}, sd.changes())
	require.Empty(t, sd.failed, "a stop leaves a failed unit loaded: only the reset clears it")
	ids, err := m.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{idA}, ids)
}

func TestAUnitThatIsNobodysLeavesItsStrayFilesToo(t *testing.T) {
	m, sd, dir := newTestManager(t)
	writeFile(t, dir, idE+".yml", configContent)
	writeFile(t, dir, pendingOf(idE), "")
	sd.active[UnitName(idE)] = true
	sd.reset()

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, nil))

	require.Equal(t, []string{"DisableNow " + UnitName(idE), "ResetFailed " + UnitName(idE)}, sd.changes())
	require.Empty(t, listDir(t, dir))
}

func TestAConnectorWithEitherFileIsNotNobodys(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
	}{
		{"a token file only", map[string]string{idE + ".token": "token-e"}},
		{"an env file that names no install", map[string]string{idE + ".env": "METRICS_ADDR=127.0.0.1:20450\n"}},
		{"an env file of another install", map[string]string{idE + ".env": "METRICS_ADDR=127.0.0.1:20450\nPCO_INSTALL=" + otherInstall + "\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, sd, dir := newTestManager(t)
			for name, content := range tc.files {
				writeFile(t, dir, name, content)
			}
			sd.active[UnitName(idE)] = true
			sd.reset()

			require.NoError(t, m.PruneInstall(t.Context(), testInstall, nil))

			require.Empty(t, sd.changes())
			require.Len(t, listDir(t, dir), len(tc.files))
		})
	}
}

func TestAUnitThatIsNobodysIsLeftAloneWhenItsTunnelIsKept(t *testing.T) {
	m, sd, _ := newTestManager(t)
	sd.active[UnitName(idE)] = true
	sd.reset()

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, []string{idE}))

	require.Empty(t, sd.changes())
}

func TestAUnitThatIsNobodysIsSaidInTheLog(t *testing.T) {
	sd := newFakeSystemd()
	var logged bytes.Buffer
	m := NewManager(sd, filepath.Join(t.TempDir(), "tunnels"), nil, zerolog.New(&logged))
	sd.active[UnitName(idE)] = true

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, nil))

	require.Contains(t, logged.String(), `"level":"warn"`)
	require.Contains(t, logged.String(), `"tunnel":"`+idE+`"`)
	require.Contains(t, logged.String(), "no env file and no token file")
}

func TestPruneInstallSaysWhenAUnitThatIsNobodysCannotBeReset(t *testing.T) {
	m, sd, _ := newTestManager(t)
	sd.failed[UnitName(idE)] = true
	sd.fail["ResetFailed "+UnitName(idE)] = errBoom

	require.ErrorIs(t, m.PruneInstall(t.Context(), testInstall, nil), errBoom)
}

func TestPruneRemovesEveryUnitWithoutResettingAny(t *testing.T) {
	m, sd, _ := newTestManager(t)
	sd.active[UnitName(idE)] = true
	sd.reset()

	require.NoError(t, m.Prune(t.Context(), nil))

	require.Equal(t, []string{"DisableNow " + UnitName(idE)}, sd.changes())
}

func TestASystemdWithoutAResetStillStopsAUnitThatIsNobodys(t *testing.T) {
	sd := &plainSystemd{newFakeSystemd()}
	m := NewManager(sd, filepath.Join(t.TempDir(), "tunnels"), nil, zerolog.Nop())
	sd.active[UnitName(idE)] = true

	require.NoError(t, m.PruneInstall(t.Context(), testInstall, nil))

	require.Equal(t, []string{"DisableNow " + UnitName(idE)}, sd.changes())
}

// plainSystemd hides the reset of its fake, as a Systemd without one does.
type plainSystemd struct{ *fakeSystemd }

func (plainSystemd) ResetFailed() {}
