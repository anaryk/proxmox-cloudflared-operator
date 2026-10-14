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
	{reflect.TypeFor[engine.IdentityView](), identityFields},
	{reflect.TypeFor[engine.ClaimView](), claimFields},
	{reflect.TypeFor[engine.ClaimantView](), claimantFields},
	{reflect.TypeFor[engine.ApprovalView](), approvalFields},
	{reflect.TypeFor[engine.GuestListView](), guestFields},
	{reflect.TypeFor[engine.RouteTraffic](), routeTrafficFields},
	{reflect.TypeFor[engine.LeaseView](), leaseFields},
	{reflect.TypeFor[engine.NodeView](), nodeFields},
	{reflect.TypeFor[engine.ClusterView](), clusterFields},
	{reflect.TypeFor[engine.BlockView](), blockFields},
	{reflect.TypeFor[engine.CoverageView](), coverageFields},
	{reflect.TypeFor[engine.UpgradeView](), upgradeFields},
	{reflect.TypeFor[engine.CARotationView](), caRotationFields},
	{reflect.TypeFor[engine.ReplicaView](), replicaFields},
	{reflect.TypeFor[engine.PlacementView](), placementFields},
	{reflect.TypeFor[engine.MoveView](), moveFields},
	{reflect.TypeFor[engine.NetworkView](), networkFields},
	{reflect.TypeFor[engine.LegView](), legFields},
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

// A field the daemon adds is found: here one made up, on a copy of the
// state.
func TestAFieldWithoutADecisionFails(t *testing.T) {
	type withMore struct {
		engine.State
		Later any `json:"later,omitempty"`
	}
	require.Equal(t, []string{"later"}, undecided(reflect.TypeFor[withMore](), stateFields))
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
	for _, m := range []map[string]decision{unapprovedFields, waitingFields, claimantFields, approvalFields, guestFields} {
		for field, d := range m {
			require.Contains(t, []action{keep, remove}, d.action, "%s: a field of an entry of a list goes with its entry", field)
		}
	}
	for field, d := range routeFields {
		require.True(t, d.action == keep || d.by == "routes", "%s: a field of a route goes with its route, or the case of the routes filters it", field)
	}
	for _, m := range []map[string]decision{identityFields, claimFields, routeTrafficFields} {
		for field, d := range m {
			if d.action != keep {
				require.NotEmpty(t, d.by, "%s says what filters it", field)
			}
		}
	}
	for _, field := range []string{"copies", "tenants"} {
		require.True(t, cases[identityFields[field].by], "%s is filtered by a case of the state", field)
	}
	for _, m := range clusterTables {
		for field, d := range m {
			if d.action != keep {
				require.True(t, cases[d.by], "%s is filtered by a case of the state", field)
			}
		}
	}
}

// A field the daemon adds to what the other answers carry is found as well.
func TestAFieldOfAnotherAnswerWithoutADecisionFails(t *testing.T) {
	type withOwners struct {
		engine.ClaimView
		Owners []string `json:"owners"`
	}
	require.Equal(t, []string{"owners"}, undecided(reflect.TypeFor[withOwners](), claimFields))
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

// clusterTables are the tables of the views of the cluster and of what a
// route says of its replicas and placement, which the filters go by.
var clusterTables = []map[string]decision{
	leaseFields, nodeFields, clusterFields, blockFields, coverageFields, upgradeFields, caRotationFields,
	replicaFields, placementFields, moveFields,
}

// A field of a view that has no decision is cleared for a reader, as a
// removed one is: the filters go by the tables, so a field the daemon adds
// reaches no reader before it is given a decision.
func TestAFieldWithoutADecisionIsCleared(t *testing.T) {
	type view struct {
		Kept    string `json:"kept"`
		Removed string `json:"removed"`
		Later   string `json:"later"`
		Hidden  string `json:"-"`
	}
	v := view{Kept: "a", Removed: "b", Later: "c", Hidden: "d"}
	cleared(&v, map[string]decision{"kept": kept, "removed": removedBy("nodes")})
	require.Equal(t, view{Kept: "a", Hidden: "d"}, v)
}

// What the tables of the views of the cluster say is what the filter does,
// field by field: kept as it is, removed to nothing.
func TestTheClusterFiltersDoWhatTheirTablesSay(t *testing.T) {
	in := withCluster(t)
	in.Routes[0].Placement = &engine.PlacementView{Tunnel: "pco-abc123-pve2", Kind: engine.TunnelPinned, Node: "pve2", Why: "pinned",
		Move: &engine.MoveView{From: "pco-abc123", To: "pco-abc123-pve2", Step: "dns", Since: t0}}
	in.Cluster.Upgrade = &engine.UpgradeView{From: "1.3.0", Since: t0, Older: []string{"pve3"}}
	in.Cluster.CARotation = &engine.CARotationView{Node: "pve3", StartedAt: t0, Done: []string{"pve1"}, Next: "pve2"}
	got := FilterState(in, sees(101))

	check := func(want, have any, decided map[string]decision) {
		t.Helper()
		w, h := reflect.ValueOf(want), reflect.ValueOf(have)
		for i := range w.NumField() {
			name, ok := jsonName(w.Type().Field(i))
			if !ok {
				continue
			}
			switch decided[name].action {
			case keep:
				require.Equal(t, w.Field(i).Interface(), h.Field(i).Interface(), "%s.%s is kept", w.Type(), name)
			case remove:
				require.True(t, h.Field(i).IsZero(), "%s.%s is removed", w.Type(), name)
			}
		}
	}
	check(*in.Lease, *got.Lease, leaseFields)
	for i := range in.Nodes {
		check(in.Nodes[i], got.Nodes[i], nodeFields)
	}
	check(*in.Cluster, *got.Cluster, clusterFields)
	check(in.Cluster.Blocks[0], got.Cluster.Blocks[0], blockFields)
	check(in.Cluster.Coverage, got.Cluster.Coverage, coverageFields)
	check(*in.Cluster.Upgrade, *got.Cluster.Upgrade, upgradeFields)
	check(*in.Cluster.CARotation, *got.Cluster.CARotation, caRotationFields)
	check(in.Routes[0], got.Routes[0], routeFields)
	check(in.Routes[0].Replicas[0], got.Routes[0].Replicas[0], replicaFields)
	check(*in.Routes[0].Placement, *got.Routes[0].Placement, placementFields)
	check(*in.Routes[0].Placement.Move, *got.Routes[0].Placement.Move, moveFields)
}
