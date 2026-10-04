package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An idle cycle proves the address again and writes no file of a binding: the
// file is written once the proof moved on by more than a quarter of its age,
// and the cycles in between get the time it was proven at.
func TestAnIdleCycleWritesNoBinding(t *testing.T) {
	e := published(t)
	path := filepath.Join(e.paths.Local, "bindings", "www.example.com.json")
	// The store replaces a file through a new one.
	file := func() os.FileInfo {
		t.Helper()
		info, err := os.Stat(path)
		require.NoError(t, err)
		return info
	}
	first := file()
	proven := e.clock.now()

	for i := 1; i <= 7; i++ {
		e.clock.advance(10 * time.Second)
		e.cycle()
		require.True(t, os.SameFile(first, file()), "cycle %d", i)
		bindings, err := e.store.Bindings()
		require.NoError(t, err)
		require.Equal(t, proven.Add(time.Duration(i)*10*time.Second), bindings["www.example.com"].VerifiedAt.UTC())
	}

	e.clock.advance(10 * time.Second)
	e.cycle()

	require.False(t, os.SameFile(first, file()), "80s after it was written")
}
