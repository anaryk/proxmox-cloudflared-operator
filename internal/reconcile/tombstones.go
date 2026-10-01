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
	Seen       time.Time `json:"seen"`       // last confirmed by a run; moves on at most every MaxGap/4
	Generation int       `json:"generation"` // generation of the writer that created it
	Nonce      string    `json:"nonce"`      // nonce of the writer that created it

	// Confirmed is set when the admin confirmed the removal while it was
	// pending, that is in a run that found the tombstone already there: it
	// then passes the mass delete guard. A grace that starts again starts
	// unconfirmed.
	Confirmed bool `json:"confirmed,omitempty"`
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

// byUs reports whether this writer, its generation and its nonce, made a
// tombstone.
func (run *dnsRun) byUs(t Tombstone) bool {
	return t.Generation == run.us.Generation && t.Nonce == run.us.Nonce
}

// continuous reports whether a tombstone was watched without a break by this
// writer: made by it, with a start, not dated in the future, and confirmed
// within MaxGap (which a zero Seen never is). Any other grace starts again, as
// nobody can tell what happened to the name in between.
func (run *dnsRun) continuous(t Tombstone) bool {
	return run.byUs(t) && !t.Since.IsZero() &&
		!t.Since.After(run.now) && !t.Seen.After(run.now) &&
		run.now.Sub(t.Seen) <= run.r.s.MaxGap
}

// seenUnwanted records that the name of key is unwanted now and returns its
// tombstone, whose grace starts now, unconfirmed, unless it was watched
// without a break. Seen moves on only once it is more than a quarter of
// MaxGap old, so that a held delete does not rewrite the store on every run.
// A run with ConfirmDeletes confirms the removal only when the tombstone was
// there before and keeps its grace: the admin confirmed what the guard
// showed, not what this run finds for the first time.
func (run *dnsRun) seenUnwanted(key string) Tombstone {
	old, ok := run.stones.m[key]
	t := old
	kept := ok && run.continuous(old)
	switch {
	case !kept:
		t = Tombstone{Since: run.now, Seen: run.now, Generation: run.us.Generation, Nonce: run.us.Nonce}
	case run.now.Sub(t.Seen) > run.r.s.MaxGap/4:
		t.Seen = run.now
	}
	if kept && run.in.ConfirmDeletes && !t.Confirmed {
		t.Confirmed = true
		run.res.Confirmed++
	}
	if !ok || t != old {
		run.stones.set(key, t)
	}
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
// this run; the one at the end also tries again what an earlier one could
// not save.
//
// Once the store holds everything the run has, the names seen wanted are
// forgotten: the run dropped their tombstones, and the store now has none of
// them, or a new one this run started.
func (run *dnsRun) saveTombstones(ctx context.Context, beforeDeletes bool) {
	if run.stones == nil {
		return
	}
	if run.stones.changed {
		if !run.fenced("saving dns tombstones") {
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
	clear(run.r.wantedSinceSave)
}
