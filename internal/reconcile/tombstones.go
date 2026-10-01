package reconcile

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"
)

// tombstoneAge is how long the tombstone of a zone the run no longer
// manages is kept.
const tombstoneAge = 30 * 24 * time.Hour

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

// graceLeft returns how long the grace of a name first seen unwanted at at
// still runs.
func (run *dnsRun) graceLeft(at time.Time) time.Duration { return at.Add(run.r.s.Grace).Sub(run.now) }
