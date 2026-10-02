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

	require.NoError(t, e.store.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance}))
	require.Equal(t, store.ProfileAppliance, e.cycle().Profile)
	require.Equal(t, store.ProfileAppliance, e.eng.State().Profile)
}
