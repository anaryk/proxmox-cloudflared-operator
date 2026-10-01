package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

type sample struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

// rawFile is a stored file as it is on disk.
type rawFile struct {
	SchemaVersion int             `json:"schemaVersion"`
	Rev           int64           `json:"rev"`
	Data          json.RawMessage `json:"data"`
}

func readRaw(t *testing.T, path string) rawFile {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var f rawFile
	require.NoError(t, json.Unmarshal(b, &f))
	return f
}

func revOf(t *testing.T, path string) int64 {
	t.Helper()
	return readRaw(t, path).Rev
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func requireMissing(t *testing.T, path string) {
	t.Helper()
	_, err := os.Lstat(path)
	require.ErrorIs(t, err, fs.ErrNotExist, path)
}

// skipAsRoot skips a test that relies on file permissions, which root ignores.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	require.NoError(t, d.Put("things", "a", sample{Name: "x", N: 1}))

	var got sample
	found, err := d.Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, sample{Name: "x", N: 1}, got)

	b, err := os.ReadFile(filepath.Join(root, "things", "a.json"))
	require.NoError(t, err)
	require.Contains(t, string(b), "\n  ", "the file is indented")
	f := readRaw(t, filepath.Join(root, "things", "a.json"))
	require.Equal(t, 1, f.SchemaVersion)
	require.EqualValues(t, 1, f.Rev)
	require.JSONEq(t, `{"name":"x","n":1}`, string(f.Data))
}

func TestGetMissing(t *testing.T) {
	d := NewDir(t.TempDir())
	var got sample
	found, err := d.Get("things", "nope", &got)
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, d.Put("things", "a", sample{}))
	found, err = d.Get("things", "nope", &got)
	require.NoError(t, err)
	require.False(t, found)
}

func TestRevIncrements(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")

	for want := int64(1); want <= 3; want++ {
		require.NoError(t, d.Put("things", "a", sample{N: int(want)}))
		require.Equal(t, want, revOf(t, path))
	}

	require.NoError(t, d.Delete("things", "a"))
	require.NoError(t, d.Put("things", "a", sample{}))
	require.EqualValues(t, 1, revOf(t, path), "a deleted object starts again")
}

func TestRevRestartsOverAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, `{"schemaVersion":1,"rev":7,"data":`)

	require.NoError(t, d.Put("things", "a", sample{N: 2}))
	require.EqualValues(t, 1, revOf(t, path))
}

func TestListSortedAndIgnoresJunk(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	for _, id := range []string{"b", "a/b", "a", "Z", "*.shop.cz"} {
		require.NoError(t, d.Put("things", id, sample{}))
	}
	dir := filepath.Join(root, "things")
	writeFile(t, filepath.Join(dir, ".hidden.json"), "{}")
	writeFile(t, filepath.Join(dir, "c.json.tmp"), "{}")
	writeFile(t, filepath.Join(dir, "d.tmp"), "{}")
	writeFile(t, filepath.Join(dir, "readme.txt"), "x")
	writeFile(t, filepath.Join(dir, "Upper.json"), "{}")
	writeFile(t, filepath.Join(dir, "sub.json", "inner.json"), "{}")

	ids, err := d.List("things")
	require.NoError(t, err)
	require.Equal(t, []string{"_wildcard.shop.cz", "a", "a_b", "b", "z"}, ids)
}

func TestListOfMissingKindIsEmpty(t *testing.T) {
	ids, err := NewDir(t.TempDir()).List("things")
	require.NoError(t, err)
	require.NotNil(t, ids)
	require.Empty(t, ids)
}

func TestDeleteMissingIsNotAnError(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	require.NoError(t, d.Delete("things", "a"))

	require.NoError(t, d.Put("things", "a", sample{}))
	require.NoError(t, d.Delete("things", "a"))
	requireMissing(t, filepath.Join(root, "things", "a.json"))
	require.NoError(t, d.Delete("things", "a"))
}

func TestGetOfInvalidJSONNamesTheFile(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, `{"schemaVersion":1,"rev":1,"data":`)

	var got sample
	found, err := d.Get("things", "a", &got)
	require.Error(t, err)
	require.False(t, found)
	require.NotErrorIs(t, err, fs.ErrNotExist)
	require.Contains(t, err.Error(), path)
}

func TestGetOfWrongShapeNamesTheFile(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, `{"schemaVersion":1,"rev":1,"data":{"name":5}}`)

	var got sample
	_, err := d.Get("things", "a", &got)
	require.Error(t, err)
	require.Contains(t, err.Error(), path)
	require.Contains(t, err.Error(), "name")
}

