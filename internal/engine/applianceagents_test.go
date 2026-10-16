package engine

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ahead has node's agent report an epoch above the controller's.
func ahead(e *env, r *fakeRemote, node string) agentproto.Epoch {
	r.report(node, e.clock.now(), t0.Add(-time.Hour))
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := r.reports[node]
	rep.Status.EpochSeen = agentproto.Epoch{InstallID: testInstall, Generation: testWriter.Generation + 1, Nonce: "f00dcafe"}
	r.reports[node] = rep
	return rep.Status.EpochSeen
}

func applianceNode(name string, index int) store.NodeEntry {
	return store.NodeEntry{Name: name, Since: t0, State: store.NodeActive, StateSince: t0, Index: &index, Kind: "appliance", VMID: 9300 + index}
}

// An appliance controller whose volume came back older than what its agents
// follow, as a relocation on replicated storage brings it, writes nothing.
func TestAControllerBelowItsAgentsEpochOnlyObserves(t *testing.T) {
	e, r, _ := clusterOf(t, applianceNode("pve2", 1), applianceNode("pve3", 2))
	e.enforce()
	ep := ahead(e, r, "pve2")
	ahead(e, r, "pve3")

	st := e.cycle()

	require.Equal(t, ModeObserve, st.Mode)
	require.Contains(t, problemsText(st), "the agent of node pve2 follows epoch "+ep.String())
	require.Contains(t, problemsText(st), "pco appliance recover")
	require.Empty(t, r.last(), "no agent is served a snapshot")
}

// One agent, broken or taken over, that reports an epoch above the
// controller's holds nobody: it is no majority of the agents that vote, and
// an agent that is silent, or not admitted, does not vote at all.
func TestOneHostileAgentHoldsNoController(t *testing.T) {
	for _, tt := range []struct {
		name  string
		nodes []store.NodeEntry
		setup func(e *env, r *fakeRemote)
	}{
		{"one of two", []store.NodeEntry{applianceNode("pve2", 1)}, func(e *env, r *fakeRemote) { ahead(e, r, "pve2") }},
		{"one of three", []store.NodeEntry{applianceNode("pve2", 1), applianceNode("pve3", 2)}, func(e *env, r *fakeRemote) {
			ahead(e, r, "pve2")
			r.report("pve3", e.clock.now(), t0.Add(-time.Hour))
		}},
		{"two, one of them silent", []store.NodeEntry{applianceNode("pve2", 1), applianceNode("pve3", 2)}, func(e *env, r *fakeRemote) {
			ahead(e, r, "pve2")
			ahead(e, r, "pve3")
			r.mu.Lock()
			rep := r.reports["pve3"]
			rep.Fresh, rep.ReceivedAt = false, e.clock.now().Add(-time.Hour)
			r.reports["pve3"] = rep
			r.mu.Unlock()
		}},
		{"two, one of them joining", []store.NodeEntry{applianceNode("pve2", 1), func() store.NodeEntry {
			n := applianceNode("pve3", 2)
			n.State, n.Index = store.NodeJoining, nil
			return n
		}()}, func(e *env, r *fakeRemote) {
			ahead(e, r, "pve2")
			ahead(e, r, "pve3")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, r, _ := clusterOf(t, tt.nodes...)
			e.enforce()
			tt.setup(e, r)

			st := e.cycle()

			require.Equal(t, ModeEnforce, st.Mode)
			require.NotContains(t, problemsText(st), "follows epoch")
		})
	}
}

// An epoch no draw of this install makes is no vote above the controller,
// whatever every agent reports.
func TestACraftedEpochHoldsNoController(t *testing.T) {
	for _, tt := range []struct {
		name string
		ep   agentproto.Epoch
	}{
		{"another install", agentproto.Epoch{InstallID: "ba9876543210", Generation: testWriter.Generation + 1, Nonce: "f00dcafe"}},
		{"no nonce", agentproto.Epoch{InstallID: testInstall, Generation: testWriter.Generation + 1}},
		{"a nonce no draw makes", agentproto.Epoch{InstallID: testInstall, Generation: testWriter.Generation + 1, Nonce: "not a nonce/../x"}},
		{"a generation above 2^53", agentproto.Epoch{InstallID: testInstall, Generation: 1<<53 + 1, Nonce: "f00dcafe"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, r, _ := clusterOf(t, applianceNode("pve2", 1), applianceNode("pve3", 2))
			e.enforce()
			for _, node := range []string{"pve2", "pve3"} {
				r.report(node, e.clock.now(), t0.Add(-time.Hour))
				r.mu.Lock()
				rep := r.reports[node]
				rep.Status.EpochSeen = tt.ep
				r.reports[node] = rep
				r.mu.Unlock()
			}

			st := e.cycle()

			require.Equal(t, ModeEnforce, st.Mode)
			require.NotContains(t, problemsText(st), "follows epoch")
		})
	}
}

