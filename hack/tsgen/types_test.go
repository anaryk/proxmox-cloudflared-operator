package main

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type inner struct {
	Name  string `json:"name"`
	Shade string `json:"shade"`
	Depth int    `json:"depth"`
}

type other struct {
	Depth int `json:"depth"`
}

type outer struct {
	inner
	other                     //nolint:govet // two embedded structs with a field of one name are the case under test
	Shade   string            `json:"shade,omitempty"`
	At      time.Time         `json:"at,omitempty"`
	Seen    time.Time         `json:"seen,omitzero"`
	Addr    netip.Addr        `json:"addr"`
	Next    *inner            `json:"next"`
	Maybe   *inner            `json:"maybe,omitempty"`
	Tags    []string          `json:"tags,omitempty"`
	Pins    map[string]string `json:"pins"`
	Count   int64             `json:"count,string"`
	Raw     json.RawMessage   `json:"raw"`
	Any     any               `json:"any"`
	Skipped string            `json:"-"`
	hidden  string
	Plain   bool
}

func TestFieldsAreWhatEncodingJSONWrites(t *testing.T) {
	w := &typeWriter{decls: make(map[string]*declaration), names: make(map[reflect.Type]string)}
	got, err := w.fields(reflect.TypeFor[outer]())
	require.NoError(t, err)
	require.Equal(t, []field{
		{name: "name", ts: "string"},
		{name: "shade", ts: "string", optional: true},
		{name: "at", ts: "string"},
		{name: "seen", ts: "string", optional: true},
		{name: "addr", ts: "string"},
		{name: "next", ts: "inner | null"},
		{name: "maybe", ts: "inner", optional: true},
		{name: "tags", ts: "string[]", optional: true},
		{name: "pins", ts: "Record<string, string>"},
		{name: "count", ts: "string"},
		{name: "raw", ts: "unknown"},
		{name: "any", ts: "unknown"},
		{name: "Plain", ts: "boolean"},
	}, got, "depth is dropped: two embedded structs have it at the same depth")

	// Marshal writes the same names, so the expectations above are those of
	// encoding/json.
	b, err := json.Marshal(outer{inner: inner{Shade: "x"}, hidden: "never written"})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	var names []string
	for _, f := range got {
		if !f.optional {
			names = append(names, f.name)
		}
	}
	require.ElementsMatch(t, names, keys(m))
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type ownJSON struct{}

func (ownJSON) MarshalJSON() ([]byte, error) { return []byte(`1`), nil } //nolint:unparam // the signature of json.Marshaler

func TestATypeThatWritesItselfMustBeNamed(t *testing.T) {
	w := &typeWriter{decls: make(map[string]*declaration), names: make(map[reflect.Type]string)}
	_, err := w.fields(reflect.TypeFor[struct {
		Own ownJSON `json:"own"`
	}]())
	require.ErrorContains(t, err, "writes its own JSON: say what in marshalsTo")
}

func anotherEvent() reflect.Type {
	type Event struct{ B int }
	return reflect.TypeFor[Event]()
}

func TestTwoTypesOfOneNameAreRefused(t *testing.T) {
	type Event struct{ A int }
	w := &typeWriter{decls: make(map[string]*declaration), names: make(map[reflect.Type]string)}
	_, err := w.named(reflect.TypeFor[Event]())
	require.NoError(t, err)
	_, err = w.named(reflect.TypeFor[Event]())
	require.NoError(t, err, "the same type is declared once")
	_, err = w.named(anotherEvent())
	require.ErrorContains(t, err, "are both Event in TypeScript")
}
