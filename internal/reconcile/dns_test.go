package reconcile

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	testMarker   = "pco:abc"
	testTunnelID = "tid1"
	testTarget   = "tid1.cfargotunnel.com"
)

var (
	zone1     = ZoneRef{ID: "zone1", Name: "example.com", CredentialID: "cred1"}
	zone2     = ZoneRef{ID: "zone2", Name: "shop.cz", CredentialID: "cred1"}
	ourTunnel = TunnelState{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: testTunnelID, Exists: true, Verified: true}

	forbidden = &cfapi.Error{Status: http.StatusForbidden, Codes: []int{10000}, Message: "Authentication error"}
	notFound  = &cfapi.Error{Status: http.StatusNotFound, Message: "Not Found"}

	// newer is a writer that took over from ours.
	newer = writerAt(6, "n6")

	// overdue is a tombstone of ours whose grace ended long before t0,
	// confirmed by a run 30 s before t0.
	overdue = watched(t0.Add(-time.Hour), t0.Add(-30*time.Second))
)

// watched is a tombstone of our generation, first seen unwanted at since and
// last confirmed at seen.
func watched(since, seen time.Time) Tombstone {
	return Tombstone{Since: since, Seen: seen, Generation: ours.Generation}
}

// memStore keeps tombstones in memory and counts how it is used. saveErrs
// fails the save of that number, counted from 1.
type memStore struct {
	m        map[string]Tombstone
	loadErr  error
	saveErrs map[int]error
	loads    int
	saves    int
}

func (s *memStore) Load(context.Context) (map[string]Tombstone, error) {
	s.loads++
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return maps.Clone(s.m), nil
}

func (s *memStore) Save(_ context.Context, m map[string]Tombstone) error {
	s.saves++
	if err := s.saveErrs[s.saves]; err != nil {
		return err
	}
	s.m = maps.Clone(m)
	return nil
}

// sinceOf returns when each tombstone's grace began.
func sinceOf(m map[string]Tombstone) map[string]time.Time {
	out := make(map[string]time.Time, len(m))
	for k, t := range m {
		out[k] = t.Since
	}
	return out
}

// dnsSpy wraps an API. lookup runs before each listing and an error it
// returns fails the listing; edit may change what a listing answers;
// failCreate may refuse a create; deleteErr answers every delete without
// passing it on; afterWrite runs after each write that went through.
type dnsSpy struct {
	cfapi.API
	lookup     func(zoneID string, f cfapi.RecordFilter) error
	edit       func(f cfapi.RecordFilter, got []cfapi.Record) []cfapi.Record
	failCreate func(r cfapi.Record) error
	deleteErr  error
	afterWrite func(method string)
	deletes    int
}

func (s *dnsSpy) Records(ctx context.Context, zoneID string, f cfapi.RecordFilter) ([]cfapi.Record, error) {
	if s.lookup != nil {
		if err := s.lookup(zoneID, f); err != nil {
			return nil, err
		}
	}
	got, err := s.API.Records(ctx, zoneID, f)
	if err == nil && s.edit != nil {
		got = s.edit(f, got)
	}
	return got, err
}

func (s *dnsSpy) CreateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	if s.failCreate != nil {
		if err := s.failCreate(r); err != nil {
			return cfapi.Record{}, err
		}
	}
	got, err := s.API.CreateRecord(ctx, zoneID, r)
	if err == nil {
		s.wrote("CreateRecord")
	}
	return got, err
}

func (s *dnsSpy) UpdateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	got, err := s.API.UpdateRecord(ctx, zoneID, r)
	if err == nil {
		s.wrote("UpdateRecord")
	}
	return got, err
}

func (s *dnsSpy) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	s.deletes++
	if s.deleteErr != nil {
		return s.deleteErr
	}
	err := s.API.DeleteRecord(ctx, zoneID, recordID)
	if err == nil {
		s.wrote("DeleteRecord")
	}
	return err
}

