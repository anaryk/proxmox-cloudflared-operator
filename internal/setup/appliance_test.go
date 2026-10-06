package setup

import (
	"context"
	"errors"
	mathrand "math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestSetupAndUninstallRefuseInTheAppliance(t *testing.T) {
	const refused = "this is an appliance: the installer on the node sets it up and removes it (pco appliance install|uninstall)"
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.s.host.profile = func() (string, error) { return store.ProfileAppliance, nil }

	require.EqualError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}), refused)
	require.EqualError(t, e.setup(Options{Yes: true, Repair: true, Node: testNode}), refused)
	require.EqualError(t, e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode}), refused)
	require.EqualError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}), refused)

	e.done()
	require.NoDirExists(t, e.paths.Cluster, "nothing was made")
	require.Empty(t, e.cf.Calls())
}

func TestAProfileThatCannotBeReadIsNoHost(t *testing.T) {
	e := newTestEnv(t)
	e.s.host.profile = func() (string, error) { return "", errors.New("reading the profile: permission denied") }

	require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "permission denied")
	require.ErrorContains(t, e.uninstall(UninstallOptions{Yes: true}), "permission denied")
	e.done()
}

func TestTheProfileOfTheNodeIsReadFromItsMarker(t *testing.T) {
	if _, err := os.Stat(store.ProfileFile); err == nil {
		t.Skip("this machine has a profile marker")
	}
	profile, err := nodeHost().profile()

	require.NoError(t, err)
	require.Equal(t, store.ProfileHost, profile)
}

func TestRecoverInstallStoresTheInstallItIsGiven(t *testing.T) {
	e := newTestEnv(t)
	require.NoError(t, e.st.Init())
	require.NoError(t, e.st.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 9, Nonce: "n9"}))
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 7))
	block := &store.ApplianceInstall{
		VMID: 120, Node: testNode, MACs: []string{"bc:24:11:00:00:10"},
		Endpoints: []store.Endpoint{{Address: "192.0.2.10:8006", ServerName: testNode}},
		CAFile:    "/var/lib/pco/pve-ca.pem",
	}
	now := func() time.Time { return t0 }

	inst, generation, err := RecoverInstall(context.Background(), e.cf, e.st, "", now, mathrand.NewChaCha8([32]byte{2}),
		store.Install{Profile: store.ProfileAppliance, Appliance: block})

	require.NoError(t, err)
	require.Equal(t, 10, generation, "above the stored generation, which is above the sentinel")
	want := store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: block}
	require.Equal(t, want, inst)
	require.Equal(t, want, e.install())
	w := e.writer()
	require.Equal(t, 10, w.Generation)
	require.NotEqual(t, "n9", w.Nonce)
	settings, err := e.st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly)
}

func TestRecoverInstallTakesTheNamedInstall(t *testing.T) {
	const other = "ba9876543210"
	e := newTestEnv(t)
	require.NoError(t, e.st.Init())
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 3))
	e.cf.SeedTunnel(testAccount, "pco-"+other, sentinel(other, 12))
	now := func() time.Time { return t0 }
	rand := mathrand.NewChaCha8([32]byte{3})

	_, _, err := RecoverInstall(context.Background(), e.cf, e.st, "", now, rand, store.Install{Profile: store.ProfileHost})
	require.ErrorContains(t, err, "choose one with --install-id")

	inst, generation, err := RecoverInstall(context.Background(), e.cf, e.st, other, now, rand, store.Install{Profile: store.ProfileHost})
	require.NoError(t, err)
	require.Equal(t, other, inst.ID)
	require.Equal(t, 13, generation)
}
