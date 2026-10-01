package reconcile

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const heldNotVerified = "tunnel configuration not verified"

// unverified is our tunnel as a run leaves it whose configuration write was
// held, failed or read back different.
var unverified = TunnelState{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: testTunnelID, Version: 3, Exists: true}

func TestDNSWritesWaitForAVerifiedTunnel(t *testing.T) {
	cases := []struct {
		name    string
		seed    *cfapi.Record
		adopt   bool
		actions []Action
	}{
		{"create", nil, false, []Action{dnsAction(CreateRecord, "app.example.com", heldNotVerified, false)}},
		{"retarget", &cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "old.cfargotunnel.com", Proxied: true, Comment: testMarker}, false,
			[]Action{dnsAction(UpdateRecord, "app.example.com", heldNotVerified, false)}},
		{"proxying turned on", &cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Comment: testMarker}, false,
			[]Action{dnsAction(UpdateRecord, "app.example.com", heldNotVerified, false)}},
		{"adoption of a CNAME", &cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "app.other.net"}, true,
			[]Action{dnsAction(UpdateRecord, "app.example.com", heldNotVerified, true)}},
		{"adoption of an address record", &cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"}, true, []Action{
			dnsAction(DeleteRecord, "app.example.com", heldNotVerified, true),
			dnsAction(CreateRecord, "app.example.com", heldNotVerified, true),
		}},
		{"nothing to change", &cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, Comment: testMarker}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDNSFake()
			if tc.seed != nil {
				f.SeedRecord(zone1.ID, *tc.seed)
			}
			before := f.RecordsIn(zone1.ID)
			in := dnsIn("app.example.com")
			in.Tunnels = []TunnelState{unverified}
			if tc.adopt {
				in.Adopt = map[string]bool{"app.example.com": true}
			}

			res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Enforce)

			require.Empty(t, res.Problems)
			require.Empty(t, dnsWrites(f))
			require.Equal(t, tc.actions, withoutDetail(res.Actions))
			require.Empty(t, res.Replaced)
			require.Equal(t, before, f.RecordsIn(zone1.ID))
		})
	}
}

func TestDNSDeletesDoNotWaitForTheTunnel(t *testing.T) {
	f := newDNSFake()
	gone := f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	f.SeedRecord(zone1.ID, probeRecord("_pco-probe-x.example.com", t0.Add(-time.Hour)))
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}
	in := dnsIn("app.example.com")
	in.Tunnels = []TunnelState{unverified}

	res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

	require.Contains(t, dnsWrites(f), "DeleteRecord zone1 "+gone.ID)
	require.Len(t, callsTo(f, "DeleteRecord"), 2, "the record and the probe")
	require.Empty(t, callsTo(f, "CreateRecord"))
	require.Empty(t, res.Problems)
}

func TestDNSObserveReportsWritesAsBefore(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("www.example.com", "old"))
	in := dnsIn("app.example.com", "www.example.com")
	in.Tunnels = []TunnelState{unverified}

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), in, Observe)

	require.Equal(t, []Action{
		dnsAction(CreateRecord, "app.example.com", "observe mode", false),
		dnsAction(UpdateRecord, "www.example.com", "observe mode", false),
	}, withoutDetail(res.Actions))
}

func TestDNSRetargetTurnsProxyingOnWithTheAutomaticTTL(t *testing.T) {
	f := newDNSFake()
	seeded := f.SeedRecord(zone1.ID, cfapi.Record{Type: "CNAME", Name: "app.example.com", Content: "old.cfargotunnel.com", TTL: 300, Comment: testMarker})
	s := &dnsSpy{API: f}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []cfapi.Record{{
		ID: seeded.ID, Type: "CNAME", Name: "app.example.com", Content: testTarget, Proxied: true, TTL: 1,
		Comment: testMarker, ModifiedOn: seeded.ModifiedOn,
	}}, s.updates)
}

// TestDNSRunOfAStoppedTunnelRunDoesNothing feeds the DNS run what a tunnel
// run that found another writer returned: no record is created, and none is
// deleted when its grace is over.
func TestDNSRunOfAStoppedTunnelRunDoesNothing(t *testing.T) {
	cases := []struct {
		name     string
		sentinel planner.Writer
		stored   planner.Writer
		verdict  WriterVerdict
		says     string
	}{
		{"foreign", writerAt(5, "zz"), ours, WriterForeign, "foreign writer"},
		{"stale", writerAt(7, "n7"), writerAt(7, "n7"), WriterStale, "stale writer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newDNSFake()
			f.SeedTunnel("acct1", testTunnel, rulesOf(tc.sentinel, app))
			f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
			store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}
			writer, _ := scripted(answer{us: ours, stored: ours}, answer{us: ours, stored: ours}, answer{us: ours, stored: tc.stored})
			tunnels := reconcilerWith(Clients{"cred1": f}, writer)
			c := &clock{t0}
			dns := newDNSAt(f, store, writerOf(ours, ours), c)

			for _, at := range []time.Duration{0, 2 * time.Minute} {
				c.t = t0.Add(at)
				tres := tunnels.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)
				require.Equal(t, tc.verdict, tres.Verdict)
				calls := len(f.Calls())

				in := dnsIn("app.example.com")
				in.Tunnels, in.TunnelVerdict = tres.Tunnels, tres.Verdict
				res := dns.Run(ctx, in, Enforce)

				require.Equal(t, tc.verdict, res.Verdict, "%s on", at)
				require.Equal(t, []string{"dns: the tunnel run of this cycle found a " + tc.says + "; changing no dns record"}, res.Problems)
				require.Empty(t, res.Actions)
				require.Len(t, f.Calls(), calls, "not even a listing")
				require.Equal(t, 0, store.loads)
			}
			require.Empty(t, dnsWrites(f))
		})
	}
}

func TestDNSRunOfAStoppedTunnelRunRemembersLikeAStaleStart(t *testing.T) {
	for _, tc := range []struct {
		verdict  WriterVerdict
		remember bool
	}{
		{WriterForeign, true},
		{WriterStale, false},
	} {
		t.Run(fmt.Sprint(tc.verdict), func(t *testing.T) {
			r := newDNS(newDNSFake(), &memStore{}, t0)
			in := dnsIn("app.example.com")
			in.TunnelVerdict = tc.verdict

			r.Run(context.Background(), in, Enforce)

			require.Equal(t, tc.remember, r.wantedSinceSave[stoneKey(zone1.ID, "app.example.com")])
		})
	}
}