func (s *dnsSpy) wrote(method string) {
	if s.afterWrite != nil {
		s.afterWrite(method)
	}
}

// writerBox is a writer callback whose answer a test may change during a run.
type writerBox struct{ us, stored planner.Writer }

func (w *writerBox) get() (planner.Writer, planner.Writer, error) { return w.us, w.stored, nil }

// newDNSFake returns a fake with account acct1 and its zones example.com
// (zone1) and shop.cz (zone2).
func newDNSFake() *cffake.Fake {
	f := newFake("acct1")
	f.AddZone(zone1.ID, zone1.Name, "acct1")
	f.AddZone(zone2.ID, zone2.Name, "acct1")
	return f
}

func newDNS(api cfapi.API, store TombstoneStore, now time.Time) *DNSReconciler {
	return newDNSWith(api, store, writerOf(ours, ours), now)
}

func newDNSWith(api cfapi.API, store TombstoneStore, writer func() (planner.Writer, planner.Writer, error), now time.Time) *DNSReconciler {
	return NewDNSReconciler(Clients{"cred1": api}, store, writer, DNSSettings{InstallID: testInstall}, (&clock{now}).now, zerolog.Nop())
}

func wantRecord(z ZoneRef, name string) planner.RecordPlan {
	return planner.RecordPlan{ZoneID: z.ID, ZoneName: z.Name, AccountID: "acct1", CredentialID: z.CredentialID, Name: name, TunnelName: testTunnel}
}

func unwantedAll(context.Context, string) (bool, error) { return true, nil }

// dnsIn wants each name in example.com, with both zones and our tunnel known,
// a complete inventory and an inventory that confirms every delete.
func dnsIn(names ...string) DNSInput {
	in := DNSInput{Zones: []ZoneRef{zone1, zone2}, Tunnels: []TunnelState{ourTunnel}, InventoryOK: true, StillUnwanted: unwantedAll}
	for _, name := range names {
		in.Records = append(in.Records, wantRecord(zone1, name))
	}
	return in
}

func ourCNAME(name, tunnelID string) cfapi.Record {
	return cfapi.Record{Type: "CNAME", Name: name, Content: tunnelID + ".cfargotunnel.com", Proxied: true, Comment: testMarker}
}

func probeRecord(name string, modified time.Time) cfapi.Record {
	return cfapi.Record{Type: "TXT", Name: name, Content: "probe", Comment: testMarker + " probe", ModifiedOn: modified}
}

func dnsAction(kind ActionKind, target, held string, destructive bool) Action {
	return Action{Kind: kind, Credential: "cred1", Target: target, Destructive: destructive, Applied: held == "", Held: held}
}

func dnsWrites(f *cffake.Fake) []string {
	return callsTo(f, "CreateRecord", "UpdateRecord", "DeleteRecord")
}

// recordsIn returns the records of a zone without their modification time.
func recordsIn(f *cffake.Fake, zoneID string) []cfapi.Record {
	records := f.RecordsIn(zoneID)
	for i := range records {
		records[i].ModifiedOn = time.Time{}
	}
	return records
}

func withoutIDs(records []cfapi.Record) []cfapi.Record {
	for i := range records {
		records[i].ID = ""
	}
	return records
}

func stoneKey(zoneID, name string) string { return zoneID + "/" + name }

// requireHeld checks that every action is a delete held for why.
func requireHeld(t *testing.T, actions []Action, why string) {
	t.Helper()
	require.NotEmpty(t, actions)
	for _, a := range actions {
		require.Equal(t, dnsAction(DeleteRecord, a.Target, why, true), withoutDetail([]Action{a})[0])
	}
}

