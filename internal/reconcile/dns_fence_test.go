package reconcile

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

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
