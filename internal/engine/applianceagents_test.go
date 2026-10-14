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

// An appliance controller whose volume came back older than what its agents
// follow, as a relocation on replicated storage brings it, writes nothing.
func TestAControllerBelowAnAgentsEpochOnlyObserves(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one, Kind: "appliance", VMID: 9301})
	e.enforce()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	r.mu.Lock()
	rep := r.reports["pve2"]
	rep.Status.EpochSeen = agentproto.Epoch{InstallID: testInstall, Generation: testWriter.Generation + 1, Nonce: "f00dcafe"}
	r.reports["pve2"] = rep
	r.mu.Unlock()

	st := e.cycle()

	require.Equal(t, ModeObserve, st.Mode)
	require.Contains(t, problemsText(st), "the agent of node pve2 follows epoch "+rep.Status.EpochSeen.String())
	require.Contains(t, problemsText(st), "pco appliance recover")
	require.Empty(t, r.last(), "no agent is served a snapshot")
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
