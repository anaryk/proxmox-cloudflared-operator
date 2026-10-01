package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

const (
	// schemaVersion is the version of the envelope around every object. It is
	// the only version this build reads and writes.
	schemaVersion = 1

	objectExt = ".json"
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9_][a-z0-9._-]{0,200}$`)

	// errNewerSchema marks a file written by a newer build.
	errNewerSchema = errors.New("written by a newer version")
)

// envelope is what a file holds around the value of an object.
type envelope struct {
	SchemaVersion int             `json:"schemaVersion"`
	Rev           int64           `json:"rev"`
	Data          json.RawMessage `json:"data"`
}

// Dir is a file-per-object JSON store rooted at a directory: the object of a
// kind and an id is the file <root>/<kind>/<id>.json.
//
// Nothing here chmods a file or relies on links, so a Dir works on pmxcfs,
// where the modes are fixed by path. Writes of one Dir are serialised; two
// processes writing the same root are not coordinated, as there is one daemon
// per store.
type Dir struct {
	root string
	mu   *sync.Mutex
}

// NewDir returns the store rooted at root. The directory is created by the
// first write.
func NewDir(root string) Dir { return Dir{root: root, mu: new(sync.Mutex)} }

// FileName maps an id to the name of its file, without the extension: it is
// lower-cased, a leading "*." becomes "_wildcard." and "/" becomes "_". Ids
// that differ only by case, or by "/" against "_", share a name, so an object
// keeps its real id inside. Anything that is not a plain file name is an
// error, which keeps every path under the root.
func FileName(id string) (string, error) {
	name := lowerASCII(id)
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		name = "_wildcard." + rest
	}
	name = strings.ReplaceAll(name, "/", "_")
	if !namePattern.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%q cannot be used as a file name", id)
	}
	return name, nil
}

// lowerASCII lower-cases A-Z only: a name is ASCII, and the lower-casing of
// some other characters, such as the Kelvin sign, would be an ASCII letter.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func (d Dir) check() error {
	if d.root == "" || d.mu == nil {
		return errors.New("store: directory not set; make it with NewDir")
	}
	return nil
}

// kindDir returns the directory of a kind.
func (d Dir) kindDir(kind string) (string, error) {
	if err := d.check(); err != nil {
		return "", err
	}
	name, err := FileName(kind)
	if err != nil {
		return "", fmt.Errorf("kind: %w", err)
	}
	return filepath.Join(d.root, name), nil
}

// file returns the directory and the path of the file of an object.
func (d Dir) file(kind, id string) (dir, path string, err error) {
	if dir, err = d.kindDir(kind); err != nil {
		return "", "", err
	}
	name, err := FileName(id)
	if err != nil {
		return "", "", fmt.Errorf("%s id: %w", kind, err)
	}
	return dir, filepath.Join(dir, name+objectExt), nil
}

// Put stores v as the object of kind and id. The file wraps v with the schema
// version and a revision that is one more than the file's before, or 1 when
// there is no file or it cannot be read. A file written by a newer version is
// not overwritten.
func (d Dir) Put(kind, id string, v any) error { return d.put(kind, id, v, false) }

// put is Put, which with onlyIfChanged leaves the file alone when it already
// holds v: every write of the cluster root is replicated to every node.
func (d Dir) put(kind, id string, v any, onlyIfChanged bool) error {
	dir, path, err := d.file(kind, id)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %s %s: %w", kind, id, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	old, found, err := readEnvelope(path)
	if errors.Is(err, errNewerSchema) {
		return fmt.Errorf("storing %s %s: %w", kind, id, err)
	}
	if err != nil {
		old = envelope{} // unreadable: the revision starts again
	} else if found && onlyIfChanged && sameData(old.Data, data) {
		return nil
	}
	out, err := json.MarshalIndent(envelope{SchemaVersion: schemaVersion, Rev: old.Rev + 1, Data: data}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s %s: %w", kind, id, err)
	}
	if err := ensureDir(dir); err != nil {
		return fmt.Errorf("storing %s %s: %w", kind, id, err)
	}
	if err := writeFileAtomic(path, path+tempExt, append(out, '\n')); err != nil {
		return fmt.Errorf("storing %s %s: %w", kind, id, err)
	}
	return nil
}

// sameData reports whether the data of a file is the compact JSON in want.
func sameData(have json.RawMessage, want []byte) bool {
	var buf bytes.Buffer
	return json.Compact(&buf, have) == nil && bytes.Equal(buf.Bytes(), want)
}

// Get reads the object of kind and id into v. It reports false, and no error,
// when there is no such object. A file that cannot be read, is not valid or
// was written by a newer version is an error that names it.
func (d Dir) Get(kind, id string, v any) (found bool, err error) {
	_, path, err := d.file(kind, id)
	if err != nil {
		return false, err
	}
	env, found, err := readEnvelope(path)
	if err != nil || !found {
		return false, err
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		return false, fmt.Errorf("%s: %s", path, jsonProblem(err))
	}
	return true, nil
}

// readEnvelope reads the file at path. A file that does not exist is not an
// error.
func readEnvelope(path string) (env envelope, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return envelope{}, false, nil
	}
	if err != nil {
		return envelope{}, false, fmt.Errorf("reading object: %w", err)
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return envelope{}, false, fmt.Errorf("%s: %s", path, jsonProblem(err))
	}
	switch {
	case env.SchemaVersion > schemaVersion:
		return envelope{}, false, fmt.Errorf("%s has schema version %d, this build reads %d: %w",
			path, env.SchemaVersion, schemaVersion, errNewerSchema)
	case env.SchemaVersion != schemaVersion:
		return envelope{}, false, fmt.Errorf("%s has no schema version", path)
	case len(env.Data) == 0:
		return envelope{}, false, fmt.Errorf("%s has no data", path)
	}
	return env, true, nil
}

// jsonProblem says what is wrong with a file without quoting it, as an error
// of the standard library may: a file can hold a secret.
func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	var kind *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("invalid JSON at offset %d", syntax.Offset)
	case errors.As(err, &kind):
		return fmt.Sprintf("field %q cannot be read as %s", kind.Field, kind.Type)
	}
	return "invalid: " + err.Error()
}

// List returns the ids of the objects of a kind, sorted: the names of their
// files without the extension. Hidden files, temporary files, anything that is
// not a .json file and names that FileName would change are not objects.
func (d Dir) List(kind string) ([]string, error) {
	dir, err := d.kindDir(kind)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", kind, err)
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), objectExt)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if name, err := FileName(stem); err == nil && name == stem {
			ids = append(ids, stem)
		}
	}
	slices.Sort(ids)
	return ids, nil
}

// Delete removes the object of kind and id. A missing object is not an error.
func (d Dir) Delete(kind, id string) error {
	_, path, err := d.file(kind, id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting %s %s: %w", kind, id, err)
	}
	return nil
}
