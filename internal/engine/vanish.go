package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Vanish guard: how many of the guests that hold a hostname may drop out of
// the listing at once before the cycle holds for the admin's confirmation.
const (
	vanishMax      = 5
	vanishPercent  = 30
	vanishExamples = 5
)

// guardVanished holds the cycle when the guests that hold a claim drop out of
// a complete listing in numbers that look like a failure rather than their
// removal: Proxmox lists no guest at all, or more than vanishMax of them and
// more than vanishPercent per cent of them are gone. A guest that only lost
// its tag or its route is still listed. Guests the admin confirmed gone do not
// count until they are listed again.
func (c *cycleRun) guardVanished() bool {
	listed := make(map[model.GuestRef]bool, len(c.snap.Guests))
	for _, g := range c.snap.Guests {
		listed[g.Ref] = true
	}
	maps.DeleteFunc(c.e.gone, func(ref model.GuestRef, _ bool) bool { return listed[ref] })

	holders := make(map[model.GuestRef]bool)
	for _, claim := range c.stored {
		if ref, err := model.ParseGuestRef(claim.Owner); err == nil {
			holders[ref] = true
		}
	}
	var vanished []model.GuestRef
	for ref := range holders {
		if !listed[ref] && !c.e.gone[ref] {
			vanished = append(vanished, ref)
		}
	}
	slices.SortFunc(vanished, func(a, b model.GuestRef) int { return model.CompareOwners(a.String(), b.String()) })
	c.e.vanished = vanished

	switch {
	case len(vanished) == 0:
		return true
	case len(c.snap.Guests) == 0:
		c.problem("Proxmox lists no guest at all, but %d guests hold a hostname (%s); nothing is changed: "+
			"check the privileges of the Proxmox API token, or run pco apply --confirm-deletes if they were removed on purpose",
			len(vanished), examples(vanished))
		return false
	case len(vanished) > vanishMax && 100*len(vanished) > vanishPercent*len(holders):
		c.problem("%d of %d guests that hold a hostname are no longer listed by Proxmox (%s); nothing is changed "+
			"until they are listed again, or run pco apply --confirm-deletes if they were removed on purpose",
			len(vanished), len(holders), examples(vanished))
		return false
	}
	return true
}

// examples names the first few guests, and how many more there are.
func examples(refs []model.GuestRef) string {
	names := make([]string, 0, vanishExamples)
	for _, ref := range refs[:min(len(refs), vanishExamples)] {
		names = append(names, ref.String())
	}
	out := strings.Join(names, ", ")
	if n := len(refs) - vanishExamples; n > 0 {
		out += fmt.Sprintf(" and %d more", n)
	}
	return out
}
