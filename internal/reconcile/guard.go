package reconcile

import (
	"fmt"
	"strings"
)

// decideGuard holds every delete due in this run when they are many, both in
// number and as a share of the records of this install, unless the admin
// confirmed them. Adoptions and probes are not counted.
//
// A zone that could not be listed counts with every tombstone this writer
// confirmed lately, as one due record each: otherwise a failing listing could
// split a mass delete into halves that each pass.
func (run *dnsRun) decideGuard(zones []*dnsZone) {
	if run.stones == nil {
		return
	}
	due, owned := 0, 0
	for _, z := range zones {
		if !z.listed {
			n := run.recentIn(z.ID)
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

// recentIn counts the tombstones of a zone that this writer confirmed within
// MaxGap.
func (run *dnsRun) recentIn(zoneID string) int {
	n := 0
	for key, t := range run.stones.m {
		if strings.HasPrefix(key, zoneID+"/") && t.Generation == run.us.Generation &&
			!t.Seen.After(run.now) && run.now.Sub(t.Seen) <= run.r.s.MaxGap {
			n++
		}
	}
	return n
}
