package reconcile

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const listingOfAcct1 = "Tunnels acct1 pco-abc-"

var knownAcct1 = map[string]string{"acct1": "cred1"}

func pinnedName(node string) string { return testTunnel + "-" + node }

// pinnedPlanFor is the plan, through cred1, of the tunnel of an account pinned to a node.
func pinnedPlanFor(account, node string, rules ...planner.IngressRule) planner.TunnelPlan {
	return planner.TunnelPlan{
		AccountID: account, CredentialID: "cred1", Name: pinnedName(node),
		Kind: planner.PlacementPinned, Node: node, Rules: rulesOf(ours, rules...),
	}
}

// pinnedAction is an action through cred1 on the tunnel of acct1 pinned to a
// node.
func pinnedAction(kind ActionKind, node, held string) Action {
	return Action{Kind: kind, Credential: "cred1", AccountID: "acct1", Target: pinnedName(node), Applied: held == "", Held: held}
}

func pinnedState(node string, tun cfapi.Tunnel, version int) TunnelState {
	return TunnelState{
		AccountID: "acct1", CredentialID: "cred1", Name: pinnedName(node), Kind: planner.PlacementPinned, Node: node,
		ID: tun.ID, Version: version, Exists: true, Verified: true,
	}
}

// callsAbout returns the calls of the fake that name the tunnel, by name or
// id, since the first n calls.
func callsAbout(f *cffake.Fake, n int, tun cfapi.Tunnel) []string {
	var out []string
	for _, call := range f.Calls()[n:] {
		fields := strings.Fields(call)
		if slices.Contains(fields, tun.ID) || slices.Contains(fields, tun.Name) {
			out = append(out, call)
		}
	}
	return out
}

func findTunnel(t *testing.T, f *cffake.Fake, account, name string) cfapi.Tunnel {
	t.Helper()
	tun, found, err := f.FindTunnel(context.Background(), account, name)
	require.NoError(t, err)
	require.True(t, found, "tunnel %s in %s", name, account)
	return tun
}

func configOf(t *testing.T, f *cffake.Fake, tun cfapi.Tunnel) cfapi.TunnelConfig {
	t.Helper()
	cfg, err := f.TunnelConfig(context.Background(), "acct1", tun.ID)
	require.NoError(t, err)
	return cfg
}

// The shared tunnel and a pinned one of the same account are two tunnels:
// each is created, written and rate limited on its own.
func TestTwoTunnelsOfOneAccountAreWrittenIndependently(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)

	res := r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app), pinnedPlanFor("acct1", "pve2", web)}, knownAcct1, Enforce)

	require.Empty(t, res.Problems)
	shared, pinned := findTunnel(t, f, "acct1", testTunnel), findTunnel(t, f, "acct1", pinnedName("pve2"))
	require.Equal(t, []Action{
		action(CreateTunnel, ""), action(PutConfig, ""),
		pinnedAction(CreateTunnel, "pve2", ""), pinnedAction(PutConfig, "pve2", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: shared.ID, Version: 1, Exists: true, Verified: true},
		pinnedState("pve2", pinned, 1),
	}, res.Tunnels)
	require.Equal(t, rulesOf(ours, app), configOf(t, f, shared).Ingress)
	require.Equal(t, rulesOf(ours, web), configOf(t, f, pinned).Ingress)

	// A change of the pinned plan alone waits for its own interval; the
	// shared tunnel is left verified.
	c.t = t0.Add(10 * time.Second)
	res = r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app), pinnedPlanFor("acct1", "pve2", web, app)}, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{pinnedAction(PutConfig, "pve2", "rate limit: next write in 5s")}, withoutDetail(res.Actions))
	require.True(t, res.Tunnels[0].Verified)
	require.False(t, res.Tunnels[1].Verified)

	c.t = t0.Add(15 * time.Second)
	res = r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app), pinnedPlanFor("acct1", "pve2", web, app)}, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{pinnedAction(PutConfig, "pve2", "")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, web, app), configOf(t, f, pinned).Ingress)

	// Both change: the shared tunnel's last write is long enough ago, the
	// pinned one's is not.
	c.t = t0.Add(16 * time.Second)
	res = r.Run(ctx, []planner.TunnelPlan{planFor("acct1", "cred1", app, web), pinnedPlanFor("acct1", "pve2", web)}, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{action(PutConfig, ""), pinnedAction(PutConfig, "pve2", "rate limit: next write in 14s")}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, app, web), configOf(t, f, shared).Ingress)
	require.Equal(t, rulesOf(ours, web, app), configOf(t, f, pinned).Ingress)
}

