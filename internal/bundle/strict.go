package bundle

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

var (
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	textUnmarshaler = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// unmarshalStrict decodes raw into v, a pointer, refusing a key at any depth
// that the type has no field for: json.Unmarshal would drop it in silence,
// and a restore would leave out what the bundle carries. It reads raw in
// place, where a json.Decoder keeps a copy of its own that a caller clearing
// a secret cannot reach. The names of the keys are the only part of the text
// an error repeats.
func unmarshalStrict(raw []byte, v any) error {
	if err := checkKeys(raw, reflect.TypeOf(v).Elem()); err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// checkKeys walks raw as a value of type t, through the objects, arrays and
// maps, and refuses the first key an object has that its struct has no field
// for. A type that decodes itself is not looked into, nor is one that takes
// the text as it is.
func checkKeys(raw []byte, t reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if decodesItself(t) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		return checkObject(raw, t)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		defer clearAll(items)
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
		for _, key := range slices.Sorted(keysOf(values)) {
			if err := checkKeys(values[key], t.Elem()); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

func checkObject(raw []byte, t reflect.Type) error {
	fields, err := objectOf(raw)
	if err != nil {
		return err
	}
	defer clearValues(fields)
	for _, key := range slices.Sorted(keysOf(fields)) {
		ft, ok := fieldOf(t, key)
		if !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		if err := checkKeys(fields[key], ft); err != nil {
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

// fieldOf finds the field of a struct a key is decoded into, as
// encoding/json does: by the name of its tag or its own, the exact name
// before one that differs in case, and through embedded structs.
func fieldOf(t reflect.Type, key string) (reflect.Type, bool) {
	var folded reflect.Type
	found := false
	for i := range t.NumField() {
		f := t.Field(i)
		name, ok := jsonName(f)
		switch {
		case !ok:
			continue
		case name == "":
			if ft, ok := fieldOf(deref(f.Type), key); ok && !found {
				folded, found = ft, true
			}
		case name == key:
			return f.Type, true
		case !found && strings.EqualFold(name, key):
			folded, found = f.Type, true
		}
	}
	return folded, found
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

func keysOf(m map[string]json.RawMessage) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
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
