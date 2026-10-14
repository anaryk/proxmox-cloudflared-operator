package main

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func agentView() agentproto.AgentView {
	return agentproto.AgentView{
		Node: "pve2", Kind: pki.KindHost, Controller: "pve1", SilentFor: 0,
		Epoch: agentproto.Epoch{InstallID: "abc123", Generation: 3, Nonce: "f00d"}, Seq: 42, SeqAge: 7 * time.Second,
		Connectors: 2, EgressTargets: 5,
		CertFingerprint: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", CertExpires: t0.Add(80 * 24 * time.Hour),
		ReadyReplicas: map[string]int{"6f1d0c2e-2b8a-4c43-9a51-3e2d7b6c1a90": 3, "aaaa0c2e-2b8a-4c43-9a51-3e2d7b6c1a90": 1}, HeartbeatAge: 4 * time.Second,
		Problems: []string{},
	}
}

func TestAgentStatusGolden(t *testing.T) {
	e := &fakeEngine{state: healthyState(), agent: agentView()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "agent", "status")
	require.NoError(t, res.err)
	requireGolden(t, "agent_status.golden", res.out)

	res = r.run("", "agent", "status", "--json")
	require.NoError(t, res.err)
	require.Contains(t, res.out, `"certFingerprint": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"`)

	v := agentView()
	v.SilentFor = 75 * time.Second
	v.StoodDown = "this node was removed from the install"
	v.Problems = []string{"no controller answered for 1m15s; this node serves what it was last told"}
	e.mu.Lock()
	e.agent = v
	e.mu.Unlock()
	res = r.run("", "agent", "status")
	require.ErrorIs(t, res.err, errReported)
	requireGolden(t, "agent_status_silent.golden", res.out)
}

func TestAgentResetEpochAsksFirst(t *testing.T) {
	e := &fakeEngine{state: healthyState()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "agent", "reset-epoch")
	require.ErrorIs(t, res.err, errNoTerminal)

	res = r.tty().run("\n", "agent", "reset-epoch")
	require.ErrorIs(t, res.err, errAborted)
	require.Empty(t, e.called())

	res = r.run("", "agent", "reset-epoch", "--yes")
	require.NoError(t, res.err)
	require.Equal(t, "The epoch the agent remembers is forgotten; it follows the store's at the next start of pco (systemctl restart pco).\n", res.out)
	require.Equal(t, []string{"reset-epoch"}, e.called())

	e.clusterErr = fmt.Errorf("%w: not in this test", engine.ErrRefused)
	res = r.run("", "agent", "reset-epoch", "--yes")
	require.ErrorIs(t, res.err, engine.ErrRefused)
}

// pco status on a node that is not the controller says where the
// controller is and how the agent of the node fares.
func TestStatusOfANodeThatIsNotTheController(t *testing.T) {
	st := healthyState()
	st.Node = "pve2"
	st.Lease = &engine.LeaseView{Holder: "pve1", Address: netip.MustParseAddr("10.0.0.1"), Generation: 3}
	v := agentView()
	v.Remote, v.Heard = true, true
	v.Problems = []string{"snapshot 43 refused: out of range"}
	e := &fakeEngine{state: st, agent: v}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "status")

	require.NoError(t, res.err, "the problems of the agent are its own to report")
	requireGolden(t, "status_follower.golden", res.out)

	e.mu.Lock()
	e.state.Lease = &engine.LeaseView{}
	e.agent.Heard, e.agent.SilentFor, e.agent.Problems = false, 3*time.Minute, nil
	e.mu.Unlock()
	res = r.run("", "status")
	require.Contains(t, res.out, "Controller:  none: no node holds the lease\n")
	require.Contains(t, res.out, "Agent:       has heard no controller (3m0s); snapshot 42")

	e.mu.Lock()
	e.state.Lease.Holder = "pve1"
	e.clusterErr = errors.New("the agent is not running")
	e.mu.Unlock()
	res = r.run("", "status")
	require.Contains(t, res.out, "Agent:       did not answer: the agent is not running\n")
}

// A node alone says nothing of a controller: its status is as it was before
// there were clusters.
func TestStatusOfANodeAloneHasNoAgent(t *testing.T) {
	st := healthyState()
	st.Node = "pve1"
	st.Lease = &engine.LeaseView{Holder: "pve1", Generation: 3}
	e := &fakeEngine{state: st, agent: agentView()}
	r := newRunner(t, serveFake(t, e))

	res := r.run("", "status")

	require.NotContains(t, res.out, "Controller:")
	require.NotContains(t, res.out, "Agent:")
}