// An agent whose container runs on another node than it was enrolled for is
// served nothing, and the admin is told.
func TestAMovedAgentIsServedNothing(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one, Kind: "appliance", VMID: 9301})
	e.enforce()
	agentCT := model.Guest{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9301}, Name: "pco", Node: "pve3", Running: true, Pool: "pco"}
	addGuest(e, agentCT)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	st := e.cycle()

	require.Contains(t, problemsText(st), "agent 9301 runs on pve3, enrolled for pve2; move it back or enrol it again")
	require.NotContains(t, r.last(), "pve2")

	agentCT.Node = "pve2"
	addGuest(e, agentCT)
	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	require.NotContains(t, problemsText(e.cycle()), "runs on pve3")
	require.Contains(t, r.last(), "pve2", "moved back, it is served again")
}

// What makes the controller serve an appliance agent nothing is judged for
// that node alone, every cycle, from Proxmox and the agent's own report: the
// other agents are served all the same, and a new controller judges it the
// same way, with nothing remembered.
func TestOnlyTheAgentThatFailsIsHeldAndARestartDoesNotForgetIt(t *testing.T) {
	ct := func(vmid int, node, lock string) model.Guest {
		return model.Guest{Ref: model.GuestRef{Kind: model.KindLXC, VMID: vmid}, Name: "pco", Node: node, Running: true, Pool: "pco", Lock: lock}
	}
	for _, tt := range []struct {
		name   string
		guests []model.Guest
		stale  bool
		want   string
	}{
		{"moved", []model.Guest{ct(9301, "pve4", "")}, false, "agent 9301 runs on pve4, enrolled for pve2"},
		{"being migrated", []model.Guest{ct(9301, "pve4", "migrate")}, false, "agent 9301 of node pve2 is being migrated to pve4"},
		{"gone from the cluster", nil, false, "agent 9301 of node pve2 is not among the cluster's resources"},
		{"silent, with a report of an epoch above", []model.Guest{ct(9301, "pve2", "")}, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, r, _ := clusterOf(t, applianceNode("pve2", 1), applianceNode("pve3", 2))
			e.enforce()
			for _, g := range append(tt.guests, ct(9302, "pve3", "")) {
				addGuest(e, g)
			}
			ahead(e, r, "pve2")
			r.report("pve3", e.clock.now(), t0.Add(-time.Hour))
			if tt.stale {
				e.clock.advance(time.Hour)
				r.report("pve3", e.clock.now(), t0.Add(-time.Hour))
			}

			for range 2 {
				st := e.cycle()

				require.Equal(t, ModeEnforce, st.Mode, "one agent holds no controller")
				if tt.want != "" {
					require.Contains(t, problemsText(st), tt.want)
				}
				require.NotContains(t, r.last(), "pve2", "the agent that fails is served nothing")
				require.Contains(t, r.last(), "pve3", "the others are served")
				e.eng = e.newEngine()
				e.clock.advance(time.Second)
				r.report("pve3", e.clock.now(), t0.Add(-time.Hour))
			}
		})
	}
}

func problemsText(st State) string { return strings.Join(st.Problems, "\n") }

// addGuest puts g into the inventory's snapshot, in place of the guest it
// was before.
func addGuest(e *env, g model.Guest) {
	e.inv.mu.Lock()
	defer e.inv.mu.Unlock()
	s := e.inv.snap
	s.Guests = append(slices.DeleteFunc(slices.Clone(s.Guests), func(x model.Guest) bool { return x.Ref == g.Ref }), g)
	e.inv.snap = s
}

// The agents of an appliance install watch no forwarding tables: the routes
// of every node's guests stay pinned to it, which is how such an install
// serves, and is not said as a loss of coverage.
func TestAnApplianceInstallPinsWithoutALossOfCoverage(t *testing.T) {
	e, r, _ := clusterOf(t, applianceNode("pve2", 1), applianceNode("pve3", 2))
	require.NoError(t, e.store.SaveInstall(applianceInstall()))
	e.enforce()
	for range 3 {
		r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
		r.report("pve3", e.clock.now(), t0.Add(-time.Hour))
		e.clock.advance(coverageHold)
		e.cycle()
	}
	st := e.cycle()

	require.NotContains(t, problemsText(st), "running watch")
	require.NotNil(t, st.Cluster)
	require.True(t, st.Cluster.Coverage.Pinned)
	require.False(t, st.Cluster.Coverage.Covered, "the routes stay pinned")
}
