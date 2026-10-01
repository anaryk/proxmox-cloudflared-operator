package reconcile

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

func TestDNSMassDeleteGuard(t *testing.T) {
	cases := []struct {
		name     string
		owned    int
		unwanted int
		confirm  bool
		adoptA   bool // also adopt a name held by an A record, whose delete is not counted
		deleted  int
		held     string
	}{
		{name: "ten of ten held", owned: 10, unwanted: 10, held: "mass delete guard: 10 of 10 records are being removed; confirm to proceed"},
		{name: "ten of ten confirmed", owned: 10, unwanted: 10, confirm: true, deleted: 10},
		{name: "two of ten", owned: 10, unwanted: 2, deleted: 2},
		{name: "five of ten is not more than five", owned: 10, unwanted: 5, deleted: 5},
		{name: "six of ten held", owned: 10, unwanted: 6, held: "mass delete guard: 6 of 10 records are being removed; confirm to proceed"},
		{name: "six of twenty is not more than the share", owned: 20, unwanted: 6, deleted: 6},
		{name: "six of twenty-five", owned: 25, unwanted: 6, deleted: 6},
		{name: "five of five is not more than five", owned: 5, unwanted: 5, deleted: 5},
		{name: "adoption deletes are not counted", owned: 10, unwanted: 5, adoptA: true, deleted: 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{m: map[string]Tombstone{}}
			in := dnsIn()
			in.ConfirmDeletes = tc.confirm
			for i := range tc.owned {
				name := fmt.Sprintf("h%02d.example.com", i)
				f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
				if i < tc.unwanted {
					store.m[stoneKey(zone1.ID, name)] = overdue
					continue
				}
				in.Records = append(in.Records, wantRecord(zone1, name))
			}
			if tc.adoptA {
				f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "web.example.com", Content: "192.0.2.10"})
				in.Records = append(in.Records, wantRecord(zone1, "web.example.com"))
				in.Adopt = map[string]bool{"web.example.com": true}
			}
			before := sinceOf(store.m)

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Len(t, callsTo(f, "DeleteRecord"), tc.deleted)
			if tc.held == "" {
				require.Empty(t, res.Problems)
				require.Empty(t, store.m)
				return
			}
			require.Equal(t, []string{tc.held}, res.Problems)
			require.Len(t, res.Actions, tc.unwanted)
			requireHeld(t, res.Actions, tc.held)
			require.Equal(t, before, sinceOf(store.m), "held deletes keep their grace")
		})
	}
}

func TestDNSMassDeleteGuardAcrossZones(t *testing.T) {
	f := newDNSFake()
	store := &memStore{m: map[string]Tombstone{}}
	for i := range 3 {
		for _, z := range []ZoneRef{zone1, zone2} {
			name := fmt.Sprintf("h%d.%s", i, z.Name)
			f.SeedRecord(z.ID, ourCNAME(name, testTunnelID))
			store.m[stoneKey(z.ID, name)] = overdue
		}
	}

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Empty(t, callsTo(f, "DeleteRecord"), "three deletes per zone are six in the run")
	require.Len(t, res.Actions, 6)
	requireHeld(t, res.Actions, "mass delete guard: 6 of 6 records are being removed; confirm to proceed")
}

// unlistable returns a client that cannot list zone2 while the returned flag
// is set.
func unlistable(f *cffake.Fake) (*dnsSpy, *bool) {
	failing := true
	return &dnsSpy{API: f, lookup: func(zoneID string, _ cfapi.RecordFilter) error {
		if zoneID == zone2.ID && failing {
			return forbidden
		}
		return nil
	}}, &failing
}

// fiveAndFive seeds five records of ours in each zone, all unwanted with
// overdue tombstones.
func fiveAndFive(f *cffake.Fake) *memStore {
	store := &memStore{m: map[string]Tombstone{}}
	for i := range 5 {
		for _, z := range []ZoneRef{zone1, zone2} {
			name := fmt.Sprintf("h%d.%s", i, z.Name)
			f.SeedRecord(z.ID, ourCNAME(name, testTunnelID))
			store.m[stoneKey(z.ID, name)] = overdue
		}
	}
	return store
}

