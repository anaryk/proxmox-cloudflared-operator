package reconcile

import (
	"fmt"
	"strings"
)

// decideGuard holds the due deletes of unconfirmed removals while many
// removals are pending, both in number and as a share of the records of this
// install. Adoptions and probes are not counted.
//
// A removal is pending from the moment its name is unwanted, in its grace or
// due, until the record is gone; one the admin confirmed is not counted. A
// zone that could not be listed counts with every unconfirmed tombstone in it,
// whoever wrote it, as one pending and one owned record each: otherwise a
// listing that keeps failing, or a new writer, could split a mass delete into
// parts that each pass. Its confirmed tombstones count as neither, so that a
// zone that stays broken does not dilute the share for the others.
//
// The guard speaks only where a confirmation can help: with an incomplete
// inventory every delete is held anyway.
func (run *dnsRun) decideGuard(zones []*dnsZone) {
	if !run.keepsTombstones() {
		return
	}
	pending, owned, unlisted := 0, 0, 0
	for _, z := range zones {
		if !z.listed {
			n := run.unconfirmedIn(z.ID)
			owned += n
			pending += n
			unlisted += n
			continue
		}
		for name, records := range z.owned {
			owned += len(records)
			if _, unwanted := z.holds[name]; unwanted && !run.confirmed(z, name) {
				pending += len(records)
			}
		}
	}
	s := run.r.s
	if pending <= s.MaxDeletes || float64(pending) <= s.MaxDeleteShare*float64(owned) {
		return
	}
	guard := fmt.Sprintf("%s: %d of %d records are being removed", HeldByGuard, pending, owned)
	if unlisted > 0 {
		guard += fmt.Sprintf(" (%d in zones that could not be listed)", unlisted)
	}
	guard += "; confirm to proceed"
	run.problem(guard)
	run.res.Guard = &GuardCount{Pending: pending, Owned: owned, Unlisted: unlisted}
	for _, z := range zones {
		for name, held := range z.holds {
			if held == "" && !run.confirmed(z, name) {
				z.holds[name] = guard
			}
		}
	}
}

func (run *dnsRun) confirmed(z *dnsZone, name string) bool {
	return run.stones.m[tombstoneKey(z.ID, name)].Confirmed
}

// unconfirmedIn counts the tombstones of a zone that are not confirmed.
func (run *dnsRun) unconfirmedIn(zoneID string) int {
	n := 0
	for key, t := range run.stones.m {
		if strings.HasPrefix(key, zoneID+"/") && !t.Confirmed {
			n++
		}
	}
	return n
}
