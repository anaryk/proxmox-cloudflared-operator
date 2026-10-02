package connector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A removal that stops half way leaves the env file, which names the install,
// so that the next prune still knows the rest is of this install.
func TestARemovalThatStopsHalfWayKeepsTheEnvFile(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), testInstall, idA, "token-a"))
	token := filepath.Join(dir, idA+".token")
	require.NoError(t, os.Remove(token))
	// A token that cannot be removed: a directory with something in it.
	require.NoError(t, os.MkdirAll(filepath.Join(token, "x"), 0o700))
	sd.reset()

	require.ErrorContains(t, m.PruneInstall(t.Context(), testInstall, nil), idA+".token")
	require.FileExists(t, filepath.Join(dir, idA+".env"), "the env file still says whose the rest is")

	require.NoError(t, os.RemoveAll(token))
	require.NoError(t, m.PruneInstall(t.Context(), testInstall, nil))
	require.Empty(t, listDir(t, dir))
	require.Equal(t, []string{"DisableNow " + unitA, "DisableNow " + unitA}, sd.changes())
}
