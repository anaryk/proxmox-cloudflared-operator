package gateway

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// jsonNames are the names a value of t has in JSON, as encoding/json gives
// them: the fields of an embedded struct without a name of its own are the
// outer struct's.
func jsonNames(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		switch {
		case name == "-":
			continue
		case f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct:
			names = append(names, jsonNames(f.Type)...)
			continue
		case !f.IsExported():
			continue
		case name == "":
			name = f.Name
		}
		names = append(names, name)
	}
	return names
}

// undecided are the fields of t that have no decision.
func undecided(t reflect.Type, decided map[string]decision) []string {
	var out []string
	for _, name := range jsonNames(t) {
		if _, ok := decided[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

// fieldTables are the structs that carry guests, each with its decisions.
var fieldTables = []struct {
	of      reflect.Type
	decided map[string]decision
}{
	{reflect.TypeFor[engine.State](), stateFields},
	{reflect.TypeFor[engine.RouteView](), routeFields},
	{reflect.TypeFor[engine.UnapprovedGuest](), unapprovedFields},
	{reflect.TypeFor[engine.Waiting](), waitingFields},
}

func TestEveryFieldHasADecision(t *testing.T) {
	for _, ft := range fieldTables {
		require.Empty(t, undecided(ft.of, ft.decided),
			"%s has fields without a decision for readers: add them to the decisions in fields.go", ft.of)
		names := jsonNames(ft.of)
		for name := range ft.decided {
			require.True(t, slices.Contains(names, name), "%s has no field %s, which has a decision", ft.of, name)
		}
	}
}

// A field the daemon adds is found: here State.Networks of the networking
// milestone, on a copy of the state.
func TestAFieldWithoutADecisionFails(t *testing.T) {
	type withNetworks struct {
		engine.State
		Networks []string `json:"networks"`
		Managed  any      `json:"managed,omitempty"`
	}
	require.Equal(t, []string{"networks", "managed"}, undecided(reflect.TypeFor[withNetworks](), stateFields))
}

// Every decision that filters or removes names its case, and every case is
// named by a decision.
func TestDecisionsNameTheirCases(t *testing.T) {
	cases := map[string]bool{}
	for _, c := range stateCases {
		cases[c.Name] = true
	}
	named := map[string]bool{}
	for field, d := range stateFields {
		switch d.action {
		case keep:
			require.Empty(t, d.by, field)
		case filter, remove:
			require.True(t, cases[d.by], "%s is filtered by %q, which is no case", field, d.by)
			named[d.by] = true
		}
	}
	for name := range cases {
		require.True(t, named[name], "the case %s filters no field", name)
	}
	for _, m := range []map[string]decision{routeFields, unapprovedFields, waitingFields} {
		for field, d := range m {
			require.Contains(t, []action{keep, remove}, d.action, "%s: a field inside a list goes with its entry", field)
		}
	}
}

// What a decision says is what the filter does: a kept field reaches a reader
// as it is, a removed one never does.
func TestTheFilterDoesWhatTheDecisionsSay(t *testing.T) {
	in := withIdentity(t)
	for _, visible := range []func() map[string]json.RawMessage{
		func() map[string]json.RawMessage { return fieldsOf(t, FilterState(in, sees())) },
		func() map[string]json.RawMessage {
			return fieldsOf(t, FilterState(in, sees(101, 102, 103, 104, 140, 141, 200, 201, 202, 9295)))
		},
	} {
		got := visible()
		want := fieldsOf(t, in)
		for field, d := range stateFields {
			switch d.action {
			case keep:
				require.Equal(t, string(want[field]), string(got[field]), "%s is kept", field)
			case remove:
				require.Contains(t, []string{"", "[]", `""`, "null"}, string(got[field]), "%s is removed", field)
			}
		}
	}
}

func fieldsOf(t *testing.T, st engine.State) map[string]json.RawMessage {
	t.Helper()
	data, err := json.Marshal(st)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}
