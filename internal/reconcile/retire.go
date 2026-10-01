package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// tombstoneAge is how long the tombstone of a zone the run no longer manages
// is kept.
const tombstoneAge = 30 * 24 * time.Hour

// decide works out, before anything is written, which records of this
// install at unwanted names may be deleted in this run, and brings the
// tombstones up to date.
func (run *dnsRun) decide(zones []*dnsZone) {
	if run.stones != nil {
		run.forgetWanted()
	}
	for _, z := range zones {
		if !z.listed {
			continue
		}
		z.holds = make(map[string]string)
		for name := range z.owned {
			if !z.isWanted(name) {
				z.holds[name] = run.retireHold(z, name)
			}
		}
		if run.keepsTombstones() {
			run.forgetGone(z)
		}
	}
	if run.keepsTombstones() {
		run.expire(zones)
		if run.in.ConfirmDeletes {
			run.confirmUnlisted(zones)
		}
	}
	run.decideGuard(zones)
}

// forgetWanted drops the tombstones of names seen wanted, whatever else the
// run knows: dropping one only makes a later delete wait longer. A name this
// run plans drops any tombstone. One remembered from an earlier run, which may
// have come from a process that was not the writer, drops only a tombstone of
// this writer: the tombstones of another writer restart wherever they are
// seen, and where they are not they keep the mass delete guard counting.
func (run *dnsRun) forgetWanted() {
	for _, rp := range run.in.Records {
		run.stones.drop(tombstoneKey(rp.ZoneID, rp.Name))
	}
	for key := range run.r.wantedSinceSave {
		if t, ok := run.stones.m[key]; ok && run.byUs(t) {
			run.stones.drop(key)
		}
	}
}

// confirmUnlisted confirms the removals of zones the run could not list: as
// long as such a zone stays unreadable nothing else ends them, and the guard
// would ask again for every later removal elsewhere.
func (run *dnsRun) confirmUnlisted(zones []*dnsZone) {
	for _, z := range zones {
		if z.listed {
			continue
		}
		for key, t := range run.stones.m {
			if strings.HasPrefix(key, z.ID+"/") && !t.Confirmed {
				t.Confirmed = true
				run.stones.set(key, t)
			}
		}
	}
}

// forgetGone drops the tombstones of a listed zone whose name holds no record
// of this install any more.
func (run *dnsRun) forgetGone(z *dnsZone) {
	for key := range run.stones.m {
		if name, ok := strings.CutPrefix(key, z.ID+"/"); ok && len(z.owned[name]) == 0 {
			run.stones.drop(key)
		}
	}
}

// expire drops the old tombstones of zones the run does not manage.
func (run *dnsRun) expire(zones []*dnsZone) {
	managed := make(map[string]bool, len(zones))
	for _, z := range zones {
		managed[z.ID] = true
	}
	for key, t := range run.stones.m {
		zoneID, _, _ := strings.Cut(key, "/")
		if !managed[zoneID] && run.now.Sub(t.Seen) > tombstoneAge {
			run.stones.drop(key)
		}
	}
}

// retireHold says why the records at an unwanted name may not be deleted in
// this run, confirming their tombstone when the run keeps tombstones. It is
// empty when they are due.
func (run *dnsRun) retireHold(z *dnsZone, name string) string {
	switch {
	case !run.in.InventoryOK:
		return heldInventory
	case run.mode == Observe:
		return heldObserve
	}
	t := run.confirm(tombstoneKey(z.ID, name))
	if left := run.graceLeft(t); left > 0 {
		return fmt.Sprintf("grace period: %s left", left)
	}
	return ""
}

// retire deletes the records of this install at a name nobody wants any more,
// when nothing holds them. Right before the delete it reads them again and
// asks the inventory once more.
func (run *dnsRun) retire(ctx context.Context, z *dnsZone, name string) {
	records := z.owned[name]
	held := z.holds[name]
	switch {
	case held != "":
	case run.noDeletes != "":
		held = run.noDeletes
	case run.stopped != "":
		held = run.stopped
	case run.in.StillUnwanted == nil:
		held = heldUnconfirmed
	case run.askFailed:
		held = heldAskFailed
	}
	if held != "" {
		for _, rec := range records {
			z.add(deleteAction(z, rec), held)
		}
		return
	}

	key := tombstoneKey(z.ID, name)
	var doomed []cfapi.Record
	for _, rec := range records {
		fresh, found, ours, err := run.recheck(ctx, z, rec)
		if err != nil {
			run.problem(fmt.Sprintf("%s: reading the record again before deleting it: %v", z.about(rec.Name), err))
			return
		}
		if found && ours {
			doomed = append(doomed, fresh)
		}
	}
	if len(doomed) == 0 {
		run.stones.drop(key) // gone, or no longer ours
		return
	}
	unwanted, err := run.in.StillUnwanted(ctx, name)
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: asking the inventory before deleting: %v", z.about(name), err))
		run.askFailed = true
		for _, rec := range doomed {
			z.add(deleteAction(z, rec), heldAskFailed)
		}
		return
	case !unwanted:
		// Published again: like a name the plan wants, it is remembered
		// until the drop is saved.
		run.stones.drop(key)
		run.r.wantedSinceSave[key] = true
		return
	}
	done := true
	for _, rec := range doomed {
		if !run.write(z, deleteAction(z, rec), func() error { return deleteRecord(ctx, z, rec.ID) }) {
			done = false
		}
	}
	if done {
		run.stones.drop(key)
	}
}

func deleteAction(z *dnsZone, rec cfapi.Record) Action {
	return Action{Kind: DeleteRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: %s %s", z.Name, rec.Type, rec.Content), Destructive: true}
}

// deleteRecord deletes a record; one that is gone already counts as deleted.
func deleteRecord(ctx context.Context, z *dnsZone, id string) error {
	if err := z.api.DeleteRecord(ctx, z.ID, id); err != nil && !cfapi.IsNotFound(err) {
		return err
	}
	return nil
}
