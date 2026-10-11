package bundle

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type strictInner struct {
	Deep int `json:"deep"`
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
		"every field": `{"promoted":"p","name":"n","inner":{"deep":1},"items":[{"deep":2},{"deep":3}],"byKey":{"a":{"deep":4}},
			"ptr":{"deep":5},"when":"2026-10-06T12:00:00Z","addr":"192.0.2.1","raw":{"anything":[1,{"goes":true}]},"Untagged":6}`,
		"a key in another case":         `{"NAME":"n","Inner":{"DEEP":1}}`,
		"null where an object could be": `{"inner":null,"items":null,"byKey":null,"ptr":null}`,
		"a type that reads itself":      `{"self":{"whatever":1}}`,
		"an object of a map":            `{"byKey":{"x":{"deep":1},"y":{"deep":2}}}`,
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
		"inside a value of a map":             {`{"byKey":{"a":{"deep":1,"x":2}}}`, `byKey: a: unknown field "x"`},
		"behind a pointer":                    {`{"ptr":{"x":1}}`, `ptr: unknown field "x"`},
		"a field that is hidden":              {`{"hidden":1}`, `unknown field "hidden"`},
		"a field that is skipped":             {`{"Skipped":1}`, `unknown field "Skipped"`},
		"a key of the embedded struct itself": {`{"strictEmbedded":{"promoted":"p"}}`, `unknown field "strictEmbedded"`},
	} {
		t.Run(name, func(t *testing.T) {
			var v strictOuter
			err := unmarshalStrict([]byte(tc.text), &v)
			require.EqualError(t, err, tc.want)
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
