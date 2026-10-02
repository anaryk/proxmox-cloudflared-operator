package setup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// installed makes the node look set up by an earlier setup that wrote m: the
// store of testInstall with a credential and the Proxmox token, the unit of
// pco, and the files of the connector of the tunnel tunnelID.
func (e *testEnv) installed(m Manifest) {
	t := e.t
	t.Helper()
	require.NoError(t, e.st.Init())
	require.NoError(t, e.st.SaveInstall(store.Install{ID: testInstall, CreatedAt: t0, Profile: store.ProfileHost}))
	require.NoError(t, e.st.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 1, Nonce: "abcd1234"}))
	require.NoError(t, e.st.SaveNode(store.NodeEntry{Name: testNode, Since: t0}))
	require.NoError(t, e.st.SavePVEToken(store.PVEToken{TokenID: "pco@pve!pco", Secret: store.NewSecret(pveSecret)}))
	require.NoError(t, e.st.SaveCredential(store.Credential{
		ID: "c0ffee00", Label: "main", Kind: "scoped", Token: store.NewSecret(cfToken), AddedAt: t0,
	}))
	m.Node, m.InstalledAt = testNode, t0
	require.NoError(t, writeManifest(filepath.Join(e.paths.Local, manifestName), m))
	e.installUnit("pco.service")

	tunnels := filepath.Join(e.paths.Local, "tunnels")
	require.NoError(t, os.MkdirAll(tunnels, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(tunnels, tunnelID+".token"), []byte("run token"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tunnels, tunnelID+".env"), []byte("METRICS_ADDR=127.0.0.1:20300\n"), 0o644))
}

// atCloudflare puts what install testInstall made at Cloudflare there, and
// what others made: the tunnel of the install (tunnelID), the tunnel of
// another install, an owned record and two that are not.
func (e *testEnv) atCloudflare() {
	e.t.Helper()
	ours := e.cf.SeedTunnel(testAccount, "pco-"+testInstall, sentinel(testInstall, 1))
	require.Equal(e.t, tunnelID, ours.ID)
	e.cf.SeedTunnel(testAccount, "pco-ba9876543210", nil)
	e.cf.SeedRecord(testZone, cfapi.Record{ID: "rec-ours", Type: "CNAME", Name: "www.example.com",
		Content: tunnelID + ".cfargotunnel.com", Proxied: true, Comment: "pco:" + testInstall})
	e.cf.SeedRecord(testZone, cfapi.Record{ID: "rec-other-install", Type: "CNAME", Name: "app.example.com",
		Content: "x.cfargotunnel.com", Proxied: true, Comment: "pco:ba9876543210"})
	e.cf.SeedRecord(testZone, cfapi.Record{ID: "rec-by-hand", Type: "A", Name: "mail.example.com",
		Content: "192.0.2.1", Comment: "pco:" + testInstall + "x by hand"})
}

func (e *testEnv) recordIDs() []string {
	var ids []string
	for _, r := range e.cf.RecordsIn(testZone) {
		ids = append(ids, r.ID)
	}
	return ids
}

func (e *testEnv) tunnelNames() []string {
	var names []string
	for _, tun := range e.cf.TunnelsIn(testAccount) {
		names = append(names, tun.Name)
	}
	return names
}

func (e *testEnv) requireStoreGone() {
	e.t.Helper()
	for _, dir := range []string{e.paths.Cluster, e.paths.Private, e.paths.Local} {
		require.NoDirExists(e.t, dir)
	}
}

func (e *testEnv) requireStoreKept() {
	e.t.Helper()
	require.Equal(e.t, testInstall, e.install().ID)
	require.Len(e.t, e.credentials(), 1)
	e.manifest()
}

func serviceStopped() []call { return []call{{line: "systemctl disable --now pco.service"}} }

func connectorsPruned() []call {
	return []call{
		{
			line: "systemctl list-units --all --plain --no-legend -- pco-cloudflared@*.service",
			out:  "pco-cloudflared@" + tunnelID + ".service loaded active running pco cloudflared connector\n",
		},
		{line: "systemctl disable --now -- pco-cloudflared@" + tunnelID + ".service"},
	}
}

func noConnectors() []call {
	return []call{{line: "systemctl list-units --all --plain --no-legend -- pco-cloudflared@*.service"}}
}

func egressRemoved() []call {
	return []call{
		{line: "nft list tables", out: "table inet filter\ntable inet pco_egress\n"},
		{line: "nft delete table inet pco_egress"},
	}
}

func noEgress() []call { return []call{{line: "nft list tables", out: "table inet filter\n"}} }

