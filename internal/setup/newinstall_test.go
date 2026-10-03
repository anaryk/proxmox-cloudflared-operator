package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	otherInstall = "ba9876543210"
	tunnelID2    = "00000000-0000-4000-8000-000000000002"
	tunnelID3    = "00000000-0000-4000-8000-000000000003"
)

// connectorOf puts the files of the connector of a tunnel on the node, as the
// manager writes them for an install; none names no install.
func (e *testEnv) connectorOf(install, tunnel string) {
	e.t.Helper()
	dir := filepath.Join(e.paths.Local, tunnelsDir)
	require.NoError(e.t, os.MkdirAll(dir, 0o700))
	env := "METRICS_ADDR=127.0.0.1:20300\nEDGE_IP_VERSION=auto\n"
	if install != "" {
		env += "PCO_INSTALL=" + install + "\n"
	}
	require.NoError(e.t, os.WriteFile(filepath.Join(dir, tunnel+".token"), []byte("run token"), 0o600))
	require.NoError(e.t, os.WriteFile(filepath.Join(dir, tunnel+".env"), []byte(env), 0o644))
}

// connectorsFound is what the host answers to a look at the connectors that
// have files and no unit loaded: each one is asked whether it runs.
func connectorsFound(tunnels ...string) []call {
	script := []call{{line: listConnectors}}
	for _, id := range tunnels {
		script = append(script, call{line: "systemctl is-active -- pco-cloudflared@" + id + ".service", out: "inactive\n", err: exitErr(3, "")})
	}
	return script
}

func (e *testEnv) requireNothingCreated() {
	e.t.Helper()
	require.NoDirExists(e.t, e.paths.Cluster)
	require.NoDirExists(e.t, e.paths.Private)
	_, _, err := e.st.Install()
	require.ErrorIs(e.t, err, store.ErrNoRoot)
}

func TestSetupRefusesANewInstallBesideTheConnectorsOfOne(t *testing.T) {
	e := newTestEnv(t)
	e.connectorOf(testInstall, tunnelID)
	e.connectorOf(testInstall, tunnelID2)
	e.script(preflight("9.0.10"), connectorsFound(tunnelID, tunnelID2))

	err := e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode})

	require.ErrorContains(t, err, "step store")
	require.ErrorContains(t, err, "install "+testInstall+" (2 connectors)")
	require.ErrorContains(t, err, "pco setup --recover")
	require.NotContains(t, err.Error(), "--install-id")
	require.ErrorContains(t, err, "pco uninstall --keep-cloudflare")
	require.ErrorContains(t, err, "--new-install")
	e.done()
	e.requireNothingCreated()
	e.requireNoSecret()
}

func TestSetupRefusesANewInstallBesideTheConnectorsOfTwo(t *testing.T) {
	e := newTestEnv(t)
	e.connectorOf(testInstall, tunnelID)
	e.connectorOf(otherInstall, tunnelID2)
	e.connectorOf(otherInstall, tunnelID3)
	e.script(preflight("9.0.10"), connectorsFound(tunnelID, tunnelID2, tunnelID3))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "install "+testInstall+" (1 connector)")
	require.ErrorContains(t, err, "install "+otherInstall+" (2 connectors)")
	require.ErrorContains(t, err, "pco setup --recover --install-id <id>")
	require.ErrorContains(t, err, "pco uninstall --keep-cloudflare")
	e.done()
	e.requireNothingCreated()
}

func TestSetupRefusesANewInstallBesideAConnectorOfNoKnownInstall(t *testing.T) {
	e := newTestEnv(t)
	e.connectorOf("", tunnelID)
	e.script(preflight("9.0.10"), connectorsFound(tunnelID))

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "no known install (1 connector)")
	require.ErrorContains(t, err, "pco setup --recover")
	e.done()
	e.requireNothingCreated()
}

func TestSetupSeesAConnectorWhoseUnitIsLoaded(t *testing.T) {
	e := newTestEnv(t)
	e.connectorOf(testInstall, tunnelID)
	e.script(preflight("9.0.10"), []call{
		{line: listConnectors, out: "pco-cloudflared@" + tunnelID + ".service loaded active running pco cloudflared connector\n"},
		{line: "systemctl is-active -- pco-cloudflared@" + tunnelID + ".service", out: "active\n"},
	})

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "install "+testInstall+" (1 connector)")
	e.done()
}

func TestSetupReadsTheInstallTheConnectorManagerWrote(t *testing.T) {
	e := newTestEnv(t)
	h := newFakeHost(t)
	e.onHost(h)
	m := connector.NewManager(unitControl{h}, filepath.Join(e.paths.Local, tunnelsDir), nil, zerolog.Nop())
	require.NoError(t, m.Ensure(context.Background(), testInstall, tunnelID, "run token"))
	require.True(t, h.active["pco-cloudflared@"+tunnelID+".service"], "the connector runs")

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "install "+testInstall+" (1 connector)")
	e.requireNothingCreated()
	h.requireUntouched()
}

func TestUninstallKeepingCloudflareLetsSetupStartOver(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	m := connector.NewManager(unitControl{h}, filepath.Join(e.paths.Local, tunnelsDir), nil, zerolog.Nop())
	require.NoError(t, m.Ensure(context.Background(), testInstall, tunnelID, "run token"))
	require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "install "+testInstall)

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
	e.reopen()
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	require.NotEqual(t, testInstall, e.install().ID)
	require.False(t, h.active["pco-cloudflared@"+tunnelID+".service"], "the old connector is gone")
}

func TestSetupRefusesWhenItCannotLookForConnectors(t *testing.T) {
	e := newTestEnv(t)
	e.script(preflight("9.0.10"), []call{{line: listConnectors, err: exitErr(1, "Failed to connect to bus")}})

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "step store")
	require.ErrorContains(t, err, "Failed to connect to bus")
	require.ErrorContains(t, err, "--new-install")
	e.done()
	e.requireNothingCreated()
}

func TestNewInstallGoesOnBesideTheConnectors(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.connectorOf(testInstall, tunnelID)
	e.script(freshInstallAfter(preflight("9.0.10"))...)

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, NewInstall: true, Node: testNode}))
	e.done()

	require.NotEqual(t, testInstall, e.install().ID, "the install is a new one")
	env, err := os.ReadFile(filepath.Join(e.paths.Local, tunnelsDir, tunnelID+".env"))
	require.NoError(t, err)
	require.Contains(t, string(env), "PCO_INSTALL="+testInstall, "the old connector is left as it is")
	e.requireNoSecret()
}

func TestRecoverIsNotHeldUpByTheConnectors(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.connectorOf(testInstall, tunnelID)
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 2))
	e.script(append([][]call{preflight("9.0.10"), stoppedForRecovery()}, recovered()...)...)

	require.NoError(t, e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	require.Equal(t, testInstall, e.install().ID)
	require.Equal(t, 3, e.writer().Generation)
}

func TestSetupKeepsItsInstallBesideTheConnectorsOfAnother(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.connectorOf(otherInstall, tunnelID2)
	e.script(preflight("9.0.10"), roleKept(), userKept(), tokenKept(), tagsKept(), cloudflaredKept(), serviceStarted())

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.done()

	require.Equal(t, testInstall, e.install().ID)
}

func TestNewInstallDoesNotGoWithRepairOrRecover(t *testing.T) {
	for _, o := range []Options{
		{NewInstall: true, Repair: true},
		{NewInstall: true, Recover: true},
	} {
		e := newTestEnv(t)
		require.ErrorContains(t, e.setup(o), "--new-install")
		e.done()
	}
}
