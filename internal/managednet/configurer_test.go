package managednet_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

func TestFreeIndex(t *testing.T) {
	g := guest(101)
	g.NICs = append(g.NICs, model.NIC{Index: 2, MAC: "bc:24:11:00:00:02", Bridge: "vmbr1"})
	i, ok := managednet.FreeIndex(g)
	require.True(t, ok)
	require.Equal(t, 1, i)

	g.NICs = nil
	i, ok = managednet.FreeIndex(g)
	require.True(t, ok)
	require.Equal(t, 0, i, "a guest without a card gets net0")

	for n := range 32 {
		g.NICs = append(g.NICs, model.NIC{Index: n, Bridge: "vmbr0"})
	}
	_, ok = managednet.FreeIndex(g)
	require.False(t, ok)
}
