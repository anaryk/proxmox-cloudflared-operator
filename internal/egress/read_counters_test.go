package egress

import (
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// counting returns the listing with the counting sets holding the targets
// with the packets given.
func (l listing) counting(t *testing.T, counted map[Target]int) listing {
	t.Helper()
	return l.edit(t, func(l listing) listing {
		elems := map[string][]any{}
		for _, x := range sortTargets(slices.Collect(maps.Keys(counted))) {
			elems[flowSet(x)] = append(elems[flowSet(x)], countedElement(x, counted[x]))
		}
		for _, name := range []string{setFlows4, setFlows6} {
			s := l.object(t, "set", name)
			delete(s, "elem")
			if v := elems[name]; len(v) > 0 {
				s["elem"] = v
			}
		}
		return l
	})
}

// testdata/listing-counters.json is the table of a node, for the connector
// uid 64123, after the connector opened ten connections to 10.77.9.2:8080,
// one to 10.77.9.2:8081 that carried a hundred requests, and three to
// [fd77:9::2]:8080.
func TestReadCountersReadsTheHandleOfTheTableAndTheConnectionsToEachTarget(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "listing-counters.json"))
	require.NoError(t, err)
	n := &fakeNft{}
	n.setLive(b)

	table, flows, err := ReadCounters(t.Context(), n)

	require.NoError(t, err)
	require.Equal(t, uint64(249), table)
	require.Equal(t, []TargetFlows{
		{Target: target("10.77.9.2:8080"), Flows: 10},
		{Target: target("10.77.9.2:8081"), Flows: 1},
		{Target: target("[fd77:9::2]:8080"), Flows: 3},
	}, flows)

	live, err := ReadLive(t.Context(), n, 64123)
	require.NoError(t, err)
	require.Empty(t, live.Differences, "the table is as pco loads it")
}

func TestReadCountersOfBothFamiliesAndKinds(t *testing.T) {
	tg := []Target{nodeTarget("10.0.0.2:8006"), target("10.0.0.5:80"), target("[fd00::5]:443")}
	n := &fakeNft{}
	n.setLive(realListing(t, "1.1.3").with(t, tg, nil).counting(t, map[Target]int{
		target("10.0.0.2:8006"): 4, target("10.0.0.5:80"): 7, target("[fd00::5]:443"): 1,
	}).bytes(t))

	table, flows, err := ReadCounters(t.Context(), n)

	require.NoError(t, err)
	require.Equal(t, uint64(3), table)
	require.Equal(t, []TargetFlows{
		{Target: nodeTarget("10.0.0.2:8006"), Flows: 4},
		{Target: target("10.0.0.5:80"), Flows: 7},
		{Target: target("[fd00::5]:443"), Flows: 1},
	}, flows, "a target of allowNode is marked as the filter holds it")
}

func TestReadCountersOfATableWithoutTargets(t *testing.T) {
	n := &fakeNft{}
	n.setLive(realListing(t, "1.0.6").with(t, nil, nil).bytes(t))

	table, flows, err := ReadCounters(t.Context(), n)

	require.NoError(t, err)
	require.Equal(t, uint64(3), table)
	require.Empty(t, flows)
}

func TestReadCountersRefusesWhatItCannotCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		live func(t *testing.T) []byte
		want string
	}{
		{"a table of an older pco", func(t *testing.T) []byte { return older(t, "1.1.3").bytes(t) }, "no set flows4"},
		{"a counting set without counters", func(t *testing.T) []byte {
			return realListing(t, "1.1.3").edit(t, func(l listing) listing {
				s := l.object(t, "set", setFlows6)
				delete(s, "stmt")
				return l
			}).bytes(t)
		}, "set flows6 has no counters"},
		{"an element without a counter", func(t *testing.T) []byte {
			return realListing(t, "1.1.3").edit(t, func(l listing) listing {
				l.object(t, "set", setFlows4)["elem"] = []any{map[string]any{"concat": []any{"10.0.0.5", 80}}}
				return l
			}).bytes(t)
		}, "set flows4 holds 10.0.0.5:80 without a counter"},
		{"an element pco cannot read", func(t *testing.T) []byte {
			return realListing(t, "1.1.3").edit(t, func(l listing) listing {
				l.object(t, "set", setFlows4)["elem"] = []any{map[string]any{"elem": map[string]any{
					"val": map[string]any{"range": []any{"10.0.0.0", "10.0.0.9"}}, "counter": map[string]any{"packets": 1, "bytes": 60},
				}}}
				return l
			}).bytes(t)
		}, "set flows4 holds an element pco cannot read"},
		{"a table without a handle", func(t *testing.T) []byte {
			return realListing(t, "1.1.3").edit(t, func(l listing) listing {
				for _, e := range l {
					if table, ok := e["table"].(map[string]any); ok {
						delete(table, "handle")
					}
				}
				return l
			}).bytes(t)
		}, "the table has no handle"},
		{"no listing at all", func(*testing.T) []byte { return []byte("{") }, "cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &fakeNft{}
			n.setLive(tc.live(t))

			_, _, err := ReadCounters(t.Context(), n)

			require.ErrorIs(t, err, ErrUnreadable)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestReadCountersOfATableOfAnOlderPcoSaysItHasNoCountingSets(t *testing.T) {
	n := &fakeNft{}
	n.setLive(older(t, "1.0.6").bytes(t))

	_, _, err := ReadCounters(t.Context(), n)

	require.ErrorIs(t, err, ErrNoCounters)
}

func TestReadCountersOfAMissingTable(t *testing.T) {
	n := &fakeNft{listErr: ErrNotLoaded}

	_, _, err := ReadCounters(t.Context(), n)

	require.ErrorIs(t, err, ErrNotLoaded)
}

// The listing is read with its bound: nft that prints more is cut short, and
// what is cut short cannot be read.
func TestReadCountersOfAListingOverTheBound(t *testing.T) {
	n, bin := newFakeNftBinary(t)
	require.NoError(t, os.WriteFile(bin+".big", nil, 0o644))

	_, _, err := ReadCounters(t.Context(), n)

	require.ErrorIs(t, err, ErrUnreadable)
	require.Equal(t, "-j list table inet pco_egress\n", readText(t, bin+".args"), "one call")
}

func TestReadCountersListsTheTableOnce(t *testing.T) {
	n, bin := newFakeNftBinary(t)

	_, _, err := ReadCounters(t.Context(), n)

	require.ErrorIs(t, err, ErrUnreadable, "the fake prints an empty listing")
	require.Equal(t, "-j list table inet pco_egress\n", readText(t, bin+".args"))
}

func TestCountersCarryTheHandleOfTheTableAsTheirGeneration(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	tg := targets("10.0.0.5:80", "10.0.0.5:8080", "10.0.0.6:443")
	live := realListing(t, "1.0.6").counting(t, map[Target]int{tg[0]: 2, tg[1]: 0, tg[2]: 5})
	n.setLive(live.bytes(t))

	first, err := f.Counters(t.Context())
	require.NoError(t, err)
	require.Equal(t, []TargetFlows{
		{Target: tg[0], Flows: 2, Generation: Generation{Table: 3}},
		{Target: tg[1], Flows: 0, Generation: Generation{Table: 3}},
		{Target: tg[2], Flows: 5, Generation: Generation{Table: 3}},
	}, first)

	again, err := f.Counters(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, again, "the same table is the same generation")

	// The table rendered anew is another table, with a handle of its own;
	// its sets get the handles they had, since those count within the table.
	rerendered := live.edit(t, func(l listing) listing {
		l.object(t, "table", tableName)["handle"] = 9
		return l
	})
	for _, name := range []string{setFlows4, setFlows6, setTargets4} {
		require.Equal(t, live.object(t, "set", name)["handle"], rerendered.object(t, "set", name)["handle"], name)
	}
	n.setLive(rerendered.bytes(t))

	next, err := f.Counters(t.Context())
	require.NoError(t, err)
	require.Len(t, next, 3)
	for i := range next {
		require.Equal(t, Generation{Table: 9}, next[i].Generation)
		require.NotEqual(t, first[i].Generation, next[i].Generation)
	}
}

func TestFlowCountsAreTheCountersByAddressAndPort(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	b, err := os.ReadFile(filepath.Join("testdata", "listing-counters.json"))
	require.NoError(t, err)
	n.setLive(b)

	generation, counts, err := f.FlowCounts(t.Context())

	require.NoError(t, err)
	require.Equal(t, "249", generation, "the handle of the table")
	require.Equal(t, map[netip.AddrPort]uint64{
		netip.MustParseAddrPort("10.77.9.2:8080"):   10,
		netip.MustParseAddrPort("10.77.9.2:8081"):   1,
		netip.MustParseAddrPort("[fd77:9::2]:8080"): 3,
	}, counts)

	n.listErr = ErrNotLoaded
	_, _, err = f.FlowCounts(t.Context())
	require.ErrorIs(t, err, ErrNotLoaded)
}

func TestCountersReturnAFailedRead(t *testing.T) {
	f, n, _, _ := newTestFilter(t)
	n.listErr = errBoom

	_, err := f.Counters(t.Context())

	require.ErrorIs(t, err, errBoom)
}
