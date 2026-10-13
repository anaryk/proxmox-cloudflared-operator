package reconcile

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// The shared tunnel of a single node says nothing of kinds or nodes; a
// pinned one names both.
func TestATunnelNamesItsNodeOnlyWhenPinned(t *testing.T) {
	for _, v := range []any{
		TunnelState{AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc"},
		planner.TunnelPlan{AccountID: "acc1", CredentialID: "cred1", Name: "pco-abc"},
	} {
		data, err := json.Marshal(v)
		require.NoError(t, err)
		require.NotContains(t, string(data), `"kind"`)
		require.NotContains(t, string(data), `"node"`)
	}
	data, err := json.Marshal(TunnelState{Name: "pco-abc-pve2", Kind: "pinned", Node: "pve2"})
	require.NoError(t, err)
	require.Contains(t, string(data), `"kind":"pinned","node":"pve2"`)
	data, err = json.Marshal(planner.TunnelPlan{Name: "pco-abc-pve2", Kind: "pinned", Node: "pve2"})
	require.NoError(t, err)
	require.Contains(t, string(data), `"kind":"pinned","node":"pve2"`)
}
