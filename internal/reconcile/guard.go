package reconcile

import (
	"fmt"
	"strings"
)

// decideGuard holds every delete due in this run when they are many, both in
// number and as a share of the records of this install, unless the admin
// confirmed them. Adoptions and probes are not counted.
//
// A zone that could not be listed counts with every tombstone of this writer
// in it, however old, as one due record each: otherwise a listing that keeps
// failing could split a mass delete into parts that each pass.
func (run *dnsRun) decideGuard(zones []*dnsZone) {
	if run.stones == nil {
		return
	}
	due, owned := 0, 0
	for _, z := range zones {
		if !z.listed {
			n := run.ourStonesIn(z.ID)
			due += n
			owned += n
			continue
		}
		for name, records := range z.owned {
			owned += len(records)
			if held, unwanted := z.holds[name]; unwanted && held == "" {
				due += len(records)
			}
		}
	}
	s := run.r.s
	if run.in.ConfirmDeletes || due <= s.MaxDeletes || float64(due) <= s.MaxDeleteShare*float64(owned) {
		return
	}
	guard := fmt.Sprintf("mass delete guard: %d of %d records", due, owned)
	for _, z := range zones {
		for name, held := range z.holds {
			if held == "" {
				z.holds[name] = guard
			}
		}
	}
	run.r.log.Warn().Int("due", due).Int("owned", owned).Msg("holding dns deletes until they are confirmed")
}

// ourStonesIn counts the tombstones of this writer in a zone.
func (run *dnsRun) ourStonesIn(zoneID string) int {
	n := 0
	for key, t := range run.stones.m {
		if strings.HasPrefix(key, zoneID+"/") && run.byUs(t) {
			n++
		}
	}
	return n
}
