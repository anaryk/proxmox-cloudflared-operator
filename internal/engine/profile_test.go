package engine

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestTheStateNamesTheProfileOfTheInstall(t *testing.T) {
	e := newEnv(t)
	require.Empty(t, e.eng.State().Profile, "no cycle has read the install yet")

	require.Equal(t, store.ProfileHost, e.cycle().Profile, "an install without a profile is a host")

	require.NoError(t, e.store.SaveInstall(applianceInstall()))
	require.Equal(t, store.ProfileAppliance, e.cycle().Profile)
	require.Equal(t, store.ProfileAppliance, e.eng.State().Profile)
}

func applianceInstall() store.Install {
	return store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID:      9200,
		Node:      "pve1",
		MACs:      []string{"bc:24:11:00:92:00"},
		Endpoints: []store.Endpoint{{Address: "10.92.0.1:8006", ServerName: "pve1"}},
	}}
}