func TestGetErrorsDoNotQuoteTheContent(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	writeFile(t, filepath.Join(root, "things", "a.json"), `{"schemaVersion":1,"rev":1,"data":{"name":"s3cr3t-value" x}}`)
	writeFile(t, filepath.Join(root, "things", "b.json"), `{"schemaVersion":1,"rev":1,"data":{"n":"s3cr3t-value"}}`)

	for _, id := range []string{"a", "b"} {
		var got sample
		_, err := d.Get("things", id, &got)
		require.Error(t, err, id)
		require.NotContains(t, err.Error(), "s3cr3t", id)
	}
}

func TestGetRejectsAFutureSchemaVersion(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, `{"schemaVersion":2,"rev":4,"data":{"name":"x"}}`)

	var got sample
	found, err := d.Get("things", "a", &got)
	require.Error(t, err)
	require.False(t, found)
	require.Contains(t, err.Error(), path)
	require.Contains(t, err.Error(), "schema version 2")
}

func TestPutDoesNotOverwriteAFutureSchemaVersion(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	const future = `{"schemaVersion":2,"rev":4,"data":{"name":"x"}}`
	writeFile(t, path, future)

	err := d.Put("things", "a", sample{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "schema version 2")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, future, string(b))
	requireMissing(t, path+".tmp")
}

func TestGetRejectsAMissingSchemaVersion(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	writeFile(t, filepath.Join(root, "things", "a.json"), `{"name":"x"}`)

	var got sample
	_, err := d.Get("things", "a", &got)
	require.Error(t, err)
	require.Contains(t, err.Error(), "a.json")
}

func TestOversizedObjectIsRefused(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	require.NoError(t, d.Put("things", "a", sample{Name: "small"}))

	err := d.Put("things", "a", sample{Name: strings.Repeat("x", 1<<20)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "limit")

	var got sample
	found, err := d.Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "small", got.Name, "the old object stays")
	requireMissing(t, path+".tmp")

	err = d.Put("things", "fresh", sample{Name: strings.Repeat("x", 1<<20)})
	require.Error(t, err)
	requireMissing(t, filepath.Join(root, "things", "fresh.json"))
	requireMissing(t, filepath.Join(root, "things", "fresh.json.tmp"))
}

func TestStaleTempFileIsIgnoredAndOverwritten(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path+".tmp", "half a wri")

	ids, err := d.List("things")
	require.NoError(t, err)
	require.Empty(t, ids)

	require.NoError(t, d.Put("things", "a", sample{Name: "x"}))
	requireMissing(t, path+".tmp")
	var got sample
	found, err := d.Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "x", got.Name)
}

func TestFailedWriteRemovesItsTempFile(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	// A non-empty directory in the way makes the final rename fail.
	writeFile(t, filepath.Join(path, "inner"), "x")

	require.Error(t, d.Put("things", "a", sample{}))
	requireMissing(t, path+".tmp")
}

func TestWriteInAReadOnlyDirectoryFailsCleanly(t *testing.T) {
	skipAsRoot(t)
	root := t.TempDir()
	d := NewDir(root)
	require.NoError(t, d.Put("things", "a", sample{N: 1}))
	dir := filepath.Join(root, "things")
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := d.Put("things", "a", sample{N: 2})
	require.ErrorIs(t, err, fs.ErrPermission)
	require.ErrorIs(t, d.Delete("things", "a"), fs.ErrPermission)
	var got sample
	found, err := d.Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 1, got.N)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestPutCreatesDirectoriesPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	d := NewDir(root)
	require.NoError(t, d.Put("things", "a", sample{}))

	info, err := os.Stat(filepath.Join(root, "things"))
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o700), info.Mode().Perm())
}

func TestPutNeverChangesTheModeOfAnExistingDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "things")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))

	require.NoError(t, NewDir(root).Put("things", "a", sample{}))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0o755), info.Mode().Perm())
}

