// Package testutil holds what the tests of more than one package need. Only
// tests import it.
package testutil

import (
	"bytes"
	"os"
	"sync"
	"testing"
)

// ShortDir returns a directory that is removed with the test and whose path is
// short enough for a unix socket in it, which t.TempDir is not on macOS.
func ShortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pco")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// SyncBuffer is a log destination that goroutines may write to.
type SyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *SyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what was written so far.
func (b *SyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
