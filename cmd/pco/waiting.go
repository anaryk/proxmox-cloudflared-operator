package main

import (
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
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
			s.printf("      ... and %d more (pco plan --json shows all)\n", more)
		}
	}
}