// A stale sentinel in one tunnel of an account stops the run before any
// other tunnel, of that account or another, is touched.
func TestAStaleVerdictOnOneTunnelStopsTheRun(t *testing.T) {
	f := newFake("acct1", "acct2")
	shared := f.SeedTunnel("acct1", testTunnel, rulesOf(writerAt(7, "n7"), app))
	pinned := f.SeedTunnel("acct1", pinnedName("pve2"), rulesOf(ours, app))
	other := f.SeedTunnel("acct2", testTunnel, rulesOf(ours, app))
	writer, asked := scripted(answer{us: ours, stored: ours}, answer{us: ours, stored: ours}, answer{us: ours, stored: writerAt(7, "n7")})
	plans := []planner.TunnelPlan{
		planFor("acct1", "cred1", web), pinnedPlanFor("acct1", "pve2", web), planFor("acct2", "cred1", web),
	}

	res := reconcilerWith(Clients{"cred1": f}, writer).Run(context.Background(), plans, nil, Enforce)

	require.Equal(t, WriterStale, res.Verdict)
	require.Equal(t, writerAt(7, "n7"), res.Sentinel)
	require.Equal(t, []string{
		"pco-abc in account acct1: leader.json names generation 7 nonce n7, not this writer (generation 5 nonce n5); this writer is stale and stops",
	}, res.Problems)
	require.Equal(t, 3, *asked)
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: shared.ID, Version: 1, Exists: true},
		{AccountID: "acct1", CredentialID: "cred1", Name: pinnedName("pve2"), Kind: planner.PlacementPinned, Node: "pve2", Unknown: true},
		unknownIn("acct2"),
	}, res.Tunnels)
	require.Empty(t, callsTo(f, "PutTunnelConfig", "CreateTunnel"))
	require.Equal(t, rulesOf(ours, app), configOf(t, f, pinned).Ingress)
	cfg, err := f.TunnelConfig(context.Background(), "acct2", other.ID)
	require.NoError(t, err)
	require.Equal(t, rulesOf(ours, app), cfg.Ingress)
}

// pinnedListings returns the listings of pinned tunnels the fake answered,
// which the probe sweep's listings are not.
func pinnedListings(f *cffake.Fake) []string {
	var out []string
	for _, call := range callsTo(f, "Tunnels") {
		if strings.HasSuffix(call, " pco-abc-") {
			out = append(out, call)
		}
	}
	return out
}

// In Observe mode the reads are the same: a pinned tunnel whose held write
// is still planned is read every run, one that is in line is read for drift.
func TestPinnedTunnelsAreReadForDriftInObserveMode(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	inLine := f.SeedTunnel("acct1", pinnedName("pve1"), rulesOf(ours, web))
	drifted := f.SeedTunnel("acct1", pinnedName("pve2"), rulesOf(ours, app))
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	plans := []planner.TunnelPlan{
		planFor("acct1", "cred1", app), pinnedPlanFor("acct1", "pve1", web), pinnedPlanFor("acct1", "pve2", web),
	}

	res := r.Run(ctx, plans, knownAcct1, Observe)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{pinnedAction(PutConfig, "pve2", heldObserve)}, withoutDetail(res.Actions))

	c.t = t0.Add(10 * time.Second)
	n := len(f.Calls())
	res = r.Run(ctx, plans, knownAcct1, Observe)
	require.Equal(t, []Action{pinnedAction(PutConfig, "pve2", heldObserve)}, withoutDetail(res.Actions))
	require.Empty(t, callsAbout(f, n, inLine))
	require.Equal(t, []string{"FindTunnel acct1 pco-abc-pve2", "TunnelConfig acct1 " + drifted.ID}, callsAbout(f, n, drifted))
	require.Equal(t, pinnedState("pve1", inLine, 1), res.Tunnels[1])
	require.False(t, res.Tunnels[2].Verified)
}

