package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// object is the JSON of a value, with the id it is stored under. keep, when
// set, says whether what a file holds may stand for the value although it
// differs.
type object struct {
	id   string
	data []byte
	keep func(stored json.RawMessage) bool
}

// write is a file that is to be written: everything about it is known, and
// checked, before anything is.
type write struct {
	kind, id  string
	dir, path string
	out       []byte
}

// put is Put, which with onlyIfChanged leaves the file alone when it already
// holds v: every write of the cluster root is replicated to every node.
func (d Dir) put(kind, id string, v any, onlyIfChanged bool) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %s %q: %w", kind, id, err)
	}
	if err := d.check(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	w, err := d.prepare(kind, object{id: id, data: data}, onlyIfChanged)
	if err != nil || w == nil {
		return err
	}
	return d.commit(w)
}

// putIf is put for a value that was read at revision rev, 0 when there was
// no object: it writes only while the file still has that revision, and
// returns the revision the file has after. Otherwise the error is ErrRevision
// with the revision the file has. A file that cannot be read is not replaced.
func (d Dir) putIf(kind, id string, v any, rev int64) (int64, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("encoding %s %q: %w", kind, id, err)
	}
	_, path, err := d.file(kind, id)
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	have, _, err := d.revisionIs(kind, id, path, rev)
	if err != nil {
		return have, err
	}
	w, err := d.prepare(kind, object{id: id, data: data}, true)
	if err != nil || w == nil {
		return have, err
	}
	if err := d.commit(w); err != nil {
		return 0, err
	}
	return have + 1, nil
}

// deleteIf removes the object of kind and id when its file has revision rev.
// Otherwise the error is ErrRevision, also when there is no file.
func (d Dir) deleteIf(kind, id string, rev int64) error {
	_, path, err := d.file(kind, id)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	switch _, found, err := d.revisionIs(kind, id, path, rev); {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: there is no %s %q", ErrRevision, kind, id)
	}
	return d.removeFile(path)
}

// revisionIs checks that the file of an object at path has revision rev, 0
// for no file, and returns the revision it has and whether it is there. The
// caller holds the lock.
func (d Dir) revisionIs(kind, id, path string, rev int64) (have int64, found bool, err error) {
	old, found, err := readEnvelope(path)
	switch {
	case err != nil:
		return 0, false, fmt.Errorf("%s %q: %w", kind, id, err)
	case found && old.ID != "" && old.ID != id:
		return 0, false, fmt.Errorf("%s %q: %s holds the object of %q", kind, id, path, old.ID)
	case found:
		have = old.Rev
	}
	if have != rev {
		return have, found, fmt.Errorf("%w: %s %q is at revision %d, not %d", ErrRevision, kind, id, have, rev)
	}
	return have, found, nil
}

// replace makes the objects of a kind the ones in objs: it writes those that
// differ from what is stored and removes every other. All of them are checked
// before the first write or removal, so what could be known beforehand, such as
// a file that is too large or one that holds another id, changes nothing. A
// failure after that leaves a part done, which a repeat completes.
func (d Dir) replace(kind string, objs []object) error {
	dir, err := d.kindDir(kind)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	wanted := make(map[string]struct{}, len(objs))
	var writes []*write
	for _, o := range objs {
		_, path, err := d.file(kind, o.id)
		if err != nil {
			return err
		}
		wanted[filepath.Base(path)] = struct{}{}
		w, err := d.prepare(kind, o, true)
		if err != nil {
			return err
		}
		if w != nil {
			writes = append(writes, w)
		}
	}
	stale, err := d.staleFiles(kind, dir, wanted)
	if err != nil {
		return err
	}
	for _, w := range writes {
		if err := d.commit(w); err != nil {
			return err
		}
	}
	for _, path := range stale {
		if err := d.removeFile(path); err != nil {
			return err
		}
	}
	return nil
}

// staleFiles returns the paths of the objects of a kind whose file name is not
// in wanted.
func (d Dir) staleFiles(kind, dir string, wanted map[string]struct{}) ([]string, error) {
	stems, err := d.List(kind)
	if err != nil {
		return nil, err
	}
	var stale []string
	for _, stem := range stems {
		if _, keep := wanted[stem+objectExt]; !keep {
			stale = append(stale, filepath.Join(dir, stem+objectExt))
		}
	}
	return stale, nil
}

// prepare works out the write of an object, or nil when there is nothing to
// write. The caller holds the lock.
func (d Dir) prepare(kind string, o object, onlyIfChanged bool) (*write, error) {
	dir, path, err := d.file(kind, o.id)
	if err != nil {
		return nil, err
	}
	old, found, err := readEnvelope(path)
	switch {
	case errors.Is(err, errNewerSchema):
		return nil, fmt.Errorf("storing %s %q: %w", kind, o.id, err)
	case err != nil:
		old = envelope{} // not readable: the revision starts again
	case found && old.ID != "" && old.ID != o.id:
		return nil, fmt.Errorf("storing %s %q: %s already holds the object of %q", kind, o.id, path, old.ID)
	case found && onlyIfChanged && old.ID == o.id && (sameData(old.Data, o.data) || o.keep != nil && o.keep(old.Data)):
		return nil, nil
	}
	out, err := json.MarshalIndent(envelope{SchemaVersion: schemaVersion, Rev: old.Rev + 1, ID: o.id, Data: o.data}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding %s %q: %w", kind, o.id, err)
	}
	out = append(out, '\n')
	if len(out) > maxFileSize {
		return nil, fmt.Errorf("storing %s %q: %d bytes are over the %d byte limit of a file", kind, o.id, len(out), maxFileSize)
	}
	return &write{kind: kind, id: o.id, dir: dir, path: path, out: out}, nil
}

// commit writes a file that prepare worked out. The caller holds the lock.
func (d Dir) commit(w *write) error {
	if err := d.ensureKindDir(w.dir); err != nil {
		return fmt.Errorf("storing %s %q: %w", w.kind, w.id, err)
	}
	if err := writeFileAtomic(w.path, w.out, d.synced); err != nil {
		return fmt.Errorf("storing %s %q: %w", w.kind, w.id, err)
	}
	return nil
}

// sameData reports whether the data of a file is the compact JSON in want.
func sameData(have json.RawMessage, want []byte) bool {
	var buf bytes.Buffer
	return json.Compact(&buf, have) == nil && bytes.Equal(buf.Bytes(), want)
}

// ensureKindDir makes the directory of a kind, but never the root above it. The
// mode is 0700 where the filesystem lets it be set.
func (d Dir) ensureKindDir(dir string) error {
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return nil
	}
	created, err := makeLeaf(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if rootErr := d.requireRoot(); rootErr != nil {
			return rootErr
		}
	}
	if err != nil || !created {
		return err
	}
	return d.synced(d.root)
}

// updateFile replaces the file name, which is in the root itself, with what
// update makes of its content, which is nil for a file that is not there. It is
// for a file that is not an object, such as a log.
func (d Dir) updateFile(name string, update func(old []byte) []byte) error {
	if err := d.check(); err != nil {
		return err
	}
	path := filepath.Join(d.root, name)
	d.mu.Lock()
	defer d.mu.Unlock()

	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := d.requireRoot(); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("reading %s: %w", name, err)
	}
	if err := writeFileAtomic(path, update(old), d.synced); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}
