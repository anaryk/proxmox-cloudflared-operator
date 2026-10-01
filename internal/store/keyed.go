package store

import (
	"fmt"
	"maps"
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

// eachObject calls fn for every object of a kind, in the order of their ids.
// An object that cannot be read stops it: a part of a set is not the set. One
// that is deleted while it runs is skipped.
func eachObject[T any](d Dir, kind string, fn func(id string, v T) error) error {
	ids, err := d.List(kind)
	if err != nil {
		return err
	}
	for _, id := range ids {
		v, found, err := getOne[T](d, kind, id)
		if err != nil {
			return err
		}
		if found {
			if err := fn(id, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadList returns the objects of a kind, in the order of their ids.
func loadList[T any](d Dir, kind string) ([]T, error) {
	out := []T{}
	err := eachObject(d, kind, func(_ string, v T) error {
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// loadMap returns the objects of a kind by the key each keeps inside it; field
// names that key in an error. The name of a file never decides the key.
func loadMap[T any](d Dir, kind, field string, key func(*T) *string) (map[string]T, error) {
	out := make(map[string]T)
	files := make(map[string]string) // key -> id of the object that holds it
	err := eachObject(d, kind, func(id string, v T) error {
		k := *key(&v)
		_, path, err := d.file(kind, id)
		if err != nil {
			return err
		}
		if k == "" {
			return fmt.Errorf("%s has no %s", path, field)
		}
		if other, dup := files[k]; dup {
			return fmt.Errorf("%s and %s both hold the %s %q", other, id, field, k)
		}
		files[k], out[k] = id, v
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
// points at. Everything is checked before the first write, so a bad entry
// changes nothing.
func saveMap[T any](d Dir, kind string, next map[string]T, key func(*T) *string) error {
	keys := slices.Sorted(maps.Keys(next))
	names := make(map[string]string, len(next)) // file name -> key
	values := make(map[string]T, len(next))
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
		values[k] = v
	}
	for _, k := range keys {
		if err := d.put(kind, k, values[k], true); err != nil {
			return err
		}
	}
	ids, err := d.List(kind)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, keep := names[id]; !keep {
			if err := d.Delete(kind, id); err != nil {
				return err
			}
		}
	}
	return nil
}
