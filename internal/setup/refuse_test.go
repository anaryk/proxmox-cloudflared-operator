package setup

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// holdLock takes the lock of the node, as a daemon run by hand does, until the
// test ends.
func (e *testEnv) holdLock() {
	e.t.Helper()
	f, err := os.OpenFile(filepath.Join(e.paths.Local, "daemon.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	require.NoError(e.t, err)
	require.NoError(e.t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	e.t.Cleanup(func() { _ = f.Close() })
}

func TestRecoverStartsAgainWhatItStopped(t *testing.T) {
	for _, active := range []bool{true, false} {
		t.Run(map[bool]string{true: "running", false: "stopped"}[active], func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			h := newFakeHost(t)
			h.active[serviceUnit] = active
			e.onHost(h)

			// The token sees no install.
			err := e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode})

			require.ErrorContains(t, err, "sees no tunnel of an install")
			require.Equal(t, active, h.active[serviceUnit], "the daemon is as it was")
			if active {
				require.Contains(t, h.ran, "systemctl stop pco.service")
				e.requireShown("was running before --recover and is started again")
			} else {
				require.NotContains(t, h.ran, "systemctl stop pco.service")
				e.requireShown("was not running before --recover and stays stopped")
			}
		})
	}
}

func TestRecoverNeverReplacesAnotherInstall(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options Options
	}{
		{"found at Cloudflare", Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode}},
		{"named", Options{Yes: true, Recover: true, InstallID: testInstall, CloudflareToken: cfToken, Node: testNode}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const stored = "ba9876543210"
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			require.NoError(t, e.st.Init())
			require.NoError(t, e.st.SaveInstall(store.Install{ID: stored, CreatedAt: t0, Profile: store.ProfileHost}))
			e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 4))
			e.onHost(newFakeHost(t))

			err := e.setup(tt.options)

			require.ErrorContains(t, err, "the store holds install "+stored)
			require.ErrorContains(t, err, "pco uninstall")
			require.ErrorContains(t, err, "--install-id "+stored)
			require.Equal(t, stored, e.install().ID, "the stored install stays")
			_, found, err := e.st.Writer()
			require.NoError(t, err)
			require.False(t, found)
		})
	}
}

func TestRecoverForcesObserveOnly(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	require.NoError(t, e.st.Init())
	settings := store.DefaultSettings()
	settings.ObserveOnly = false
	require.NoError(t, e.st.SaveSettings(settings))
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 4))
	e.onHost(newFakeHost(t))

	require.NoError(t, e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode}))

	got, err := e.st.Settings()
	require.NoError(t, err)
	require.True(t, got.ObserveOnly)
	require.Equal(t, 5, e.writer().Generation)
}

func TestRecoverDoesNotGuess(t *testing.T) {
	t.Run("a named install the token does not see", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit(serviceUnit)
		e.onHost(newFakeHost(t))

		err := e.setup(Options{Yes: true, Recover: true, InstallID: testInstall, CloudflareToken: cfToken, Node: testNode})

		require.ErrorContains(t, err, "sees no tunnel of install "+testInstall)
		require.ErrorContains(t, err, "check the id, or recover with a token that sees the account of its tunnel",
			"the id was typed here")
		var none *planner.NoTunnelsError
		require.ErrorAs(t, err, &none)
		require.Equal(t, testInstall, none.InstallID)
		_, found, err := e.st.Writer()
		require.NoError(t, err)
		require.False(t, found, "no generation 1 is taken for granted")
	})
	t.Run("a tunnel in the account of a zone the account listing lacks", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit(serviceUnit)
		e.cf.AddAccount("acc2", "Other")
		e.cf.AddZone("zone2", "example.net", "acc2")
		e.cf.SeedTunnel("acc2", "pco-"+testInstall, sentinel(testInstall, 9))
		e.api = func(api cfapi.API) cfapi.API { return hiddenAccount{API: api, id: "acc2"} }
		e.onHost(newFakeHost(t))

		require.NoError(t, e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode}))

		require.Equal(t, testInstall, e.install().ID)
		require.Equal(t, 10, e.writer().Generation)
	})
}

