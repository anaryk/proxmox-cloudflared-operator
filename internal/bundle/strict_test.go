package bundle

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

type strictInner struct {
	Deep int `json:"deep"`
}

type strictOther struct {
	Other int `json:"other"`
}

type strictEmbedded struct {
	Promoted string `json:"promoted"`
}

// strictSelf reads its own JSON, whatever it is.
type strictSelf struct{ seen bool }

func (s *strictSelf) UnmarshalJSON([]byte) error {
	s.seen = true
	return nil
}

type strictOuter struct {
	strictEmbedded
	Name     string                 `json:"name"`
	Inner    strictInner            `json:"inner"`
	Items    []strictInner          `json:"items,omitempty"`
	Fixed    [2]strictInner         `json:"fixed,omitempty"`
	ByKey    map[string]strictInner `json:"byKey,omitempty"`
	Ptr      *strictInner           `json:"ptr,omitempty"`
	When     time.Time              `json:"when,omitzero"`
	Addr     netip.Addr             `json:"addr,omitzero"`
	Raw      json.RawMessage        `json:"raw,omitempty"`
	Self     strictSelf             `json:"self,omitzero"`
	Untagged int
	Skipped  int `json:"-"`
	hidden   int
}

func TestAStrictDecodeTakesWhatTheTypeHasAFieldFor(t *testing.T) {
	for name, text := range map[string]string{
		"every field": `{"promoted":"p","name":"n","inner":{"deep":1},"items":[{"deep":2},{"deep":3}],"fixed":[{"deep":1},{"deep":2}],
			"byKey":{"a":{"deep":4}},"ptr":{"deep":5},"when":"2026-10-06T12:00:00Z","addr":"192.0.2.1","raw":{"anything":[1,{"goes":true}]},"Untagged":6}`,
		"a key in another case":         `{"NAME":"n","Inner":{"DEEP":1}}`,
		"null where an object could be": `{"inner":null,"items":null,"byKey":null,"ptr":null}`,
		"a type that reads itself":      `{"self":{"whatever":1}}`,
		"an object of a map":            `{"byKey":{"x":{"deep":1},"y":{"deep":2}}}`,
		"fewer items than the array":    `{"fixed":[{"deep":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var v strictOuter
			require.NoError(t, unmarshalStrict([]byte(text), &v))
		})
	}
	var v strictOuter
	require.NoError(t, unmarshalStrict([]byte(`{"promoted":"p","name":"n","self":{}}`), &v))
	require.Equal(t, "p", v.Promoted)
	require.Equal(t, "n", v.Name)
	require.True(t, v.Self.seen)
}

func TestAStrictDecodeRefusesAKeyAtAnyDepth(t *testing.T) {
	for name, tc := range map[string]struct{ text, want string }{
		"at the top":                          {`{"name":"n","surprise":1}`, `unknown field "surprise"`},
		"inside an object":                    {`{"inner":{"deep":1,"deeper":2}}`, `inner: unknown field "deeper"`},
		"inside an item":                      {`{"items":[{"deep":1},{"deep":2,"x":3}]}`, `items: item 2: unknown field "x"`},
		"inside an item of a fixed array":     {`{"fixed":[{"x":3}]}`, `fixed: item 1: unknown field "x"`},
		"inside a value of a map":             {`{"byKey":{"a":{"deep":1,"x":2}}}`, `byKey: a: unknown field "x"`},
		"behind a pointer":                    {`{"ptr":{"x":1}}`, `ptr: unknown field "x"`},
		"a field that is hidden":              {`{"hidden":1}`, `unknown field "hidden"`},
		"a field that is skipped":             {`{"Skipped":1}`, `unknown field "Skipped"`},
		"a key of the embedded struct itself": {`{"strictEmbedded":{"promoted":"p"}}`, `unknown field "strictEmbedded"`},
		"more items than the array holds":     {`{"fixed":[{"deep":1},{"deep":2},{"deep":3}]}`, `fixed: 3 items for an array of 2`},
	} {
		t.Run(name, func(t *testing.T) {
			var v strictOuter
			err := unmarshalStrict([]byte(tc.text), &v)
			require.EqualError(t, err, tc.want)
			require.Zero(t, v.hidden, "nothing is decoded")
		})
	}
}

func TestAStrictDecodeRefusesWhatIsNoObjectOfTheType(t *testing.T) {
	for name, text := range map[string]string{
		"text after the object":     `{"name":"n"} {}`,
		"an array":                  `[{"name":"n"}]`,
		"a string":                  `"name"`,
		"an item that is no object": `{"items":[1]}`,
		"a value of the wrong kind": `{"name":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			var v strictOuter
			require.Error(t, unmarshalStrict([]byte(text), &v))
		})
	}
}

func TestAStrictDecodeOfAMapChecksItsValues(t *testing.T) {
	var m map[string]strictInner
	require.NoError(t, unmarshalStrict([]byte(`{"a":{"deep":1}}`), &m))
	require.Equal(t, map[string]strictInner{"a": {Deep: 1}}, m)
	require.EqualError(t, unmarshalStrict([]byte(`{"a":{"deep":1,"x":2}}`), &m), `a: unknown field "x"`)
}

