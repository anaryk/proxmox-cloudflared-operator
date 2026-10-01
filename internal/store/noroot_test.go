package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func TestErrNoRootSaysWhatIsWrong(t *testing.T) {
	require.EqualError(t, ErrNoRoot, "store root is missing")
}

func TestAStoreThatWasNeverInitialisedIsErrNoRoot(t *testing.T) {
	p, _ := mountedPaths(t)
	s, err := Open(p)
	require.NoError(t, err)

	_, found, err := s.Writer()
	require.ErrorIs(t, err, ErrNoRoot, "the answer for \"run pco setup\"")
	require.NotErrorIs(t, err, ErrNotMounted)
	require.False(t, found)
	require.Contains(t, err.Error(), p.Cluster)
	_, err = s.Credentials()
	require.ErrorIs(t, err, ErrNoRoot)
	require.Contains(t, err.Error(), p.Private)
	require.ErrorIs(t, s.SaveInstall(Install{ID: "abc", CreatedAt: t0}), ErrNoRoot)

	require.NoError(t, s.Init())
	_, found, err = s.Writer()
	require.NoError(t, err)
	require.False(t, found, "a store that is set up has no writer yet, which is not the same")
}

func TestAMissingRootWhileMountedIsErrNoRootAndNotErrNotMounted(t *testing.T) {
	p, marker := mountedPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 1, Nonce: "aa"}))
	require.NoError(t, os.RemoveAll(p.Cluster))

	_, _, err = s.Writer()
	require.ErrorIs(t, err, ErrNoRoot)
	require.NotErrorIs(t, err, ErrNotMounted)
	require.ErrorIs(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 2, Nonce: "bb"}), ErrNoRoot)
	require.ErrorIs(t, s.DeleteNode("pve1"), ErrNoRoot)
	require.ErrorIs(t, s.AppendAdopted(t0, "example.com", adoptedSample(0)), ErrNoRoot)
	_, err = s.Claims()
	require.ErrorIs(t, err, ErrNoRoot)
	require.ErrorIs(t, s.SaveClaims(nil), ErrNoRoot)

	// The private root is still there: only the cluster root is reported.
	creds, err := s.Credentials()
	require.NoError(t, err)
	require.Empty(t, creds)

	// A filesystem that is not mounted is the first thing to be said.
	require.NoError(t, os.Remove(marker))
	_, _, err = s.Writer()
	require.ErrorIs(t, err, ErrNotMounted)
	require.NotErrorIs(t, err, ErrNoRoot)
}

// failsAfter returns a mount check that passes n times and then fails, as the
// cluster filesystem does when it goes away in the middle of Init.
func failsAfter(n int) func() error {
	calls := 0
	return func() error {
		calls++
		if calls > n {
			return fmt.Errorf("%w: gone", ErrNotMounted)
		}
		return nil
	}
}

func TestInitRemovesTheRootItMadeWhenTheMountIsLost(t *testing.T) {
	p := testPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	check := failsAfter(1) // passes before the first directory is made
	s.cluster.guard = check
	s.private.guard = check

	err = s.Init()
	require.ErrorIs(t, err, ErrNotMounted)
	requireMissing(t, p.Cluster)
	requireMissing(t, p.Private)
}

func TestInitKeepsTheRootsThatWereThereBefore(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "install.json"), "kept")
	s, err := Open(p)
	require.NoError(t, err)
	check := failsAfter(1) // passes before the private root is made, fails after it
	s.cluster.guard = check
	s.private.guard = check

	err = s.Init()
	require.ErrorIs(t, err, ErrNotMounted)
	requireMissing(t, p.Private)
	b, err := os.ReadFile(filepath.Join(p.Cluster, "meta", "install.json"))
	require.NoError(t, err)
	require.Equal(t, "kept", string(b), "a root that was there is not ours to remove")
}

func TestInitChecksTheMountAfterEachRootItMakes(t *testing.T) {
	p := testPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	check := failsAfter(2) // passes before the cluster root and after it, fails after the private one
	s.cluster.guard = check
	s.private.guard = check

	err = s.Init()
	require.ErrorIs(t, err, ErrNotMounted)
	requireMissing(t, p.Private)
	_, statErr := os.Stat(p.Cluster)
	require.NoError(t, statErr, "the cluster root was made while the filesystem was mounted")
}
