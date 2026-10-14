package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// servedAt says whether the tunnel of the account sends host to a target, not
// to the 503 block.
func servedAt(t *testing.T, e *env, host string) bool {
	t.Helper()
	for _, r := range e.rules() {
		if r.Hostname == host {
			return r.Service != planner.BlockedService
		}
	}
	return false
}

// A new controller that waited out the takeover hold for a node whose agent
// never reported writes add-only in its first cycle: a route its own view
// would withdraw keeps its rule, and the line says why. The cycle after it
// goes by what it sees.
func TestTheFirstCycleAfterAHoldWithASilentNodeWithdrawsNothing(t *testing.T) {
	one := 1
	e, r, _ := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "app-1", "app.example.com -> :8080")))
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	require.True(t, servedAt(t, e, "app.example.com"))

	// A controller takes over; pve2's agent does not report to it, and the
	// guest of app.example.com fails its identity check.
	r.forget()
	e.res.stop("app.example.com", "the MAC of the address is not the guest's")
	e.eng = e.newEngine()
	e.clock.advance(time.Second)
	st := e.cycle()
	require.Contains(t, st.Hold, "waiting for the agents of pve2")

	e.clock.advance(takeoverHold + time.Second)
	writes := len(e.writes())
	st = e.cycle()

	require.True(t, hasProblem(st, "the agents of pve2 did not report within the takeover hold; nothing is withdrawn at Cloudflare in this cycle"), st.Problems)
	require.True(t, servedAt(t, e, "app.example.com"), "kept in the first cycle")
	require.True(t, servedAt(t, e, "www.example.com"))
	for _, w := range e.writes()[writes:] {
		require.NotContains(t, w, "DeleteRecord")
	}

	e.clock.advance(10 * time.Second)
	st = e.cycle()

	require.False(t, hasProblem(st, "nothing is withdrawn at Cloudflare in this cycle"), st.Problems)
	require.False(t, servedAt(t, e, "app.example.com"), "the next cycle withdraws it")
	require.True(t, servedAt(t, e, "www.example.com"))
}

// The cycle that withdraws nothing keeps only what pco served: a rule
// someone put into the tunnel at Cloudflare for a hostname the install holds
// no claim on is not written again under this controller's sentinel, and its
// target reaches no verified configuration the agents keep in their sets.
func TestTheAddOnlyCycleKeepsNoRuleSomeoneElsePutIn(t *testing.T) {
	one := 1
	e, r, _ := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	stranger := planner.IngressRule{Hostname: "evil.example.com", Service: "tcp://10.9.9.9:22"}
	ts := e.tunnels()
	require.Len(t, ts, 1)
	_, err := e.cf.PutTunnelConfig(t.Context(), testAccount, ts[0].ID, append([]planner.IngressRule{stranger}, e.rules()...))
	require.NoError(t, err)

	r.forget()
	e.eng = e.newEngine()
	e.clock.advance(time.Second)
	e.cycle()
	e.clock.advance(takeoverHold + time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "nothing is withdrawn at Cloudflare in this cycle"), st.Problems)
	require.NotContains(t, e.rules(), stranger)
	for _, ts := range e.eng.verified {
		require.NotContains(t, ts, egressTargets("10.9.9.9:22")[0])
	}
}

// A takeover whose agents all report in time writes as it sees from its
// first cycle: there is nothing it did not hear.
func TestAHoldThatEveryAgentEndedLeavesNoAddOnlyCycle(t *testing.T) {
	one := 1
	e, r, _ := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "app-1", "app.example.com -> :8080")))
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()

	e.res.stop("app.example.com", "the MAC of the address is not the guest's")
	e.eng = e.newEngine()
	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	st := e.cycle()

	require.Empty(t, st.Hold)
	require.False(t, hasProblem(st, "nothing is withdrawn at Cloudflare in this cycle"), st.Problems)
	require.False(t, servedAt(t, e, "app.example.com"))
}
