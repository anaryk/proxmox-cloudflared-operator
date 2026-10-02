package setup

import (
	"context"
	"go/build"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A create that fails leaves nothing, and what an admin makes afterwards is
// the admin's: the note made before the create is taken back.
func TestAFailedCreateLeavesTheObjectToTheAdmin(t *testing.T) {
	refused := exitErr(255, "create failed")
	for _, tt := range []struct {
		name   string
		refuse string
		noted  func(Manifest) bool
		admin  func(e *testEnv, h *fakeHost)
		stays  func(e *testEnv, h *fakeHost) bool
	}{
		{
			name:   "role",
			refuse: "pveum role add PCO --privs " + privs9,
			noted:  func(m Manifest) bool { return m.CreatedRole },
			admin: func(_ *testEnv, h *fakeHost) {
				h.roles[roleID] = []string{"VM.Audit", "Sys.Audit", "VM.GuestAgent.Audit", "SDN.Audit"}
			},
			stays: func(_ *testEnv, h *fakeHost) bool { _, ok := h.roles[roleID]; return ok },
		},
		{
			name:   "user",
			refuse: "pveum user add pco@pve --comment pco operator",
			noted:  func(m Manifest) bool { return m.CreatedUser },
			admin:  func(_ *testEnv, h *fakeHost) { h.users = append(h.users, userID) },
			stays:  func(_ *testEnv, h *fakeHost) bool { return slices.Contains(h.users, userID) },
		},
		{
			name:   "grant",
			refuse: "pveum acl modify / --users pco@pve --roles PCO",
			noted:  func(m Manifest) bool { return m.GrantedACL },
			admin: func(_ *testEnv, h *fakeHost) {
				h.acl = append(h.acl, pveACL{Path: "/", Type: "user", UGID: userID, Role: roleID})
			},
		},
		{
			name:   "tags",
			refuse: "pvesh set /cluster/options --registered-tags a;cf-tunnel;cf-tunnel-managed",
			noted:  func(m Manifest) bool { return len(m.RegisteredTags) > 0 },
			admin:  func(_ *testEnv, h *fakeHost) { h.tags = []string{"a", "cf-tunnel", "cf-tunnel-managed"} },
			stays: func(_ *testEnv, h *fakeHost) bool {
				return slices.Equal(h.tags, []string{"a", "cf-tunnel", "cf-tunnel-managed"})
			},
		},
		{
			name:   "package",
			refuse: "apt-get install -y cloudflared",
			noted:  func(m Manifest) bool { return m.InstalledCloudflared },
			admin:  func(_ *testEnv, h *fakeHost) { h.cloudflared = true },
			stays:  func(_ *testEnv, h *fakeHost) bool { return h.cloudflared },
		},
		{
			name:   "key",
			refuse: "curl",
			noted:  func(m Manifest) bool { return m.AddedKeyring },
			admin: func(e *testEnv, _ *fakeHost) {
				require.NoError(e.t, os.WriteFile(e.s.host.keyring, []byte("the admin's key"), 0o644))
			},
			stays: func(e *testEnv, _ *fakeHost) bool {
				b, err := os.ReadFile(e.s.host.keyring)
				return err == nil && string(b) == "the admin's key"
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			h := newFakeHost(t)
			h.refuse[tt.refuse] = refused
			e.onHost(h)

			require.ErrorContains(t, e.setup(Options{Yes: true, Node: testNode}), "create failed")
			require.False(t, tt.noted(e.manifest()), "the note of what failed is taken back")

			// The admin makes it by hand; setup finds it there and keeps it.
			delete(h.refuse, tt.refuse)
			tt.admin(e, h)
			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			require.False(t, tt.noted(e.manifest()), "what the admin made is not setup's")

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))
			if tt.stays != nil {
				require.True(t, tt.stays(e, h), "uninstall keeps what the admin made")
			}
		})
	}
}

