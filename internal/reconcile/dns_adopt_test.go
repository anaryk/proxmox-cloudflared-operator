package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

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
			require.Equal(t, []cfapi.Record{{ID: seeded.ID, Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, TTL: 1, Comment: testMarker}},
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
			require.Equal(t, []cfapi.Record{{ID: "rec-2", Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, TTL: 1, Comment: testMarker}},
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
	original := cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10", TTL: 300, Comment: "web front, by hand"}
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
			for _, part := range []string{"A", "app.example.com", "192.0.2.10", "proxied false", "ttl 300", `"web front, by hand"`, seeded.ID} {
				require.Contains(t, last, part, "the admin can restore the record by hand")
			}
		})
	}
}

// TestDNSAdoptionAnswerLost loses the answer to the first write of an
// adoption: the write may have landed, so the snapshot must be there.
func TestDNSAdoptionAnswerLost(t *testing.T) {
	cases := []struct {
		name   string
		record cfapi.Record
	}{
		{"CNAME changed in place", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "app.other.net"}},
		{"address record deleted", cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			seeded := f.SeedRecord(zone1.ID, tc.record)
			s := &dnsSpy{API: f, lostAnswers: true}
			in := dnsIn("app.example.com")
			in.Adopt = map[string]bool{"app.example.com": true}

			res := newDNS(s, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Len(t, dnsWrites(f), 1)
			require.Equal(t, []cfapi.Record{seeded}, res.Replaced)
			require.NotEmpty(t, res.Problems)
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

// TestDNSAdoptionCompletesInACancelledRun cancels the run right after an
// adoption deleted the address record: the name must not stay empty.
func TestDNSAdoptionCompletesInACancelledRun(t *testing.T) {
	original := cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10", TTL: 300, Comment: "by hand"}
	cases := []struct {
		name   string
		refuse func(r cfapi.Record) error
		want   cfapi.Record
	}{
		{"the CNAME is created", nil,
			cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, TTL: 1, Comment: testMarker}},
		{"the original is put back", func(r cfapi.Record) error {
			if r.Type == "CNAME" {
				return errors.New("refused by the test")
			}
			return nil
		}, original},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, original)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := &dnsSpy{API: f, failCreate: tc.refuse, afterWrite: func(method string) {
				if method == "DeleteRecord" {
					cancel()
				}
			}}
			in := dnsIn("app.example.com")
			in.Adopt = map[string]bool{"app.example.com": true}

			newDNS(s, &memStore{}, t0).Run(ctx, in, Enforce)

			require.Equal(t, []cfapi.Record{tc.want}, withoutIDs(recordsIn(f, zone1.ID)))
		})
	}
}

// adoptions are the two kinds of adoption, with the actions each takes.
var adoptions = []struct {
	name    string
	record  cfapi.Record
	actions func(held string) []Action
}{
	{"CNAME", cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "app.other.net"}, func(held string) []Action {
		return []Action{dnsAction(UpdateRecord, "app.example.com", held, true)}
	}},
	{"address record", cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10", TTL: 300}, func(held string) []Action {
		return []Action{dnsAction(DeleteRecord, "app.example.com", held, true), dnsAction(CreateRecord, "app.example.com", held, true)}
	}},
}

func TestDNSAdoptionStoresTheRecordFirst(t *testing.T) {
	for _, tc := range adoptions {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			seeded := f.SeedRecord(zone1.ID, tc.record)
			var zones []ZoneRef
			var snapshots []cfapi.Record
			in := dnsIn("app.example.com")
			in.Adopt = map[string]bool{"app.example.com": true}
			in.BeforeReplace = func(_ context.Context, zone ZoneRef, rec cfapi.Record) error {
				calls := f.Calls()
				require.Empty(t, dnsWrites(f), "before any write")
				require.Equal(t, "Records zone1", calls[len(calls)-1], "after the record was read again")
				zones = append(zones, zone)
				snapshots = append(snapshots, rec)
				return nil
			}

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Equal(t, []ZoneRef{zone1}, zones)
			require.Equal(t, []cfapi.Record{seeded}, snapshots, "once, with the record as it was")
			require.Equal(t, tc.actions(""), withoutDetail(res.Actions))
			require.Equal(t, []cfapi.Record{seeded}, res.Replaced)
		})
	}
}

