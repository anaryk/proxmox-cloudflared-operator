package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// ErrWentBack is the error of WentBack: an object this tenure wrote was read
// at a lower revision than the one written, or is gone though the tenure did
// not delete it. The store was restored under the running writer, from a
// copy that carries the same leader.json, and a write now would build on
// what the writer no longer knows. It comes wrapped, naming the object.
var ErrWentBack = errors.New("the store went backwards")

// tenure records every object a writer wrote in its tenure with the revision
// it wrote, so that a store that goes back is noticed. A store keeps none
// until StartTenure.
type tenure struct {
	mu      sync.Mutex
	on      bool
	written map[string]writtenObject // by path
	err     error                    // the first step back seen
}

// writtenObject is what the tenure wrote at a path.
type writtenObject struct {
	root, kind, id string
	rev            int64
}

func (w writtenObject) String() string { return fmt.Sprintf("%s %q in %s", w.kind, w.id, w.root) }

// StartTenure starts a tenure of this store as the writer: from now on it
// remembers the revision of every object it writes, and every read compares.
// A tenure started again forgets the last one and what it saw.
func (s *Store) StartTenure() {
	t := s.tenure
	t.mu.Lock()
	defer t.mu.Unlock()
	t.on, t.written, t.err = true, make(map[string]writtenObject), nil
}

// WentBack returns an error, ErrWentBack, once a read of this tenure found an
// object it wrote at a lower revision, or found it missing though the tenure
// did not delete it; nil until then, and outside a tenure.
func (s *Store) WentBack() error {
	t := s.tenure
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// active reports whether a tenure runs. It is false for a Dir without one.
func (t *tenure) active() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.on
}

// wrote records that the tenure wrote revision rev of an object.
func (t *tenure) wrote(root, kind, id, path string, rev int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.on {
		t.written[path] = writtenObject{root: root, kind: kind, id: id, rev: rev}
	}
}

// deleted records that the tenure deleted the object at path.
func (t *tenure) deleted(path string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.written, path)
}

// saw compares a read of the object at path with what the tenure wrote there.
func (t *tenure) saw(path string, env envelope, found bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.written[path]
	switch {
	case !t.on || !ok || t.err != nil:
	case !found:
		t.err = fmt.Errorf("%w: %s is gone, and this tenure wrote revision %d of it and did not delete it", ErrWentBack, w, w.rev)
	case env.Rev < w.rev:
		t.err = fmt.Errorf("%w: %s is at revision %d, and this tenure wrote revision %d", ErrWentBack, w, env.Rev, w.rev)
	}
}

// listed compares a listing of the kind directory dir, the stems of its
// files, with the objects the tenure wrote there.
func (t *tenure) listed(dir string, stems []string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.on || t.err != nil {
		return
	}
	listed := make(map[string]bool, len(stems))
	for _, stem := range stems {
		listed[stem] = true
	}
	var missing []string
	for path := range t.written {
		if stem, ok := strings.CutSuffix(filepath.Base(path), objectExt); ok && filepath.Dir(path) == dir && !listed[stem] {
			missing = append(missing, path)
		}
	}
	if len(missing) == 0 {
		return
	}
	w := t.written[slices.Min(missing)]
	t.err = fmt.Errorf("%w: %s is gone, and this tenure wrote revision %d of it and did not delete it", ErrWentBack, w, w.rev)
}
