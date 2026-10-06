package setup

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// These tests pin what uninstall must leave alone, each against a change of
// the code that would remove it.

func TestUninstallKeepsTheUserAndTokenSetupDidNotCreate(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		manifest                  Manifest
		userStays, tokenStays     bool
		tokenRemoved, userDeleted bool
	}{
		{"neither setup's", Manifest{GrantedACL: true}, true, true, false, false},
		{"only the token setup's", Manifest{CreatedToken: true, GrantedACL: true}, true, false, true, false},
		{"only the user setup's", Manifest{CreatedUser: true, GrantedACL: true}, false, false, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(tt.manifest)
			h := newFakeHost(t)
			h.users = append(h.users, UserID)
			h.tokens, h.secret = []string{tokenName}, pveSecret
			h.acl = append(h.acl, pveACL{Path: "/", Type: "user", UGID: UserID, Role: "Auditors"})
			e.onHost(h)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))

			require.Equal(t, tt.userStays, slices.Contains(h.users, UserID))
			require.Equal(t, tt.tokenStays, slices.Contains(h.tokens, tokenName))
			require.Equal(t, tt.tokenRemoved, slices.Contains(h.ran, "pveum user token remove pco@pve pco"))
			require.Equal(t, tt.userDeleted, slices.Contains(h.ran, "pveum user delete pco@pve"))
		})
	}
}

func TestUninstallKeepsTheTagsAnAdminRegistered(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	h.tags = []string{"a", "cf-tunnel"}
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))
	require.Equal(t, []string{"a", "cf-tunnel", "cf-tunnel-managed"}, h.tags)
	require.Equal(t, []string{"cf-tunnel-managed"}, e.manifest().RegisteredTags)

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, RemoveCloudflared: true}))

	require.Equal(t, []string{"a", "cf-tunnel"}, h.tags)
}

func TestPurgeDeletesOnlyTheTunnelsOfTheInstall(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.atCloudflare()
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall+"-pve2", nil)
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall+"_probe_abc123", nil)
	e.cf.SeedTunnel(testAccount, "pco-"+testInstall+"_probe_ABC", nil)
	e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	require.Equal(t, []string{"pco-ba9876543210", "pco-" + testInstall + "-pve2", "pco-" + testInstall + "_probe_ABC"},
		e.tunnelNames(), "the tunnel of the install and its probe go; a tunnel of another name stays")
}

func TestUninstallRemovesOfCloudflaredOnlyWhatTheManifestLists(t *testing.T) {
	for _, tt := range []struct {
		name        string
		manifest    Manifest
		sourcesGo   bool
		packageGoes bool
	}{
		{"a source setup wrote, a package it did not install", Manifest{AddedAptSource: true}, true, false},
		{"a package setup installed, a source it did not write", Manifest{InstalledCloudflared: true}, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(tt.manifest)
			require.NoError(t, os.WriteFile(e.s.host.sources, sourcesContent(e.s.host.keyring), 0o644))
			script := [][]call{connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned()}
			if tt.packageGoes {
				script = append(script, []call{{line: "apt-get remove -y cloudflared"}})
			}
			e.script(script...)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true, RemoveCloudflared: true}))
			e.done()

			if tt.sourcesGo {
				require.NoFileExists(t, e.s.host.sources)
			} else {
				require.FileExists(t, e.s.host.sources, "a source setup did not write stays, whatever it holds")
			}
		})
	}
}

func TestTheStoreIsNotRemovedWhileItIsNotMounted(t *testing.T) {
	e := newTestEnv(t)
	marker := filepath.Join(e.base, "mounted")
	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	e.paths.MountCheck = marker
	e.reopen()
	e.installed(Manifest{})
	unmount := func(*testing.T, []string) { require.NoError(t, os.Remove(marker)) }
	e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsSeen(),
		[]call{{line: "systemctl disable --now -- pco-cloudflared@" + tunnelID + ".service", do: unmount}})

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true})

	// Uninstall looks itself before anything of the store goes; that the
	// store refuses its own writes then too is not what it rests on.
	require.ErrorIs(t, err, store.ErrNotMounted)
	require.ErrorContains(t, err, "removing the store")
	e.done()
	for _, dir := range []string{e.paths.Cluster, e.paths.Private, e.paths.Local} {
		require.DirExists(t, dir)
	}
}

