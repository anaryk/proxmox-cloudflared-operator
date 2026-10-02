package main

import (
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

const (
	// maxItemsShown is how many items of one thing that waits are listed.
	maxItemsShown = 20

	waitingTitle    = "Waits for a confirmation (pco apply --confirm-deletes accepts all of it):"
	unaffectedTitle = "Destructive actions that are pending; a confirmation does not affect them:"
)

// renderWaiting lists what waits for a confirmation, as the daemon offers it:
// the sentence of each, and the first maxItemsShown of its items.
func renderWaiting(s *screen, waiting []engine.Waiting) {
	for _, w := range waiting {
		s.printf("  - %s\n", w.Detail)
		for _, item := range w.Items[:min(len(w.Items), maxItemsShown)] {
			s.printf("      %s\n", item)
		}
		if more := len(w.Items) - maxItemsShown; more > 0 {
			s.printf("      ... and %d more\n", more)
		}
	}
}

// unaffected returns the destructive actions of the last cycle that were not
// carried out and that a confirmation does not let through: removals in their
// grace while the guard holds nothing, holds of observe-only mode, adoptions.
func unaffected(st engine.State) []reconcile.Action {
	offered := make(map[string]bool)
	for _, w := range st.Waiting {
		if w.Kind == engine.WaitingRemovals {
			for _, name := range w.Items {
				offered[name] = true
			}
		}
	}
	return slices.DeleteFunc(heldDeletes(st.Actions), func(a reconcile.Action) bool {
		return a.Kind == reconcile.DeleteRecord && offered[a.Target]
	})
}

// heldDeletes returns the destructive actions the last cycle did not carry
// out.
func heldDeletes(actions []reconcile.Action) []reconcile.Action {
	return slices.DeleteFunc(pending(actions), func(act reconcile.Action) bool { return !act.Destructive })
}