func TestDNSCreate(t *testing.T) {
	f := newDNSFake()
	store := &memStore{}

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, []string{"Records zone1", "Records zone2", "Records zone1", "CreateRecord zone1 app.example.com"}, f.Calls())
	require.Equal(t, []cfapi.Record{{ID: "rec-1", Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: testMarker}},
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
				ID: seeded.ID, Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true,
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

			res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Empty(t, res.Problems)
			require.Empty(t, dnsWrites(f))
			require.Empty(t, res.Actions)
			require.Equal(t, tc.lost, res.Lost)
			if tc.conflict {
				require.Equal(t, []Conflict{{Zone: "example.com", Name: changed.Name, Type: changed.Type, Content: changed.Content}}, res.Conflicts)
			} else {
				require.Empty(t, res.Conflicts)
			}
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

func TestDNSTextRecordDoesNotBlock(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "TXT", Name: "app.example.com", Content: "v=spf1 -all"})

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, res.Conflicts)
	require.Equal(t, []string{"CreateRecord zone1 app.example.com"}, dnsWrites(f))
}

func TestDNSOwnedAddressRecordAtWantedName(t *testing.T) {
	address := cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10", Comment: testMarker}
	cases := []struct {
		name    string
		records []cfapi.Record
	}{
		{"address record", []cfapi.Record{address}},
		// Cloudflare refuses this pair; a record that is ours is never lost.
		{"address record next to our CNAME", []cfapi.Record{address, ourCNAME("app.example.com", testTunnelID)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			var want []Conflict
			for _, rec := range tc.records {
				f.SeedRecord(zone1.ID, rec)
				want = append(want, Conflict{Zone: "example.com", Name: rec.Name, Type: rec.Type, Content: rec.Content})
			}

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Empty(t, res.Problems)
			require.Empty(t, dnsWrites(f), "an address record is not turned into a CNAME without the admin")
			require.Equal(t, want, res.Conflicts)
			require.Empty(t, res.Lost)
		})
	}
}

func TestDNSAdoptCNAMEInPlace(t *testing.T) {
	cases := []struct {
		name    string
		content string
		adopt   string
	}{
		{"other content", "app.other.net", "app.example.com"},
		{"identical content, name in other case", testTarget, "APP.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			seeded := f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: tc.content, Comment: "set by hand"})
			in := dnsIn("app.example.com")
			in.Adopt = map[string]bool{tc.adopt: true}

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, []string{"UpdateRecord zone1 " + seeded.ID}, dnsWrites(f))
			require.Equal(t, []cfapi.Record{{ID: seeded.ID, Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: testMarker}},
				recordsIn(f, zone1.ID))
			require.Equal(t, []Action{dnsAction(UpdateRecord, "app.example.com", "", true)}, withoutDetail(res.Actions))
			require.Contains(t, res.Actions[0].Detail, "was CNAME "+tc.content)
			require.Empty(t, res.Conflicts)
			require.Empty(t, res.Lost)
			require.Equal(t, []cfapi.Record{seeded}, res.Replaced)
		})
	}
}

func TestDNSAdoptAddressRecordReplaced(t *testing.T) {
	cases := []struct{ typ, content string }{
		{"A", "192.0.2.10"},
		{"AAAA", "2001:db8::10"},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			f := newDNSFake()
			seeded := f.SeedRecord(zone1.ID, cfapi.Record{Type: tc.typ, Name: "app.example.com", Content: tc.content, Proxied: true})
			in := dnsIn("app.example.com")
			in.Adopt = map[string]bool{"app.example.com": true}

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, []string{"DeleteRecord zone1 " + seeded.ID, "CreateRecord zone1 app.example.com"}, dnsWrites(f))
			require.Equal(t, []cfapi.Record{{ID: "rec-2", Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: testMarker}},
				recordsIn(f, zone1.ID))
			require.Equal(t, []Action{
				dnsAction(DeleteRecord, "app.example.com", "", true),
				dnsAction(CreateRecord, "app.example.com", "", true),
			}, withoutDetail(res.Actions))
			for _, a := range res.Actions {
				require.Contains(t, a.Detail, "was "+tc.typ+" "+tc.content)
			}
			require.Empty(t, res.Conflicts)
			require.Equal(t, []cfapi.Record{seeded}, res.Replaced)
		})
	}
}