// hiddenAccount is a Cloudflare whose account listing lacks an account the
// token still reaches through its zones.
type hiddenAccount struct {
	cfapi.API
	id string
}

func (h hiddenAccount) Accounts(ctx context.Context) ([]cfapi.Account, error) {
	accounts, err := h.API.Accounts(ctx)
	var out []cfapi.Account
	for _, a := range accounts {
		if a.ID != h.id {
			out = append(out, a)
		}
	}
	return out, err
}

func TestADaemonRunByHandCountsAsRunning(t *testing.T) {
	t.Run("setup stores no credential", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit(serviceUnit)
		e.onHost(newFakeHost(t))
		e.holdLock()

		require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))

		e.requireShown(`pco is running: add credentials with "pco credential add"`)
		require.Empty(t, e.credentials())
	})
	t.Run("uninstall removes nothing", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit(serviceUnit)
		h := newFakeHost(t)
		e.onHost(h)
		require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
		h.active[serviceUnit] = false // systemd's daemon is stopped, one by hand runs
		e.holdLock()

		err := e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true})

		require.ErrorContains(t, err, "daemon.lock")
		require.ErrorContains(t, err, "nothing was removed")
		require.Contains(t, h.users, UserID)
		require.True(t, h.cloudflared)
		require.Equal(t, testNode, e.manifest().Node, "the store stays")
	})
	t.Run("recover writes no store", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit(serviceUnit)
		e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 4))
		e.onHost(newFakeHost(t))
		e.holdLock()

		err := e.setup(Options{Yes: true, Recover: true, CloudflareToken: cfToken, Node: testNode})

		require.ErrorContains(t, err, "daemon.lock")
		_, found, err := e.st.Install()
		require.NoError(t, err)
		require.False(t, found)
	})
}

func TestAnInstallWithoutAWriterPointsToRecover(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	require.NoError(t, e.st.Init())
	require.NoError(t, e.st.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileHost}))
	e.onHost(newFakeHost(t))

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	require.Equal(t, 1, e.writer().Generation, "generation 1 is fenced: a writer above it is never taken for this one")
	e.requireShown("pco setup --recover")
}

func TestSetupChecksTheStoredProxmoxSecret(t *testing.T) {
	refused := &pve.APIError{Status: http.StatusUnauthorized, Message: "authentication failure"}
	for _, tt := range []struct {
		name   string
		answer error
		remade bool
		fails  string
	}{
		{"accepted", nil, false, ""},
		{"refused", refused, true, ""},
		{"not reached", errors.New("dial tcp 127.0.0.1:8006: connect: connection refused"), false, "connection refused"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			h := newFakeHost(t)
			e.onHost(h)
			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			before := e.pveToken()
			var checked []string
			e.s.host.checkToken = func(_ context.Context, tok store.PVEToken) error {
				checked = append(checked, tok.TokenID)
				return tt.answer
			}

			err := e.setup(Options{Yes: true, Repair: true, Node: testNode})

			require.Equal(t, []string{tokenID}, checked, "one read with the stored token")
			if tt.fails != "" {
				require.ErrorContains(t, err, "step token")
				require.ErrorContains(t, err, tt.fails)
				require.NotContains(t, h.ran, "pveum user token remove pco@pve pco")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.remade, !before.Secret.Equal(e.pveToken().Secret))
			if tt.remade {
				e.requireShown("Proxmox refuses the stored secret")
			}
		})
	}
}

func TestTheRoleOthersHoldStays(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	require.True(t, e.manifest().CreatedRole)
	h.users = append(h.users, "alice@pve")
	h.acl = append(h.acl, pveACL{Path: "/vms/100", Type: "user", UGID: "alice@pve", Role: RoleID})

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))

	require.Contains(t, h.roles, RoleID)
	require.NotContains(t, h.ran, "pveum role delete PCO")
	e.requireShown("alice@pve")
	require.Contains(t, h.acl, pveACL{Path: "/vms/100", Type: "user", UGID: "alice@pve", Role: RoleID}, "alice keeps her grant")
	require.NotContains(t, h.users, UserID)
}
