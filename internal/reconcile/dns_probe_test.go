package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

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
