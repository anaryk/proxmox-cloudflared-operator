package registry

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func routes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("app%02d.example.com", i)
	}
	return out
}

func TestANodeThatFailsNothingIsAdmittedAtOnce(t *testing.T) {
	moves, wait, why := Admit("pve2", nil, nil, 10)
	require.Empty(t, moves)
	require.False(t, wait)
	require.Empty(t, why)
}

func TestARouteThatFailsEverywhereDoesNotBlockTheNode(t *testing.T) {
	failing := []string{"down.example.com", "app.example.com"}
	moves, wait, why := Admit("pve2", failing, []string{"app.example.com"}, 10)
	require.Equal(t, []string{"app.example.com"}, moves)
	require.True(t, wait)
	require.NotEmpty(t, why)

	moves, wait, why = Admit("pve2", []string{"down.example.com"}, []string{"app.example.com"}, 10)
	require.Empty(t, moves)
	require.False(t, wait, "a route no replica reaches has nowhere to move to")
	require.Empty(t, why)
}

func TestARouteTheNodeReachesIsNoneOfItsConcern(t *testing.T) {
	moves, wait, _ := Admit("pve2", nil, routes(5), 10)
	require.Empty(t, moves)
	require.False(t, wait)
}

func TestTheRoutesToMoveAreTheFailingOnesAnotherReplicaReaches(t *testing.T) {
	failing := []string{"c.example.com", "a.example.com", "b.example.com", "x.example.com"}
	reached := []string{"b.example.com", "a.example.com", "c.example.com", "y.example.com"}
	moves, wait, why := Admit("pve2", failing, reached, 10)
	require.Equal(t, []string{"a.example.com", "b.example.com", "c.example.com"}, moves)
	require.True(t, wait, "the node waits for the moves to finish")
	require.Equal(t, "pve2 fails 3 shared routes that other replicas reach; they move to pinned tunnels before it becomes active", why)
}

func TestARouteListedTwiceMovesOnce(t *testing.T) {
	moves, _, _ := Admit("pve2", []string{"a.example.com", "a.example.com"}, []string{"a.example.com", "a.example.com"}, 10)
	require.Equal(t, []string{"a.example.com"}, moves)
}

func TestOneRouteIsOneRouteInTheReason(t *testing.T) {
	_, _, why := Admit("pve2", []string{"a.example.com"}, []string{"a.example.com"}, 10)
	require.Equal(t, "pve2 fails 1 shared route that other replicas reach; they move to pinned tunnels before it becomes active", why)
}

func TestTenMovesAreMadeAndElevenKeepTheNodeJoining(t *testing.T) {
	ten := routes(10)
	moves, wait, why := Admit("pve2", ten, ten, 10)
	require.Equal(t, ten, moves)
	require.True(t, wait)
	require.NotEmpty(t, why)

	eleven := routes(11)
	moves, wait, why = Admit("pve2", eleven, eleven, 10)
	require.Empty(t, moves, "past the limit nothing moves until an admin admits the node")
	require.True(t, wait)
	require.Equal(t, "pve2 fails 11 shared routes that other replicas reach, more than cluster.admissionMaxMoves allows (10); "+
		"it stays joining until pco node admit", why)
}

func TestTheLimitIsTheSettingsAndNotATen(t *testing.T) {
	r := routes(3)
	moves, _, _ := Admit("pve2", r, r, 3)
	require.Len(t, moves, 3)

	moves, wait, why := Admit("pve2", r, r, 2)
	require.Empty(t, moves)
	require.True(t, wait)
	require.Contains(t, why, "(2)")

	moves, wait, _ = Admit("pve2", nil, nil, 0)
	require.Empty(t, moves)
	require.False(t, wait, "nothing to move is within any limit")
}

func TestAdmitIsTheSameEveryCycleUntilTheMovesAreDone(t *testing.T) {
	failing := routes(4)
	first, _, _ := Admit("pve2", failing, failing, 10)
	second, _, _ := Admit("pve2", failing, failing, 10)
	require.Equal(t, first, second)

	moves, wait, _ := Admit("pve2", nil, failing, 10)
	require.Empty(t, moves, "once the routes are pinned the node no longer fails a shared one")
	require.False(t, wait)
}
