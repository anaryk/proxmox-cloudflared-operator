package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestWriteMakesAndReplacesAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	require.NoError(t, Write(path, []byte("one"), Options{}))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "one", string(got))

	require.NoError(t, Write(path, []byte("two"), Options{}))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "two", string(got))
	require.Equal(t, []string{"state.json"}, names(t, dir), "no temporary file is left behind")
}

func TestWriteGivesTheFileItsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")

	require.NoError(t, Write(path, []byte("x"), Options{Mode: 0o644}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o644), info.Mode().Perm())
}

// Without a mode the file keeps the 0600 of its temporary file and nothing
// chmods it, as the cluster filesystem refuses.
func TestWriteWithoutAModeLeavesTheModeAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	require.NoError(t, Write(path, []byte("x"), Options{}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
}

func TestWriteOfAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")

	require.NoError(t, Write(path, nil, Options{Mode: 0o600}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Zero(t, info.Size())
}

func TestAWriteThatFailsLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "taken")
	require.NoError(t, os.MkdirAll(filepath.Join(path, "inside"), 0o700), "a directory is where the file should go")

	require.Error(t, Write(path, []byte("x"), Options{Mode: 0o600}))

	require.Equal(t, []string{"taken"}, names(t, dir), "the temporary file is removed again")
}

func TestWriteIntoAMissingDirectoryFails(t *testing.T) {
	err := Write(filepath.Join(t.TempDir(), "nope", "state.json"), []byte("x"), Options{})

	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestWritersOfOneFileDoNotMeetInATemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() { require.NoError(t, Write(path, []byte(strconv.Itoa(i)), Options{})) })
	}
	wg.Wait()

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	n, err := strconv.Atoi(string(got))
	require.NoError(t, err, "a whole write, not a mix of two: %q", got)
	require.Less(t, n, 16)
	require.Equal(t, []string{"state.json"}, names(t, dir))
}
