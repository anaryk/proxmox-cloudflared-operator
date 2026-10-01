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
	original := cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10", Proxied: true, TTL: 1, Comment: "web front, by hand"}
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
