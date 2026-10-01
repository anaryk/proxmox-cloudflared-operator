package reconcile

import (
	"context"
	"maps"
	"net/http"
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