func TestDNSAdoptionCreateFails(t *testing.T) {
	original := cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10", Proxied: true, Comment: "web front, by hand"}
	refused := errors.New("refused by the test")
	cases := []struct {
		name     string
		refuse   func(r cfapi.Record) error
		restored bool
	}{
		{"original put back", func(r cfapi.Record) error {
			if r.Type == "CNAME" {
				return refused
			}
			return nil
		}, true},
		{"putting it back fails too", func(cfapi.Record) error { return refused }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			seeded := f.SeedRecord(zone1.ID, original)
			s := &dnsSpy{API: f, failCreate: tc.refuse}
			in := dnsIn("app.example.com")
			in.Adopt = map[string]bool{"app.example.com": true}

			res := newDNS(s, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Equal(t, []cfapi.Record{seeded}, res.Replaced, "the snapshot is kept whatever happens next")
			require.Equal(t, []string{"DeleteRecord zone1 " + seeded.ID}, dnsWrites(f)[:1])
			require.NotEmpty(t, res.Problems)
			last := res.Problems[len(res.Problems)-1]
			require.Contains(t, last, "app.example.com in zone example.com: adoption failed")
			if tc.restored {
				require.Equal(t, []cfapi.Record{original}, withoutIDs(recordsIn(f, zone1.ID)))
				require.Contains(t, last, "put back")
				return
			}
			require.Empty(t, f.RecordsIn(zone1.ID))
			for _, part := range []string{"A", "app.example.com", "192.0.2.10", "proxied true", `"web front, by hand"`, seeded.ID} {
				require.Contains(t, last, part, "the admin can restore the record by hand")
			}
		})
	}
}

func TestDNSAdoptSeveralRecordsStaysConflict(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.11"})
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"})
	in := dnsIn("app.example.com")
	in.Adopt = map[string]bool{"app.example.com": true}

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Empty(t, res.Problems)
	require.Empty(t, dnsWrites(f))
	require.Empty(t, res.Actions)
	require.Equal(t, []Conflict{
		{Zone: "example.com", Name: "app.example.com", Type: "A", Content: "192.0.2.10"},
		{Zone: "example.com", Name: "app.example.com", Type: "A", Content: "192.0.2.11"},
	}, res.Conflicts)
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
			require.Equal(t, []cfapi.Record{ourCNAME("gone.example.com", testTunnelID)}, withoutIDs(recordsIn(f, zone1.ID)))
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

func TestDNSProbeCleanup(t *testing.T) {
	for _, inventoryOK := range []bool{true, false} {
		t.Run(fmt.Sprintf("inventory complete %v", inventoryOK), func(t *testing.T) {
			f := newDNSFake()
			var old []string
			for i := range 6 {
				p := f.SeedRecord(zone1.ID, probeRecord(fmt.Sprintf("_pco-probe-old%d.example.com", i), t0.Add(-11*time.Minute)))
				old = append(old, "DeleteRecord zone1 "+p.ID)
			}
			f.SeedRecord(zone1.ID, probeRecord("_pco-probe-edge.example.com", t0.Add(-10*time.Minute)))
			f.SeedRecord(zone1.ID, probeRecord("_pco-probe-new.example.com", t0.Add(-time.Minute)))
			store := &memStore{}
			in := dnsIn()
			in.InventoryOK = inventoryOK

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, old, dnsWrites(f), "probes are not held by the mass delete guard")
			require.Len(t, res.Actions, 6)
			for _, a := range res.Actions {
				require.Equal(t, dnsAction(DeleteRecord, a.Target, "", true), withoutDetail([]Action{a})[0])
			}
			var names []string
			for _, r := range f.RecordsIn(zone1.ID) {
				names = append(names, r.Name)
			}
			require.Equal(t, []string{"_pco-probe-edge.example.com", "_pco-probe-new.example.com"}, names)
			require.Equal(t, 0, store.saves, "probes get no tombstone")
		})
	}
}

