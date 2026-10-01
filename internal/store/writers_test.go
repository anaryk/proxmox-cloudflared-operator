package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// failures collects errors from goroutines, which cannot call require.
type failures struct {
	mu   sync.Mutex
	errs []error
}

func (f *failures) add(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) < 5 {
		f.errs = append(f.errs, err)
	}
}

func TestTwoDirsWritingOneObjectNeverFail(t *testing.T) {
	root := t.TempDir()
	dirs := []Dir{NewDir(root), NewDir(root)} // two processes on one root
	payload := strings.Repeat("x", 4000)
	const writers, puts = 4, 100

	var fail failures
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 3 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				var got sample
				found, err := dirs[0].Get("things", "a", &got)
				if err != nil {
					fail.add(err)
					return
				}
				if found && (got.Name != payload || got.N < 0 || got.N >= 2*writers*puts) {
					fail.add(os.ErrInvalid)
					return
				}
			}
		})
	}
	var wg sync.WaitGroup
	for d := range dirs {
		for w := range writers {
			wg.Go(func() {
				for i := range puts {
					n := (d*writers+w)*puts + i
					if err := dirs[d].Put("things", "a", sample{Name: payload, N: n}); err != nil {
						fail.add(err)
						return
					}
				}
			})
		}
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	require.Empty(t, fail.errs)
	var got sample
	found, err := dirs[1].Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, payload, got.Name)
	entries, err := os.ReadDir(filepath.Join(root, "things"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file is left behind")
}

func TestAWriteLeavesTheTempFileOfAnotherWriterAlone(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	dir := filepath.Join(root, "things")
	stale := filepath.Join(dir, ".a.json.1234.tmp")
	writeFile(t, stale, "left by another writer")

	require.NoError(t, d.Put("things", "a", sample{N: 1}))
	b, err := os.ReadFile(stale)
	require.NoError(t, err)
	require.Equal(t, "left by another writer", string(b), "a write does not touch the temporary file of another")

	ids, err := d.List("things")
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, ids)
}

func TestOpenSweepsOnlyOldTempFiles(t *testing.T) {
	p := testPaths(t)
	now := t0
	old := now.Add(-11 * time.Minute)
	young := now.Add(-9 * time.Minute)
	oldFiles := []string{
		filepath.Join(p.Cluster, "claims", ".a.json.1.tmp"),
		filepath.Join(p.Cluster, ".adopted.jsonl.2.tmp"),
		filepath.Join(p.Private, "credentials", ".c.json.3.tmp"),
		filepath.Join(p.Local, "bindings", ".b.json.4.tmp"),
		filepath.Join(p.Local, ".probe-5.tmp"),
	}
	youngFiles := []string{
		filepath.Join(p.Cluster, "claims", ".a.json.6.tmp"),
		filepath.Join(p.Cluster, ".adopted.jsonl.7.tmp"),
		filepath.Join(p.Private, "credentials", ".c.json.8.tmp"),
		filepath.Join(p.Local, "bindings", ".b.json.9.tmp"),
	}
	kept := []string{
		filepath.Join(p.Cluster, "claims", "a.json"),
		filepath.Join(p.Cluster, "adopted.jsonl"),
		filepath.Join(p.Local, "bindings", "b.json"),
	}
	for _, f := range oldFiles {
		writeFile(t, f, "x")
		require.NoError(t, os.Chtimes(f, old, old))
	}
	for _, f := range youngFiles {
		writeFile(t, f, "x")
		require.NoError(t, os.Chtimes(f, young, young))
	}
	for _, f := range kept {
		writeFile(t, f, "x")
		require.NoError(t, os.Chtimes(f, old, old))
	}

	_, err := open(p, func() time.Time { return now })
	require.NoError(t, err)

	for _, f := range oldFiles {
		requireMissing(t, f)
	}
	for _, f := range append(youngFiles, kept...) {
		_, err := os.Stat(f)
		require.NoError(t, err, f)
	}
}

func TestOpenNeverTouchesTheTunnelDirectory(t *testing.T) {
	p := testPaths(t)
	tunnels := filepath.Join(p.Local, "tunnels")
	files := []string{
		filepath.Join(tunnels, ".00000000-0000-0000-0000-000000000000.token.1.tmp"),
		filepath.Join(tunnels, "00000000-0000-0000-0000-000000000000.token"),
		filepath.Join(tunnels, ".00000000-0000-0000-0000-000000000000.pending"),
	}
	old := t0.Add(-time.Hour)
	for _, f := range files {
		writeFile(t, f, "x")
		require.NoError(t, os.Chtimes(f, old, old))
	}
	before, err := os.Stat(tunnels)
	require.NoError(t, err)

	_, err = open(p, func() time.Time { return t0 })
	require.NoError(t, err)
	for _, f := range files {
		_, err := os.Stat(f)
		require.NoError(t, err, "%s is the connector manager's", f)
	}
	after, err := os.Stat(tunnels)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime())
}

func TestOpenWithTheRealClockKeepsAFreshTempFile(t *testing.T) {
	p := testPaths(t)
	fresh := filepath.Join(p.Local, "bindings", ".b.json.1.tmp")
	writeFile(t, fresh, "x")
	_, err := Open(p)
	require.NoError(t, err)
	_, err = os.Stat(fresh)
	require.NoError(t, err, "a writer may be using it")
}

func TestFailedWriteLeavesNoTempFileOfItsOwn(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	dir := filepath.Join(root, "things")
	// A non-empty directory in the way makes the final rename fail.
	writeFile(t, filepath.Join(dir, "a.json", "inner"), "x")

	require.Error(t, d.Put("things", "a", sample{}))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasSuffix(e.Name(), ".tmp"), e.Name())
	}
}
