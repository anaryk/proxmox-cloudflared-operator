package present

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestNodeWords(t *testing.T) {
	for _, s := range NodeStateOrder {
		require.NotEqual(t, string(s), "", "every state of the order has a word")
		require.True(t, len(NodeStateText(string(s))) >= len(s), s)
	}
	require.Equal(t, "active", NodeStateText(string(agentproto.Active)))
	require.Equal(t, "not numbered yet (an entry of an earlier release)", NodeStateText(""))
	require.Equal(t, "odd", NodeStateText("odd"), "a state of another version is shown as it is")

	require.Equal(t, "controller", NodeRoleText(engine.NodeView{Controller: true}))
	require.Equal(t, "agent", NodeRoleText(engine.NodeView{}))

	require.Equal(t, "no status yet", NodeAgentText(engine.NodeView{}))
	require.Equal(t, "heard 3s ago", NodeAgentText(engine.NodeView{Build: "0.9.0", StatusAge: 3*time.Second + 400*time.Millisecond}))
	require.Equal(t, "heard 0s ago, watching", NodeAgentText(engine.NodeView{WatchRunning: true}))

	require.Equal(t, "", RotationText(nil))
	require.Equal(t, "running for node pve3: 2 of 6 steps done, waiting on tunnels",
		RotationText(&engine.CARotationView{Node: "pve3", Done: []string{"credentials", "proxmox"}, Next: "tunnels"}))
	require.Equal(t, "running for nodes pve3, pve4 and pve2: 0 of 6 steps done, waiting on credentials",
		RotationText(&engine.CARotationView{Node: "pve3", Also: []string{"pve4", "pve2"}, Next: "credentials"}))
	require.Equal(t, "running for node pve3: 2 of 6 steps done, waiting on tunnels; the CA waits: no credential reaches tunnel pco-abc in account acc9",
		RotationText(&engine.CARotationView{Node: "pve3", Done: []string{"credentials", "proxmox"}, Next: "tunnels",
			Held: "the CA waits: no credential reaches tunnel pco-abc in account acc9"}))
	require.Equal(t, "finished for nodes pve3 and pve4", RotationText(&engine.CARotationView{Node: "pve3", Also: []string{"pve4"}, FinishedAt: time.Unix(1, 0)}))
	require.Equal(t, "finished for node pve3", RotationText(&engine.CARotationView{Node: "pve3", FinishedAt: time.Unix(1, 0)}))
	require.Equal(t, "finished for node pve3; the new CA refuses 1 nodes still",
		RotationText(&engine.CARotationView{Node: "pve3", FinishedAt: time.Unix(1, 0), Refused: []string{"pve2"}}))
}
