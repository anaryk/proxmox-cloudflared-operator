package reconcile

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"
)

// tombstoneAge is how long the tombstone of a zone the run no longer manages
// is kept.
const tombstoneAge = 30 * 24 * time.Hour

// Tombstone is the grace of the records of this install at a name nobody
// wants any more.
type Tombstone struct {
	Since      time.Time `json:"since"`      // first seen unwanted, start of the grace
	Seen       time.Time `json:"seen"`       // last run that confirmed it
	Generation int       `json:"generation"` // writer generation that created it
}

// TombstoneStore persists tombstones by zoneID + "/" + record name.
type TombstoneStore interface {
	Load(ctx context.Context) (map[string]Tombstone, error)
	Save(ctx context.Context, m map[string]Tombstone) error
}

// tombstones are the tombstones of one run.
type tombstones struct {
	m       map[string]Tombstone
	changed bool // since the last save
}

// tombstoneKey is the key of the tombstone of a name in a zone.
func tombstoneKey(zoneID, name string) string { return zoneID + "/" + strings.ToLower(name) }

func (t *tombstones) set(key string, stone Tombstone) {
	t.m[key] = stone
	t.changed = true
}

func (t *tombstones) drop(key string) {
	if _, ok := t.m[key]; ok {
		delete(t.m, key)
		t.changed = true
	}
}

// continuous reports whether a tombstone was watched without a break by this
// writer: created by its generation, not dated in the future, and confirmed
// within MaxGap. Any other grace starts again, as nobody can tell what
// happened to the name in between.
func (run *dnsRun) continuous(t Tombstone) bool {
	return t.Generation == run.us.Generation &&
		!t.Since.After(run.now) && !t.Seen.After(run.now) &&
		run.now.Sub(t.Seen) <= run.r.s.MaxGap
}

// confirm records that the name of key is unwanted now and returns its
// tombstone, whose grace starts now unless it was watched without a break.
func (run *dnsRun) confirm(key string) Tombstone {
	t, ok := run.stones.m[key]
	if !ok || !run.continuous(t) {
		t = Tombstone{Since: run.now, Generation: run.us.Generation}
	}
	t.Seen = run.now
	run.stones.set(key, t)
	return t
}

// graceLeft returns how long the grace of a tombstone still runs.
func (run *dnsRun) graceLeft(t Tombstone) time.Duration {
	return t.Since.Add(run.r.s.Grace).Sub(run.now)
}

// keepsTombstones reports whether this run may start or confirm tombstones:
// only an enforcing run that knows the whole inventory may.
func (run *dnsRun) keepsTombstones() bool { return run.stones != nil && run.in.InventoryOK }

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
		run.stones.m = make(map[string]Tombstone)
	}
	return true
}

// decide works out, before anything is written, which records of this
// install at unwanted names may be deleted in this run, and brings the
// tombstones up to date.
func (run *dnsRun) decide(zones []*dnsZone) {
	if run.stones != nil {
		// A wanted name loses its tombstone whatever else the run knows:
		// dropping one only makes a later delete wait longer.
		for _, rp := range run.in.Records {
			run.stones.drop(tombstoneKey(rp.ZoneID, rp.Name))
		}
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
	}
	run.decideGuard(zones)
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

// saveTombstones stores the tombstones when they changed since the last save.
// The save before the deletes must succeed for any record to be deleted in
// this run; after a failed one there is nothing to save afterwards.
func (run *dnsRun) saveTombstones(ctx context.Context, beforeDeletes bool) {
	if run.stones == nil || !run.stones.changed || run.noDeletes != "" || !run.fenced("saving dns tombstones") {
		return
	}
	if err := run.r.store.Save(ctx, maps.Clone(run.stones.m)); err != nil {
		run.problem(fmt.Sprintf("saving dns tombstones: %v", err))
		if beforeDeletes {
			run.noDeletes = heldUnsaved
		}
		return
	}
	run.stones.changed = false
}