func TestATokenOfAnotherNameIsNotQuoted(t *testing.T) {
	e := newTestEnv(t)
	e.script(preflightNew("9.0.10"), roleKept(), userKept(),
		[]call{
			{line: "pveum user token list pco@pve --output-format json", out: `[]`},
			{
				line: "pveum user token add pco@pve pco --privsep 0 --output-format json",
				out:  `{"full-tokenid":"pco@pve!` + pveSecret + `","value":"` + pveSecret + `"}`,
			},
		})

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "another token")
	e.done()
	e.requireNoSecret()
}

func TestTheHostRunnerGivesAFixedEnvironment(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env here")
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("SECRET_OF_THE_CALLER", "x")

	for _, name := range []string{"/usr/bin/env", "env"} {
		out, err := NewHostRunner().Run(context.Background(), name)

		require.NoError(t, err, name)
		require.Equal(t, commandEnv(), strings.Split(strings.TrimSpace(out), "\n"), name)
	}
}

func TestSetupLeavesTheAdminsSourceAndKey(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	const admins = "Types: deb\nURIs: https://mirror.example.com/cloudflared\nSuites: any\nComponents: main\n"
	require.NoError(t, os.WriteFile(e.s.host.sources, []byte(admins), 0o644))
	require.NoError(t, os.WriteFile(e.s.host.keyring, []byte("the admin's key"), 0o644))

	require.NoError(t, e.setup(Options{Yes: true, Node: testNode}))

	got, err := os.ReadFile(e.s.host.sources)
	require.NoError(t, err)
	require.Equal(t, admins, string(got))
	key, err := os.ReadFile(e.s.host.keyring)
	require.NoError(t, err)
	require.Equal(t, "the admin's key", string(key))
	require.False(t, slices.ContainsFunc(h.ran, func(c string) bool { return strings.HasPrefix(c, "curl ") }), "the key is not fetched again")
	m := e.manifest()
	require.False(t, m.AddedAptSource)
	require.False(t, m.AddedKeyring)
	e.requireShown("is not the apt source setup writes")
}

func TestSetupRefusesAnEmptyKey(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	e.s.run = emptyKey{h}

	err := e.setup(Options{Yes: true, Node: testNode})

	require.ErrorContains(t, err, "is empty")
	entries, err := os.ReadDir(filepath.Join(e.base, "keyrings"))
	require.NoError(t, err)
	require.Empty(t, entries, "neither the key nor its temporary file stays")
	require.NotContains(t, h.ran, "apt-get install -y cloudflared")
}

// emptyKey is a host whose download of the key gives an empty file.
type emptyKey struct{ *fakeHost }

func (k emptyKey) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := k.fakeHost.Run(ctx, name, args...)
	if name == "curl" {
		require.NoError(k.t, os.WriteFile(flag(args, "--output"), nil, 0o600))
	}
	return out, err
}

func TestAnNftThatFailsIsAFailure(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.script(connectorsSeen(), []call{{line: "nft list tables", err: exitErr(1, "Error: cache initialization failed: Operation not permitted")}},
		serviceStopped(), connectorsPruned())

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true})

	require.ErrorContains(t, err, "Operation not permitted")
	e.done()
	e.requireStoreKept()
}

func TestWhatIsGoneAtDeleteTimeIsNoFailure(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.atCloudflare()
	e.api = func(api cfapi.API) cfapi.API { return goneOnDelete{api} }
	e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()
	e.requireStoreGone()
}

// goneOnDelete is a Cloudflare where what is deleted was deleted already.
type goneOnDelete struct{ cfapi.API }

func (goneOnDelete) DeleteRecord(context.Context, string, string) error {
	return &cfapi.Error{Status: http.StatusNotFound, Message: "Record not found"}
}

func (goneOnDelete) DeleteTunnel(context.Context, string, string) error {
	return &cfapi.Error{Status: http.StatusNotFound, Message: "Tunnel not found"}
}

func TestSetupWorksOnTheStoreItIsGiven(t *testing.T) {
	e := newTestEnv(t)
	s := New(e.run, e.ask, e.st, nil, func() time.Time { return t0 }, nil)

	require.Equal(t, e.paths, s.paths())
	require.Equal(t, filepath.Join(e.paths.Local, manifestName), s.manifestPath())
}
