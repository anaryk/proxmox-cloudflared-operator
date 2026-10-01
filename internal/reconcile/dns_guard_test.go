package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
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
		{name: "ten of ten held", owned: 10, unwanted: 10, held: "mass delete guard: 10 of 10 records"},
		{name: "ten of ten confirmed", owned: 10, unwanted: 10, confirm: true, deleted: 10},
		{name: "two of ten", owned: 10, unwanted: 2, deleted: 2},
		{name: "six of ten held", owned: 10, unwanted: 6, held: "mass delete guard: 6 of 10 records"},
		{name: "six of twenty is not more than the share", owned: 20, unwanted: 6, deleted: 6},
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

			require.Empty(t, res.Problems)
			require.Len(t, callsTo(f, "DeleteRecord"), tc.deleted)
			if tc.held == "" {
				require.Empty(t, store.m)
				return
			}
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
	requireHeld(t, res.Actions, "mass delete guard: 6 of 6 records")
}

func TestDNSMassDeleteGuardCountsUnlistedZones(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	store := &memStore{m: map[string]Tombstone{}}
	for i := range 5 {
		for _, z := range []ZoneRef{zone1, zone2} {
			name := fmt.Sprintf("h%d.%s", i, z.Name)
			f.SeedRecord(z.ID, ourCNAME(name, testTunnelID))
			store.m[stoneKey(z.ID, name)] = overdue
		}
	}
	listing := true
	s := &dnsSpy{API: f, lookup: func(zoneID string, _ cfapi.RecordFilter) error {
		if zoneID == zone2.ID && !listing {
			return forbidden
		}
		return nil
	}}

	listing = false
	res := newDNSWith(s, store, writerOf(ours, ours), t0).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, callsTo(f, "DeleteRecord"), "the zone that cannot be listed still counts")
	requireHeld(t, res.Actions, "mass delete guard: 10 of 10 records")

	listing = true
	res = newDNSWith(s, store, writerOf(ours, ours), t0.Add(30*time.Second)).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, callsTo(f, "DeleteRecord"))
	requireHeld(t, res.Actions, "mass delete guard: 10 of 10 records")

	in := dnsIn()
	in.ConfirmDeletes = true
	newDNSWith(s, store, writerOf(ours, ours), t0.Add(60*time.Second)).Run(ctx, in, Enforce)
	require.Len(t, callsTo(f, "DeleteRecord"), 10)
}

func TestDNSMassDeleteGuardIgnoresOldTombstonesOfUnlistedZones(t *testing.T) {
	cases := []struct {
		name  string
		stone Tombstone
	}{
		{"another generation", Tombstone{Since: overdue.Since, Seen: overdue.Seen, Generation: ours.Generation - 1}},
		{"not seen lately", watched(t0.Add(-time.Hour), t0.Add(-10*time.Minute))},
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
			for i := range 5 {
				store.m[stoneKey(zone2.ID, fmt.Sprintf("h%d.shop.cz", i))] = tc.stone
			}
			s := &dnsSpy{API: f, lookup: func(zoneID string, _ cfapi.RecordFilter) error {
				if zoneID == zone2.ID {
					return forbidden
				}
				return nil
			}}

			newDNS(s, store, t0).Run(context.Background(), dnsIn(), Enforce)

			require.Len(t, callsTo(f, "DeleteRecord"), 2, "a tombstone this writer did not confirm lately does not count")
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
	cases := []struct {
		name     string
		since    time.Duration // how long before the run the names were first seen unwanted
		unwanted int           // of 10 records
		deleted  int
		held     string
	}{
		{"in the longer grace", 2 * time.Minute, 1, 0, "grace period: 3m0s left"},
		{"grace just over", 5 * time.Minute, 1, 1, ""},
		{"two deletes are many", 6 * time.Minute, 2, 0, "mass delete guard: 2 of 10 records"},
		{"two deletes just due are many", 5 * time.Minute, 2, 0, "mass delete guard: 2 of 10 records"},
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

			require.Empty(t, res.Problems)
			require.Len(t, callsTo(f, "DeleteRecord"), tc.deleted)
			require.Len(t, res.Actions, tc.unwanted)
			for _, a := range res.Actions {
				require.Equal(t, tc.held, a.Held)
			}
		})
	}
}
