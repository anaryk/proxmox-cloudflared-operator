package setup

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUninstallAfterAKilledUninstallFinishes(t *testing.T) {
	full := Options{Yes: true, CloudflareToken: cfToken, Node: testNode}
	remove := UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}
	clean := newTestEnv(t)
	clean.installUnit(serviceUnit)
	h := newFakeHost(t)
	clean.onHost(h)
	require.NoError(t, clean.setup(full))
	h.ran = nil
	require.NoError(t, clean.uninstall(remove))
	commands := slices.Clone(h.ran)

	for k := range commands {
		for _, before := range []bool{true, false} {
			t.Run(sweepName(k+1, before, commands[k]), func(t *testing.T) {
				t.Parallel()
				e := newTestEnv(t)
				e.installUnit(serviceUnit)
				host := newFakeHost(t)
				e.onHost(host)
				require.NoError(t, e.setup(full))
				host.ran, host.killedAt, host.before = nil, k+1, before

				require.True(t, killed(t, func() error { return e.s.Uninstall(context.Background(), remove) }))
				host.killedAt = 0
				require.NoError(t, e.uninstall(remove), "a second uninstall finishes the rest")

				e.requireNothingLeft(host)
			})
		}
	}
}

func TestSetupNotesTheGrantItAdds(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	e.onHost(newFakeHost(t))
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	require.True(t, e.manifest().GrantedACL)

	// A grant an admin made is not setup's.
	e = newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	h.users = append(h.users, UserID)
	h.roles[RoleID] = []string{"VM.Audit", "Sys.Audit", "VM.GuestAgent.Audit", "SDN.Audit"}
	h.acl = append(h.acl, pveACL{Path: "/", Type: "user", UGID: UserID, Role: RoleID})
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	m := e.manifest()
	require.False(t, m.GrantedACL)
	require.False(t, m.CreatedUser)
	require.False(t, m.CreatedRole)
}

func TestUninstallRevokesTheGrantOfAUserOrRoleThatStays(t *testing.T) {
	for _, tt := range []struct {
		name                 string
		adminUser, adminRole bool
	}{
		{"the admin's user", true, false},
		{"the admin's role", false, true},
		{"both the admin's", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installUnit(serviceUnit)
			h := newFakeHost(t)
			if tt.adminUser {
				h.users = append(h.users, UserID)
			}
			if tt.adminRole {
				h.roles[RoleID] = []string{"VM.Audit", "Sys.Audit", "VM.GuestAgent.Audit", "SDN.Audit"}
			}
			e.onHost(h)
			require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
			require.True(t, e.manifest().GrantedACL)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))

			require.Contains(t, h.ran, "pveum acl delete / --users pco@pve --roles PCO")
			require.False(t, slices.ContainsFunc(h.acl, func(a pveACL) bool { return a.UGID == UserID }), "the grant is gone")
			require.Equal(t, tt.adminUser, slices.Contains(h.users, UserID), "the admin's user stays")
			_, role := h.roles[RoleID]
			require.Equal(t, tt.adminRole, role, "the admin's role stays")
		})
	}
}

func TestUninstallChecksThatTheGrantWentWithUserAndRole(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	h.ran = nil

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))

	require.NotContains(t, h.ran, "pveum acl delete / --users pco@pve --roles PCO", "Proxmox drops it with the user and the role")
	last := slices.Index(h.ran, "pveum role delete PCO")
	require.Positive(t, last)
	require.Contains(t, h.ran[last+1:], "pveum acl list --output-format json", "the grants are listed again afterwards")
	require.NotContains(t, e.ask.text(), "is still there")

	// A Proxmox that kept the grant is reported.
	e = newTestEnv(t)
	e.installUnit(serviceUnit)
	h = newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	e.s.run = keepGrants{h}

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))
	e.requireShown("is still there")
}

// keepGrants is a host whose deletions of a user and a role leave their
// grants.
type keepGrants struct{ *fakeHost }

func (k keepGrants) Run(ctx context.Context, name string, args ...string) (string, error) {
	acl := slices.Clone(k.acl)
	out, err := k.fakeHost.Run(ctx, name, args...)
	if name == "pveum" && len(args) > 1 && (args[0] == "user" || args[0] == "role") && args[1] == "delete" {
		k.acl = acl
	}
	return out, err
}

func TestAFailedPackageUpdateKeepsTheSourceNoted(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	h.refuse["apt-get update"] = exitErr(100, "E: The repository 'https://pkg.cloudflare.com/cloudflared any Release' is not signed.")
	e.onHost(h)

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "step cloudflared")
	require.ErrorContains(t, err, "is not signed")
	require.ErrorContains(t, err, "pco setup")
	m := e.manifest()
	require.True(t, m.AddedAptSource)
	require.True(t, m.AddedKeyring)
	require.FileExists(t, e.s.host.sources)

	delete(h.refuse, "apt-get update")
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))
	e.requireNothingLeft(h)
}

func TestACloudflaredThatDoesNotRunIsNotReinstalled(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	h.refuse[cloudflaredBin+" --version"] = exitErr(126, "cannot execute binary file: Exec format error")
	e.onHost(h)

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "step cloudflared")
	require.ErrorContains(t, err, "Exec format error")
	require.NotContains(t, h.ran, "apt-get install -y cloudflared")
	require.False(t, e.manifest().InstalledCloudflared)
}

func TestUninstallKeepsTheKeyAKeptSourceNames(t *testing.T) {
	for _, tt := range []struct {
		name     string
		manifest Manifest
		sources  string
	}{
		{"a source the admin changed", Manifest{AddedAptSource: true, AddedKeyring: true}, "changed by the admin\nSigned-By: %s\n"},
		{"a source the admin wrote", Manifest{AddedKeyring: true}, "Types: deb\nSigned-By: %s\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(tt.manifest)
			sources := fmt.Sprintf(tt.sources, e.s.host.keyring)
			require.NoError(t, os.WriteFile(e.s.host.sources, []byte(sources), 0o644))
			require.NoError(t, os.WriteFile(e.s.host.keyring, []byte(gpgKey), 0o644))
			e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned())

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true, RemoveCloudflared: true}))
			e.done()

			got, err := os.ReadFile(e.s.host.sources)
			require.NoError(t, err)
			require.Equal(t, sources, string(got))
			require.FileExists(t, e.s.host.keyring, "the key the kept source names stays")
			e.requireShown(e.s.host.keyring)
		})
	}
}
