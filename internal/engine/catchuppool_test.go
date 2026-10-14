package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// What catchUp said in an epoch is forgotten with the epoch: the memory
// holds one epoch at a time.
func TestTheMemoryOfACatchUpIsOfOneEpoch(t *testing.T) {
	var m catchUpMemory
	one := agentproto.Epoch{InstallID: "i", Generation: 1, Nonce: "a"}
	two := agentproto.Epoch{InstallID: "i", Generation: 2, Nonce: "b"}

	m.of(one).said["no card 06:50:43:4f:00:01"] = true
	m.of(one).refused["pve2"] = "a line"
	m.of(one).guessed, m.of(one).moved = true, 5
	require.True(t, m.of(one).said["no card 06:50:43:4f:00:01"])

	got := m.of(two)
	require.Empty(t, got.said)
	require.Empty(t, got.refused)
	require.False(t, got.guessed)
	require.Zero(t, got.moved)
}
