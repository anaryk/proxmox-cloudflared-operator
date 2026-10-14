package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// schemaVersion is the version of the envelope around every object but the
// frozen ones. It is the only version this build reads and writes. It is a
// variable only so that a test can raise it, as a later release will.
var schemaVersion = 1

const objectExt = ".json"

// ErrNoRoot is the error of an operation on a root directory that does not
// exist, while the cluster filesystem is mounted: the store was never set up
// here, or its root was removed. Callers turn it into "run pco setup". It comes
// wrapped, with the path of the root.
var ErrNoRoot = errors.New("store root is missing")

// ErrRevision is the error of a write for an object read at a revision it no
// longer has: someone else wrote it, made it or removed it in between.
// Nothing was written.
var ErrRevision = errors.New("the object changed since it was read")

// errNewerSchema marks a file written by a newer version.
var errNewerSchema = errors.New("written by a newer version")

// damagedError is the error of a file that is there and holds no object this
// build could have written: no envelope, no data, another id, data of another
// shape. A file of a newer schema is not damaged, only newer. The message is
// the error's own.
type damagedError struct{ err error }

func (e *damagedError) Error() string { return e.err.Error() }

func (e *damagedError) Unwrap() error { return e.err }

func damaged(err error) error { return &damagedError{err: err} }

// isDamaged reports whether err says a file is damaged.
func isDamaged(err error) bool {
	var d *damagedError
	return errors.As(err, &d)
}

// envelope is what a file holds around the value of an object. The id is the
// one the object was stored under, since a file name cannot tell ids apart that
// share it.
type envelope struct {
	SchemaVersion int             `json:"schemaVersion"`
	Rev           int64           `json:"rev"`
	ID            string          `json:"id"`
	Data          json.RawMessage `json:"data"`
}

// Dir is a file-per-object JSON store rooted at a directory: the object of a
// kind and an id is the file <root>/<kind>/<FileName(id)>.json.
//
// The root must exist, and a Dir never creates it: on the cluster filesystem a
// root that is gone means the filesystem is, and a directory made then lands
// on the disk underneath it. Nothing here chmods a file or relies on links, so
// a Dir works on pmxcfs, where the modes are fixed by path. Writes of one Dir
// are serialised; processes writing the same root are not coordinated with
// each other beyond every write being a rename of a file of its own.
type Dir struct {
	root  string
	mu    *sync.Mutex
	guard func() error // run before every operation; nil for none

	// durable makes every rename, removal and new directory outlive a crash,
	// through a sync of the directory it changed. pmxcfs refuses that sync,
	// so it is off there. flush is the sync, which a test replaces.
	durable bool
	flush   func(dir string) error

	// tenure is the record of what this store wrote in the current tenure,
	// shared by the roots of one Store; nil keeps none.
	tenure *tenure

	// maxSize is the largest file the Dir writes; 0 is maxFileSize, the most
	// the cluster filesystem takes.
	maxSize int
}

// NewDir returns the store rooted at root.
func NewDir(root string) Dir { return newDir(root, nil, nil) }

func newDir(root string, guard func() error, t *tenure) Dir {
	return Dir{root: root, mu: new(sync.Mutex), guard: guard, flush: syncDir, tenure: t}
}

// limit is the largest file the Dir writes.
func (d Dir) limit() int {
	if d.maxSize == 0 {
		return maxFileSize
	}
	return d.maxSize
}

// synced makes the entries of dir durable, when the Dir is.
func (d Dir) synced(dir string) error {
	if !d.durable {
		return nil
	}
	if err := d.flush(dir); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}

// check refuses an operation on a Dir that was not made by NewDir, and one the
// guard refuses.
func (d Dir) check() error {
	if d.root == "" || d.mu == nil {
		return errors.New("store: directory not set; make it with NewDir")
	}
	if d.guard != nil {
		return d.guard()
	}
	return nil
}

