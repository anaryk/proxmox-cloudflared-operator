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
	ourTunnel = TunnelState{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: testTunnelID, Exists: true}

	forbidden = &cfapi.Error{Status: http.StatusForbidden, Codes: []int{10000}, Message: "Authentication error"}
)

// memStore keeps tombstones in memory and counts how it is used.
type memStore struct {
	m       map[string]time.Time
	loadErr error
	saveErr error
	loads   int
	saves   int
}

func (s *memStore) Load(context.Context) (map[string]time.Time, error) {
	s.loads++
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return maps.Clone(s.m), nil
}

func (s *memStore) Save(_ context.Context, m map[string]time.Time) error {
	s.saves++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.m = maps.Clone(m)
	return nil
}

// dnsSpy wraps an API to change what a listing answers: lookup runs before
// each call to Records, and an error it returns fails that call.
type dnsSpy struct {
	cfapi.API
	lookup func(zoneID string, f cfapi.RecordFilter) error
}

func (s *dnsSpy) Records(ctx context.Context, zoneID string, f cfapi.RecordFilter) ([]cfapi.Record, error) {
	if err := s.lookup(zoneID, f); err != nil {
		return nil, err
	}
	return s.API.Records(ctx, zoneID, f)
}

// newDNSFake returns a fake with account acct1 and its zones example.com
// (zone1) and shop.cz (zone2).
func newDNSFake() *cffake.Fake {
	f := newFake("acct1")
	f.AddZone(zone1.ID, zone1.Name, "acct1")
	f.AddZone(zone2.ID, zone2.Name, "acct1")
	return f
}

func newDNS(api cfapi.API, store TombstoneStore, now time.Time) *DNSReconciler {
	return NewDNSReconciler(Clients{"cred1": api}, store, DNSSettings{InstallID: testInstall}, (&clock{now}).now, zerolog.Nop())
}

func wantRecord(z ZoneRef, name string) planner.RecordPlan {
	return planner.RecordPlan{ZoneID: z.ID, ZoneName: z.Name, AccountID: "acct1", CredentialID: z.CredentialID, Name: name, TunnelName: testTunnel}
}

// dnsIn wants each name in example.com, with both zones and our tunnel known
// and a complete inventory.
func dnsIn(names ...string) DNSInput {
	in := DNSInput{Zones: []ZoneRef{zone1, zone2}, Tunnels: []TunnelState{ourTunnel}, InventoryOK: true}
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

func TestDNSCreate(t *testing.T) {
	f := newDNSFake()
	store := &memStore{}

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []string{"Records zone1", "Records zone2", "Records zone1", "CreateRecord zone1 app.example.com"}, f.Calls())
	require.Equal(t, []cfapi.Record{{ID: "rec-1", Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: testMarker}},
		recordsIn(f, zone1.ID))
	require.Equal(t, []Action{dnsAction(CreateRecord, "app.example.com", "", false)}, withoutDetail(res.Actions))
	require.Contains(t, res.Actions[0].Detail, testTarget)
	require.Empty(t, res.Conflicts)
	require.Empty(t, res.Lost)
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
			require.Equal(t, []string{"UpdateRecord zone1 " + seeded.ID}, dnsWrites(f))
			require.Equal(t, []cfapi.Record{{
				ID: seeded.ID, Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true,
				Comment: testMarker + " keep this note",
			}}, recordsIn(f, zone1.ID))
			require.Equal(t, []Action{dnsAction(UpdateRecord, "app.example.com", "", false)}, withoutDetail(res.Actions))
			require.Contains(t, res.Actions[0].Detail, tc.was)
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
			store := &memStore{m: map[string]time.Time{stoneKey(zone1.ID, "gone.example.com"): t0.Add(-time.Hour)}}

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
			stones := map[string]time.Time{stoneKey(zone1.ID, "gone.example.com"): t0.Add(-time.Hour)}
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
	stone := map[string]time.Time{stoneKey(zone1.ID, "gone.example.com"): t0}

	res := newDNS(f, store, t0).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, dnsWrites(f))
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "grace period: 1m0s left", true)}, withoutDetail(res.Actions))
	require.Equal(t, stone, store.m)
	require.Equal(t, 1, store.saves)

	res = newDNS(f, store, t0.Add(30*time.Second)).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, dnsWrites(f))
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "grace period: 30s left", true)}, withoutDetail(res.Actions))
	require.Equal(t, stone, store.m, "the grace runs from the first sighting")
	require.Equal(t, 1, store.saves, "nothing changed")

	res = newDNS(f, store, t0.Add(61*time.Second)).Run(ctx, dnsIn(), Enforce)
	require.Empty(t, res.Problems)
	calls := f.Calls()
	require.Equal(t, []string{"Records zone1", "DeleteRecord zone1 " + gone.ID}, calls[len(calls)-2:], "read again right before the delete")
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", "", true)}, withoutDetail(res.Actions))
	require.Empty(t, f.RecordsIn(zone1.ID))
	require.Empty(t, store.m)
	require.Equal(t, 2, store.saves)
}

