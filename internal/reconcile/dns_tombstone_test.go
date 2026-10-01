package reconcile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

func TestDNSGraceSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	gone := f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	store := &memStore{}
	key := stoneKey(zone1.ID, "gone.example.com")

	res := newDNS(f, store, t0).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, dnsWrites(f))
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "grace period: 1m0s left", true)}, withoutDetail(res.Actions))
	require.Equal(t, map[string]Tombstone{key: watched(t0, t0)}, store.m)

	res = newDNS(f, store, t0.Add(30*time.Second)).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, dnsWrites(f))
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "grace period: 30s left", true)}, withoutDetail(res.Actions))
	require.Equal(t, map[string]Tombstone{key: watched(t0, t0.Add(30*time.Second))}, store.m, "the grace runs from the first sighting")

	res = newDNS(f, store, t0.Add(61*time.Second)).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	calls := f.Calls()
	require.Equal(t, []string{"Records zone1", "DeleteRecord zone1 " + gone.ID}, calls[len(calls)-2:], "read again right before the delete")
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "", true)}, withoutDetail(res.Actions))
	require.Empty(t, f.RecordsIn(zone1.ID))
	require.Empty(t, store.m)
	require.Equal(t, 4, store.saves, "one per run, and in the last a second one after the delete")
}

func TestDNSWantedAgainClearsTombstone(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("app.example.com", testTunnelID))
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "app.example.com"): watched(t0.Add(-30*time.Second), t0.Add(-30*time.Second))}}

	res := newDNS(f, store, t0).Run(ctx, dnsIn("app.example.com"), Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, res.Actions)
	require.Empty(t, store.m)
	require.Equal(t, 1, store.saves)

	later := t0.Add(2 * time.Minute)
	res = newDNS(f, store, later).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, dnsWrites(f), "the grace starts again")
	require.Equal(t, []Action{dnsAction(DeleteRecord, "app.example.com", "grace period: 1m0s left", true)}, withoutDetail(res.Actions))
	require.Equal(t, map[string]Tombstone{stoneKey(zone1.ID, "app.example.com"): watched(later, later)}, store.m)
}

func TestDNSTombstoneOfAnotherGeneration(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	key := stoneKey(zone1.ID, "gone.example.com")
	earlier := overdue
	earlier.Generation = ours.Generation - 1
	store := &memStore{m: map[string]Tombstone{key: earlier}}

	res := newDNS(f, store, t0).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, dnsWrites(f), "a grace another writer started does not count")
	requireHeld(t, res.Actions, "grace period: 1m0s left")
	require.Equal(t, map[string]Tombstone{key: watched(t0, t0)}, store.m)

	res = newDNS(f, store, t0.Add(time.Minute)).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	require.Len(t, callsTo(f, "DeleteRecord"), 1)
}

// TestDNSGraceRestartsAfterBreak starts a tombstone at t0, wants the name
// again at t0+20s in a run that may not be able to clear the tombstone, and
// drops the name again later. No delete may come before a full new grace.
func TestDNSGraceRestartsAfterBreak(t *testing.T) {
	cases := []struct {
		name   string
		resume time.Duration // when the name is unwanted again
		wanted func(f *cffake.Fake, store *memStore, in *DNSInput) Mode
	}{
		{"observe mode", 7 * 24 * time.Hour, func(_ *cffake.Fake, _ *memStore, _ *DNSInput) Mode { return Observe }},
		{"inventory incomplete", 7 * 24 * time.Hour, func(_ *cffake.Fake, _ *memStore, in *DNSInput) Mode {
			in.InventoryOK = false
			return Enforce
		}},
		{"listing fails", 7 * 24 * time.Hour, func(f *cffake.Fake, _ *memStore, _ *DNSInput) Mode {
			f.FailNext("dns.read", 1, forbidden)
			return Enforce
		}},
		{"save fails", 7 * 24 * time.Hour, func(_ *cffake.Fake, store *memStore, _ *DNSInput) Mode {
			store.saveErrs = map[int]error{store.saves + 1: errors.New("disk full")}
			return Enforce
		}},
		{"seen wanted within the gap", 40 * time.Second, func(_ *cffake.Fake, _ *memStore, _ *DNSInput) Mode { return Enforce }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			store := &memStore{}

			newDNS(f, store, t0).Run(ctx, dnsIn(), Enforce)
			require.Len(t, store.m, 1)

			in := dnsIn("gone.example.com")
			mode := tc.wanted(f, store, &in)
			newDNS(f, store, t0.Add(20*time.Second)).Run(ctx, in, mode)

			resume := t0.Add(tc.resume)
			for _, after := range []time.Duration{0, 30 * time.Second, 59 * time.Second} {
				res := newDNS(f, store, resume.Add(after)).Run(ctx, dnsIn(), Enforce)
				require.Empty(t, dnsWrites(f), "%s after the name was unwanted again", after)
				requireHeld(t, res.Actions, fmt.Sprintf("grace period: %s left", time.Minute-after))
			}
			newDNS(f, store, resume.Add(time.Minute)).Run(ctx, dnsIn(), Enforce)
			require.Len(t, callsTo(f, "DeleteRecord"), 1, "deleted once the new grace is over")
		})
	}
}

