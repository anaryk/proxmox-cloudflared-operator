package reconcile

import "fmt"

// due reports whether the records of this install at an unwanted name have
// passed their grace.
func (run *dnsRun) due(z *dnsZone, name string) bool {
	if !run.keepsTombstones() || z.isWanted(name) {
		return false
	}
	at, ok := run.stones.m[tombstoneKey(z.ID, name)]
	return ok && run.graceLeft(at) <= 0
}

// decideGuard holds every delete due in this run when they are many, both in
// number and as a share of the records of this install, unless the admin
// confirmed them. Adoptions and probes are not counted.
func (run *dnsRun) decideGuard(zones []*dnsZone) {
	due, owned := 0, 0
	for _, z := range zones {
		for name, records := range z.owned {
			owned += len(records)
			if run.due(z, name) {
				due += len(records)
			}
		}
	}
	s := run.r.s
	if run.in.ConfirmDeletes || due <= s.MaxDeletes || float64(due) <= s.MaxDeleteShare*float64(owned) {
		return
	}
	run.guard = fmt.Sprintf("mass delete guard: %d of %d records", due, owned)
	run.r.log.Warn().Int("due", due).Int("owned", owned).Msg("holding dns deletes until they are confirmed")
}
