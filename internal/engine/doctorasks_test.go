package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The doctor of the controller hands the other nodes a doctor action, which
// the next cycle puts in their snapshots, and takes each answer from the
// status of the node it was for.
func TestTheDoctorAsksTheAgentsOfTheOtherNodes(t *testing.T) {
	one, two := 1, 2
	e, r, _ := clusterOf(t,
		store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one},
		store.NodeEntry{Name: "pve3", Since: t0, State: store.NodeActive, StateSince: t0, Index: &two},
	)
	e.enforce()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	r.report("pve3", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	epoch := r.last()["pve2"].Epoch

	answers := make(chan map[string]agentproto.ActionResult)
	go func() { answers <- e.eng.AskDoctor(t.Context(), []string{"pve2", "pve3"}, 30*time.Second) }()
	<-e.eng.trigger
	e.cycle()

	actions := r.last()["pve2"].Actions
	require.Len(t, actions, 1)
	require.Equal(t, agentproto.ActionDoctor, actions[0].Kind)
	asked3 := r.last()["pve3"].Actions
	require.Len(t, asked3, 1)
	require.NotEqual(t, actions[0].ID, asked3[0].ID)

	findings := json.RawMessage(`[{"check":"cloudflared","level":"ok","detail":"fine"}]`)
	// pve3 cannot answer for pve2.
	e.eng.TakeAgentEvents("pve3", agentproto.Status{Node: "pve3", AgentStartedAt: t0,
		Actions: []agentproto.ActionResult{{ID: actions[0].ID, OK: true, Data: json.RawMessage(`[]`)}}}, epoch)
	e.eng.TakeAgentEvents("pve2", agentproto.Status{Node: "pve2", AgentStartedAt: t0,
		Actions: []agentproto.ActionResult{{ID: actions[0].ID, OK: true, Data: findings}}}, epoch)
	e.eng.TakeAgentEvents("pve3", agentproto.Status{Node: "pve3", AgentStartedAt: t0,
		Actions: []agentproto.ActionResult{{ID: asked3[0].ID, Error: "the daemon is starting"}}}, epoch)

	got := <-answers
	require.Equal(t, map[string]agentproto.ActionResult{
		"pve2": {ID: actions[0].ID, OK: true, Data: findings},
		"pve3": {ID: asked3[0].ID, Error: "the daemon is starting"},
	}, got)

	e.cycle()
	require.Empty(t, r.last()["pve2"].Actions, "taken back once answered")
}

// An agent that does not answer in time is left out, and its action taken
// back.
func TestTheDoctorStopsWaitingForAnAgent(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	e.enforce()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.eng.timeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		return ctx, cancel
	}

	got := e.eng.AskDoctor(t.Context(), []string{"pve2"}, 30*time.Second)

	require.Empty(t, got)
	require.Empty(t, e.eng.asks.actionsFor("pve2"))
}

// One ask runs at a time: another that cannot start within its wait hands
// no agent anything.
func TestOneDoctorAskRunsAtATime(t *testing.T) {
	one := 1
	e, _, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	e.eng.asks.one <- struct{}{} // an ask that runs
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got := e.eng.AskDoctor(ctx, []string{"pve2"}, 30*time.Second)

	require.Empty(t, got)
	e.eng.asks.mu.Lock()
	require.Empty(t, e.eng.asks.acts, "nothing was handed out")
	e.eng.asks.mu.Unlock()
	require.Empty(t, e.eng.trigger, "no cycle was asked for")
}
