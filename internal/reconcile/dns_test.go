package reconcile

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func TestDNSCreate(t *testing.T) {
	f := newDNSFake()
	store := &memStore{}

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, []string{"Records zone1", "Records zone2", "CreateRecord zone1 app.example.com"}, f.Calls(),
		"the listing shows that nothing holds the name")
	require.Equal(t, []cfapi.Record{{ID: "rec-1", Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, TTL: 1, Comment: testMarker}},
		recordsIn(f, zone1.ID))
	require.Equal(t, []Action{dnsAction(CreateRecord, "app.example.com", "", false)}, withoutDetail(res.Actions))
	require.Contains(t, res.Actions[0].Detail, testTarget)
	require.Empty(t, res.Conflicts)
	require.Empty(t, res.Lost)
	require.Empty(t, res.Replaced)
	require.Equal(t, 1, store.loads)
	require.Equal(t, 0, store.saves, "no tombstone changed")
}

func TestDNSNoOp(t *testing.T) {
	cases := []struct {
		name   string
		record cfapi.Record
	}{
		{"same record", ourCNAME("app.example.com", testTunnelID)},
		{"name and content differ in case", ourCNAME("App.Example.COM", "TID1")},
		{"comment carries a note after the marker", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: testMarker + " web front"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, tc.record)
			store := &memStore{}

			res := newDNS(f, store, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, []string{"Records zone1", "Records zone2"}, f.Calls())
			require.Empty(t, res.Actions)
			require.Empty(t, res.Conflicts)
			require.Equal(t, 0, store.saves)
		})
	}
}

func TestDNSUpdate(t *testing.T) {
	cases := []struct {
		name   string
		record cfapi.Record
		was    string
	}{
		{"tunnel id changed", ourCNAME("app.example.com", "old"), "was CNAME old.cfargotunnel.com"},
		{"not proxied", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Comment: testMarker}, "was CNAME " + testTarget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			tc.record.Comment += " keep this note"
			seeded := f.SeedRecord(zone1.ID, tc.record)

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, []string{"Records zone1", "Records zone2", "Records zone1", "UpdateRecord zone1 " + seeded.ID}, f.Calls(),
				"the record is read again right before the update")
			require.Equal(t, []cfapi.Record{{
				ID: seeded.ID, Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, TTL: 1,
				Comment: testMarker + " keep this note",
			}}, recordsIn(f, zone1.ID))
			require.Equal(t, []Action{dnsAction(UpdateRecord, "app.example.com", "", false)}, withoutDetail(res.Actions))
			require.Contains(t, res.Actions[0].Detail, tc.was)
		})
	}
}

func TestDNSRetargetRereadsRecord(t *testing.T) {
	cases := []struct {
		name     string
		record   cfapi.Record                                // as listed: ours, in need of an update
		change   func(rec cfapi.Record) (cfapi.Record, bool) // what the read right before the update shows; false: gone
		conflict bool
		lost     []string
	}{
		{
			name:   "marker removed, points elsewhere",
			record: ourCNAME("app.example.com", "old"),
			change: func(rec cfapi.Record) (cfapi.Record, bool) {
				rec.Comment = "taken over by hand"
				return rec, true
			},
			conflict: true,
		},
		{
			name:   "marker removed, still points at our tunnel",
			record: cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Comment: testMarker},
			change: func(rec cfapi.Record) (cfapi.Record, bool) {
				rec.Comment = ""
				return rec, true
			},
			conflict: true,
			lost:     []string{"app.example.com"},
		},
		{
			name:   "turned into an address record",
			record: ourCNAME("app.example.com", "old"),
			change: func(rec cfapi.Record) (cfapi.Record, bool) {
				rec.Type, rec.Content = "A", "192.0.2.10"
				return rec, true
			},
			conflict: true,
		},
		{
			name:   "gone",
			record: ourCNAME("app.example.com", "old"),
			change: func(cfapi.Record) (cfapi.Record, bool) { return cfapi.Record{}, false },
		},
		{
			name:   "renamed",
			record: ourCNAME("app.example.com", "old"),
			change: func(rec cfapi.Record) (cfapi.Record, bool) {
				rec.Name = "other.example.com"
				return rec, true
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			seeded := f.SeedRecord(zone1.ID, tc.record)
			var changed cfapi.Record
			s := &dnsSpy{API: f, edit: func(filter cfapi.RecordFilter, got []cfapi.Record) []cfapi.Record {
				if filter.Name == "" {
					return got
				}
				rec, ok := tc.change(seeded)
				changed = rec
				if !ok {
					return nil
				}
				return []cfapi.Record{rec}
			}}
			var log bytes.Buffer
			r := NewDNSReconciler(Clients{"cred1": s}, &memStore{}, writerOf(ours, ours), DNSSettings{InstallID: testInstall},
				(&clock{t0}).now, zerolog.New(&log).Level(zerolog.DebugLevel))

			res := r.Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Empty(t, res.Problems)
			require.Empty(t, dnsWrites(f))
			require.Empty(t, res.Actions)
			require.Equal(t, tc.lost, res.Lost)
			if tc.conflict {
				require.Equal(t, []Conflict{{Zone: "example.com", Name: changed.Name, Type: changed.Type, Content: changed.Content}}, res.Conflicts)
				return
			}
			require.Empty(t, res.Conflicts)
			require.Equal(t, 1, strings.Count(log.String(), `"level":"debug"`), "log: %s", log.String())
			require.Contains(t, log.String(), seeded.ID)
			require.Contains(t, log.String(), "app.example.com")
		})
	}
}