func TestDNSWantedAgainClearsTombstone(t *testing.T) {
	ctx := context.Background()
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("app.example.com", testTunnelID))
	store := &memStore{m: map[string]time.Time{stoneKey(zone1.ID, "app.example.com"): t0.Add(-30 * time.Second)}}

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
	require.Equal(t, map[string]time.Time{stoneKey(zone1.ID, "app.example.com"): later}, store.m)
}

func TestDNSInventoryIncompleteHolds(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	f.SeedRecord(zone1.ID, ourCNAME("old.example.com", testTunnelID))
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "web.example.com", Content: "192.0.2.10"})
	stones := map[string]time.Time{
		stoneKey(zone1.ID, "gone.example.com"):     t0.Add(-time.Hour),
		stoneKey(zone1.ID, "app.example.com"):      t0.Add(-time.Hour),           // wanted again
		stoneKey(zone1.ID, "vanished.example.com"): t0.Add(-time.Hour),           // its record is gone
		stoneKey("zone9", "old.example.org"):       t0.Add(-40 * 24 * time.Hour), // its zone is no longer managed
	}
	store := &memStore{m: maps.Clone(stones)}
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
	require.Equal(t, stones, store.m)
	require.Equal(t, 0, store.saves)
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
			store := &memStore{m: map[string]time.Time{}}
			in := dnsIn()
			in.ConfirmDeletes = tc.confirm
			for i := range tc.owned {
				name := fmt.Sprintf("h%02d.example.com", i)
				f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
				if i < tc.unwanted {
					store.m[stoneKey(zone1.ID, name)] = t0.Add(-2 * time.Minute)
					continue
				}
				in.Records = append(in.Records, wantRecord(zone1, name))
			}
			if tc.adoptA {
				f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "web.example.com", Content: "192.0.2.10"})
				in.Records = append(in.Records, wantRecord(zone1, "web.example.com"))
				in.Adopt = map[string]bool{"web.example.com": true}
			}
			before := maps.Clone(store.m)

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Len(t, callsTo(f, "DeleteRecord"), tc.deleted)
			if tc.held == "" {
				require.Empty(t, store.m)
				return
			}
			require.Len(t, res.Actions, tc.unwanted)
			for _, a := range res.Actions {
				require.Equal(t, dnsAction(DeleteRecord, a.Target, tc.held, true), withoutDetail([]Action{a})[0])
			}
			require.Equal(t, before, store.m, "held deletes keep their tombstones")
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
			names := []string{}
			for _, r := range f.RecordsIn(zone1.ID) {
				names = append(names, r.Name)
			}
			require.Equal(t, []string{"_pco-probe-edge.example.com", "_pco-probe-new.example.com"}, names)
			require.Equal(t, 0, store.saves, "probes get no tombstone")
		})
	}
}

func TestDNSProbeCommentMustBeExact(t *testing.T) {
	f := newDNSFake()
	rec := probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour))
	rec.Comment += " kept by hand"
	f.SeedRecord(zone1.ID, rec)

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Empty(t, dnsWrites(f), "an ordinary record of ours waits for its grace")
	require.Equal(t, []Action{dnsAction(DeleteRecord, "_pco-probe-x.example.com", "grace period: 1m0s left", true)}, withoutDetail(res.Actions))
}

