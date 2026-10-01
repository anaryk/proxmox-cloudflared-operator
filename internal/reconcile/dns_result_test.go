package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestDNSDecided pins when a run tells its caller that it got as far as the
// admin's one-shot requests: confirmed deletes and adoptions.
func TestDNSDecided(t *testing.T) {
	cases := []struct {
		name     string
		writer   []answer
		settings DNSSettings
		change   func(in *DNSInput, store *memStore)
		observe  bool
		want     bool
	}{
		{name: "an enforcing run", want: true},
		{name: "an observing run", observe: true},
		{name: "the writer changes during the run", writer: []answer{{us: ours, stored: ours}, {us: ours, stored: newer}}, want: true},
		{name: "another writer stored", writer: []answer{{us: ours, stored: newer}}},
		{name: "the writer cannot be read", writer: []answer{{err: errors.New("lease lost")}}},
		{name: "the writer is not valid", writer: []answer{{us: writerAt(5, ""), stored: writerAt(5, "")}}},
		{name: "the writer is of another install", settings: DNSSettings{InstallID: "xyz"}},
		{name: "the tunnel run found a foreign writer", change: func(in *DNSInput, _ *memStore) { in.TunnelVerdict = WriterForeign }},
		{name: "the tunnel run found a stale writer", change: func(in *DNSInput, _ *memStore) { in.TunnelVerdict = WriterStale }},
		{name: "the tombstones cannot be read", change: func(_ *DNSInput, store *memStore) { store.loadErr = errDisk }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("www.example.com", "old"))
			store := &memStore{}
			in := dnsIn("app.example.com", "www.example.com")
			in.ConfirmDeletes = true
			in.Adopt = map[string]bool{"app.example.com": true}
			if tc.change != nil {
				tc.change(&in, store)
			}
			writer := writerOf(ours, ours)
			if tc.writer != nil {
				writer, _ = scripted(tc.writer...)
			}
			settings := tc.settings
			if settings.InstallID == "" {
				settings.InstallID = testInstall
			}
			r := NewDNSReconciler(Clients{"cred1": f}, store, writer, settings, (&clock{t0}).now, zerolog.Nop())

			mode := Enforce
			if tc.observe {
				mode = Observe
			}
			res := r.Run(context.Background(), in, mode)

			require.Equal(t, tc.want, res.Decided)
		})
	}
}

func TestDNSConfirmedCounts(t *testing.T) {
	confirmed := overdue
	confirmed.Confirmed = true
	restarted := overdue
	restarted.Generation = ours.Generation - 1
	cases := []struct {
		name    string
		stones  map[string]Tombstone
		listed  bool
		confirm bool
		partial bool // inventory incomplete
		want    int
	}{
		{name: "pending removals", stones: map[string]Tombstone{"a": overdue, "b": overdue}, listed: true, confirm: true, want: 2},
		{name: "already confirmed", stones: map[string]Tombstone{"a": confirmed, "b": overdue}, listed: true, confirm: true, want: 1},
		{name: "a grace that starts again", stones: map[string]Tombstone{"a": restarted, "b": overdue}, listed: true, confirm: true, want: 1},
		{name: "found for the first time", stones: map[string]Tombstone{"b": overdue}, listed: true, confirm: true, want: 1},
		{name: "a zone that cannot be listed", stones: map[string]Tombstone{"a": overdue, "b": confirmed}, confirm: true, want: 1},
		{name: "no confirmation asked", stones: map[string]Tombstone{"a": overdue, "b": overdue}, listed: true},
		{name: "inventory incomplete", stones: map[string]Tombstone{"a": overdue, "b": overdue}, listed: true, confirm: true, partial: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{m: map[string]Tombstone{}}
			for _, name := range []string{"a", "b"} {
				host := fmt.Sprintf("%s.shop.cz", name)
				f.SeedRecord(zone2.ID, ourCNAME(host, testTunnelID))
				if stone, ok := tc.stones[name]; ok {
					store.m[stoneKey(zone2.ID, host)] = stone
				}
			}
			s, failing := unlistable(f)
			*failing = !tc.listed
			in := dnsIn()
			in.ConfirmDeletes = tc.confirm
			in.InventoryOK = !tc.partial

			res := newDNS(s, store, t0).Run(context.Background(), in, Enforce)

			require.True(t, res.Decided)
			require.Equal(t, tc.want, res.Confirmed)
		})
	}
}

// TestDNSConfirmedOnlyWhatWasSaved confirms two pending removals in runs whose
// tombstones may not reach the store: only a confirmation that was saved is
// reported.
func TestDNSConfirmedOnlyWhatWasSaved(t *testing.T) {
	cases := []struct {
		name     string
		saveErrs map[int]error
		writer   []answer
		want     int
		saved    bool
	}{
		{name: "saved before the deletes", want: 2, saved: true},
		{name: "saved at the end of the run", saveErrs: map[int]error{1: errDisk}, want: 2, saved: true},
		{name: "never saved", saveErrs: map[int]error{1: errDisk, 2: errDisk}},
		{name: "the writer changed before the save", writer: []answer{{us: ours, stored: ours}, {us: ours, stored: newer}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{m: map[string]Tombstone{}, saveErrs: tc.saveErrs}
			for i := range 2 {
				name := fmt.Sprintf("h%d.example.com", i)
				f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
				store.m[stoneKey(zone1.ID, name)] = watched(t0.Add(-30*time.Second), t0.Add(-time.Minute))
			}
			writer := writerOf(ours, ours)
			if tc.writer != nil {
				writer, _ = scripted(tc.writer...)
			}
			in := dnsIn()
			in.ConfirmDeletes = true

			res := newDNSWith(f, store, writer, t0).Run(context.Background(), in, Enforce)

			require.True(t, res.Decided)
			require.Equal(t, tc.want, res.Confirmed)
			for key, stone := range store.m {
				require.Equal(t, tc.saved, stone.Confirmed, key)
			}
		})
	}
}
