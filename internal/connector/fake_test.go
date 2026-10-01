package connector

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

const (
	idA = "11111111-1111-4111-8111-111111111111"
	idB = "22222222-2222-4222-8222-222222222222"
	idC = "33333333-3333-4333-8333-333333333333"
)

// fakeSystemd is a Systemd that records its calls and keeps the running state
// the real one would. Tests change its fields between calls, never during one.
type fakeSystemd struct {
	calls  []string
	active map[string]bool
	loaded []string         // what ListUnits answers, besides the units that run
	fail   map[string]error // by "Method unit"
}

func newFakeSystemd() *fakeSystemd {
	return &fakeSystemd{active: map[string]bool{}, fail: map[string]error{}}
}

func (f *fakeSystemd) record(method, unit string) error {
	call := method + " " + unit
	f.calls = append(f.calls, call)
	return f.fail[call]
}

func (f *fakeSystemd) EnableNow(_ context.Context, unit string) error {
	if err := f.record("EnableNow", unit); err != nil {
		return err
	}
	f.active[unit] = true
	return nil
}

func (f *fakeSystemd) DisableNow(_ context.Context, unit string) error {
	if err := f.record("DisableNow", unit); err != nil {
		return err
	}
	delete(f.active, unit)
	f.loaded = slices.DeleteFunc(f.loaded, func(u string) bool { return u == unit })
	return nil
}

func (f *fakeSystemd) Restart(_ context.Context, unit string) error {
	if err := f.record("Restart", unit); err != nil {
		return err
	}
	f.active[unit] = true
	return nil
}

func (f *fakeSystemd) IsActive(_ context.Context, unit string) (bool, error) {
	if err := f.record("IsActive", unit); err != nil {
		return false, err
	}
	return f.active[unit], nil
}

func (f *fakeSystemd) ListUnits(_ context.Context, pattern string) ([]string, error) {
	if err := f.record("ListUnits", pattern); err != nil {
		return nil, err
	}
	units := slices.Clone(f.loaded)
	for u := range f.active {
		if !slices.Contains(units, u) {
			units = append(units, u)
		}
	}
	slices.Sort(units)
	return units, nil
}

// changes are the calls that act on a unit; asking about one is not a change.
func (f *fakeSystemd) changes() []string {
	var out []string
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "IsActive ") && !strings.HasPrefix(c, "ListUnits ") {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSystemd) reset() { f.calls = nil }

// newTestManager returns a manager over a directory that does not exist yet,
// so that tests also see it being created.
func newTestManager(t *testing.T) (*Manager, *fakeSystemd, string) {
	t.Helper()
	sd := newFakeSystemd()
	dir := filepath.Join(t.TempDir(), "tunnels")
	return NewManager(sd, dir, nil, zerolog.Nop()), sd, dir
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(b)
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Mode().Perm()
}