func TestDNSProbeChangedBeforeDelete(t *testing.T) {
	cases := []struct {
		name   string
		change func(rec cfapi.Record) cfapi.Record
	}{
		{"comment changed", func(rec cfapi.Record) cfapi.Record {
			rec.Comment = "kept by hand"
			return rec
		}},
		{"type changed", func(rec cfapi.Record) cfapi.Record {
			rec.Type = "CNAME"
			return rec
		}},
		{"modified just now", func(rec cfapi.Record) cfapi.Record {
			rec.ModifiedOn = t0
			return rec
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			probe := f.SeedRecord(zone1.ID, probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)))
			s := &dnsSpy{API: f, edit: func(filter cfapi.RecordFilter, got []cfapi.Record) []cfapi.Record {
				if filter.Name == "" {
					return got
				}
				return []cfapi.Record{tc.change(probe)}
			}}

			res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn(), Enforce)

			require.Empty(t, res.Problems)
			require.Empty(t, dnsWrites(f))
		})
	}
}

func TestDNSProbeOfUnknownAge(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)))
	s := &dnsSpy{API: f, edit: func(_ cfapi.RecordFilter, got []cfapi.Record) []cfapi.Record {
		for i := range got {
			got[i].ModifiedOn = time.Time{}
		}
		return got
	}}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Empty(t, dnsWrites(f))
	require.Empty(t, res.Actions)
}

func TestDNSProbeLookalikesAreOrdinaryRecords(t *testing.T) {
	cases := []struct {
		name   string
		record cfapi.Record
	}{
		{"CNAME with the probe comment", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: testMarker + " probe"}},
		{"CNAME with a probe name and comment", cfapi.Record{Type: "CNAME", Name: "_pco-probe-x.example.com", Content: testTarget, Proxied: true, Comment: testMarker + " probe"}},
		{"TXT with the probe comment and another name", cfapi.Record{Type: "TXT", Name: "app.example.com", Content: "probe", Comment: testMarker + " probe"}},
		{"probe with a longer comment", cfapi.Record{Type: "TXT", Name: "_pco-probe-x.example.com", Content: "probe", Comment: testMarker + " probe kept by hand"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			tc.record.ModifiedOn = t0.Add(-time.Hour)
			f.SeedRecord(zone1.ID, tc.record)
			store := &memStore{}

			res := newDNS(f, store, t0).Run(context.Background(), dnsIn(), Enforce)

			require.Empty(t, dnsWrites(f), "an ordinary record of ours waits for its grace")
			require.Equal(t, []Action{dnsAction(DeleteRecord, tc.record.Name, "grace period: 1m0s left", true)}, withoutDetail(res.Actions))
			require.Len(t, store.m, 1)
		})
	}
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

func TestDNSWriterStaleAtStart(t *testing.T) {
	for _, mode := range []Mode{Enforce, Observe} {
		t.Run(fmt.Sprintf("mode %d", mode), func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}

			res := newDNSWith(f, store, writerOf(ours, newer), t0).Run(context.Background(), dnsIn("app.example.com"), mode)

			require.Equal(t, WriterStale, res.Verdict)
			require.Empty(t, f.Calls(), "not even a listing")
			require.Equal(t, 0, store.loads)
			require.Equal(t, 0, store.saves)
			require.Empty(t, res.Actions)
			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], "stale")
		})
	}
}

