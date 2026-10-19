package engine

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
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

// The appliance reads the quorum from the API, not from a members file: its
// cluster view says what that read found, and lists the nodes of the
// inventory as the members, even in a cycle that holds later on.
func TestTheClusterViewOfAnApplianceHasTheQuorumOfTheAPI(t *testing.T) {
	e, r, listed := clusterOf(t, applianceNode("pve2", 1))
	*listed = nil
	require.NoError(t, e.store.SaveInstall(applianceInstall()))
	e.quorate = func(context.Context) (bool, error) { return true, nil }
	e.inv.set(inventory.Snapshot{
		Nodes:    []inventory.Node{{Name: testNode, Addr: nodeAddr, Online: true, Local: true}, {Name: "pve2", Online: true}},
		Complete: true,
	})
	e.eng = e.newEngine()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	st := e.cycle()

	require.NotNil(t, st.Cluster)
	require.True(t, st.Cluster.Quorate)
	require.Equal(t, []string{"pve1", "pve2"}, st.Cluster.Members)
}

// The init of an appliance registers the controller's own node without a
// state, and no first start numbers it as on a host: the registry does, so
// that the node is a replica of the shared tunnel, whose connector it runs.
// Without that, a route of another node's guest stayed in the shared tunnel
// as reached by its only replica, while the controller's connector, which
// has no egress target for it, took a share of its requests.
func TestTheOwnNodeOfAnApplianceControllerIsNumberedAndActive(t *testing.T) {
	e, r, _ := clusterOf(t, applianceNode("pve2", 1))
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0}))
	require.NoError(t, e.store.SaveInstall(applianceInstall()))
	e.enforce()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	st := e.cycle()

	n := entryOf(t, e, testNode)
	require.Equal(t, store.NodeActive, n.State)
	require.NotNil(t, n.Index)
	require.Equal(t, 0, *n.Index)
	i := slices.IndexFunc(st.Nodes, func(v NodeView) bool { return v.Name == testNode })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, store.NodeActive, st.Nodes[i].State)
}

// A host registers its own node the same way, and its first start numbers
// it before any cycle writes: the registry leaves such an entry alone.
func TestTheOwnNodeOfAHostIsLeftToItsFirstStart(t *testing.T) {
	e, r, _ := clusterOf(t, applianceNode("pve2", 1))
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0}))
	e.enforce()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	e.cycle()

	require.Empty(t, entryOf(t, e, testNode).State)
}

// applianceTwoNodes is an appliance install of two nodes: the controller on
// pve1, registered by its init without a state, and an agent container on
// pve2, with the guest db on pve2.
func applianceTwoNodes(t *testing.T) *world {
	t.Helper()
	e, r, listed := clusterOf(t, applianceNode("pve2", 1))
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0}))
	require.NoError(t, e.store.SaveInstall(applianceInstall()))
	*listed = nil
	e.quorate = func(context.Context) (bool, error) { return true, nil }
	l := holding(testWriter)
	e.lease = l
	e.res.addrs["db.example.com"] = dbAddr
	e.inv.set(inventory.Snapshot{
		Guests: []model.Guest{
			carded(guest(102, "db", "db.example.com -> :8080"), "pve2", dbCard),
			{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9200}, Name: "pco", Node: testNode, Running: true, Pool: "pco"},
			{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9301}, Name: "pco", Node: "pve2", Running: true, Pool: "pco"},
		},
		Nodes: []inventory.Node{
			{Name: testNode, Addr: nodeAddr, Online: true, Local: true},
			{Name: "pve2", Addr: netip.MustParseAddr("10.0.0.3"), Online: true},
		},
		Complete: true,
	})
	e.eng = e.newEngine()
	e.enforce()
	return &world{env: e, r: r, lease: l, listed: listed, acts: map[string]*agentAct{
		"pve2": {kind: pki.KindAppliance, started: t0.Add(-time.Hour)},
	}}
}

