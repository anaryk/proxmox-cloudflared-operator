package store

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
)

// getOne reads the object of kind and id.
func getOne[T any](d Dir, kind, id string) (v T, found bool, err error) {
	var obj T
	found, err = d.Get(kind, id, &obj)
	if err != nil || !found {
		return v, false, err
	}
	return obj, true, nil
}

// eachObject calls fn for every object of a kind, in the order of their file
// names, with the path of its file and the id it was stored under. An object
// that cannot be read stops it: a part of a set is not the set. One that is
// deleted while it runs is skipped.
func eachObject[T any](d Dir, kind string, fn func(path, id string, v T) error) error {
	names, err := d.List(kind)
	if err != nil {
		return err
	}
	dir, err := d.kindDir(kind)
	if err != nil {
		return err
	}
	for _, name := range names {
		path := filepath.Join(dir, name+objectExt)
		env, found, err := d.readFile(path)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := belongsIn(path, name, env.ID); err != nil {
			return err
		}
		var v T
		if err := decode(env.Data, &v, false); err != nil {
			return fmt.Errorf("%s: %s", path, jsonProblem(err))
		}
		if err := fn(path, env.ID, v); err != nil {
			return err
		}
	}
	return nil
}

// belongsIn checks that the file at path, whose name is name without the
// extension, is where the object of id is kept. A copy of a file under another
// name would otherwise be a second object that Delete, which goes by the name,
// never reaches.
func belongsIn(path, name, id string) error {
	want, err := FileName(id)
	if err != nil {
		return fmt.Errorf("%s holds an id that is not valid: %w", path, err)
	}
	if want != name {
		return fmt.Errorf("%s holds the object of %q, which belongs in %s", path, id, want+objectExt)
	}
	return nil
}

// loadList returns the objects of a kind, in the order of their file names.
func loadList[T any](d Dir, kind string) ([]T, error) {
	out := []T{}
	err := eachObject(d, kind, func(_, _ string, v T) error {
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// loadMap returns the objects of a kind by the key each keeps inside it; field
// names that key in an error. The key must be the id the object was stored
// under: an object put in the wrong place by hand is an error, not a guess.
func loadMap[T any](d Dir, kind, field string, key func(*T) *string) (map[string]T, error) {
	out := make(map[string]T)
	err := eachObject(d, kind, func(path, id string, v T) error {
		k := *key(&v)
		switch {
		case k == "":
			return fmt.Errorf("%s has no %s", path, field)
		case k != id:
			return fmt.Errorf("%s holds the object of %q, which names the %s %q", path, id, field, k)
		}
		if _, dup := out[k]; dup {
			return fmt.Errorf("%s: another file holds the %s %q too", path, field, k)
		}
		out[k] = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// saveMap makes the objects of a kind the ones in next, which are keyed by
// hostname: it writes those that differ from what is stored and deletes the
// rest. The key is the id of an object and is kept inside it, in the field key
// points at. Everything is checked and encoded before the first write, so a bad
// entry changes nothing.
func saveMap[T any](d Dir, kind string, next map[string]T, key func(*T) *string) error {
	keys := slices.Sorted(maps.Keys(next))
	names := make(map[string]string, len(next)) // file name -> key
	objs := make([]object, 0, len(next))
	for _, k := range keys {
		name, err := FileName(k)
		if err != nil {
			return fmt.Errorf("%s: %w", kind, err)
		}
		if other, dup := names[name]; dup {
			return fmt.Errorf("%s: %q and %q share the file %s", kind, other, k, name)
		}
		names[name] = k
		v := next[k]
		switch f := key(&v); {
		case *f == "":
			*f = k
		case *f != k:
			return fmt.Errorf("%s: the key %q holds an object with hostname %q", kind, k, *f)
		}
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("encoding %s %q: %w", kind, k, err)
		}
		objs = append(objs, object{id: k, data: data})
	}
	return d.replace(kind, objs)
}