func TestDNSWriterUnusableAtStart(t *testing.T) {
	cases := []struct {
		name     string
		writer   func() (planner.Writer, planner.Writer, error)
		settings DNSSettings
		problem  string
	}{
		{"writer cannot be read", func() (planner.Writer, planner.Writer, error) {
			return planner.Writer{}, planner.Writer{}, errors.New("lease lost")
		}, DNSSettings{InstallID: testInstall}, "lease lost"},
		{"writer not valid", writerOf(writerAt(5, ""), writerAt(5, "")), DNSSettings{InstallID: testInstall}, "cannot write as this writer"},
		{"no install id", writerOf(ours, ours), DNSSettings{}, "install"},
		{"another install id", writerOf(ours, ours), DNSSettings{InstallID: "xyz"}, "install"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			store := &memStore{}
			r := NewDNSReconciler(Clients{"cred1": f}, store, tc.writer, tc.settings, (&clock{t0}).now, zerolog.Nop())

			res := r.Run(context.Background(), dnsIn("app.example.com"), Enforce)

			require.Equal(t, WriterProceed, res.Verdict)
			require.Len(t, res.Problems, 1)
			require.Contains(t, res.Problems[0], tc.problem)
			require.Empty(t, f.Calls())
			require.Equal(t, 0, store.loads)
		})
	}
}

// TestDNSWriterChangesBeforeWrite lets the writer answer for the run at the
// start and differently from then on: the first write of each kind must not
// go out, and neither must anything after it.
func TestDNSWriterChangesBeforeWrite(t *testing.T) {
	listings := []string{"Records zone1", "Records zone2"}
	oneRead := []string{"Records zone1", "Records zone2", "Records zone1"}
	scenarios := []struct {
		name    string
		setup   func(f *cffake.Fake, store *memStore, in *DNSInput)
		calls   []string // every call the run makes
		stopped int      // actions held because the writer changed
	}{
		{"create", func(_ *cffake.Fake, _ *memStore, in *DNSInput) {
			in.Records = append(in.Records, wantRecord(zone1, "app.example.com"), wantRecord(zone1, "www.example.com"))
		}, oneRead, 2},
		{"update", func(f *cffake.Fake, _ *memStore, in *DNSInput) {
			f.SeedRecord(zone1.ID, ourCNAME("app.example.com", "old"))
			in.Records = append(in.Records, wantRecord(zone1, "app.example.com"))
		}, oneRead, 1},
		{"adoption", func(f *cffake.Fake, _ *memStore, in *DNSInput) {
			f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "app.other.net"})
			in.Records = append(in.Records, wantRecord(zone1, "app.example.com"))
			in.Adopt = map[string]bool{"app.example.com": true}
		}, oneRead, 1},
		{"adoption of an address record", func(f *cffake.Fake, _ *memStore, in *DNSInput) {
			f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"})
			in.Records = append(in.Records, wantRecord(zone1, "app.example.com"))
			in.Adopt = map[string]bool{"app.example.com": true}
		}, oneRead, 2},
		{"probe", func(f *cffake.Fake, _ *memStore, _ *DNSInput) {
			f.SeedRecord(zone1.ID, probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)))
		}, oneRead, 1},
		{"every write after the first", func(f *cffake.Fake, _ *memStore, in *DNSInput) {
			f.SeedRecord(zone1.ID, ourCNAME("www.example.com", "old"))
			f.SeedRecord(zone1.ID, probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)))
			in.Records = append(in.Records, wantRecord(zone1, "app.example.com"), wantRecord(zone1, "www.example.com"))
		}, oneRead, 3},
		{"tombstone save", func(f *cffake.Fake, _ *memStore, _ *DNSInput) {
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
		}, listings, 0},
		{"delete", func(f *cffake.Fake, store *memStore, _ *DNSInput) {
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			store.m = map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}
		}, listings, 1},
	}
	changes := []struct {
		name    string
		later   answer
		verdict WriterVerdict
	}{
		{"taken over", answer{us: ours, stored: newer}, WriterStale},
		{"identity changed", answer{us: writerAt(5, "n9"), stored: writerAt(5, "n9")}, WriterStale},
		{"cannot be read", answer{err: errors.New("lease lost")}, WriterProceed},
	}
	for _, sc := range scenarios {
		for _, ch := range changes {
			t.Run(sc.name+", "+ch.name, func(t *testing.T) {
				f := newDNSFake()
				store := &memStore{}
				in := dnsIn()
				sc.setup(f, store, &in)
				writer, _ := scripted(answer{us: ours, stored: ours}, ch.later)

				res := newDNSWith(f, store, writer, t0).Run(context.Background(), in, Enforce)

				require.Equal(t, sc.calls, f.Calls())
				require.Equal(t, 0, store.saves)
				require.Equal(t, ch.verdict, res.Verdict)
				require.Len(t, res.Problems, 1)
				require.Empty(t, res.Replaced)
				for _, a := range res.Actions {
					require.False(t, a.Applied)
				}
				stopped := 0
				for _, a := range res.Actions {
					if a.Held == "writer changed" {
						stopped++
					}
				}
				require.Equal(t, sc.stopped, stopped, "actions: %v", res.Actions)
			})
		}
	}
}