// A known account is the caller's whole: its shared tunnel without a shared
// plan and its pinned tunnels without a plan are emptied; tunnels of other
// installs, probes and names that are no node's are never touched.
func TestAKnownAccountEmptiesOnlyTunnelsWithoutAPlan(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	shared := f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	orphan := f.SeedTunnel("acct1", pinnedName("pve1"), rulesOf(ours, web))
	var others []cfapi.Tunnel
	for _, name := range []string{"pco-abcd-pve1", "pco-abc-PVE1", "pco-abc-pve1.x", "pco-abc_probe_x", "pco-abc-_probe_x", "pco-xyz-pve1"} {
		others = append(others, f.SeedTunnel("acct1", name, rulesOf(ours, app)))
	}
	n := len(f.Calls())

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(ctx, []planner.TunnelPlan{pinnedPlanFor("acct1", "pve2", web)}, knownAcct1, Enforce)

	require.Empty(t, res.Problems)
	pinned := findTunnel(t, f, "acct1", pinnedName("pve2"))
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: shared.ID, Version: 2, Exists: true, Verified: true},
		pinnedState("pve1", orphan, 2),
		pinnedState("pve2", pinned, 1),
	}, res.Tunnels)
	require.Equal(t, []Action{
		action(PutConfig, ""), pinnedAction(PutConfig, "pve1", ""),
		pinnedAction(CreateTunnel, "pve2", ""), pinnedAction(PutConfig, "pve2", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, emptyRules(ours), configOf(t, f, shared).Ingress)
	require.Equal(t, emptyRules(ours), configOf(t, f, orphan).Ingress)
	require.Equal(t, rulesOf(ours, web), configOf(t, f, pinned).Ingress)
	for _, tun := range others {
		require.Empty(t, callsAbout(f, n, tun), "%s is not a tunnel of this install pinned to a node", tun.Name)
		require.Equal(t, rulesOf(ours, app), configOf(t, f, tun).Ingress, tun.Name)
	}
	require.Empty(t, callsTo(f, "DeleteTunnel"))
}

// The pinned tunnels of an account are listed once a plan pinned a tunnel
// there, and from then on: a tunnel left without a plan is still found and
// emptied. An account without any pinned plan, or one the caller does not
// know whole, is not listed.
func TestThePinnedTunnelsOfAnAccountAreListedOncePlannedThere(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1", "acct2")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	shared := []planner.TunnelPlan{planFor("acct1", "cred1", app), planFor("acct2", "cred1", app)}
	known := map[string]string{"acct1": "cred1", "acct2": "cred1"}

	res := r.Run(ctx, shared, known, Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, pinnedListings(f), "no listing without a pinned plan")

	c.t = t0.Add(10 * time.Second)
	res = r.Run(ctx, append(shared, pinnedPlanFor("acct2", "pve2", web)), map[string]string{"acct1": "cred1"}, Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, pinnedListings(f), "an account the caller does not know whole is not listed")
	require.Len(t, res.Tunnels, 3)

	c.t = t0.Add(20 * time.Second)
	res = r.Run(ctx, append(shared, pinnedPlanFor("acct2", "pve2", web)), known, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []string{"Tunnels acct2 pco-abc-"}, pinnedListings(f))
	pinned := findTunnel(t, f, "acct2", pinnedName("pve2"))

	c.t = t0.Add(80 * time.Second)
	res = r.Run(ctx, shared, known, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []string{"Tunnels acct2 pco-abc-", "Tunnels acct2 pco-abc-"}, pinnedListings(f), "listed again after a minute, without a pinned plan")
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: findTunnel(t, f, "acct1", testTunnel).ID, Version: 1, Exists: true, Verified: true},
		{AccountID: "acct2", CredentialID: "cred1", Name: testTunnel, ID: findTunnel(t, f, "acct2", testTunnel).ID, Version: 1, Exists: true, Verified: true},
		{
			AccountID: "acct2", CredentialID: "cred1", Name: pinnedName("pve2"), Kind: planner.PlacementPinned, Node: "pve2",
			ID: pinned.ID, Version: 2, Exists: true, Verified: true,
		},
	}, res.Tunnels)
	require.Equal(t, []Action{{
		Kind: PutConfig, Credential: "cred1", AccountID: "acct2", Target: pinnedName("pve2"), Applied: true,
		Detail: "in account acct2: 2 rules replace version 1",
	}}, res.Actions)
	cfg, err := f.TunnelConfig(ctx, "acct2", pinned.ID)
	require.NoError(t, err)
	require.Equal(t, emptyRules(ours), cfg.Ingress)
}

