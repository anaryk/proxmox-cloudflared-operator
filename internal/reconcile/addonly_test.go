package reconcile

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// A run that may only add writes the rules of the plan and keeps every rule
// the tunnel serves that the plan leaves out or would block: nothing it
// serves is withdrawn. The rules it kept are said, so that the egress sets
// keep what they send a connector to.
func TestARunThatMayOnlyAddKeepsWhatTheTunnelServes(t *testing.T) {
	f := newFake("acct1")
	old := writerAt(4, "n4")
	tun := f.SeedTunnel("acct1", testTunnel, rulesOf(old, app, web))
	blocked := planner.IngressRule{Hostname: "web.example.com", Service: planner.BlockedService}
	added := planner.IngressRule{Hostname: "new.example.com", Service: "http://10.0.0.7:80"}

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		RunAddOnly(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", added, blocked)}, nil, Enforce, anyHost)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, rulesOf(ours, app, added, web), configIn(t, f, "acct1").Ingress,
		"the plan's rule added, the rule it drops and the one it would block kept, the sentinel of this writer")
	require.Len(t, res.Tunnels, 1)
	require.Equal(t, tun.ID, res.Tunnels[0].ID)
	require.True(t, res.Tunnels[0].Verified)
	require.Equal(t, []planner.IngressRule{app, web}, res.Tunnels[0].Kept)
}

func anyHost(string) bool { return true }

// A run that may only add keeps nothing the caller does not name as its own,
// and nothing of a configuration pco did not write: a rule someone put into
// the tunnel is not written again under this writer's sentinel.
func TestARunThatMayOnlyAddKeepsOnlyWhatPcoServed(t *testing.T) {
	stranger := planner.IngressRule{Hostname: "evil.example.com", Service: "http://10.9.9.9:22"}
	tests := []struct {
		name   string
		remote []planner.IngressRule
		keep   func(string) bool
	}{
		{"a hostname the caller does not own", rulesOf(writerAt(4, "n4"), app, stranger), func(h string) bool { return h == app.Hostname }},
		{"no sentinel of this install", []planner.IngressRule{app, stranger, catchAll}, anyHost},
		{"a sentinel of another install", rulesOf(planner.Writer{InstallID: "zzz", Generation: 1, Nonce: "x"}, app, stranger), anyHost},
		{"a sentinel of a later generation", rulesOf(writerAt(9, "n9"), app, stranger), anyHost},
		{"no filter", rulesOf(writerAt(4, "n4"), app, stranger), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake("acct1")
			f.SeedTunnel("acct1", testTunnel, tt.remote)

			res := newReconciler(Clients{"cred1": f}, &clock{t0}).
				RunAddOnly(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", web)}, nil, Enforce, tt.keep)

			for _, st := range res.Tunnels {
				require.NotContains(t, st.Kept, stranger)
			}
			for _, call := range callsTo(f, "PutTunnelConfig") {
				require.NotContains(t, call, "evil")
			}
			if len(callsTo(f, "PutTunnelConfig")) > 0 {
				require.NotContains(t, configIn(t, f, "acct1").Ingress, stranger)
			}
		})
	}
}

// A run that may only add leaves a tunnel without a plan as it is, where a
// run would empty it.
func TestARunThatMayOnlyAddEmptiesNoTunnel(t *testing.T) {
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		RunAddOnly(context.Background(), nil, map[string]string{"acct1": "cred1"}, Enforce, anyHost)

	require.Empty(t, res.Problems)
	require.Empty(t, callsTo(f, "PutTunnelConfig"))
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
}

// The same plan run normally drops what the plan leaves out: the add-only
// run is what keeps it.
func TestARunDropsWhatThePlanLeavesOut(t *testing.T) {
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app, web))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, rulesOf(ours, app), configIn(t, f, "acct1").Ingress)
	require.Empty(t, res.Tunnels[0].Kept)
}

// A DNS run told to delete nothing holds every delete with the reason it is
// given, and still makes and points the records the plan wants.
func TestADNSRunThatDeletesNothing(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, ourCNAME("gone.example.com", testTunnelID))
	store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "gone.example.com"): overdue}}
	in := dnsIn("new.example.com")
	in.NoDeletes = "takeover: nothing is withdrawn in the first cycle"

	res := newDNS(f, store, t0).Run(context.Background(), in, Enforce)

	require.Equal(t, []string{"CreateRecord " + zone1.ID + " new.example.com"}, dnsWrites(f))
	require.Contains(t, res.Actions, Action{
		Kind: DeleteRecord, Credential: "cred1", Target: "gone.example.com", Destructive: true,
		Detail: "in zone example.com: CNAME " + testTunnelID + ".cfargotunnel.com",
		Held:   "takeover: nothing is withdrawn in the first cycle",
	})
}
