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
		RunAddOnly(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", added, blocked)}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, WriterProceed, res.Verdict)
	require.Equal(t, rulesOf(ours, app, added, web), configIn(t, f, "acct1").Ingress,
		"the plan's rule added, the rule it drops and the one it would block kept, the sentinel of this writer")
	require.Len(t, res.Tunnels, 1)
	require.Equal(t, tun.ID, res.Tunnels[0].ID)
	require.True(t, res.Tunnels[0].Verified)
	require.Equal(t, []planner.IngressRule{app, web}, res.Tunnels[0].Kept)
}

// A run that may only add leaves a tunnel without a plan as it is, where a
// run would empty it.
func TestARunThatMayOnlyAddEmptiesNoTunnel(t *testing.T) {
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		RunAddOnly(context.Background(), nil, map[string]string{"acct1": "cred1"}, Enforce)

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