// A listing that fails is a problem, and the planned pinned tunnel is still
// looked up by name; one that Cloudflare's rate limit refuses waits.
func TestAFailedListingOfPinnedTunnels(t *testing.T) {
	ctx := context.Background()
	rateLimited := &cfapi.Error{Status: http.StatusTooManyRequests, Message: "Too many requests", RetryAfter: time.Minute}
	cases := []struct {
		name    string
		err     error
		problem string
		waiting []string
	}{
		{"error", errors.New("boom"), "account acct1: listing its pinned tunnels: boom", nil},
		{"rate limit", rateLimited, "", []string{TunnelRead + "acct1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			pinned := f.SeedTunnel("acct1", pinnedName("pve2"), rulesOf(ours, web))
			s := &spy{API: f, tunnelsErr: map[string]error{"acct1": tc.err}}

			res := newReconciler(Clients{"cred1": s}, &clock{t0}).
				Run(ctx, []planner.TunnelPlan{pinnedPlanFor("acct1", "pve2", web)}, knownAcct1, Observe)

			if tc.problem != "" {
				require.Equal(t, []string{tc.problem}, res.Problems)
			} else {
				require.Empty(t, res.Problems)
			}
			require.Equal(t, tc.waiting, res.Waiting.Reads)
			require.Equal(t, []TunnelState{
				{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel},
				pinnedState("pve2", pinned, 1),
			}, res.Tunnels)
			require.Contains(t, f.Calls(), "FindTunnel acct1 pco-abc-pve2")
		})
	}
}

// A plan for a pinned tunnel is written only to the tunnel of this install
// pinned to the node it names.
func TestAPinnedPlanMustNameTheTunnelOfItsNode(t *testing.T) {
	cases := []struct {
		name  string
		plan  planner.TunnelPlan
		fault string
	}{
		{"another install", planner.TunnelPlan{Name: "pco-xyz-pve2", Kind: planner.PlacementPinned, Node: "pve2"},
			"pco-xyz-pve2 in account acct1: the plan is not for the tunnel of this install pinned to node pve2, pco-abc-pve2"},
		{"another node", planner.TunnelPlan{Name: pinnedName("pve3"), Kind: planner.PlacementPinned, Node: "pve2"},
			"pco-abc-pve3 in account acct1: the plan is not for the tunnel of this install pinned to node pve2, pco-abc-pve2"},
		{"pinned name without the kind", planner.TunnelPlan{Name: pinnedName("pve2")},
			"pco-abc-pve2 in account acct1: the plan is not for the tunnel of this install, pco-abc"},
		{"a node that names no tunnel", planner.TunnelPlan{Name: "pco-abc-PVE2", Kind: planner.PlacementPinned, Node: "PVE2"},
			`pco-abc-PVE2 in account acct1: the plan is not for a tunnel this install can have: node name "PVE2": want lower-case letters, digits and hyphens, not at the ends`},
		{"a kind that is not known", planner.TunnelPlan{Name: pinnedName("pve2"), Kind: "floating", Node: "pve2"},
			`pco-abc-pve2 in account acct1: the plan is not for a tunnel this install can have: placement kind "floating" is not known`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake("acct1")
			plan := tc.plan
			plan.AccountID, plan.CredentialID, plan.Rules = "acct1", "cred1", rulesOf(ours, web)

			res := newReconciler(Clients{"cred1": f}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{plan}, nil, Enforce)

			require.Equal(t, []string{tc.fault}, res.Problems)
			require.Equal(t, WriterProceed, res.Verdict)
			require.Empty(t, f.TunnelsIn("acct1"))
			require.Len(t, res.Tunnels, 1)
			require.True(t, res.Tunnels[0].Unknown)
		})
	}
}