func TestConcurrentPutsStayConsistent(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	const writers = 16

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Go(func() { errs <- d.Put("things", "a", sample{N: i}) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.EqualValues(t, writers, revOf(t, filepath.Join(root, "things", "a.json")))
	var got sample
	found, err := d.Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
}

func TestFileNameIsSafe(t *testing.T) {
	valid := []struct{ name, id, want string }{
		{"plain", "shop.cz", "shop.cz"},
		{"upper case", "Shop.CZ", "shop.cz"},
		{"wildcard", "*.shop.cz", "_wildcard.shop.cz"},
		{"wildcard upper case", "*.Shop.CZ", "_wildcard.shop.cz"},
		{"owner", "qemu/101", "qemu_101"},
		{"slash", "a/b", "a_b"},
		{"leading underscore", "_x", "_x"},
		{"hyphen and digits", "pve-1", "pve-1"},
		{"longest", strings.Repeat("a", 201), strings.Repeat("a", 201)},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := FileName(tc.id)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}

	invalid := []struct{ name, id string }{
		{"empty", ""},
		{"dot dot", ".."},
		{"dot dot slash", "../x"},
		{"dot dot inside", "a..b"},
		{"dot dot after slash", "x/.."},
		{"single dot", "."},
		{"hidden", ".hidden"},
		{"leading hyphen", "-x"},
		{"space", "a b"},
		{"bare star", "*"},
		{"star inside", "a*b"},
		{"star without dot", "*shop.cz"},
		{"backslash", `a\b`},
		{"nul", "a\x00b"},
		{"newline", "a\nb"},
		{"non ascii", "café.cz"},
		{"kelvin sign", "Key"},
		{"too long", strings.Repeat("a", 202)},
		{"300 characters", strings.Repeat("a", 300)},
	}
	for _, tc := range invalid {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			got, err := FileName(tc.id)
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}

func TestFileNameKeepsAWildcardApartFromItsTwin(t *testing.T) {
	wild, err := FileName("*.shop.cz")
	require.NoError(t, err)
	plain, err := FileName("shop.cz")
	require.NoError(t, err)
	require.NotEqual(t, wild, plain)
}

func TestFileNameIsIdempotent(t *testing.T) {
	for _, id := range []string{"*.Shop.CZ", "qemu/101", "A.b"} {
		once, err := FileName(id)
		require.NoError(t, err)
		twice, err := FileName(once)
		require.NoError(t, err)
		require.Equal(t, once, twice, id)
	}
}

func TestDirRefusesUnsafeKindsAndIDs(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	d := NewDir(root)

	require.Error(t, d.Put("..", "a", sample{}))
	require.Error(t, d.Put("things", "../a", sample{}))
	require.Error(t, d.Put("", "a", sample{}))
	require.Error(t, d.Put("things", "", sample{}))
	var got sample
	_, err := d.Get("../x", "a", &got)
	require.Error(t, err)
	_, err = d.Get("things", "..", &got)
	require.Error(t, err)
	_, err = d.List("../x")
	require.Error(t, err)
	require.Error(t, d.Delete("things", "../a"))
	require.Error(t, d.Delete("../x", "a"))

	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	for _, e := range entries {
		require.Equal(t, "root", e.Name(), "nothing is created next to the root")
	}
	requireMissing(t, root)
}

func TestZeroDirIsRefused(t *testing.T) {
	var d Dir
	require.Error(t, d.Put("things", "a", sample{}))
	var got sample
	_, err := d.Get("things", "a", &got)
	require.Error(t, err)
	_, err = d.List("things")
	require.Error(t, err)
	require.Error(t, d.Delete("things", "a"))
}

func TestPutRefusesAValueThatCannotBeEncoded(t *testing.T) {
	root := t.TempDir()
	require.Error(t, NewDir(root).Put("things", "a", make(chan int)))
	requireMissing(t, filepath.Join(root, "things", "a.json"))
}

func TestPutIfChangedLeavesAnUnchangedFileAlone(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	require.NoError(t, d.put("things", "a", sample{N: 1}, true))
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, d.put("things", "a", sample{N: 1}, true))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after), "the file was not replaced")
	require.EqualValues(t, 1, revOf(t, path))

	require.NoError(t, d.put("things", "a", sample{N: 2}, true))
	require.EqualValues(t, 2, revOf(t, path))

	// Put itself always writes.
	require.NoError(t, d.Put("things", "a", sample{N: 2}))
	require.EqualValues(t, 3, revOf(t, path))
}

func TestPutIfChangedRewritesAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	d := NewDir(root)
	path := filepath.Join(root, "things", "a.json")
	writeFile(t, path, "garbage")

	require.NoError(t, d.put("things", "a", sample{N: 1}, true))
	var got sample
	found, err := d.Get("things", "a", &got)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 1, got.N)
}

func TestModeErrorsThatPmxcfsRaises(t *testing.T) {
	path := "/etc/pve/pco/things"
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.ENOSYS} {
		err := &fs.PathError{Op: "chmod", Path: path, Err: errno}
		require.True(t, unsupported(err), errno.Error())
	}
	require.False(t, unsupported(&fs.PathError{Op: "chmod", Path: path, Err: syscall.EIO}))
	require.False(t, unsupported(&fs.PathError{Op: "chmod", Path: path, Err: syscall.ENOENT}))
	require.False(t, unsupported(errors.New("other")))
}

func TestReadOnlyErrors(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EACCES, syscall.EROFS} {
		require.True(t, readOnly(&fs.PathError{Op: "mkdir", Path: "/etc/pve/pco", Err: errno}), errno.Error())
	}
	require.False(t, readOnly(&fs.PathError{Op: "mkdir", Path: "/etc/pve/pco", Err: syscall.ENOTDIR}))
}
