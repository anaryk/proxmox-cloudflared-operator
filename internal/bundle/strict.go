package bundle

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

var (
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// unmarshalStrict decodes raw into v, a pointer, refusing a key at any depth
// that the type has no field for, or that two fields would take: either
// json.Unmarshal drops in silence, and a restore would leave out what the
// bundle carries. It reads raw in place, where a json.Decoder keeps a copy of
// its own that a caller clearing a secret cannot reach; the copies its own
// walk makes are cleared afterwards. That is a best effort: what the runtime
// moved or copied meanwhile is beyond it. The names of the keys are the only
// part of the text an error repeats.
func unmarshalStrict(raw []byte, v any) error {
	if err := checkKeys(raw, reflect.TypeOf(v).Elem()); err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// checkKeys walks raw as a value of type t, through the objects, arrays and
// maps, and refuses the first key an object has that its struct has no field
// for, and an array with more items than a fixed one holds. A type that
// decodes itself is not looked into, nor is one that takes the text as it is.
func checkKeys(raw []byte, t reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	t = deref(t)
	if decodesItself(t) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		return checkObject(raw, t)
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		defer clearAll(items)
		if t.Kind() == reflect.Array && len(items) > t.Len() {
			return fmt.Errorf("%d items for an array of %d", len(items), t.Len())
		}
		for i, item := range items {
			if err := checkKeys(item, t.Elem()); err != nil {
				return fmt.Errorf("item %d: %w", i+1, err)
			}
		}
	case reflect.Map:
		values, err := objectOf(raw)
		if err != nil {
			return err
		}
		defer clearValues(values)
		for _, key := range slices.Sorted(maps.Keys(values)) {
			if err := checkKeys(values[key], t.Elem()); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

func checkObject(raw []byte, t reflect.Type) error {
	values, err := objectOf(raw)
	if err != nil {
		return err
	}
	defer clearValues(values)
	fields := fieldsOf(t)
	for _, key := range slices.Sorted(maps.Keys(values)) {
		ft, err := fieldFor(fields, key)
		if err != nil {
			return err
		}
		if err := checkKeys(values[key], ft); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

// decodesItself says whether a type reads its own JSON, as time.Time and
// the settings of the managed network do, or its own text, as an address.
func decodesItself(t reflect.Type) bool {
	p := reflect.PointerTo(t)
	return p.Implements(jsonUnmarshaler) || p.Implements(textUnmarshaler)
}

// field is a field a key may be decoded into, at the depth of embedding it
// was found at.
type field struct {
	typ    reflect.Type
	depth  int
	tagged bool
}

// fieldsOf lists the fields of a struct by the key each is decoded from, as
// encoding/json sees them: its own and those promoted from the embedded
// structs, a type looked into once.
func fieldsOf(t reflect.Type) map[string][]field {
	byKey := make(map[string][]field)
	seen := map[reflect.Type]bool{t: true}
	var walk func(t reflect.Type, depth int)
	walk = func(t reflect.Type, depth int) {
		for i := range t.NumField() {
			f := t.Field(i)
			name, ok := jsonName(f)
			switch {
			case !ok:
			case name == "":
				if e := deref(f.Type); !seen[e] {
					seen[e] = true
					walk(e, depth+1)
				}
			default:
				byKey[name] = append(byKey[name], field{typ: f.Type, depth: depth, tagged: f.Tag.Get("json") != ""})
			}
		}
	}
	walk(t, 0)
	return byKey
}

// fieldFor finds the field a key is decoded into: under its exact name
// first, then under one that differs in case. Of the fields under one name
// the least deeply embedded is taken, and at one depth the one with a tag;
// two at one depth with or without a tag alike are ambiguous, and
// encoding/json decodes the key into neither.
func fieldFor(fields map[string][]field, key string) (reflect.Type, error) {
	candidates, ok := fields[key]
	if !ok {
		for _, name := range slices.Sorted(maps.Keys(fields)) {
			if strings.EqualFold(name, key) {
				candidates = fields[name]
				break
			}
		}
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("unknown field %q", key)
	}
	least := slices.MinFunc(candidates, func(a, b field) int { return a.depth - b.depth })
	var tagged, all []field
	for _, c := range candidates {
		if c.depth != least.depth {
			continue
		}
		all = append(all, c)
		if c.tagged {
			tagged = append(tagged, c)
		}
	}
	switch {
	case len(all) == 1:
		return all[0].typ, nil
	case len(tagged) == 1:
		return tagged[0].typ, nil
	}
	return nil, fmt.Errorf("field %q is ambiguous: two fields are decoded from it, so neither is", key)
}

// jsonName is the key a field is decoded from; "" for an embedded struct
// whose fields are promoted, and false for a field that is not decoded.
func jsonName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if f.Anonymous && name == "" && deref(f.Type).Kind() == reflect.Struct {
		return "", true
	}
	if !f.IsExported() {
		return "", false
	}
	if name == "" {
		name = f.Name
	}
	return name, true
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func objectOf(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// clearValues and clearAll clear the copies the walk made: a value could be
// a secret.
func clearValues(m map[string]json.RawMessage) {
	for _, v := range m {
		clear(v)
	}
}

func clearAll(items []json.RawMessage) {
	for _, v := range items {
		clear(v)
	}
}