func TestDNSClockSteppedBack(t *testing.T) {
	cases := []struct {
		name  string
		stone Tombstone
	}{
		{"grace began in the future", watched(t0.Add(time.Hour), t0.Add(-30*time.Second))},
		{"last seen in the future", watched(t0.Add(-time.Hour), t0.Add(time.Hour))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			key := stoneKey(zone1.ID, "gone.example.com")
			store := &memStore{m: map[string]Tombstone{key: tc.stone}}

			res := newDNS(f, store, t0).Run(ctx, dnsIn(), Enforce)
			require.Empty(t, dnsWrites(f))
			requireHeld(t, res.Actions, "grace period: 1m0s left")
			require.Equal(t, map[string]Tombstone{key: watched(t0, t0)}, store.m)

			newDNS(f, store, t0.Add(time.Minute)).Run(ctx, dnsIn(), Enforce)
			require.Len(t, callsTo(f, "DeleteRecord"), 1, "held for one grace, not until the clock catches up")
		})
	}
}

func TestDNSInventoryIncompleteHolds(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	f.SeedRecord(zone1.ID, ourCNAME("old.example.com", testTunnelID))
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "web.example.com", Content: "192.0.2.10"})
	kept := map[string]Tombstone{
		stoneKey(zone1.ID, "gone.example.com"):     overdue,
		stoneKey(zone1.ID, "vanished.example.com"): overdue, // its record is gone
		stoneKey("zone9", "old.example.org"):       watched(t0.Add(-40*24*time.Hour), t0.Add(-40*24*time.Hour)),
	}
	store := &memStore{m: maps.Clone(kept)}
	store.m[stoneKey(zone1.ID, "app.example.com")] = overdue // wanted again
	in := dnsIn("app.example.com", "web.example.com")
	in.Adopt = map[string]bool{"web.example.com": true}
	in.InventoryOK = false

	res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{"CreateRecord zone1 app.example.com"}, dnsWrites(f), "creates go on, deletes wait")
	require.Equal(t, []Action{
		dnsAction(CreateRecord, "app.example.com", "", false),
		dnsAction(DeleteRecord, "gone.example.com", "inventory incomplete", true),
		dnsAction(DeleteRecord, "old.example.com", "inventory incomplete", true),
		dnsAction(DeleteRecord, "web.example.com", "inventory incomplete", true),
		dnsAction(CreateRecord, "web.example.com", "inventory incomplete", true),
	}, withoutDetail(res.Actions))
	require.Equal(t, kept, store.m, "only the tombstone of the wanted name is dropped; none is confirmed or started")
}

func TestDNSStillUnwanted(t *testing.T) {
	cases := []struct {
		name      string
		answer    func(context.Context, string) (bool, error)
		keepStone bool
		held      string
		problem   string
	}{
		{name: "no confirmation", keepStone: true, held: "no inventory confirmation"},
		{
			name:   "published again",
			answer: func(context.Context, string) (bool, error) { return false, nil },
		},
		{
			name:      "inventory cannot answer",
			answer:    func(context.Context, string) (bool, error) { return false, errors.New("node unreachable") },
			keepStone: true,
			held:      "inventory confirmation failed",
			problem:   "gone.example.com in zone example.com: asking the inventory before deleting: node unreachable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			key := stoneKey(zone1.ID, "gone.example.com")
			store := &memStore{m: map[string]Tombstone{key: overdue}}
			in := dnsIn()
			var asked []string
			if tc.answer != nil {
				in.StillUnwanted = func(ctx context.Context, name string) (bool, error) {
					calls := f.Calls()
					require.Equal(t, "Records zone1", calls[len(calls)-1], "asked after the record was read again")
					asked = append(asked, name)
					return tc.answer(ctx, name)
				}
			} else {
				in.StillUnwanted = nil
			}

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, dnsWrites(f))
			require.Len(t, f.RecordsIn(zone1.ID), 1)
			if tc.answer != nil {
				require.Equal(t, []string{"gone.example.com"}, asked)
			}
			if tc.keepStone {
				require.Contains(t, store.m, key)
			} else {
				require.Empty(t, store.m, "the name is wanted again")
			}
			if tc.held != "" {
				requireHeld(t, res.Actions, tc.held)
			} else {
				require.Empty(t, res.Actions)
			}
			if tc.problem != "" {
				require.Equal(t, []string{tc.problem}, res.Problems)
			} else {
				require.Empty(t, res.Problems)
			}
		})
	}
}

