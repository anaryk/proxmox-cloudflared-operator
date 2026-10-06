package registry

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func numbered(name string, index int) store.NodeEntry {
	return store.NodeEntry{Name: name, Index: &index}
}

func unnumbered(name string) store.NodeEntry { return store.NodeEntry{Name: name} }

// full is a registry whose entries hold every index there is to give.
func full() []store.NodeEntry {
	var entries []store.NodeEntry
	for i := 0; i <= MaxIndex; i++ {
		entries = append(entries, numbered(fmt.Sprintf("pve%d", i), i))
	}
	return entries
}

func TestTheFirstNodeGetsIndexZero(t *testing.T) {
	got, err := AssignIndex(nil, "pve1", nil)
	require.NoError(t, err)
	require.Equal(t, 0, got)
}

func TestTheNodeRegisteredBeforeTheClusterReleaseGetsZeroToo(t *testing.T) {
	got, err := AssignIndex([]store.NodeEntry{unnumbered("pve1")}, "pve1", nil)
	require.NoError(t, err)
	require.Equal(t, 0, got)
}

func TestTheNextNodeGetsTheLowestFreeIndex(t *testing.T) {
	entries := []store.NodeEntry{numbered("pve1", 0), numbered("pve2", 1)}
	got, err := AssignIndex(entries, "pve3", nil)
	require.NoError(t, err)
	require.Equal(t, 2, got)
}

func TestAGapIsFilledBeforeTheListGrows(t *testing.T) {
	entries := []store.NodeEntry{numbered("pve1", 0), numbered("pve2", 1), numbered("pve4", 3)}
	got, err := AssignIndex(entries, "pve5", nil)
	require.NoError(t, err)
	require.Equal(t, 2, got)
}

func TestANodeThatHasAnIndexKeepsIt(t *testing.T) {
	entries := []store.NodeEntry{numbered("pve1", 0), numbered("pve2", 7)}
	got, err := AssignIndex(entries, "pve2", nil)
	require.NoError(t, err)
	require.Equal(t, 7, got)
}

func TestTheIndexANameHadBeforeIsTheOneItGetsBack(t *testing.T) {
	entries := []store.NodeEntry{numbered("pve1", 0), numbered("pve2", 1)}
	got, err := AssignIndex(entries, "pve3", map[string]int{"pve3": 9})
	require.NoError(t, err)
	require.Equal(t, 9, got)
}

func TestTheEntryOfARemovedNodeKeepsItsIndexReserved(t *testing.T) {
	gone := numbered("pve2", 1)
	gone.State = "removed"
	entries := []store.NodeEntry{numbered("pve1", 0), gone}

	got, err := AssignIndex(entries, "pve3", nil)
	require.NoError(t, err)
	require.Equal(t, 2, got, "a node that may be enrolled again under its name must find its index free")

	got, err = AssignIndex(entries, "pve2", nil)
	require.NoError(t, err)
	require.Equal(t, 1, got)
}

func TestAnIndexThatAnotherNameHadIsNotGivenAway(t *testing.T) {
	entries := []store.NodeEntry{numbered("pve1", 0)}
	got, err := AssignIndex(entries, "pve3", map[string]int{"pve2": 1})
	require.NoError(t, err)
	require.Equal(t, 2, got)

	got, err = AssignIndex(entries, "pve2", map[string]int{"pve2": 1})
	require.NoError(t, err)
	require.Equal(t, 1, got)
}

func TestAnIndexAnotherNodeHoldsNowIsNotTheOneAPreviousNameGetsBack(t *testing.T) {
	entries := []store.NodeEntry{numbered("pve1", 0), numbered("pve2", 1)}
	got, err := AssignIndex(entries, "pve3", map[string]int{"pve3": 1})
	require.NoError(t, err)
	require.Equal(t, 2, got)
}

func TestAPreviousIndexOutsideTheRangeIsNotGivenBack(t *testing.T) {
	for _, previous := range []int{-1, 255, 256, 1000} {
		t.Run(fmt.Sprint(previous), func(t *testing.T) {
			got, err := AssignIndex(nil, "pve1", map[string]int{"pve1": previous})
			require.NoError(t, err)
			require.Equal(t, 0, got)
		})
	}
}

func Test255IsNeverGiven(t *testing.T) {
	entries := full()[:MaxIndex] // 0 to 253 taken
	got, err := AssignIndex(entries, "new", nil)
	require.NoError(t, err)
	require.Equal(t, 254, got)

	entries = full() // 0 to 254 taken
	_, err = AssignIndex(entries, "new", nil)
	require.ErrorIs(t, err, ErrNoFreeIndex)
}

func TestTheTwoHundredFiftySixthNodeIsRefused(t *testing.T) {
	entries := full()
	require.Len(t, entries, 255)

	_, err := AssignIndex(entries, "pve-256", nil)
	require.ErrorIs(t, err, ErrNoFreeIndex)
	require.ErrorContains(t, err, "pve-256")
	require.ErrorContains(t, err, "255")

	got, err := AssignIndex(entries, "pve200", nil)
	require.NoError(t, err, "a node that has its index keeps it however full the registry is")
	require.Equal(t, 200, got)
}

func TestTheNodesOwnEntryWithoutAnIndexTakesNoneFromTheList(t *testing.T) {
	entries := []store.NodeEntry{numbered("pve1", 0), unnumbered("pve2"), unnumbered("pve3")}
	got, err := AssignIndex(entries, "pve2", nil)
	require.NoError(t, err)
	require.Equal(t, 1, got)
}

func TestTwoNodesAskedOneAfterTheOtherGetDifferentIndexes(t *testing.T) {
	var entries []store.NodeEntry
	seen := map[int]string{}
	for i := range 40 {
		name := fmt.Sprintf("pve%d", i)
		got, err := AssignIndex(entries, name, nil)
		require.NoError(t, err)
		require.NotContains(t, seen, got)
		seen[got] = name
		entries = append(entries, numbered(name, got))
	}
	require.Equal(t, 0, mustIndex(t, entries, "pve0"))
	require.Equal(t, 39, mustIndex(t, entries, "pve39"))
}

func mustIndex(t *testing.T, entries []store.NodeEntry, name string) int {
	t.Helper()
	for _, e := range entries {
		if e.Name == name {
			require.NotNil(t, e.Index)
			return *e.Index
		}
	}
	t.Fatalf("no entry %s", name)
	return -1
}