// userRemoved removes the token and the user setup created, and the tags.
func userRemoved() []call {
	return []call{
		{line: "pveum user list --output-format json", out: usersWith},
		{line: "pveum user token list pco@pve --output-format json", out: tokensWith},
		{line: "pveum user token remove pco@pve pco"},
		{line: "pveum user delete pco@pve"},
		{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"a;cf-tunnel;cf-tunnel-managed"}`},
		{line: "pvesh set /cluster/options --registered-tags a"},
	}
}

var setupsUser = Manifest{CreatedUser: true, CreatedToken: true, RegisteredTags: []string{"cf-tunnel", "cf-tunnel-managed"}}

func TestUninstallRemovesOnlyManifestItems(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.script(serviceStopped(), connectorsPruned(), egressRemoved(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	// The role was there before setup: it is kept, and not even looked at.
	require.NotContains(t, strings.Join(e.run.ran, "\n"), "role")
	e.requireShown("role PCO")
	require.Equal(t, []string{"rec-other-install", "rec-by-hand"}, e.recordIDs())
	require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
	require.NoFileExists(t, filepath.Join(e.paths.Local, "tunnels", tunnelID+".token"))
	e.requireStoreGone()
	e.requireNoSecret()
}

func TestUninstallOrder(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.script(serviceStopped(), connectorsPruned(), egressRemoved(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))

	at := func(prefix string) int {
		t.Helper()
		i := slices.IndexFunc(e.events, func(ev string) bool { return strings.HasPrefix(ev, prefix) })
		require.GreaterOrEqual(t, i, 0, "%s happened", prefix)
		return i
	}
	order := []int{
		at("run systemctl disable --now pco.service"),
		at("cf DeleteRecord " + testZone + " rec-ours"),
		at("run systemctl list-units"),
		at("run systemctl disable --now -- pco-cloudflared@"),
		at("cf DeleteTunnel " + testAccount + " " + tunnelID),
		at("run nft"),
		at("run pveum"),
	}
	require.IsIncreasing(t, order, "the daemon, the records, the connectors, the tunnel, the filter, then Proxmox")

	// What is deleted at Cloudflare is listed before anything is deleted.
	listed := slices.IndexFunc(e.ask.lines, func(l string) bool { return strings.Contains(l, "www.example.com") })
	require.GreaterOrEqual(t, listed, 0)
	require.Contains(t, e.ask.text(), "pco-"+testInstall)
}

func TestUninstallAsksEachQuestion(t *testing.T) {
	withCloudflared := Manifest{InstalledCloudflared: true, AddedAptSource: true, AddedKeyring: true}
	for _, tt := range []struct {
		name          string
		options       UninstallOptions
		answers       []answer
		purged, apt   bool
		cloudflaredGo bool
	}{
		{
			name:    "all asked, only the main question answered yes",
			answers: []answer{{"Remove pco", true}, {"Cloudflare", false}, {"cloudflared", false}},
		},
		{
			name:    "--yes answers only the main question",
			options: UninstallOptions{Yes: true},
			answers: []answer{{"Cloudflare", true}, {"cloudflared", true}},
			purged:  true, cloudflaredGo: true,
		},
		{
			name:    "the flags answer the others",
			options: UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true},
			purged:  true, cloudflaredGo: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(withCloudflared)
			e.atCloudflare()
			for _, path := range []string{e.s.host.keyring, e.s.host.sources} {
				content := gpgKey
				if path == e.s.host.sources {
					content = string(sourcesContent(e.s.host.keyring))
				}
				require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			}
			e.ask.answers = tt.answers
			script := [][]call{serviceStopped(), connectorsPruned(), egressRemoved()}
			if tt.cloudflaredGo {
				script = append(script, []call{{line: "apt-get remove -y cloudflared"}})
			}
			e.script(script...)

			require.NoError(t, e.uninstall(tt.options))
			e.done()
			require.Empty(t, e.ask.answers, "every question was asked")

			if tt.purged {
				require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
			} else {
				require.Len(t, e.tunnelNames(), 2, "nothing is deleted at Cloudflare")
				require.Len(t, e.recordIDs(), 3)
			}
			if tt.cloudflaredGo {
				require.NoFileExists(t, e.s.host.sources)
				require.NoFileExists(t, e.s.host.keyring)
			} else {
				require.FileExists(t, e.s.host.sources)
				require.FileExists(t, e.s.host.keyring)
			}
			e.requireStoreGone()
		})
	}
}

func TestUninstallThatIsNotConfirmedChangesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.ask.answers = []answer{{"Remove pco", false}}

	require.ErrorIs(t, e.uninstall(UninstallOptions{PurgeCloudflare: true}), ErrAborted)

	require.Empty(t, e.run.ran)
	require.Len(t, e.tunnelNames(), 2)
	require.Len(t, e.recordIDs(), 3)
	for _, call := range e.cf.Calls() {
		require.NotRegexp(t, "^(Delete|Create|Update|Put)", call, "Cloudflare is only read")
	}
	e.requireStoreKept()
	// The question says what would go, at Cloudflare too.
	for _, want := range []string{
		"pco@pve", "cf-tunnel-managed", e.paths.Cluster, e.paths.Private, e.paths.Local,
		"www.example.com", "pco-" + testInstall,
	} {
		e.requireShown(want)
	}
	require.NotContains(t, e.ask.text(), "app.example.com", "a record of another install is not listed")
}

func TestUninstallRefusesWithoutRoot(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.s.host.euid = func() int { return 1000 }

	require.ErrorContains(t, e.uninstall(UninstallOptions{Yes: true}), "root")
	require.Empty(t, e.run.ran)
	e.requireStoreKept()
}

func TestASecondUninstallFinishesTheRest(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	// Cloudflare still sees the connector when the tunnel is deleted.
	e.cf.SetConnectors(testAccount, tunnelID, []cfapi.Connector{{ID: "conn-1", Connections: 4}})
	e.script(serviceStopped(), connectorsPruned(), egressRemoved(), userRemoved())

	err := e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true})

	require.ErrorContains(t, err, "pco uninstall again")
	e.done()
	e.requireShown(tunnelID)
	require.Equal(t, []string{"rec-other-install", "rec-by-hand"}, e.recordIDs(), "the rest went on")
	e.requireStoreKept()

	e.cf.SetConnectors(testAccount, tunnelID, nil)
	e.script(serviceStopped(), noConnectors(), noEgress(),
		[]call{
			{line: "pveum user list --output-format json", out: usersWithout},
			{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"a"}`},
		})

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()
	require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
	e.requireStoreGone()
	e.requireNoSecret()
}