func TestDNSTombstoneLoadFailureStopsRun(t *testing.T) {
	f := newDNSFake()
	store := &memStore{loadErr: errors.New("disk full")}

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "loading dns tombstones: disk full")
	require.Empty(t, f.Calls())
	require.Empty(t, res.Actions)
}

func TestDNSPreDeleteSaveFails(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	f.SeedRecord(zone1.ID, probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)))
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "web.example.com", Content: "192.0.2.10"})
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}, saveErrs: map[int]error{1: errors.New("disk full")}}
	in := dnsIn("app.example.com", "web.example.com")
	in.Adopt = map[string]bool{"web.example.com": true}

	res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

	require.Equal(t, []string{"saving dns tombstones: disk full"}, res.Problems)
	require.Equal(t, []string{"CreateRecord zone1 app.example.com"}, dnsWrites(f), "creates go on, no delete of any kind")
	require.Equal(t, []Action{
		dnsAction(DeleteRecord, "_pco-probe-x.example.com", "tombstones not saved", true),
		dnsAction(CreateRecord, "app.example.com", "", false),
		dnsAction(DeleteRecord, "gone.example.com", "tombstones not saved", true),
		dnsAction(DeleteRecord, "web.example.com", "tombstones not saved", true),
		dnsAction(CreateRecord, "web.example.com", "tombstones not saved", true),
	}, withoutDetail(res.Actions))
	require.Equal(t, 1, store.saves, "no second try after the deletes that did not happen")
}

func TestDNSSaveAfterDeletesFails(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}, saveErrs: map[int]error{2: errors.New("disk full")}}

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Equal(t, []string{"saving dns tombstones: disk full"}, res.Problems)
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "", true)}, withoutDetail(res.Actions))
	require.Empty(t, f.RecordsIn(zone1.ID), "the delete stays done")
}

func TestDNSMaxGapSetting(t *testing.T) {
	cases := []struct {
		name    string
		gap     time.Duration // since the tombstone was last confirmed
		deleted int
	}{
		{"within the gap", 10 * time.Minute, 1},
		{"beyond the gap", 10*time.Minute + time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): watched(t0.Add(-time.Hour), t0.Add(-tc.gap))}}
			s := DNSSettings{InstallID: testInstall, MaxGap: 10 * time.Minute}
			r := NewDNSReconciler(Clients{"cred1": f}, store, writerOf(ours, ours), s, (&clock{t0}).now, zerolog.Nop())

			r.Run(context.Background(), dnsIn(), Enforce)

			require.Len(t, callsTo(f, "DeleteRecord"), tc.deleted)
		})
	}
}

func TestDNSDeleteFailureKeepsTombstone(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	key := stoneKey(zone1.ID, "gone.example.com")
	store := &memStore{m: map[string]Tombstone{key: overdue}}
	f.Deny("dns.write")

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "gone.example.com in zone example.com: deleting the record")
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", forbidden.Error(), true)}, withoutDetail(res.Actions))
	require.Equal(t, overdue.Since, store.m[key].Since)
	require.Len(t, f.RecordsIn(zone1.ID), 1)
}

func TestDNSDeleteNotFoundCountsAsDeleted(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}
	s := &dnsSpy{API: f, deleteErr: notFound}

	res := newDNS(s, store, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, 1, s.deletes)
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "", true)}, withoutDetail(res.Actions))
	require.Empty(t, store.m)
}

