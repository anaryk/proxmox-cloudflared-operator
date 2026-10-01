package reconcile

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"
)

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

type tombstones struct {
	m       map[string]Tombstone
	changed bool // since the last save
}

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