func TestDNSMassDeleteGuardCountsUnlistedZones(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	store := fiveAndFive(f)
	s, failing := unlistable(f)

	res := newDNSWith(s, store, writerOf(ours, ours), t0).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, callsTo(f, "DeleteRecord"), "the zone that cannot be listed still counts")
	held := "mass delete guard: 10 of 10 records are being removed (5 in zones that could not be listed); confirm to proceed"
	requireHeld(t, res.Actions, held)
	require.Contains(t, res.Problems, held)

	*failing = false
	res = newDNSWith(s, store, writerOf(ours, ours), t0.Add(30*time.Second)).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, callsTo(f, "DeleteRecord"))
	requireHeld(t, res.Actions, "mass delete guard: 10 of 10 records are being removed; confirm to proceed")

	in := dnsIn()
	in.ConfirmDeletes = true
	newDNSWith(s, store, writerOf(ours, ours), t0.Add(60*time.Second)).Run(ctx, in, Enforce)
	require.Len(t, callsTo(f, "DeleteRecord"), 10)
}

func TestDNSMassDeleteGuardUnlistedZoneOverTime(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	store := fiveAndFive(f)
	s, _ := unlistable(f)
	c := &clock{t0}
	r := newDNSAt(s, store, writerOf(ours, ours), c)

	// Once a minute for ten minutes: the tombstones of the zone that cannot be
	// listed are confirmed by no run and grow far older than MaxGap.
	for at := time.Duration(0); at <= 10*time.Minute; at += time.Minute {
		c.t = t0.Add(at)
		res := r.Run(ctx, dnsIn(), Enforce)
		require.Empty(t, callsTo(f, "DeleteRecord"), "%s on", at)
		requireHeld(t, res.Actions, "mass delete guard: 10 of 10 records are being removed (5 in zones that could not be listed); confirm to proceed")
	}
}

// TestDNSMassDeleteGuardRecovery lets the zone that could not be listed come
// back with its graces restarted: what is due is still part of a mass delete.
func TestDNSMassDeleteGuardRecovery(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	store := fiveAndFive(f)
	s, failing := unlistable(f)
	c := &clock{t0}
	r := newDNSAt(s, store, writerOf(ours, ours), c)

	for at := time.Duration(0); at <= 5*time.Minute; at += 30 * time.Second {
		*failing = at <= 150*time.Second
		c.t = t0.Add(at)
		res := r.Run(ctx, dnsIn(), Enforce)
		require.Empty(t, callsTo(f, "DeleteRecord"), "%s on", at)
		require.Len(t, res.Problems, 1+boolInt(*failing), "%s on: %v", at, res.Problems)
		require.True(t, strings.HasPrefix(res.Problems[len(res.Problems)-1], "mass delete guard: 10 of 10 records are being removed"))
	}

	in := dnsIn()
	in.ConfirmDeletes = true
	c.t = t0.Add(330 * time.Second)
	r.Run(ctx, in, Enforce)
	require.Len(t, callsTo(f, "DeleteRecord"), 10, "one confirmation lets them go")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestDNSMassDeleteGuardFailover has another writer take over while a zone
// cannot be listed: the tombstones there are not its own, and still count.
func TestDNSMassDeleteGuardFailover(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	store := fiveAndFive(f)
	s, _ := unlistable(f)
	c := &clock{t0}
	r := NewDNSReconciler(Clients{"cred1": s}, store, writerOf(newer, newer), DNSSettings{InstallID: testInstall}, c.now, zerolog.Nop())

	for _, at := range []time.Duration{0, 30 * time.Second, 61 * time.Second, 2 * time.Minute} {
		c.t = t0.Add(at)
		res := r.Run(ctx, dnsIn(), Enforce)
		require.Empty(t, callsTo(f, "DeleteRecord"), "%s on", at)
		require.Contains(t, res.Problems,
			"mass delete guard: 10 of 10 records are being removed (5 in zones that could not be listed); confirm to proceed")
	}
}

func TestDNSMassDeleteGuardShareWithUnlistedZone(t *testing.T) {
	f := newDNSFake()
	store := &memStore{m: map[string]Tombstone{}}
	in := dnsIn()
	for i := range 20 {
		name := fmt.Sprintf("h%02d.example.com", i)
		f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
		if i == 0 {
			store.m[stoneKey(zone1.ID, name)] = overdue
			continue
		}
		in.Records = append(in.Records, wantRecord(zone1, name))
	}
	for i := range 5 {
		store.m[stoneKey(zone2.ID, fmt.Sprintf("h%d.shop.cz", i))] = watched(t0.Add(-48*time.Hour), t0.Add(-24*time.Hour))
	}
	s, _ := unlistable(f)

	newDNS(s, store, t0).Run(context.Background(), in, Enforce)

	require.Len(t, callsTo(f, "DeleteRecord"), 1, "6 pending of 25 is not more than the share")
}

func TestDNSMassDeleteGuardCountsTombstonesOfOtherWriters(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *Tombstone)
	}{
		{"another generation", func(t *Tombstone) { t.Generation = ours.Generation - 1 }},
		{"another nonce", func(t *Tombstone) { t.Nonce = "n9" }},
		{"confirmed", func(t *Tombstone) { t.Confirmed = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{m: map[string]Tombstone{}}
			for i := range 2 {
				name := fmt.Sprintf("h%d.example.com", i)
				f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
				store.m[stoneKey(zone1.ID, name)] = overdue
			}
			other := overdue
			tc.change(&other)
			for i := range 5 {
				store.m[stoneKey(zone2.ID, fmt.Sprintf("h%d.shop.cz", i))] = other
			}
			s, _ := unlistable(f)

			res := newDNS(s, store, t0).Run(context.Background(), dnsIn(), Enforce)

			require.Empty(t, callsTo(f, "DeleteRecord"), "every tombstone of a zone that cannot be listed counts")
			requireHeld(t, res.Actions, "mass delete guard: 7 of 7 records are being removed (5 in zones that could not be listed); confirm to proceed")
		})
	}
}

