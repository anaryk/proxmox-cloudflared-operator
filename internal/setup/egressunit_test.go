package setup

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// The unit that loads the egress table is enabled by setup, so that the table
// is there at boot before a connector exists, and before the daemon starts.
func TestSetupEnablesTheEgressUnitBeforeTheDaemon(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.installUnit(egressUnit)
	h := newFakeHost(t)
	e.onHost(h)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	require.True(t, h.enabled[egressUnit])
	require.True(t, h.active[egressUnit])
	egress := slices.Index(h.ran, "systemctl enable --now pco-egress.service")
	daemon := slices.Index(h.ran, "systemctl enable --now pco.service")
	require.GreaterOrEqual(t, egress, 0, "%v", h.ran)
	require.Greater(t, daemon, egress, "the unit is ordered before the daemon")
	e.requireShown("pco-egress.service: enabled and running")
}

func TestSetupWithoutTheEgressUnitFileLeavesItAlone(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	for _, line := range h.ran {
		require.NotContains(t, line, egressUnit)
	}
	require.True(t, h.enabled[serviceUnit])
}

func TestSetupEnablesTheEgressUnitWhileADaemonRunsByHand(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.installUnit(egressUnit)
	h := newFakeHost(t)
	e.onHost(h)
	e.holdLock()

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	require.True(t, h.enabled[egressUnit])
	require.False(t, h.active[serviceUnit], "a second daemon is not started")
}

func TestSetupThatCannotEnableTheEgressUnitSaysSoAndStartsNoDaemon(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.installUnit(egressUnit)
	h := newFakeHost(t)
	h.refuse["systemctl enable --now pco-egress.service"] = exitErr(1, "Job for pco-egress.service failed")
	e.onHost(h)

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "enabling pco-egress.service")
	require.ErrorContains(t, err, "Job for pco-egress.service failed")
	require.False(t, h.enabled[serviceUnit])

	delete(h.refuse, "systemctl enable --now pco-egress.service")
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}), "a run after the fault finishes the setup")
	require.True(t, h.enabled[egressUnit])
	require.True(t, h.enabled[serviceUnit])
}

func TestUninstallDisablesTheEgressUnitSetupEnabled(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.installUnit(egressUnit)
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	require.True(t, h.enabled[egressUnit])

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true, RemoveCloudflared: true}))

	require.Contains(t, h.ran, "systemctl disable --now pco-egress.service")
	require.False(t, h.enabled[egressUnit])
	require.False(t, h.active[egressUnit])
}
