package main

import (
	"fmt"
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