// TestDNSConfirmationCoversPendingRemovals unpublishes six names of ten
// within one grace: the guard holds them, one confirmation lets each go as its
// grace ends, and a name unpublished after it is not covered.
func TestDNSConfirmationCoversPendingRemovals(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	names := make([]string, 10)
	for i := range names {
		names[i] = fmt.Sprintf("h%d.example.com", i)
		f.SeedRecord(zone1.ID, ourCNAME(names[i], testTunnelID))
	}
	wantAllBut := func(unwanted int) DNSInput {
		in := dnsIn(names[unwanted:]...)
		return in
	}
	store := &memStore{}
	c := &clock{t0}
	r := newDNSAt(f, store, writerOf(ours, ours), c)
	stone := func(i int) Tombstone { return store.m[stoneKey(zone1.ID, names[i])] }

	res := r.Run(ctx, wantAllBut(3), Enforce)
	require.Empty(t, res.Problems, "three are not many")

	c.t = t0.Add(20 * time.Second)
	res = r.Run(ctx, wantAllBut(6), Enforce)
	require.Equal(t, []string{"mass delete guard: 6 of 10 records are being removed; confirm to proceed"}, res.Problems,
		"six in their grace are many")

	c.t = t0.Add(30 * time.Second)
	in := wantAllBut(6)
	in.ConfirmDeletes = true
	res = r.Run(ctx, in, Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, callsTo(f, "DeleteRecord"), "none is due yet")
	for i := range 6 {
		require.True(t, stone(i).Confirmed, names[i])
	}

	c.t = t0.Add(40 * time.Second)
	r.Run(ctx, wantAllBut(7), Enforce)
	require.False(t, stone(6).Confirmed, "unpublished after the confirmation")

	c.t = t0.Add(61 * time.Second)
	r.Run(ctx, wantAllBut(7), Enforce)
	require.Len(t, callsTo(f, "DeleteRecord"), 3, "the first three, without a second confirmation")

	c.t = t0.Add(81 * time.Second)
	r.Run(ctx, wantAllBut(7), Enforce)
	require.Len(t, callsTo(f, "DeleteRecord"), 6)
}

func TestDNSConfirmedRemovalsPassTheGuard(t *testing.T) {
	f := newDNSFake()
	store := &memStore{m: map[string]Tombstone{}}
	confirmed := overdue
	confirmed.Confirmed = true
	var confirmedIDs []string
	for i := range 20 {
		name := fmt.Sprintf("h%02d.example.com", i)
		rec := f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
		switch {
		case i < 6:
			store.m[stoneKey(zone1.ID, name)] = confirmed
			confirmedIDs = append(confirmedIDs, "DeleteRecord zone1 "+rec.ID)
		case i < 13:
			store.m[stoneKey(zone1.ID, name)] = overdue
		}
	}
	in := dnsIn()
	for i := 13; i < 20; i++ {
		in.Records = append(in.Records, wantRecord(zone1, fmt.Sprintf("h%02d.example.com", i)))
	}

	res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

	held := "mass delete guard: 7 of 20 records are being removed; confirm to proceed"
	require.Equal(t, []string{held}, res.Problems)
	require.Equal(t, confirmedIDs, dnsWrites(f), "the confirmed ones go, the seven others wait")
	for _, a := range res.Actions {
		if !a.Applied {
			require.Equal(t, held, a.Held)
		}
	}
}