// In an appliance install the route of a guest is served through the
// tunnel pinned to the guest's node, whose connector the node's agent runs
// with a token of its own: the controller's connector of the shared tunnel
// has no egress target for a guest of another node, and must not take a
// share of its requests. The agent of an appliance was handed no pinned
// tunnel, so a route pinned there pointed at a tunnel nobody ran.
func TestAnApplianceAgentRunsTheTunnelPinnedToItsNode(t *testing.T) {
	w := applianceTwoNodes(t)
	pinned2, err := planner.PinnedTunnelName(testInstall, "pve2")
	require.NoError(t, err)

	var st State
	for range 40 {
		st = w.tick(5 * time.Second)
	}

	require.Equal(t, store.NodeActive, entryOf(t, w.env, testNode).State)
	rt := route(st, "db.example.com")
	require.NotNil(t, rt.Placement)
	require.Equal(t, TunnelPinned, rt.Placement.Kind)
	require.Equal(t, "pve2", rt.Placement.Node)
	require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))
	s, ok := w.r.lastOf("pve2")
	require.True(t, ok)
	i := slices.IndexFunc(s.Tunnels, func(tun agentproto.Tunnel) bool { return tun.Name == pinned2 })
	require.GreaterOrEqual(t, i, 0, "the agent of pve2 is handed its pinned tunnel: %v", s.Tunnels)
	require.True(t, s.Tunnels[i].Run)
	require.NotEmpty(t, s.Tunnels[i].Token.Reveal())
	require.Equal(t, planner.StateActive, rt.State)
	j := slices.IndexFunc(st.Nodes, func(v NodeView) bool { return v.Name == "pve2" })
	require.GreaterOrEqual(t, j, 0)
	require.Equal(t, []string{s.Tunnels[i].TunnelID}, st.Nodes[j].PinnedReady, "the node view names the pinned tunnel whose connector pve2's agent reports ready")
	k := slices.IndexFunc(st.Nodes, func(v NodeView) bool { return v.Name == testNode })
	require.Empty(t, st.Nodes[k].PinnedReady, "pve1 has no pinned tunnel")
}

// An appliance cluster upgraded from a release that left the route of
// another node's guest in the shared tunnel, served by that node's shared
// connector alone, moves it to the tunnel pinned to the guest's node by
// the steps of a move: the pinned tunnel gets its rule and its connector
// runs on the node first, the record points at the shared tunnel until
// then, and the shared tunnel keeps its rule at the target's address until
// the old rules' time after the switch passed. Nothing is withdrawn.
func TestARouteOfAnApplianceAgentsGuestMovesFromTheSharedTunnelToItsNode(t *testing.T) {
	w := applianceTwoNodes(t)
	// As before the fix: the controller's node is no replica, and the
	// route is shared, reached by pve2 alone.
	w.settings(func(s *store.Settings) { s.Cluster.Replicas = []string{"pve2"} })
	for range 6 {
		w.tick(5 * time.Second)
	}
	require.Equal(t, tunnelName, w.recordTunnel(t, "db.example.com"))
	require.Equal(t, []string{tunnelName}, w.servedIn(t, "db.example.com"))
	require.Equal(t, "http://"+dbAddr.String()+":8080", w.sharedRule(t, "db.example.com").Service)
	pinned2, err := planner.PinnedTunnelName(testInstall, "pve2")
	require.NoError(t, err)

	w.settings(func(s *store.Settings) { s.Cluster.Replicas = nil })
	var steps []string
	ran := false
	for range 120 {
		w.tick(5 * time.Second)
		w.requireNothingWithdrawn(t, "db.example.com")
		if s, ok := w.r.lastOf("pve2"); ok && slices.ContainsFunc(s.Tunnels, func(tun agentproto.Tunnel) bool { return tun.Name == pinned2 && tun.Run }) {
			ran = true
		}
		m, moving := w.moveOf(t, "qemu/102")
		if moving && (len(steps) == 0 || steps[len(steps)-1] != m.Step) {
			steps = append(steps, m.Step)
		}
		rec := w.recordTunnel(t, "db.example.com")
		switch {
		case moving && m.Step != store.MoveCNAMEsSwitched:
			require.Equal(t, tunnelName, rec, "the record stays until the pinned connector rolled out")
		case rec == pinned2:
			require.True(t, ran, "the agent of pve2 ran the pinned connector before the record switched")
		}
		if moving {
			require.Equal(t, "http://"+dbAddr.String()+":8080", w.sharedRule(t, "db.example.com").Service, "the shared rule serves the target while the move lasts, step %s", m.Step)
		}
	}

	require.Equal(t, []string{store.MoveRulesAdded, store.MoveRolledOut, store.MoveCNAMEsSwitched}, steps)
	require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))
	require.Equal(t, []string{pinned2}, w.servedIn(t, "db.example.com"), "the shared rule is gone once the old rules' time passed")
	_, ok := w.moveOf(t, "qemu/102")
	require.False(t, ok, "the move is done")
}

// sharedRule is the rule of host in the shared tunnel; an empty one when
// there is none.
func (w *world) sharedRule(t *testing.T, host string) planner.IngressRule {
	t.Helper()
	for _, r := range w.rulesByTunnel(t)[tunnelName] {
		if r.Hostname == host {
			return r
		}
	}
	return planner.IngressRule{}
}