func TestDNSUnknownTunnelHoldsCreate(t *testing.T) {
	cases := []struct {
		name    string
		tunnels []TunnelState
	}{
		{"tunnel not known", nil},
		{"tunnel does not exist", []TunnelState{{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: testTunnelID}}},
		{"tunnel of another account", []TunnelState{{AccountID: "acct2", CredentialID: "cred1", Name: testTunnel, ID: testTunnelID, Exists: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, ourCNAME("www.example.com", "old"))
			store := &memStore{}
			in := dnsIn("app.example.com", "www.example.com")
			in.Tunnels = tc.tunnels

			res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, []string{"Records zone1", "Records zone2"}, f.Calls())
			require.Equal(t, []Action{
				dnsAction(CreateRecord, "app.example.com", "tunnel not created yet", false),
				dnsAction(UpdateRecord, "www.example.com", "tunnel not created yet", false),
			}, withoutDetail(res.Actions))
			require.Equal(t, 0, store.saves, "a wanted record gets no tombstone")
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
	store := &memStore{m: map[string]time.Time{stoneKey(zone1.ID, "gone.example.com"): t0.Add(-time.Hour)}}
	in := dnsIn("app.example.com", "stale.example.com", "web.example.com")
	in.Adopt = map[string]bool{"web.example.com": true}

	res := newDNS(f, store, t0).Run(context.Background(), in, Observe)

	require.Empty(t, res.Problems)
	for _, call := range f.Calls() {
		require.True(t, strings.HasPrefix(call, "Records "), "only reads: %s", call)
	}
	require.Equal(t, 0, store.loads)
	require.Equal(t, 0, store.saves)
	require.Equal(t, []Action{
		dnsAction(DeleteRecord, "_pco-probe-x.example.com", "observe mode", true),
		dnsAction(CreateRecord, "app.example.com", "observe mode", false),
		dnsAction(DeleteRecord, "gone.example.com", "observe mode", true),
		dnsAction(UpdateRecord, "stale.example.com", "observe mode", false),
		dnsAction(DeleteRecord, "web.example.com", "observe mode", true),
		dnsAction(CreateRecord, "web.example.com", "observe mode", true),
	}, withoutDetail(res.Actions))
	require.Equal(t, before, f.RecordsIn(zone1.ID))
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

func TestDNSTombstoneSaveFailure(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	store := &memStore{m: map[string]time.Time{stoneKey(zone1.ID, "gone.example.com"): t0.Add(-time.Hour)}, saveErr: errors.New("disk full")}

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "saving dns tombstones: disk full")
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
			store := &memStore{m: map[string]time.Time{}}
			in := dnsIn()
			for i := range 10 {
				name := fmt.Sprintf("h%02d.example.com", i)
				f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
				if i < tc.unwanted {
					store.m[stoneKey(zone1.ID, name)] = t0.Add(-tc.since)
					continue
				}
				in.Records = append(in.Records, wantRecord(zone1, name))
			}
			r := NewDNSReconciler(Clients{"cred1": f}, store, s, (&clock{t0}).now, zerolog.Nop())

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

func TestDNSDeleteFailureKeepsTombstone(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	stones := map[string]time.Time{stoneKey(zone1.ID, "gone.example.com"): t0.Add(-time.Hour)}
	store := &memStore{m: maps.Clone(stones)}
	f.Deny("dns.write")

	res := newDNS(f, store, t0).Run(context.Background(), dnsIn(), Enforce)

	require.Len(t, res.Problems, 1)
	require.Contains(t, res.Problems[0], "gone.example.com in zone example.com: deleting the record")
	require.Equal(t, []Action{dnsAction(DeleteRecord, "gone.example.com", forbidden.Error(), true)}, withoutDetail(res.Actions))
	require.Equal(t, stones, store.m)
	require.Len(t, f.RecordsIn(zone1.ID), 1)
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
		change    func(f *cffake.Fake, gone cfapi.Record) error // runs right before the read by name
		problem   string
		keepStone bool
	}{
		{
			name: "marker removed meanwhile",
			change: func(f *cffake.Fake, gone cfapi.Record) error {
				gone.Comment = "kept by hand"
				f.SeedRecord(zone1.ID, gone)
				return nil
			},
		},
		{
			name: "read fails",
			change: func(*cffake.Fake, cfapi.Record) error {
				return errors.New("connection reset by peer")
			},
			problem:   "gone.example.com in zone example.com: reading the records again before deleting them",
			keepStone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			gone := f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			stones := map[string]time.Time{stoneKey(zone1.ID, "gone.example.com"): t0.Add(-time.Hour)}
			store := &memStore{m: maps.Clone(stones)}
			s := &dnsSpy{API: f, lookup: func(_ string, filter cfapi.RecordFilter) error {
				if filter.Name == "gone.example.com" {
					return tc.change(f, gone)
				}
				return nil
			}}

			res := newDNS(s, store, t0).Run(context.Background(), dnsIn(), Enforce)

			require.Empty(t, dnsWrites(f))
			require.Empty(t, res.Actions)
			require.Len(t, f.RecordsIn(zone1.ID), 1)
			if tc.problem != "" {
				require.Len(t, res.Problems, 1)
				require.Contains(t, res.Problems[0], tc.problem)
			} else {
				require.Empty(t, res.Problems)
			}
			if tc.keepStone {
				require.Equal(t, stones, store.m)
			} else {
				require.Empty(t, store.m)
			}
		})
	}
}

func TestDNSTombstoneHousekeeping(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("kept.example.com", testTunnelID))
	store := &memStore{m: map[string]time.Time{
		stoneKey(zone1.ID, "kept.example.com"):  t0.Add(-10 * time.Second), // record there, in its grace
		stoneKey(zone1.ID, "gone.example.com"):  t0.Add(-10 * time.Second), // record gone
		stoneKey("zone9", "old.example.org"):    t0.Add(-31 * 24 * time.Hour),
		stoneKey("zone9", "recent.example.org"): t0.Add(-29 * 24 * time.Hour),
		stoneKey(zone2.ID, "unread.shop.cz"):    t0.Add(-31 * 24 * time.Hour), // its zone cannot be read
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
	require.Equal(t, map[string]time.Time{
		stoneKey(zone1.ID, "kept.example.com"):  t0.Add(-10 * time.Second),
		stoneKey("zone9", "recent.example.org"): t0.Add(-29 * 24 * time.Hour),
		stoneKey(zone2.ID, "unread.shop.cz"):    t0.Add(-31 * 24 * time.Hour),
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

func TestDNSNoInstallID(t *testing.T) {
	f := newDNSFake()
	r := NewDNSReconciler(Clients{"cred1": f}, &memStore{}, DNSSettings{}, (&clock{t0}).now, zerolog.Nop())

	res := r.Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Len(t, res.Problems, 1)
	require.Empty(t, f.Calls())
}
