package registry

import (
	"fmt"
	"slices"
)

// Admit decides the routing admission of a node that joins. failing are the
// shared routes the node does not reach, reachedElsewhere the ones another
// fresh active replica does. The routes in both move to a pinned tunnel first,
// and the node waits until they are there, so that Cloudflare never sends a
// share of a route's requests to a replica that cannot serve it. A route no
// replica reaches has nowhere to move to and does not hold the node back.
//
// More than maxMoves routes keep the node joining, with nothing moved, until an
// admin admits it: a node with no networking configured yet fails every route,
// and moving them all would be an outage. The same call, made every cycle,
// says wait until the moves are done and then that there is none.
func Admit(joining string, failing, reachedElsewhere []string, maxMoves int) (moves []string, wait bool, why string) {
	reached := make(map[string]bool, len(reachedElsewhere))
	for _, r := range reachedElsewhere {
		reached[r] = true
	}
	for _, r := range failing {
		if reached[r] {
			moves = append(moves, r)
		}
	}
	slices.Sort(moves)
	moves = slices.Compact(moves)

	switch n := len(moves); {
	case n == 0:
		return nil, false, ""
	case n > maxMoves:
		return nil, true, fmt.Sprintf("%s fails %s that other replicas reach, more than cluster.admissionMaxMoves allows (%d); "+
			"it stays joining until pco node admit", joining, routesOf(n), maxMoves)
	default:
		return moves, true, fmt.Sprintf("%s fails %s that other replicas reach; they move to pinned tunnels before it becomes active",
			joining, routesOf(n))
	}
}

func routesOf(n int) string {
	if n == 1 {
		return "1 shared route"
	}
	return fmt.Sprintf("%d shared routes", n)
}
