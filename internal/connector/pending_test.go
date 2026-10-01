package connector

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestEnsureRestartsForARotationWhoseFirstAttemptFailed(t *testing.T) {
	tests := []struct {
		name      string
		fail      string // the call that fails in the second attempt
		removable bool   // whether the marker can be removed by the third
	}{
		{"restart fails, marker stuck", "Restart", false},
		{"restart fails, marker cleared meanwhile", "Restart", true},
		{"activity check fails, marker stuck", "IsActive", false},
		{"activity check fails, marker cleared meanwhile", "IsActive", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, sd, dir := newTestManager(t)
			marker := filepath.Join(dir, pendingOf(idA))
			require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))
			undeletable(t, dir)
			sd.reset()

			// The first rotation queues its restart, and the marker stays.
			require.Error(t, m.Ensure(t.Context(), idA, "token-1"))
			require.Equal(t, []string{"Restart " + unitA}, sd.changes())

			// The second replaces the token and then fails before its restart.
			sd.fail[tc.fail+" "+unitA] = errBoom
			require.ErrorIs(t, m.Ensure(t.Context(), idA, "token-2"), errBoom)
			require.Equal(t, "token-2", readFile(t, dir, idA+".token"))
			delete(sd.fail, tc.fail+" "+unitA)
			if tc.removable {
				require.NoError(t, os.Remove(filepath.Join(marker, "x")))
			}
			sd.reset()

			// The third replaces nothing, and the unit must not stay on token-1.
			err := m.Ensure(t.Context(), idA, "token-2")
			require.Equal(t, []string{"Restart " + unitA}, sd.changes())
			if tc.removable {
				require.NoError(t, err)
				require.NoFileExists(t, marker)
			} else {
				require.Error(t, err)
			}

			sd.reset()
			_ = m.Ensure(t.Context(), idA, "token-2")
			require.Empty(t, sd.changes(), "and it is restarted once")
		})
	}
}

func TestEnsureRestartsAfterAnEnvFileWasReplacedAndTheTokenWriteFailed(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))
	undeletable(t, dir)
	sd.reset()
	require.Error(t, m.Ensure(t.Context(), idA, "token-1"))
	require.Equal(t, []string{"Restart " + unitA}, sd.changes())

	// An env file that has to be replaced, and a token file that cannot be.
	writeFile(t, dir, idA+".env", "METRICS_ADDR=10.0.0.1:20300\n")
	token := filepath.Join(dir, idA+".token")
	require.NoError(t, os.Remove(token))
	require.NoError(t, os.MkdirAll(filepath.Join(token, "x"), 0o700))
	require.ErrorContains(t, m.Ensure(t.Context(), idA, "token-2"), "writing "+idA+".token")
	require.Equal(t, "METRICS_ADDR=127.0.0.1:20300\n", readFile(t, dir, idA+".env"), "the env file was replaced first")

	// The token is what it was, so that the next call replaces no file.
	require.NoError(t, os.RemoveAll(token))
	writeFile(t, dir, idA+".token", "token-1")
	sd.reset()
	require.Error(t, m.Ensure(t.Context(), idA, "token-1"))

	require.Equal(t, []string{"Restart " + unitA}, sd.changes(), "the new address was never applied")
}

func TestEnsureRestartsForAMarkerThatIsAnotherFile(t *testing.T) {
	m, sd, dir := newTestManager(t)
	marker := filepath.Join(dir, pendingOf(idA))
	require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))
	undeletable(t, dir)
	require.Error(t, m.Ensure(t.Context(), idA, "token-0"))

	// Another marker takes the place of the first at once, with no call in
	// between that could see it missing. The first is kept under another
	// name, so that its inode cannot be handed to the new file.
	require.NoError(t, os.Rename(marker, marker+".old"))
	writeFile(t, dir, pendingOf(idA), "")
	sd.reset()

	require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))

	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
	require.NoFileExists(t, marker)
}

func TestEnsureForgetsAMarkerOnceItIsGone(t *testing.T) {
	tests := []struct {
		name string
		gone func(t *testing.T, marker string)
	}{
		{"it is removed by someone else", func(t *testing.T, marker string) {
			require.NoError(t, os.RemoveAll(marker))
		}},
		{"it is cleared by the next call", func(t *testing.T, marker string) {
			require.NoError(t, os.Remove(filepath.Join(marker, "x"))) // now an empty directory
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, sd, dir := newTestManager(t)
			marker := filepath.Join(dir, pendingOf(idA))
			require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))
			undeletable(t, dir)
			seen, err := os.Lstat(marker)
			require.NoError(t, err)
			require.Error(t, m.Ensure(t.Context(), idA, "token-0")) // restarts, remembers, cannot clear
			sd.reset()

			tc.gone(t, marker)
			require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))
			require.Empty(t, sd.changes())

			// A new marker appears and a file system that reuses inode numbers
			// gives it the identity of the old one.
			writeFile(t, dir, pendingOf(idA), "")
			m.lstat = func(name string) (fs.FileInfo, error) {
				if name == marker {
					return seen, nil
				}
				return os.Lstat(name)
			}
			require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))

			require.Equal(t, []string{"Restart " + unitA}, sd.changes(), "the old marker is not remembered")
		})
	}
}

func TestPruneForgetsTheMarkerItRemembered(t *testing.T) {
	m, sd, dir := newTestManager(t)
	require.NoError(t, m.Ensure(t.Context(), idA, "token-0"))
	undeletable(t, dir)
	require.Error(t, m.Ensure(t.Context(), idA, "token-0"))
	require.Contains(t, m.queued, idA)

	require.Error(t, m.Prune(t.Context(), nil), "the marker is stuck")

	// The files are gone, so that any later Ensure writes them and acts at
	// once. What decides here is that nothing is kept for a tunnel that is
	// gone, which would otherwise stay in memory for good.
	require.NotContains(t, m.queued, idA)
	sd.active[unitA] = true
	sd.reset()
	require.Error(t, m.Ensure(t.Context(), idA, "token-0"))
	require.Equal(t, []string{"Restart " + unitA}, sd.changes())
}

func TestEnsureWarnsOnceForEachStaleFileItCannotRemove(t *testing.T) {
	sd := newFakeSystemd()
	dir := filepath.Join(t.TempDir(), "tunnels")
	var logged bytes.Buffer
	m := NewManager(sd, dir, nil, zerolog.New(&logged))
	warnings := func() int { return strings.Count(logged.String(), `"level":"warn"`) }
	stuck := func(name string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, name, "x"), 0o700)) // cannot be removed with os.Remove
	}
	ensure := func(id string) {
		t.Helper()
		require.NoError(t, m.Ensure(t.Context(), id, "token"))
	}
	stuck(".stuck.tmp")

	ensure(idA)
	ensure(idB)
	ensure(idA)
	require.Equal(t, 1, warnings(), "one warning for the file, not one per call")

	stuck(".other.tmp")
	ensure(idA)
	ensure(idA)
	require.Equal(t, 2, warnings(), "a second file is a second warning")
	require.Contains(t, logged.String(), ".other.tmp")

	require.NoError(t, os.RemoveAll(filepath.Join(dir, ".stuck.tmp")))
	ensure(idA)
	require.Equal(t, 2, warnings())
	stuck(".stuck.tmp")
	ensure(idA)
	require.Equal(t, 3, warnings(), "a file that came back is reported again")
}