func TestDNSConfirmationLostWhenGraceRestarts(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *Tombstone)
	}{
		{"another writer", func(t *Tombstone) { t.Generation = ours.Generation - 1 }},
		{"break in observation", func(t *Tombstone) { t.Seen = t0.Add(-time.Hour) }},
		{"clock stepped back", func(t *Tombstone) { t.Since = t0.Add(time.Hour) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			key := stoneKey(zone1.ID, "gone.example.com")
			stone := overdue
			stone.Confirmed = true
			tc.change(&stone)
			store := &memStore{m: map[string]Tombstone{key: stone}}

			res := newDNS(f, store, t0).Run(context.Background(), dnsIn(), Enforce)

			requireHeld(t, res.Actions, "grace period: 1m0s left")
			require.Equal(t, watched(t0, t0), store.m[key], "a new grace, not confirmed")
		})
	}
}

func TestDNSConfirmDeletesLiftsOnlyTheGuard(t *testing.T) {
	cases := []struct {
		name   string
		stone  Tombstone
		change func(in *DNSInput)
		held   string
	}{
		{"grace not over", watched(t0.Add(-30*time.Second), t0.Add(-30*time.Second)), func(*DNSInput) {}, "grace period: 30s left"},
		{"inventory incomplete", overdue, func(in *DNSInput) { in.InventoryOK = false }, "inventory incomplete"},
		{"no inventory confirmation", overdue, func(in *DNSInput) { in.StillUnwanted = nil }, "no inventory confirmation"},
		{"inventory publishes the name", overdue, func(in *DNSInput) {
			in.StillUnwanted = func(context.Context, string) (bool, error) { return false, nil }
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{m: map[string]Tombstone{}}
			for i := range 10 {
				name := fmt.Sprintf("h%d.example.com", i)
				f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
				store.m[stoneKey(zone1.ID, name)] = tc.stone
			}
			in := dnsIn()
			in.ConfirmDeletes = true
			tc.change(&in)

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, callsTo(f, "DeleteRecord"))
			if tc.held != "" {
				requireHeld(t, res.Actions, tc.held)
			}
		})
	}
}

func TestDNSSettingsApply(t *testing.T) {
	s := DNSSettings{InstallID: testInstall, Grace: 5 * time.Minute, MaxDeletes: 1, MaxDeleteShare: 0.1}
	guard := "mass delete guard: 2 of 10 records are being removed; confirm to proceed"
	cases := []struct {
		name     string
		since    time.Duration // how long before the run the names were first seen unwanted
		unwanted int           // of 10 records
		deleted  int
		held     string
	}{
		{"in the longer grace", 2 * time.Minute, 1, 0, "grace period: 3m0s left"},
		{"grace just over", 5 * time.Minute, 1, 1, ""},
		{"two deletes are many", 6 * time.Minute, 2, 0, guard},
		{"two deletes just due are many", 5 * time.Minute, 2, 0, guard},
		{"two in their grace are many", 2 * time.Minute, 2, 0, "grace period: 3m0s left"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{m: map[string]Tombstone{}}
			in := dnsIn()
			for i := range 10 {
				name := fmt.Sprintf("h%02d.example.com", i)
				f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
				if i < tc.unwanted {
					store.m[stoneKey(zone1.ID, name)] = watched(t0.Add(-tc.since), t0.Add(-30*time.Second))
					continue
				}
				in.Records = append(in.Records, wantRecord(zone1, name))
			}
			r := NewDNSReconciler(Clients{"cred1": f}, store, writerOf(ours, ours), s, (&clock{t0}).now, zerolog.Nop())

			res := r.Run(context.Background(), in, Enforce)

			if tc.unwanted > 1 {
				require.Equal(t, []string{guard}, res.Problems)
			} else {
				require.Empty(t, res.Problems)
			}
			require.Len(t, callsTo(f, "DeleteRecord"), tc.deleted)
			require.Len(t, res.Actions, tc.unwanted)
			for _, a := range res.Actions {
				require.Equal(t, tc.held, a.Held)
			}
		})
	}
}
