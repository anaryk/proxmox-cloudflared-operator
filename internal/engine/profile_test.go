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

func TestTheEngineSaysWhatItRunsAs(t *testing.T) {
	e := newEnv(t)
	profile, node := e.eng.RunsAs()
	require.Empty(t, profile, "no cycle has read the install yet")
	require.Equal(t, testNode, node)

	e.cycle()
	profile, node = e.eng.RunsAs()
	require.Equal(t, store.ProfileHost, profile)
	require.Equal(t, testNode, node)
}

func applianceInstall() store.Install {
	return store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID:      9200,
		Node:      "pve1",
		MACs:      []string{"bc:24:11:00:92:00"},
		Endpoints: []store.Endpoint{{Address: "10.92.0.1:8006", ServerName: "pve1"}},
	}}
}

// The controller of an appliance install runs in its container, whose entry
// the install's init makes without a kind: a view shows it as the appliance
// it is, not as a host.
func TestTheControllerOfAnApplianceIsShownAsTheAppliance(t *testing.T) {
	e := newEnv(t)
	inst := applianceInstall()
	inst.Appliance.Node = testNode
	require.NoError(t, e.store.SaveInstall(inst))
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0}))
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: "pve2", Since: t0, Kind: "appliance", VMID: 9201}))
	e.cycle()

	byName := map[string]NodeView{}
	for _, n := range e.eng.Nodes() {
		byName[n.Name] = n
	}
	require.Equal(t, "appliance", byName[testNode].Kind)
	require.Equal(t, 9200, byName[testNode].VMID)
	require.Equal(t, "appliance", byName["pve2"].Kind)
}