func TestDNSTwoOwnedRecordsAtOneName(t *testing.T) {
	f := newDNSFake()
	first := f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "gone.example.com", Content: "192.0.2.10", Comment: testMarker})
	second := f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "gone.example.com", Content: "192.0.2.11", Comment: testMarker})
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}
	in := dnsIn()
	asked := 0
	in.StillUnwanted = func(context.Context, string) (bool, error) {
		asked++
		return true, nil
	}

	res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{"DeleteRecord zone1 " + first.ID, "DeleteRecord zone1 " + second.ID}, dnsWrites(f))
	require.Equal(t, 1, asked)
	require.Empty(t, store.m)
}

func TestDNSFreshReadBeforeDelete(t *testing.T) {
	cases := []struct {
		name      string
		change    func(rec cfapi.Record) []cfapi.Record // what the read right before the delete shows
		fail      bool
		keepStone bool
	}{
		{name: "marker removed meanwhile", change: func(rec cfapi.Record) []cfapi.Record {
			rec.Comment = "kept by hand"
			return []cfapi.Record{rec}
		}},
		{name: "type changed meanwhile", change: func(rec cfapi.Record) []cfapi.Record {
			rec.Type, rec.Content = "A", "192.0.2.10"
			return []cfapi.Record{rec}
		}},
		{name: "name changed meanwhile", change: func(rec cfapi.Record) []cfapi.Record {
			rec.Name = "other.example.com"
			return []cfapi.Record{rec}
		}},
		{name: "vanished", change: func(cfapi.Record) []cfapi.Record { return nil }},
		{name: "read fails", fail: true, keepStone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			gone := f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			key := stoneKey(zone1.ID, "gone.example.com")
			store := &memStore{m: map[string]Tombstone{key: overdue}}
			s := &dnsSpy{API: f}
			s.lookup = func(_ string, filter cfapi.RecordFilter) error {
				if tc.fail && filter.Name == "gone.example.com" {
					return errors.New("connection reset by peer")
				}
				return nil
			}
			s.edit = func(filter cfapi.RecordFilter, got []cfapi.Record) []cfapi.Record {
				if filter.Name == "gone.example.com" {
					return tc.change(gone)
				}
				return got
			}

			res := newDNS(s, store, t0).Run(context.Background(), dnsIn(), Enforce)

			require.Empty(t, dnsWrites(f))
			require.Empty(t, res.Actions)
			require.Len(t, f.RecordsIn(zone1.ID), 1)
			if tc.fail {
				require.Len(t, res.Problems, 1)
				require.Contains(t, res.Problems[0], "gone.example.com in zone example.com: reading the records again before deleting them")
			} else {
				require.Empty(t, res.Problems)
			}
			if tc.keepStone {
				require.Contains(t, store.m, key)
			} else {
				require.Empty(t, store.m)
			}
		})
	}
}

func TestDNSTombstoneHousekeeping(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("kept.example.com", testTunnelID))
	store := &memStore{m: map[string]Tombstone{
		stoneKey(zone1.ID, "kept.example.com"):  watched(t0.Add(-10*time.Second), t0.Add(-10*time.Second)), // record there, in its grace
		stoneKey(zone1.ID, "gone.example.com"):  overdue,                                                   // record gone
		stoneKey("zone9", "old.example.org"):    watched(t0.Add(-31*24*time.Hour), t0.Add(-31*24*time.Hour)),
		stoneKey("zone9", "recent.example.org"): watched(t0.Add(-29*24*time.Hour), t0.Add(-29*24*time.Hour)),
		stoneKey(zone2.ID, "unread.shop.cz"):    watched(t0.Add(-31*24*time.Hour), t0.Add(-31*24*time.Hour)), // its zone cannot be read
	}}
	s := &dnsSpy{API: f, lookup: func(zoneID string, _ cfapi.RecordFilter) error {
		if zoneID == zone2.ID {
			return forbidden
		}
		return nil
	}}

	res := newDNS(s, store, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "zone shop.cz")
	require.Equal(t, map[string]Tombstone{
		stoneKey(zone1.ID, "kept.example.com"):  watched(t0.Add(-10*time.Second), t0),
		stoneKey("zone9", "recent.example.org"): watched(t0.Add(-29*24*time.Hour), t0.Add(-29*24*time.Hour)),
		stoneKey(zone2.ID, "unread.shop.cz"):    watched(t0.Add(-31*24*time.Hour), t0.Add(-31*24*time.Hour)),
	}, store.m)
}
