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
// zone that could not be listed counts with every tombstone in it, whoever
// wrote it, as one owned record each, and as one pending record unless it is
// confirmed: otherwise a listing that keeps failing, or a new writer, could
// split a mass delete into parts that each pass.
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
			all, unconfirmed := run.stonesIn(z.ID)
			owned += all
			pending += unconfirmed
			unlisted += unconfirmed
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
	guard := fmt.Sprintf("mass delete guard: %d of %d records are being removed", pending, owned)
	if unlisted > 0 {
		guard += fmt.Sprintf(" (%d in zones that could not be listed)", unlisted)
	}
	guard += "; confirm to proceed"
	run.problem(guard)
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

// stonesIn counts the tombstones of a zone, and those of them that are not
// confirmed.
func (run *dnsRun) stonesIn(zoneID string) (all, unconfirmed int) {
	for key, t := range run.stones.m {
		if strings.HasPrefix(key, zoneID+"/") {
			all++
			if !t.Confirmed {
				unconfirmed++
			}
		}
	}
	return all, unconfirmed
}