func TestDNSAdoptionWithoutASnapshotIsHeld(t *testing.T) {
	for _, tc := range adoptions {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, tc.record)
			before := f.RecordsIn(zone1.ID)
			in := dnsIn("app.example.com", "www.example.com")
			in.Adopt = map[string]bool{"app.example.com": true}
			in.BeforeReplace = func(context.Context, ZoneRef, cfapi.Record) error { return errDisk }

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Equal(t, []string{"app.example.com in zone example.com: storing the record before the adoption: disk full"}, res.Problems)
			require.Equal(t, []string{"CreateRecord zone1 www.example.com"}, dnsWrites(f), "the run goes on with the next name")
			require.Equal(t, append(tc.actions("snapshot not stored"), dnsAction(CreateRecord, "www.example.com", "", false)), withoutDetail(res.Actions))
			require.Empty(t, res.Replaced)
			require.Equal(t, before, f.RecordsIn(zone1.ID)[:1])
		})
	}
}

func TestDNSWriterChangeAfterTheSnapshotStopsTheAdoption(t *testing.T) {
	for _, tc := range adoptions {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			f.SeedRecord(zone1.ID, tc.record)
			w := &writerBox{us: ours, stored: ours}
			in := dnsIn("app.example.com")
			in.Adopt = map[string]bool{"app.example.com": true}
			in.BeforeReplace = func(context.Context, ZoneRef, cfapi.Record) error {
				w.stored = newer
				return nil
			}

			res := newDNSWith(f, &memStore{}, w.get, t0).Run(context.Background(), in, Enforce)

			require.Equal(t, WriterStale, res.Verdict)
			require.Empty(t, dnsWrites(f))
			require.Equal(t, tc.actions("writer changed"), withoutDetail(res.Actions))
			require.Empty(t, res.Replaced)
		})
	}
}

func TestDNSNoSnapshotForAnAdoptionThatIsHeld(t *testing.T) {
	cases := []struct {
		name        string
		change      func(in *DNSInput, store *memStore)
		mode        Mode
		addressOnly bool // only the replacement of an address record deletes
	}{
		{"observe mode", func(*DNSInput, *memStore) {}, Observe, false},
		{"tunnel not verified", func(in *DNSInput, _ *memStore) { in.Tunnels = []TunnelState{unverified} }, Enforce, false},
		{"inventory incomplete", func(in *DNSInput, _ *memStore) { in.InventoryOK = false }, Enforce, true},
		{"tombstones not saved", func(_ *DNSInput, store *memStore) {
			store.m = map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}
			store.saveErrs = map[int]error{1: errDisk}
		}, Enforce, true},
	}
	for _, adoption := range adoptions {
		for _, tc := range cases {
			if tc.addressOnly && adoption.record.Type == "CNAME" {
				continue
			}
			t.Run(adoption.name+", "+tc.name, func(t *testing.T) {
				f := newDNSFake()
				f.SeedRecord(zone1.ID, adoption.record)
				f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
				store := &memStore{}
				in := dnsIn("app.example.com")
				in.Adopt = map[string]bool{"app.example.com": true}
				called := 0
				in.BeforeReplace = func(context.Context, ZoneRef, cfapi.Record) error {
					called++
					return nil
				}
				tc.change(&in, store)

				newDNS(f, store, t0).Run(context.Background(), in, tc.mode)

				require.Zero(t, called)
				require.Empty(t, callsTo(f, "CreateRecord", "UpdateRecord"))
			})
		}
	}
}
