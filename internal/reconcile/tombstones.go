package reconcile

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

const (
	// probeAge is how old a probe record must be before it counts as left
	// behind; a younger one may belong to a check still running.
	probeAge = 10 * time.Minute

	// tombstoneAge is how long the tombstone of a zone the run no longer
	// manages is kept.
	tombstoneAge = 30 * 24 * time.Hour
)

// TombstoneStore persists when an owned record was first seen unwanted.
type TombstoneStore interface {
	Load(ctx context.Context) (map[string]time.Time, error) // key: zoneID + "/" + record name
	Save(ctx context.Context, m map[string]time.Time) error
}

// tombstones are the tombstones of one run.
type tombstones struct {
	m       map[string]time.Time
	changed bool
}

// tombstoneKey is the key of the tombstone of a name in a zone.
func tombstoneKey(zoneID, name string) string { return zoneID + "/" + strings.ToLower(name) }

func (t *tombstones) drop(key string) {
	if _, ok := t.m[key]; ok {
		delete(t.m, key)
		t.changed = true
	}
}

// since returns when the name of key was first seen unwanted, marking it now
// when it was not seen before.
func (t *tombstones) since(key string, now time.Time) time.Time {
	at, ok := t.m[key]
	if !ok {
		at = now
		t.m[key] = at
		t.changed = true
	}
	return at
}

// loadTombstones reads the tombstones and reports whether the run may go on:
// without them the grace of no record is known.
func (run *dnsRun) loadTombstones(ctx context.Context) bool {
	m, err := run.r.store.Load(ctx)
	if err != nil {
		run.problem(fmt.Sprintf("loading dns tombstones: %v; changing no dns record", err))
		return false
	}
	run.stones = &tombstones{m: maps.Clone(m)}
	if run.stones.m == nil {
		run.stones.m = make(map[string]time.Time)
	}
	return true
}

// keepsTombstones reports whether this run may change tombstones: only an
// enforcing run that knows the whole inventory may.
func (run *dnsRun) keepsTombstones() bool { return run.stones != nil && run.in.InventoryOK }

// saveTombstones drops the old tombstones of zones the run does not manage and
// stores the tombstones when they changed.
func (run *dnsRun) saveTombstones(ctx context.Context, zones []*dnsZone) {
	if !run.keepsTombstones() {
		return
	}
	managed := make(map[string]bool, len(zones))
	for _, z := range zones {
		managed[z.ID] = true
	}
	for key, at := range run.stones.m {
		zoneID, _, _ := strings.Cut(key, "/")
		if !managed[zoneID] && run.now.Sub(at) > tombstoneAge {
			run.stones.drop(key)
		}
	}
	if !run.stones.changed {
		return
	}
	if err := run.r.store.Save(ctx, run.stones.m); err != nil {
		run.problem(fmt.Sprintf("saving dns tombstones: %v", err))
	}
}

// forgetSettled drops the tombstones of a listed zone whose name is wanted
// again or holds no record of this install any more.
func (run *dnsRun) forgetSettled(z *dnsZone) {
	for key := range run.stones.m {
		name, ok := strings.CutPrefix(key, z.ID+"/")
		if ok && (z.isWanted(name) || len(z.owned[name]) == 0) {
			run.stones.drop(key)
		}
	}
}

// due reports whether the records of this install at an unwanted name have
// passed their grace.
func (run *dnsRun) due(z *dnsZone, name string) bool {
	if !run.keepsTombstones() || z.isWanted(name) {
		return false
	}
	at, ok := run.stones.m[tombstoneKey(z.ID, name)]
	return ok && run.graceLeft(at) <= 0
}

// graceLeft returns how long the grace of a name first seen unwanted at at
// still runs.
func (run *dnsRun) graceLeft(at time.Time) time.Duration { return at.Add(run.r.s.Grace).Sub(run.now) }

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

// retire deletes the records of this install at a name nobody wants any more,
// unless the inventory, the mode, the grace or the mass delete guard holds
// them.
func (run *dnsRun) retire(ctx context.Context, z *dnsZone, name string) {
	records := z.owned[name]
	if held := run.retireHold(z, name); held != "" {
		for _, rec := range records {
			z.add(deleteAction(z, rec), held)
		}
		return
	}

	// The listing may be old by now: delete only what a fresh read still shows
	// as ours and unwanted.
	fresh, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: name})
	if err != nil {
		run.problem(fmt.Sprintf("%s: reading the records again before deleting them: %v", z.about(name), err))
		return
	}
	done := true
	for _, rec := range records {
		i := slices.IndexFunc(fresh, func(f cfapi.Record) bool { return f.ID == rec.ID })
		if i < 0 || !run.owns(fresh[i]) || z.isWanted(fresh[i].Name) {
			continue
		}
		id := fresh[i].ID
		if !run.write(z, deleteAction(z, fresh[i]), func() error { return deleteRecord(ctx, z, id) }) {
			done = false
		}
	}
	if done {
		run.stones.drop(tombstoneKey(z.ID, name))
	}
}

// retireHold says why the records at an unwanted name are not deleted in this
// run, starting their grace when it has not started yet. It is empty when they
// may be deleted.
func (run *dnsRun) retireHold(z *dnsZone, name string) string {
	switch {
	case !run.in.InventoryOK:
		return heldInventory
	case run.mode == Observe:
		return heldObserve
	}
	at := run.stones.since(tombstoneKey(z.ID, name), run.now)
	if left := run.graceLeft(at); left > 0 {
		return fmt.Sprintf("grace period: %s left", left)
	}
	return run.guard
}

func deleteAction(z *dnsZone, rec cfapi.Record) Action {
	return Action{Kind: DeleteRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: %s %s", z.Name, rec.Type, rec.Content), Destructive: true}
}

// isProbe reports whether a record is one the credential check leaves behind
// when it cannot delete it.
func isProbe(rec cfapi.Record, marker string) bool { return rec.Comment == marker+" probe" }

// sweepProbes deletes probe records old enough to be left behind. They have no
// grace and do not count towards the mass delete guard.
func (run *dnsRun) sweepProbes(ctx context.Context, z *dnsZone) {
	for _, rec := range z.probes {
		if run.now.Sub(rec.ModifiedOn) <= probeAge {
			continue
		}
		a := Action{Kind: DeleteRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: %s probe left behind", z.Name, rec.Type), Destructive: true}
		run.write(z, a, func() error { return deleteRecord(ctx, z, rec.ID) })
	}
}
