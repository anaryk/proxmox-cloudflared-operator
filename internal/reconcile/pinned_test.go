package reconcile

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// Every pinned tunnel, and the listing of every account, has a slot of its
// own within the minute, so the reads of many spread instead of coming in
// one burst.
func TestSlotsSpreadOverTheMinute(t *testing.T) {
	keys := []string{"acct1", "acct2", "acct1/pco-abc-pve1", "acct1/pco-abc-pve2", "acct1/pco-abc-pve3"}
	phases := map[time.Duration]bool{}
	for _, key := range keys {
		s := slot(key, t0)
		require.False(t, s.After(t0), key)
		require.Less(t, t0.Sub(s), driftInterval, key)
		require.Equal(t, s, slot(key, s), "a slot is its own slot")
		require.Equal(t, s, slot(key, s.Add(driftInterval-time.Nanosecond)))
		require.Equal(t, s.Add(driftInterval), slot(key, t0.Add(driftInterval)))
		phases[t0.Sub(s)] = true

		require.False(t, due(key, s, s))
		require.False(t, due(key, s.Add(time.Second), s.Add(driftInterval-time.Second)))
		require.True(t, due(key, s.Add(time.Second), s.Add(driftInterval)))
		require.True(t, due(key, s, s.Add(driftInterval)))
		require.True(t, due(key, s, s.Add(-time.Second)), "a clock that stepped back")
		require.True(t, due(key, time.Time{}, s), "never read")
	}
	require.Greater(t, len(phases), 1, "the keys do not all share one slot")
}

// runLog is what one run of a series saw and found.
type runLog struct {
	at    time.Time
	calls []string
	res   TunnelResult
}

// runEvery10s runs the reconciler every ten seconds, from ten seconds after
// start up to span after it.
func runEvery10s(ctx context.Context, r *TunnelReconciler, f *cffake.Fake, c *clock, plans []planner.TunnelPlan, known map[string]string, start time.Time, span time.Duration) []runLog {
	var out []runLog
	for d := 10 * time.Second; d <= span; d += 10 * time.Second {
		c.t = start.Add(d)
		n := len(f.Calls())
		res := r.Run(ctx, plans, known, Enforce)
		out = append(out, runLog{at: c.t, calls: slices.Clone(f.Calls()[n:]), res: res})
	}
	return out
}

// runsWith returns the times of the runs that made the call.
func runsWith(log []runLog, call string) []time.Time {
	var out []time.Time
	for _, run := range log {
		if slices.Contains(run.calls, call) {
			out = append(out, run.at)
		}
	}
	return out
}

// dueRuns returns, among runs every ten seconds after start, the ones at
// which a read of key last done at start is due: the first run at or after
// each slot of key.
func dueRuns(key string, start time.Time, span time.Duration) []time.Time {
	var out []time.Time
	next := slot(key, start).Add(driftInterval)
	for d := 10 * time.Second; d <= span; d += 10 * time.Second {
		if at := start.Add(d); !at.Before(next) {
			out = append(out, at)
			next = next.Add(driftInterval)
		}
	}
	return out
}

// nodeWithSlotAfter returns a node whose tunnel in acct1, read at t0, is next
// due in a run later than after, among runs every ten seconds.
func nodeWithSlotAfter(t *testing.T, after time.Time) string {
	t.Helper()
	for _, node := range []string{"pve2", "pve3", "pve4", "pve5", "pve6", "pve7", "pve8", "pve9"} {
		if dueRuns("acct1/"+pinnedName(node), t0, 2*time.Minute)[0].After(after) {
			return node
		}
	}
	t.Fatalf("no node's tunnel is due after %s", after)
	return ""
}

// withoutLookups leaves out the lookups by name, which a run makes for a
// pinned tunnel only when the listing of the account was not made in it.
func withoutLookups(calls []string) []string {
	return slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return strings.HasPrefix(c, "FindTunnel ") })
}