func TestDNSRereadFailureHoldsWrite(t *testing.T) {
	cases := []struct {
		name    string
		record  cfapi.Record
		wanted  []string
		problem string
	}{
		{"update", ourCNAME("app.example.com", "old"), []string{"app.example.com"}, "app.example.com in zone example.com: reading the record again before changing it"},
		{"probe", probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)), nil, "_pco-probe-x.example.com in zone example.com: reading the probe again before deleting it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, tc.record)
			s := &dnsSpy{API: f, lookup: func(_ string, filter cfapi.RecordFilter) error {
				if filter.Name != "" {
					return errors.New("connection reset by peer")
				}
				return nil
			}}

			res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn(tc.wanted...), Enforce)

			require.Empty(t, dnsWrites(f))
			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], tc.problem)
		})
	}
}

func TestDNSForeignRecordUntouched(t *testing.T) {
	cases := []struct {
		name   string
		record cfapi.Record
		lost   []string
	}{
		{"other content", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "app.other.net"}, nil},
		{"identical content", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: "added by hand"}, []string{"app.example.com"}},
		{"address record", cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"}, nil},
		{"marker of another install", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "x.cfargotunnel.com", Comment: "pco:xyz"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			seeded := f.SeedRecord(zone1.ID, tc.record)
			before := f.RecordsIn(zone1.ID)

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Empty(t, res.Problems)
			require.Empty(t, dnsWrites(f))
			require.Empty(t, res.Actions)
			require.Equal(t, []Conflict{{Zone: "example.com", Name: "app.example.com", Type: seeded.Type, Content: seeded.Content}}, res.Conflicts)
			require.Equal(t, tc.lost, res.Lost)
			require.Equal(t, before, f.RecordsIn(zone1.ID))
		})
	}
}

// Cloudflare keeps a TXT record beside a proxied CNAME.
func TestDNSTextRecordAtTheNameStandsBesideTheCNAME(t *testing.T) {
	f := newDNSFake()
	txt := f.SeedRecord(zone1.ID, cfapi.Record{Type: "TXT", Name: "app.example.com", Content: "v=spf1 -all"})

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, res.Conflicts, "a text record is no conflict")
	require.Equal(t, []string{"CreateRecord zone1 app.example.com"}, dnsWrites(f))
	require.Equal(t, []Action{dnsAction(CreateRecord, "app.example.com", "", false)}, withoutDetail(res.Actions))
	records := recordsIn(f, zone1.ID)
	require.Len(t, records, 2)
	txt.ModifiedOn = time.Time{}
	require.Equal(t, txt, records[0], "the text record is not touched")
	require.Equal(t, cfapi.Record{ID: records[1].ID, Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, TTL: 1, Comment: testMarker},
		records[1])

	// Once there, the CNAME is the run's own and the text record still nobody's.
	res = newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, res.Conflicts)
	require.Empty(t, res.Actions)
	require.Len(t, dnsWrites(f), 1)
}

func TestDNSTextRecordBesideAnAddressRecordIsNoConflict(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "TXT", Name: "app.example.com", Content: "v=spf1 -all"})
	addr := f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"})
	before := f.RecordsIn(zone1.ID)

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Equal(t, []Conflict{{Zone: "example.com", Name: "app.example.com", Type: "A", Content: addr.Content}}, res.Conflicts)
	require.Empty(t, dnsWrites(f))
	require.Equal(t, before, f.RecordsIn(zone1.ID))
}

