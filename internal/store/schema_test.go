package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTheSchemaVersionIsTheOneThisBuildWrites(t *testing.T) {
	p := testPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}))

	version, path, err := NewestSchema(p)

	require.NoError(t, err)
	require.Equal(t, SchemaVersion(), version)
	require.NotEmpty(t, path)
}

func TestTheNewestSchemaIsFoundInAnyRootAndKind(t *testing.T) {
	p := testPaths(t)
	write := func(path, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	write(filepath.Join(p.Cluster, "routes", "a.json"), `{"schemaVersion":1,"rev":1,"id":"a","data":{}}`)
	write(filepath.Join(p.Private, "credentials", "c.json"), `{"schemaVersion":1,"rev":1,"id":"c","data":{}}`)
	newer := filepath.Join(p.Local, "kind-of-a-later-version", "x.json")
	write(newer, `{"schemaVersion":3,"rev":1,"id":"x","data":{}}`)
	write(filepath.Join(p.Cluster, "routes", "b.json"), `{"schemaVersion":2,"rev":1,"id":"b","data":{}}`)
	// Neither is an object of the store: a temporary file, a hidden one, one
	// at the root, one that is not JSON, and one deeper down.
	write(filepath.Join(p.Cluster, "routes", ".b.json.123.tmp"), `{"schemaVersion":9}`)
	write(filepath.Join(p.Cluster, "routes", ".hidden.json"), `{"schemaVersion":9}`)
	write(filepath.Join(p.Local, "manifest.json"), `{"schemaVersion":9}`)
	write(filepath.Join(p.Cluster, "routes", "c.json"), `not json`)
	write(filepath.Join(p.Local, "upgrades", "previous", "x.json"), `{"schemaVersion":9}`)

	version, path, err := NewestSchema(p)

	require.NoError(t, err)
	require.Equal(t, 3, version)
	require.Equal(t, newer, path)
}

func TestAStoreWithoutObjectsHasNoSchema(t *testing.T) {
	base := t.TempDir()
	p := Paths{Cluster: filepath.Join(base, "missing"), Private: filepath.Join(base, "gone"), Local: base}

	version, path, err := NewestSchema(p)

	require.NoError(t, err)
	require.Zero(t, version)
	require.Empty(t, path)
}

func TestAKindThatCannotBeReadIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads the directory whatever its mode")
	}
	p := testPaths(t)
	dir := filepath.Join(p.Cluster, "routes")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, _, err := NewestSchema(p)

	require.ErrorContains(t, err, dir)
}