func TestAFailedRemovalKeepsTheStore(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.script(serviceStopped(), connectorsPruned(), noEgress(),
		[]call{
			{line: "pveum user list --output-format json", out: usersWith},
			{line: "pveum user token list pco@pve --output-format json", out: tokensWith},
			{line: "pveum user token remove pco@pve pco", err: exitErr(255, "cluster not ready - no quorum?")},
			{line: "pveum user delete pco@pve"},
			{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"cf-tunnel;cf-tunnel-managed"}`},
			{line: "pvesh set /cluster/options --delete registered-tags"},
		})

	err := e.uninstall(UninstallOptions{Yes: true})

	require.ErrorContains(t, err, "pco uninstall again")
	e.done()
	e.requireShown("no quorum")
	e.requireStoreKept()
}

func TestUninstallRemovesTheRoleOnlyWhenUnchanged(t *testing.T) {
	for _, tt := range []struct {
		name    string
		privs   string
		deleted bool
	}{
		{"as setup made it on 9", privs9, true},
		{"as setup made it on 8", privs8, true},
		{"with a privilege an admin added", privs9 + ",Datastore.Audit", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{CreatedRole: true})
			script := [][]call{serviceStopped(), connectorsPruned(), noEgress(), {roleWith(tt.privs)}}
			if tt.deleted {
				script = append(script, []call{{line: "pveum role delete PCO"}})
			}
			e.script(script...)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
			e.done()
			if !tt.deleted {
				e.requireShown("Datastore.Audit")
			}
		})
	}
}

func TestUninstallRemovesTheEgressFilter(t *testing.T) {
	for _, tt := range []struct {
		name   string
		unit   bool
		egress []call
	}{
		{"without nft", false, []call{{line: "nft list tables", err: notFound("nft")}}},
		{"without the table", false, noEgress()},
		{"with the table", false, egressRemoved()},
		{"with the unit", true, append([]call{{line: "systemctl disable --now pco-egress.service"}}, egressRemoved()...)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{})
			if tt.unit {
				e.installUnit("pco-egress.service")
			}
			e.script(serviceStopped(), connectorsPruned(), tt.egress)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true}))
			e.done()
			e.requireStoreGone()
		})
	}
}

func TestUninstallSkipsWhatACredentialMayNotList(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	// Cloudflare refuses to show the token the tunnels: it manages none.
	e.cf.Deny("tunnel.read")
	e.script(serviceStopped(), connectorsPruned(), egressRemoved(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	e.requireShown("may not list the tunnels of account " + testAccount)
	require.Equal(t, []string{"rec-other-install", "rec-by-hand"}, e.recordIDs())
	require.Len(t, e.tunnelNames(), 2)
	e.requireStoreGone()
}

func TestUninstallKeepsTheStoreWhenCloudflareCannotBeListed(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.cf.FailNext("zones", 1, errors.New("connection reset by peer"))
	e.script(serviceStopped(), connectorsPruned(), egressRemoved(), userRemoved())

	err := e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true})

	require.ErrorContains(t, err, "connection reset by peer")
	require.ErrorContains(t, err, "pco uninstall again")
	e.done()
	e.requireStoreKept()
}