func TestDNSAdoptionLeavesTheTextRecordOfTheName(t *testing.T) {
	f := newDNSFake()
	txt := f.SeedRecord(zone1.ID, cfapi.Record{Type: "TXT", Name: "app.example.com", Content: "v=spf1 -all"})
	addr := f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"})
	in := dnsIn("app.example.com")
	in.Adopt = map[string]bool{"app.example.com": true}

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{"DeleteRecord zone1 " + addr.ID, "CreateRecord zone1 app.example.com"}, dnsWrites(f))
	require.Equal(t, []cfapi.Record{addr}, res.Replaced)
	records := f.RecordsIn(zone1.ID)
	require.Len(t, records, 2)
	require.Equal(t, txt, records[0])
	require.Equal(t, "CNAME", records[1].Type)
}

// Any other record of someone else at the name is left to Cloudflare, as
// before: the create is tried, and Cloudflare refuses a CNAME beside it.
func TestDNSOtherRecordAtTheNameRefusesTheCNAME(t *testing.T) {
	f := newDNSFake()
	mx := f.SeedRecord(zone1.ID, cfapi.Record{Type: "MX", Name: "app.example.com", Content: "mail.example.com"})

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Equal(t, []string{"CreateRecord zone1 app.example.com"}, dnsWrites(f))
	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "app.example.com in zone example.com: creating the record")
	require.Len(t, res.Actions, 1)
	require.Contains(t, res.Actions[0].Held, "(codes 81053)")
	require.Empty(t, res.Conflicts)
	require.Equal(t, []cfapi.Record{mx}, f.RecordsIn(zone1.ID))
}

func TestDNSOwnedAddressRecordAtWantedName(t *testing.T) {
	address := cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10", Comment: testMarker}
	cases := []struct {
		name    string
		records []cfapi.Record
		tunnels []TunnelState
	}{
		{"address record", []cfapi.Record{address}, []TunnelState{ourTunnel}},
		// Cloudflare refuses this pair; a record that is ours is never lost.
		{"address record next to our CNAME", []cfapi.Record{address, ourCNAME("app.example.com", testTunnelID)}, []TunnelState{ourTunnel}},
		{"address record, tunnel not known", []cfapi.Record{address}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			var want []Conflict
			for _, rec := range tc.records {
				f.SeedRecord(zone1.ID, rec)
				want = append(want, Conflict{Zone: "example.com", Name: rec.Name, Type: rec.Type, Content: rec.Content})
			}
			in := dnsIn("app.example.com")
			in.Tunnels = tc.tunnels

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Empty(t, dnsWrites(f), "an address record is not turned into a CNAME without the admin")
			require.Equal(t, want, res.Conflicts)
			require.Empty(t, res.Lost)
			require.Empty(t, res.Actions, "nothing is planned for the name")
		})
	}
}

func TestDNSDuplicatePlans(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("app.example.com", "old"))
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "app.example.com"): overdue}}
	in := dnsIn("app.example.com", "www.example.com")
	// Either plan alone would be written: an update in example.com, a create
	// in shop.cz.
	in.Records = append(in.Records, wantRecord(zone2, "app.example.com"))

	res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "app.example.com: planned 2 times")
	require.Contains(t, res.Problems[0], "zone example.com")
	require.Contains(t, res.Problems[0], "zone shop.cz")
	require.Equal(t, []string{"CreateRecord zone1 www.example.com"}, dnsWrites(f), "nothing is written for the name planned twice")
	require.Empty(t, store.m, "the name is still wanted")
}

func TestDNSMarkerMustBeFirstToken(t *testing.T) {
	cases := []struct {
		comment string
		owned   bool
	}{
		{"notes pco:abc", false},
		{"pco:abcd", false},
		{"pco:abcd note", false},
		{"PCO:ABC", false},
		{"pco:abc.old", false},
		{"pco:abc", true},
		{"pco:abc note", true},
		{"pco:abc\tnote", true},
	}
	for _, tc := range cases {
		t.Run(tc.comment, func(t *testing.T) {
			f := newDNSFake()
			gone := f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "gone.example.com", Content: testTarget, Proxied: true, Comment: tc.comment})
			app := f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "old.cfargotunnel.com", Proxied: true, Comment: tc.comment})
			store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}

			res := newDNS(f, store, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Empty(t, res.Problems)
			if tc.owned {
				require.Equal(t, []string{"UpdateRecord zone1 " + app.ID, "DeleteRecord zone1 " + gone.ID}, dnsWrites(f))
				require.Empty(t, res.Conflicts)
				return
			}
			require.Empty(t, dnsWrites(f))
			require.Equal(t, []Conflict{{Zone: "example.com", Name: "app.example.com", Type: "CNAME", Content: "old.cfargotunnel.com"}}, res.Conflicts)
			require.Len(t, f.RecordsIn(zone1.ID), 2)
			require.NotContains(t, store.m, stoneKey(zone1.ID, "gone.example.com"), "a record that is not ours keeps no tombstone")
		})
	}
}