func TestDNSWriterChangesBeforeSecondDelete(t *testing.T) {
	f := newDNSFake()
	store := &memStore{m: map[string]Tombstone{}}
	for _, name := range []string{"a.example.com", "b.example.com"} {
		f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
		store.m[stoneKey(zone1.ID, name)] = overdue
	}
	w := &writerBox{us: ours, stored: ours}
	s := &dnsSpy{API: f, afterWrite: func(string) { w.stored = newer }}

	res := newDNSWith(s, store, w.get, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Equal(t, WriterStale, res.Verdict)
	require.Len(t, callsTo(f, "DeleteRecord"), 1)
	require.Equal(t, []Action{
		dnsAction(DeleteRecord, "a.example.com", "", true),
		dnsAction(DeleteRecord, "b.example.com", "writer changed", true),
	}, withoutDetail(res.Actions))
	require.Equal(t, 1, store.saves, "only the save before the deletes")
}

func TestDNSWriterChangesBetweenAdoptionHalves(t *testing.T) {
	f := newDNSFake()
	seeded := f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"})
	w := &writerBox{us: ours, stored: ours}
	s := &dnsSpy{API: f, afterWrite: func(string) { w.stored = newer }}
	in := dnsIn("app.example.com")
	in.Adopt = map[string]bool{"app.example.com": true}

	res := newDNSWith(s, &memStore{}, w.get, t0).Run(context.Background(), in, Enforce)

	require.Equal(t, WriterStale, res.Verdict)
	require.Equal(t, []string{"DeleteRecord zone1 " + seeded.ID}, dnsWrites(f))
	require.Equal(t, []cfapi.Record{seeded}, res.Replaced)
	require.Equal(t, []Action{
		dnsAction(DeleteRecord, "app.example.com", "", true),
		dnsAction(CreateRecord, "app.example.com", "writer changed", true),
	}, withoutDetail(res.Actions))
	last := res.Problems[len(res.Problems)-1]
	require.Contains(t, last, "adoption failed")
	require.Contains(t, last, "A app.example.com 192.0.2.10")
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

func TestDNSRecordOfOursAppearsDuringRun(t *testing.T) {
	f := newDNSFake()
	s := &dnsSpy{API: f, lookup: func(_ string, filter cfapi.RecordFilter) error {
		if filter.Name == "app.example.com" {
			f.SeedRecord(zone1.ID, ourCNAME("app.example.com", "old"))
		}
		return nil
	}}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "app.example.com in zone example.com: a record of this install appeared during the run")
	require.Empty(t, dnsWrites(f))
	require.Empty(t, res.Actions)
	require.Empty(t, res.Conflicts)
	require.Empty(t, res.Lost)
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

func TestDNSLookupFailureSkipsName(t *testing.T) {
	f := newDNSFake()
	s := &dnsSpy{API: f, lookup: func(_ string, filter cfapi.RecordFilter) error {
		if filter.Name == "app.example.com" {
			return errors.New("connection reset by peer")
		}
		return nil
	}}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com", "www.example.com"), Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "app.example.com in zone example.com")
	require.Equal(t, []string{"CreateRecord zone1 www.example.com"}, dnsWrites(f))
	require.Equal(t, []Action{dnsAction(CreateRecord, "www.example.com", "", false)}, withoutDetail(res.Actions))
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