// Two embedded structs that take one key between them: vet refuses such a
// type in source, so the tests make it at run time.
type PromotedA struct {
	Promoted string `json:"promoted"`
}

type PromotedB struct {
	Promoted string `json:"promoted"`
}

type ThingUntagged struct {
	Thing strictInner
}

type ThingTagged struct {
	Thing strictOther `json:"Thing"`
}

// structOf makes a struct that embeds the types, with the named fields after
// them, and returns a pointer to a value of it.
func structOf(embedded []reflect.Type, own ...reflect.StructField) reflect.Value {
	var fields []reflect.StructField
	for _, e := range embedded {
		fields = append(fields, reflect.StructField{Name: e.Name(), Type: e, Anonymous: true})
	}
	return reflect.New(reflect.StructOf(append(fields, own...)))
}

func TestAStrictDecodeRefusesAKeyTwoEmbeddedFieldsWouldTake(t *testing.T) {
	// encoding/json decodes such a key into neither field, in silence.
	twice := structOf([]reflect.Type{reflect.TypeFor[PromotedA](), reflect.TypeFor[PromotedB]()})
	err := unmarshalStrict([]byte(`{"promoted":"p"}`), twice.Interface())
	require.EqualError(t, err, `field "promoted" is ambiguous: two fields are decoded from it, so neither is`)
	require.NoError(t, json.Unmarshal([]byte(`{"promoted":"p"}`), twice.Interface()))
	require.Empty(t, twice.Elem().FieldByName("PromotedA").Interface().(PromotedA).Promoted, "what encoding/json does with it")
	require.Empty(t, twice.Elem().FieldByName("PromotedB").Interface().(PromotedB).Promoted)

	own := structOf([]reflect.Type{reflect.TypeFor[PromotedA](), reflect.TypeFor[PromotedB]()},
		reflect.StructField{Name: "Promoted", Type: reflect.TypeFor[strictInner](), Tag: `json:"promoted"`})
	require.NoError(t, unmarshalStrict([]byte(`{"promoted":{"deep":1}}`), own.Interface()), "the struct's own field is taken over the embedded ones")
	require.Equal(t, strictInner{Deep: 1}, own.Elem().FieldByName("Promoted").Interface())

	tagged := structOf([]reflect.Type{reflect.TypeFor[ThingUntagged](), reflect.TypeFor[ThingTagged]()})
	require.NoError(t, unmarshalStrict([]byte(`{"Thing":{"other":1}}`), tagged.Interface()), "at one depth the field with a tag is taken")
	require.Equal(t, ThingTagged{Thing: strictOther{Other: 1}}, tagged.Elem().FieldByName("ThingTagged").Interface())
	require.EqualError(t, unmarshalStrict([]byte(`{"Thing":{"deep":1}}`), tagged.Interface()), `Thing: unknown field "deep"`)
}

// selfDecoding walks the fields of the types and lists those the check does
// not look into, because they decode themselves.
func selfDecoding(roots ...reflect.Type) []string {
	seen := make(map[reflect.Type]bool)
	var found []string
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		t = deref(t)
		if seen[t] {
			return
		}
		seen[t] = true
		if decodesItself(t) {
			found = append(found, t.String())
			return
		}
		switch t.Kind() {
		case reflect.Struct:
			for i := range t.NumField() {
				walk(t.Field(i).Type)
			}
		case reflect.Slice, reflect.Array, reflect.Map:
			walk(t.Elem())
		}
	}
	for _, r := range roots {
		walk(r)
	}
	slices.Sort(found)
	return found
}

func TestTheTypesTheStrictDecodeDoesNotLookIntoAreKnown(t *testing.T) {
	// Each of these reads its own JSON or text and is trusted to refuse what
	// it does not know; a store type that gains an UnmarshalJSON joins the
	// list, and this test says so.
	got := selfDecoding(
		reflect.TypeFor[document](),
		reflect.TypeFor[store.Settings](), reflect.TypeFor[model.Route](), reflect.TypeFor[store.Approval](),
		reflect.TypeFor[store.Segment](), reflect.TypeFor[store.NodeSegment](), reflect.TypeFor[store.NodeEntry](),
		reflect.TypeFor[credentialDocument](), reflect.TypeFor[store.LeaderRecord](), reflect.TypeFor[store.ClusterIdentity](),
		reflect.TypeFor[store.CARotation](), reflect.TypeFor[store.Network](), reflect.TypeFor[store.ServiceAddr](),
		reflect.TypeFor[store.NodeServiceAddr](), reflect.TypeFor[store.Lease](), reflect.TypeFor[store.Consent](),
		reflect.TypeFor[store.Attachment](), reflect.TypeFor[planner.Claim](), reflect.TypeFor[map[string]reconcile.Tombstone](),
		reflect.TypeFor[store.Placement](), reflect.TypeFor[store.Move](),
	)

	require.Equal(t, []string{
		"jsontext.Value", // json.RawMessage
		"netip.Addr",
		"netip.Prefix",
		"store.ClusterSettings",
		"store.Duration",
		"store.ManagedSettings",
		"time.Time",
	}, got)
}