func TestDNSListingFailureSkipsZone(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"transport failure", errors.New("connection reset by peer")},
		{"denied", forbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			stones := map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}
			store := &memStore{m: maps.Clone(stones)}
			in := dnsIn("app.example.com")
			in.Records = append(in.Records, wantRecord(zone2, "www.shop.cz"))
			f.FailNext("dns.read", 1, tc.err) // the listing of example.com, the first zone by name

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Len(t, res.Problems, 1, "problems: %v", res.Problems)
			require.Contains(t, res.Problems[0], "zone example.com")
			require.Equal(t, []string{"CreateRecord zone2 www.shop.cz"}, dnsWrites(f))
			require.Equal(t, []Action{dnsAction(CreateRecord, "www.shop.cz", "", false)}, withoutDetail(res.Actions))
			gone := ourCNAME("gone.example.com", testTunnelID)
			gone.TTL = 1
			require.Equal(t, []cfapi.Record{gone}, withoutIDs(recordsIn(f, zone1.ID)))
			require.Equal(t, stones, store.m)
			require.Equal(t, 0, store.saves)
		})
	}
}

func TestDNSMissingClient(t *testing.T) {
	f := newDNSFake()
	in := dnsIn("app.example.com")
	in.Zones = []ZoneRef{zone1, {ID: zone2.ID, Name: zone2.Name, CredentialID: "cred2"}}

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Equal(t, []string{"zone shop.cz: no client for credential cred2"}, res.Problems)
	require.Equal(t, []string{"CreateRecord zone1 app.example.com"}, dnsWrites(f))
}

func TestDNSUnknownTunnelHoldsCreate(t *testing.T) {
	cases := []struct {
		name    string
		tunnels []TunnelState
		held    string
	}{
		{"tunnel not known", nil, "tunnel not created yet"},
		{"tunnel does not exist", []TunnelState{{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: testTunnelID}}, "tunnel not created yet"},
		{"tunnel of another account", []TunnelState{{AccountID: "acct2", CredentialID: "cred1", Name: testTunnel, ID: testTunnelID, Exists: true}}, "tunnel not created yet"},
		{"tunnel state unknown", []TunnelState{{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, Unknown: true}}, "tunnel state unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("www.example.com", "old"))
			store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "www.example.com"): overdue}}
			in := dnsIn("app.example.com", "www.example.com")
			in.Tunnels = tc.tunnels

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, []string{"Records zone1", "Records zone2"}, f.Calls())
			require.Equal(t, []Action{
				dnsAction(CreateRecord, "app.example.com", tc.held, false),
				dnsAction(UpdateRecord, "www.example.com", tc.held, false),
			}, withoutDetail(res.Actions))
			require.Empty(t, store.m, "a wanted record keeps no tombstone")
		})
	}
}

func TestDNSObserveModeWritesNothing(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("stale.example.com", "old"))
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "web.example.com", Content: "192.0.2.10"})
	f.SeedRecord(zone1.ID, probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)))
	before := f.RecordsIn(zone1.ID)
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}
	in := dnsIn("app.example.com", "stale.example.com", "web.example.com")
	in.Adopt = map[string]bool{"web.example.com": true}
	writer, asked := scripted(answer{us: ours, stored: ours})

	res := newDNSWith(f, store, writer, t0).Run(context.Background(), in, Observe)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	for _, call := range f.Calls() {
		require.True(t, strings.HasPrefix(call, "Records "), "only reads: %s", call)
	}
	require.Equal(t, 0, store.loads)
	require.Equal(t, 0, store.saves)
	require.Equal(t, 1, *asked, "the writer is read once, as nothing is written")
	require.Equal(t, []Action{
		dnsAction(DeleteRecord, "_pco-probe-x.example.com", "observe mode", true),
		dnsAction(CreateRecord, "app.example.com", "observe mode", false),
		dnsAction(DeleteRecord, "gone.example.com", "observe mode", true),
		dnsAction(UpdateRecord, "stale.example.com", "observe mode", false),
		dnsAction(DeleteRecord, "web.example.com", "observe mode", true),
		dnsAction(CreateRecord, "web.example.com", "observe mode", true),
	}, withoutDetail(res.Actions))
	require.Empty(t, res.Replaced)
	require.Equal(t, before, f.RecordsIn(zone1.ID))
}