// requireRoot reports an error, ErrNoRoot, when the root is not there. It is
// what a read of a file or a directory that does not exist asks, to tell
// "nothing stored" from "no store".
func (d Dir) requireRoot() error {
	info, err := os.Stat(d.root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%w: %s", ErrNoRoot, d.root)
	case err != nil:
		return fmt.Errorf("store root %s: %w", d.root, err)
	case !info.IsDir():
		return fmt.Errorf("store root %s is not a directory", d.root)
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
// version, the id and a revision that is one more than the file's before, or 1
// when there is no file or it cannot be read. It refuses to replace the file
// of another id that maps to the same name, and one written by a newer version.
func (d Dir) Put(kind, id string, v any) error { return d.put(kind, id, v, false) }

// Get reads the object of kind and id into v. It reports false, and no error,
// when there is no such object. A file that cannot be read, is not valid, was
// written by a newer version or holds the object of another id is an error that
// names it.
func (d Dir) Get(kind, id string, v any) (found bool, err error) {
	return d.get(kind, id, v, false)
}

// get is Get, which with strict also refuses a key the value has no field for.
func (d Dir) get(kind, id string, v any, strict bool) (found bool, err error) {
	_, found, err = d.getRev(kind, id, v, strict)
	return found, err
}

// getRev is get that also returns the revision of the file, 0 when there is
// none.
func (d Dir) getRev(kind, id string, v any, strict bool) (rev int64, found bool, err error) {
	_, path, err := d.file(kind, id)
	if err != nil {
		return 0, false, err
	}
	env, found, err := d.readFile(path)
	if err != nil || !found {
		return 0, false, err
	}
	if env.ID != id {
		return 0, false, damaged(fmt.Errorf("%s holds the object of %q, not of %q", path, env.ID, id))
	}
	if err := decode(env.Data, v, strict); err != nil {
		return 0, false, damaged(fmt.Errorf("%s: %s", path, jsonProblem(err)))
	}
	return env.Rev, true, nil
}

// readFile reads the file at path, in a kind directory of the root. A file that
// is not there is not found, unless the root is gone too.
func (d Dir) readFile(path string) (env envelope, found bool, err error) {
	if d.tenure.active() {
		// A write renames its file and then records it: a read in between
		// would take the revision before for a step back.
		d.mu.Lock()
		defer d.mu.Unlock()
	}
	env, found, err = readEnvelope(path)
	switch {
	case err != nil:
		return envelope{}, false, err
	case !found:
		if err := d.requireRoot(); err != nil {
			return envelope{}, false, err
		}
		d.tenure.saw(path, envelope{}, false)
		return envelope{}, false, nil
	case env.ID == "":
		return envelope{}, false, damaged(fmt.Errorf("%s has no id", path))
	}
	d.tenure.saw(path, env, true)
	return env, true, nil
}

// read is readEnvelope for a caller that holds the lock, compared with what
// the tenure wrote.
func (d Dir) read(path string) (env envelope, found bool, err error) {
	env, found, err = readEnvelope(path)
	if err == nil {
		d.tenure.saw(path, env, found)
	}
	return env, found, err
}

// readEnvelope reads the file at path. A file that does not exist is not an
// error. The id is not checked: a file without one is for the caller to
// refuse or to replace.
func readEnvelope(path string) (env envelope, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return envelope{}, false, nil
	}
	if err != nil {
		return envelope{}, false, fmt.Errorf("reading object: %w", err)
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return envelope{}, false, damaged(fmt.Errorf("%s: %s", path, jsonProblem(err)))
	}
	switch want := schemaAt(path); {
	case env.SchemaVersion > want:
		return envelope{}, false, fmt.Errorf("%s has schema version %d, this build reads %d: %w",
			path, env.SchemaVersion, want, errNewerSchema)
	case env.SchemaVersion == 0:
		return envelope{}, false, damaged(fmt.Errorf("%s has no schema version", path))
	case env.SchemaVersion != want:
		return envelope{}, false, fmt.Errorf("%s has schema version %d, this build reads %d", path, env.SchemaVersion, want)
	case len(env.Data) == 0 || string(bytes.TrimSpace(env.Data)) == "null":
		return envelope{}, false, damaged(fmt.Errorf("%s has no data", path))
	}
	return env, true, nil
}

// decode reads the data of an object into v.
func decode(data json.RawMessage, v any, strict bool) error {
	if !strict {
		return json.Unmarshal(data, v)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// jsonProblem says what is wrong with a file without quoting it, as an error
// of the standard library may: a file can hold a secret. The name of a key the
// value has no field for is the exception, as it is part of the question.
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

// List returns the file stems of the objects of a kind, sorted: the names of
// their files without the extension. A stem is the id of its object only when
// FileName leaves the id as it is; for an id that is long, or has capitals, a
// "/" or a leading "*.", it is not, and the id is in the file. Get takes an id,
// never a stem of that kind. Hidden files, temporary files, anything that is not
// a .json file and names that FileName would change are not objects. A kind
// with no directory has no objects, but a root that is gone is an error.
func (d Dir) List(kind string) ([]string, error) {
	dir, err := d.kindDir(kind)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, d.requireRoot()
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

// Delete removes the object of kind and id. A missing object is not an error;
// the file of another id that maps to the same name, and one written by a newer
// version, are not removed.
func (d Dir) Delete(kind, id string) error {
	_, path, err := d.file(kind, id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	env, found, err := d.read(path)
	switch {
	case errors.Is(err, errNewerSchema):
		return fmt.Errorf("deleting %s %q: %w", kind, id, err)
	case err == nil && found && env.ID != "" && env.ID != id:
		return fmt.Errorf("deleting %s %q: %s holds the object of %q", kind, id, path, env.ID)
	}
	return d.removeFile(path)
}

// listObjects is List that, while a tenure runs, also notes an object the
// tenure wrote and did not delete that the listing misses.
func (d Dir) listObjects(kind string) ([]string, error) {
	if !d.tenure.active() {
		return d.List(kind)
	}
	dir, err := d.kindDir(kind)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names, err := d.List(kind)
	if err == nil {
		d.tenure.listed(dir, names)
	}
	return names, err
}

// removeFile removes a file. One that is not there is no error, unless the
// root is gone too. The caller holds the lock.
func (d Dir) removeFile(path string) error {
	if err := d.tenure.failed(); err != nil {
		return fmt.Errorf("deleting %s: %w", filepath.Base(path), err)
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := d.requireRoot(); err != nil {
			return err
		}
		d.tenure.deleted(path)
		return nil
	}
	if err == nil {
		d.tenure.deleted(path)
		err = d.synced(filepath.Dir(path))
	}
	if err != nil {
		return fmt.Errorf("deleting %s: %w", filepath.Base(path), err)
	}
	return nil
}