// The shared tunnel is read every run. A pinned tunnel whose plan is as it
// was last verified is read once a minute, at its own slot, and so is the
// listing of the account; between the reads the run reports what it last
// saw, and an edit at Cloudflare is written back at the next read.
func TestPinnedTunnelsAreReadAtTheirOwnSlotOnceAMinute(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	// A second node whose slot falls in another run than the first's.
	second := nodeWithSlotAfter(t, dueRuns("acct1/"+pinnedName("pve1"), t0, 2*time.Minute)[0])
	plans := []planner.TunnelPlan{planFor("acct1", "cred1", app), pinnedPlanFor("acct1", "pve1", web), pinnedPlanFor("acct1", second, web)}
	res := r.Run(ctx, plans, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	shared, one, two := findTunnel(t, f, "acct1", testTunnel), findTunnel(t, f, "acct1", pinnedName("pve1")), findTunnel(t, f, "acct1", pinnedName(second))
	c.t = t0.Add(5 * time.Second)
	_, err := f.PutTunnelConfig(ctx, "acct1", two.ID, rulesOf(ours, app))
	require.NoError(t, err)

	log := runEvery10s(ctx, r, f, c, plans, knownAcct1, t0, 5*time.Minute)

	readOne, readTwo := runsWith(log, "TunnelConfig acct1 "+one.ID), runsWith(log, "TunnelConfig acct1 "+two.ID)
	require.Equal(t, dueRuns("acct1", t0, 5*time.Minute), runsWith(log, listingOfAcct1))
	require.Equal(t, dueRuns("acct1/"+one.Name, t0, 5*time.Minute), readOne)
	require.Equal(t, dueRuns("acct1/"+two.Name, t0, 5*time.Minute), readTwo)
	require.Len(t, readTwo, 5)
	require.NotEqual(t, readOne[0], readTwo[0], "the two tunnels are read in different runs")
	for _, run := range log {
		require.Empty(t, run.res.Problems, "at %s", run.at)
		require.Contains(t, run.calls, "TunnelConfig acct1 "+shared.ID, "at %s: the shared tunnel is read every run", run.at)
		require.Equal(t, pinnedState("pve1", one, 1), run.res.Tunnels[1], "at %s", run.at)
		switch {
		case run.at.Before(readTwo[0]):
			require.Empty(t, run.res.Actions, "at %s", run.at)
			require.Equal(t, pinnedState(second, two, 1), run.res.Tunnels[2], "at %s: what was last seen", run.at)
		case run.at.Equal(readTwo[0]):
			require.Equal(t, []Action{pinnedAction(PutConfig, second, "")}, withoutDetail(run.res.Actions), "the edit is written back")
			require.Equal(t, pinnedState(second, two, 3), run.res.Tunnels[2])
		default:
			require.Empty(t, run.res.Actions, "at %s", run.at)
			require.Equal(t, pinnedState(second, two, 3), run.res.Tunnels[2], "at %s", run.at)
		}
	}
	require.Equal(t, rulesOf(ours, web), configOf(t, f, two).Ingress)
}

// A change of the plan reads the tunnel at once, whatever its slot, and so
// does a clock that stepped back.
func TestAPinnedTunnelIsReadAtOnceWhenItsPlanChanges(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	plans := []planner.TunnelPlan{planFor("acct1", "cred1", app), pinnedPlanFor("acct1", "pve2", web)}
	res := r.Run(ctx, plans, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	pinned := findTunnel(t, f, "acct1", pinnedName("pve2"))

	c.t = t0.Add(20 * time.Second)
	n := len(f.Calls())
	changed := []planner.TunnelPlan{plans[0], pinnedPlanFor("acct1", "pve2", web, app)}
	res = r.Run(ctx, changed, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	require.Equal(t, []Action{pinnedAction(PutConfig, "pve2", "")}, withoutDetail(res.Actions))
	require.Equal(t, []string{"TunnelConfig acct1 " + pinned.ID, "PutTunnelConfig acct1 " + pinned.ID, "TunnelConfig acct1 " + pinned.ID},
		withoutLookups(callsAbout(f, n, pinned)))
	require.Equal(t, rulesOf(ours, web, app), configOf(t, f, pinned).Ingress)

	// Back to before the listing and the reads were made.
	c.t = t0.Add(-10 * time.Second)
	n = len(f.Calls())
	res = r.Run(ctx, changed, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	require.Empty(t, res.Actions)
	require.Contains(t, f.Calls()[n:], listingOfAcct1)
	require.Equal(t, []string{"TunnelConfig acct1 " + pinned.ID}, withoutLookups(callsAbout(f, n, pinned)))
}

// A pinned tunnel gone at Cloudflare is forgotten as soon as the listing of
// its account lacks it, so it is made anew then rather than reported as last
// seen until its own slot.
func TestAPinnedTunnelGoneFromTheListingIsForgotten(t *testing.T) {
	ctx := context.Background()
	// A node whose tunnel's slot comes after the listing's, so that the
	// listing is what finds the tunnel gone.
	listingDue := dueRuns("acct1", t0, 2*time.Minute)[0]
	node := nodeWithSlotAfter(t, listingDue)
	f := newFake("acct1")
	c := &clock{t0}
	r := newReconciler(Clients{"cred1": f}, c)
	plans := []planner.TunnelPlan{pinnedPlanFor("acct1", node, web)}
	res := r.Run(ctx, plans, knownAcct1, Enforce)
	require.Empty(t, res.Problems)
	gone := findTunnel(t, f, "acct1", pinnedName(node))
	c.t = t0.Add(5 * time.Second)
	require.NoError(t, f.DeleteTunnel(ctx, "acct1", gone.ID))

	log := runEvery10s(ctx, r, f, c, plans, knownAcct1, t0, listingDue.Sub(t0))

	require.Equal(t, []time.Time{listingDue}, runsWith(log, "CreateTunnel acct1 "+pinnedName(node)))
	for _, run := range log[:len(log)-1] {
		require.Equal(t, pinnedState(node, gone, 1), run.res.Tunnels[1], "at %s: as last seen", run.at)
	}
	last := log[len(log)-1]
	require.Empty(t, last.res.Problems)
	require.Equal(t, []Action{pinnedAction(CreateTunnel, node, ""), pinnedAction(PutConfig, node, "")}, withoutDetail(last.res.Actions))
	require.NotEqual(t, gone.ID, last.res.Tunnels[1].ID)
	require.True(t, last.res.Tunnels[1].Verified)
}

// A tunnel without a plan is emptied only when this install wrote what it
// holds; one that holds nothing is left as it is, and so, with a problem, is
// one that holds rules of another writer.
func TestATunnelWithoutAPlanIsEmptiedOnlyWhenThisInstallWroteIt(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	shared := f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	handmade := f.SeedTunnel("acct1", pinnedName("lab"), []planner.IngressRule{app, catchAll})
	bare := f.SeedTunnel("acct1", pinnedName("none"), nil)
	other := planner.Writer{InstallID: "xyz", Generation: 3, Nonce: "n3"}
	others := f.SeedTunnel("acct1", pinnedName("old"), rulesOf(other, app))

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).
		Run(ctx, []planner.TunnelPlan{pinnedPlanFor("acct1", "pve2", web)}, knownAcct1, Enforce)

	require.Equal(t, []string{
		"pco-abc-lab in account acct1: the configuration holds no sentinel of this install; left as it is",
		"pco-abc-old in account acct1: the configuration holds no sentinel of this install; left as it is",
	}, res.Problems)
	pinned := findTunnel(t, f, "acct1", pinnedName("pve2"))
	require.Equal(t, []Action{
		action(PutConfig, ""),
		pinnedAction(PutConfig, "lab", "the configuration holds no sentinel of this install"),
		pinnedAction(PutConfig, "old", "the configuration holds no sentinel of this install"),
		pinnedAction(CreateTunnel, "pve2", ""), pinnedAction(PutConfig, "pve2", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: shared.ID, Version: 2, Exists: true, Verified: true},
		{AccountID: "acct1", CredentialID: "cred1", Name: pinnedName("lab"), Kind: planner.PlacementPinned, Node: "lab", ID: handmade.ID, Version: 1, Exists: true},
		pinnedState("none", bare, 0),
		{AccountID: "acct1", CredentialID: "cred1", Name: pinnedName("old"), Kind: planner.PlacementPinned, Node: "old", ID: others.ID, Version: 1, Exists: true},
		pinnedState("pve2", pinned, 1),
	}, res.Tunnels)
	require.Equal(t, emptyRules(ours), configOf(t, f, shared).Ingress)
	require.Equal(t, []planner.IngressRule{app, catchAll}, configOf(t, f, handmade).Ingress)
	require.Empty(t, configOf(t, f, bare).Ingress)
	require.Equal(t, rulesOf(other, app), configOf(t, f, others).Ingress)
}

// A plan for a pinned tunnel is written over a configuration pco wrote, or
// over none; rules without any sentinel are somebody else's and stay. The
// shared tunnel is taken over whatever it holds, as a lost store is
// recovered through it.
func TestAPinnedPlanDoesNotWriteOverAConfigurationWithoutASentinel(t *testing.T) {
	ctx := context.Background()
	f := newFake("acct1")
	shared := f.SeedTunnel("acct1", testTunnel, []planner.IngressRule{app, catchAll})
	taken := f.SeedTunnel("acct1", pinnedName("pve2"), []planner.IngressRule{app, catchAll})
	fresh := f.SeedTunnel("acct1", pinnedName("pve3"), nil)
	plans := []planner.TunnelPlan{planFor("acct1", "cred1", web), pinnedPlanFor("acct1", "pve2", web), pinnedPlanFor("acct1", "pve3", web)}

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).Run(ctx, plans, knownAcct1, Enforce)

	require.Equal(t, []string{
		"pco-abc-pve2 in account acct1: the configuration holds no sentinel, so pco did not write it; left as it is",
	}, res.Problems)
	require.Equal(t, []Action{
		action(PutConfig, ""),
		pinnedAction(PutConfig, "pve2", "the configuration holds no sentinel, so pco did not write it"),
		pinnedAction(PutConfig, "pve3", ""),
	}, withoutDetail(res.Actions))
	require.Equal(t, rulesOf(ours, web), configOf(t, f, shared).Ingress)
	require.Equal(t, []planner.IngressRule{app, catchAll}, configOf(t, f, taken).Ingress)
	require.Equal(t, rulesOf(ours, web), configOf(t, f, fresh).Ingress)
	require.False(t, res.Tunnels[1].Verified)
	require.True(t, res.Tunnels[2].Verified)
}

// A pinned tunnel whose configuration another install wrote is left as it
// is: only a sentinel of this install lets a pinned plan write over rules.
func TestAPinnedPlanDoesNotWriteOverAnotherInstallsConfiguration(t *testing.T) {
	f := newFake("acct1")
	seeded := []planner.IngressRule{app, {Hostname: "g9.n9.pco-xyz.invalid", Service: "http_status:404"}, catchAll}
	taken := f.SeedTunnel("acct1", pinnedName("pve2"), seeded)
	plans := []planner.TunnelPlan{planFor("acct1", "cred1", web), pinnedPlanFor("acct1", "pve2", web)}

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).Run(context.Background(), plans, knownAcct1, Enforce)

	require.Contains(t, res.Problems, "pco-abc-pve2 in account acct1: the configuration holds no sentinel of this install; left as it is")
	require.Equal(t, seeded, configOf(t, f, taken).Ingress)
	require.False(t, res.Tunnels[1].Verified)
}

// A plan may say shared for the shared tunnel; the state says it with an
// empty kind, as for a plan that says nothing.
func TestAPlanMayCallTheSharedTunnelShared(t *testing.T) {
	f := newFake("acct1")
	plan := planFor("acct1", "cred1", app)
	plan.Kind = planner.PlacementShared

	res := newReconciler(Clients{"cred1": f}, &clock{t0}).Run(context.Background(), []planner.TunnelPlan{plan}, nil, Enforce)

	require.Empty(t, res.Problems)
	require.Equal(t, []TunnelState{
		{AccountID: "acct1", CredentialID: "cred1", Name: testTunnel, ID: tunnelIn(t, f, "acct1").ID, Version: 1, Exists: true, Verified: true},
	}, res.Tunnels)
}