func TestDNSWriteFailureGoesOn(t *testing.T) {
	cases := []struct {
		name    string
		seed    bool // app.example.com has a record of ours that points elsewhere
		kind    ActionKind
		problem string
	}{
		{"create denied", false, CreateRecord, "app.example.com in zone example.com: creating the record"},
		{"update denied", true, UpdateRecord, "app.example.com in zone example.com: updating the record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			if tc.seed {
				f.SeedRecord(zone1.ID, ourCNAME("app.example.com", "old"))
			}
			f.FailNext("dns.write", 1, forbidden)

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com", "www.example.com"), Enforce)

			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], tc.problem)
			require.Equal(t, "CreateRecord zone1 www.example.com", dnsWrites(f)[1], "the next record is still written")
			require.Equal(t, []Action{
				dnsAction(tc.kind, "app.example.com", forbidden.Error(), false),
				dnsAction(CreateRecord, "www.example.com", "", false),
			}, withoutDetail(res.Actions))
		})
	}
}

// An adoption looks at the name again, and finds a record of this install
// that the listing did not show.
func TestDNSRecordOfOursAppearsDuringAnAdoption(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "app.other.net"})
	s := &dnsSpy{API: f, lookup: func(_ string, filter cfapi.RecordFilter) error {
		if filter.Name == "app.example.com" {
			f.SeedRecord(zone1.ID, ourCNAME("app.example.com", "old"))
		}
		return nil
	}}
	in := dnsIn("app.example.com")
	in.Adopt = map[string]bool{"app.example.com": true}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "app.example.com in zone example.com: a record of this install appeared during the run")
	require.Empty(t, dnsWrites(f))
	require.Empty(t, res.Actions)
	require.Empty(t, res.Conflicts)
	require.Empty(t, res.Lost)
}

func TestDNSLookupFailureSkipsAnAdoption(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "app.other.net"})
	s := &dnsSpy{API: f, lookup: func(_ string, filter cfapi.RecordFilter) error {
		if filter.Name == "app.example.com" {
			return errors.New("connection reset by peer")
		}
		return nil
	}}
	in := dnsIn("app.example.com", "www.example.com")
	in.Adopt = map[string]bool{"app.example.com": true}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "app.example.com in zone example.com")
	require.Equal(t, []string{"CreateRecord zone1 www.example.com"}, dnsWrites(f))
	require.Equal(t, []Action{dnsAction(CreateRecord, "www.example.com", "", false)}, withoutDetail(res.Actions))
}

func TestDNSOutputOrder(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "z.example.com", Content: testTarget})
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "y.example.com", Content: "192.0.2.10"})
	f.SeedRecord(zone2.ID, cfapi.Record{Type: "CNAME", Name: "l.shop.cz", Content: testTarget})
	in := DNSInput{
		Zones:   []ZoneRef{zone2, zone1},
		Tunnels: []TunnelState{ourTunnel},
		Records: []planner.RecordPlan{
			wantRecord(zone2, "b.shop.cz"),
			wantRecord(zone1, "z.example.com"),
			wantRecord(zone2, "l.shop.cz"),
			wantRecord(zone1, "y.example.com"),
			wantRecord(zone2, "a.shop.cz"),
			wantRecord(zone1, "c.example.com"),
		},
		InventoryOK: true,
	}

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{"Records zone1", "Records zone2"}, f.Calls()[:2], "zones by name")
	require.Equal(t, []Action{
		dnsAction(CreateRecord, "c.example.com", "", false),
		dnsAction(CreateRecord, "a.shop.cz", "", false),
		dnsAction(CreateRecord, "b.shop.cz", "", false),
	}, withoutDetail(res.Actions))
	require.Equal(t, []Conflict{
		{Zone: "example.com", Name: "y.example.com", Type: "A", Content: "192.0.2.10"},
		{Zone: "example.com", Name: "z.example.com", Type: "CNAME", Content: testTarget},
		{Zone: "shop.cz", Name: "l.shop.cz", Type: "CNAME", Content: testTarget},
	}, res.Conflicts)
	require.Equal(t, []string{"l.shop.cz", "z.example.com"}, res.Lost)
}