// A role that goes while a user that stays still holds it leaves a grant that
// pveum no longer shows and every call of pveum warns about.
func TestAFailedRevokeKeepsTheRole(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	h.users = append(h.users, userID) // the admin's
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	m := e.manifest()
	require.True(t, m.CreatedRole)
	require.False(t, m.CreatedUser)
	h.refuse["pveum acl delete / --users pco@pve --roles PCO"] = exitErr(255, "cluster not ready - no quorum?")

	err := e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true})

	require.ErrorContains(t, err, "pco uninstall again")
	require.Contains(t, h.roles, roleID)
	require.NotContains(t, h.ran, "pveum role delete PCO")
	e.requireShown("role PCO is kept: the grant of it to pco@pve could not be revoked")

	delete(h.refuse, "pveum acl delete / --users pco@pve --roles PCO")
	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))
	require.NotContains(t, h.roles, roleID)
	require.Contains(t, h.users, userID, "the admin's user stays")
}

func TestSetupDoesNotStartASecondDaemon(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	e.holdLock() // a daemon run by hand

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	require.Contains(t, h.ran, "systemctl enable pco.service")
	require.NotContains(t, h.ran, "systemctl enable --now pco.service")
	require.False(t, h.active[serviceUnit])
	require.True(t, h.enabled[serviceUnit])
	e.requireShown("runs on this node outside systemd")
	e.requireShown("a new Proxmox token reaches a daemon only when it starts")
}

func TestUninstallSaysWhatItDidBeforeADaemonRunByHand(t *testing.T) {
	t.Run("pco.service stopped already", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit(serviceUnit)
		h := newFakeHost(t)
		e.onHost(h)
		require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
		h.active[serviceUnit], h.enabled[serviceUnit] = false, false
		e.holdLock()
		h.ran = nil

		err := e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true})

		require.ErrorContains(t, err, "nothing was removed")
		require.NotContains(t, h.ran, "systemctl disable --now pco.service")
		require.False(t, h.enabled[serviceUnit])
	})
	t.Run("pco.service stopped, and another daemon still runs", func(t *testing.T) {
		e := newTestEnv(t)
		e.installUnit(serviceUnit)
		h := newFakeHost(t)
		e.onHost(h)
		require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
		require.True(t, h.active[serviceUnit])
		e.holdLock()

		err := e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true})

		require.ErrorContains(t, err, "pco.service was stopped and disabled")
		require.ErrorContains(t, err, "nothing else was removed")
		require.Contains(t, h.users, userID)
	})
}

func TestARefusalTheUserOrTokenExplainsStopsTheStep(t *testing.T) {
	past := t0.Add(-24 * time.Hour).Unix()
	for _, tt := range []struct {
		name             string
		userAttrs, token map[string]any
		says             string
	}{
		{"user disabled", map[string]any{"enable": 0}, nil, "user pco@pve is disabled"},
		{"user expired", map[string]any{"enable": 1, "expire": past}, nil, "user pco@pve expired"},
		{"token expired", nil, map[string]any{"expire": past}, "token pco@pve!pco expired"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			h := newFakeHost(t)
			e.onHost(h)
			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			h.userAttrs, h.tokenAttrs = tt.userAttrs, tt.token
			e.s.host.checkToken = func(context.Context, store.PVEToken) error {
				return &pve.APIError{Status: http.StatusUnauthorized, Message: "authentication failure"}
			}
			before := e.pveToken()

			err := e.setup(Options{Yes: true, Repair: true, Node: testNode})

			require.ErrorContains(t, err, "step token")
			require.ErrorContains(t, err, tt.says)
			require.NotContains(t, h.ran, "pveum user token remove pco@pve pco")
			require.True(t, before.Secret.Equal(e.pveToken().Secret), "the token is not made anew")
		})
	}
}

func TestSetupDoesNotDependOnTheDaemon(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	require.NoError(t, err)
	require.NotContains(t, pkg.Imports, "github.com/anaryk/proxmox-cloudflared-operator/internal/daemon")
}
