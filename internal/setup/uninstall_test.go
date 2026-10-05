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

// requireNoDelete fails when anything was deleted, created or changed at
// Cloudflare.
func (e *testEnv) requireNoDelete() {
	e.t.Helper()
	for _, call := range e.cf.Calls() {
		require.NotRegexp(e.t, "^(Delete|Create|Update|Put)", call, "Cloudflare is only read")
	}
}

// The reads of what uninstall looks at before it asks.

const listConnectors = "systemctl list-units --all --plain --no-legend -- pco-cloudflared@*.service"

func connectorsSeen() []call {
	return []call{{line: listConnectors, out: "pco-cloudflared@" + tunnelID + ".service loaded active running pco cloudflared connector\n"}}
}

func noConnectorsSeen() []call { return []call{{line: listConnectors}} }

func egressSeen() []call {
	return []call{{line: "nft list tables", out: "table inet filter\ntable inet pco_egress\n"}}
}

func noEgressSeen() []call { return []call{{line: "nft list tables", out: "table inet filter\n"}} }

// userRead reads the user, the token and the tags setup created.
func userRead() []call {
	return []call{
		{line: "pveum user list --output-format json", out: usersWith},
		{line: "pveum user token list pco@pve --output-format json", out: tokensWith},
		{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"a;cf-tunnel;cf-tunnel-managed"}`},
	}
}

// The removals.

func serviceStopped() []call { return []call{{line: "systemctl disable --now pco.service"}} }

func connectorsPruned() []call {
	return append(connectorsSeen(), call{line: "systemctl disable --now -- pco-cloudflared@" + tunnelID + ".service"})
}

func egressDeleted() []call { return []call{{line: "nft delete table inet pco_egress"}} }

// userRemoved removes the token and the user setup created, and the tags,
// as userRead found them.
func userRemoved() []call {
	return []call{
		{line: "pveum user token remove pco@pve pco"},
		{line: "pveum user delete pco@pve"},
		{line: "pvesh set /cluster/options --registered-tags a"},
	}
}

// isRead reports whether a command only reads.
func isRead(command string) bool {
	return strings.HasPrefix(command, listConnectors) || command == "nft list tables" ||
		strings.HasPrefix(command, "pveum ") && strings.Contains(command, " list ") || strings.HasPrefix(command, "pvesh get ")
}

var setupsUser = Manifest{CreatedUser: true, CreatedToken: true, RegisteredTags: []string{"cf-tunnel", "cf-tunnel-managed"}}

func TestUninstallRemovesOnlyManifestItems(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	// The role was there before setup: it is kept, and not even looked at.
	require.NotContains(t, strings.Join(e.run.ran, "\n"), "role")
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
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	at := func(prefix string) int {
		t.Helper()
		i := slices.IndexFunc(e.events, func(ev string) bool { return strings.HasPrefix(ev, prefix) })
		require.GreaterOrEqual(t, i, 0, "%s happened", prefix)
		return i
	}
	stop := at("run systemctl disable --now pco.service")
	require.True(t, slices.ContainsFunc(e.events[:stop], func(ev string) bool { return strings.HasPrefix(ev, "run pvesh get") }),
		"what is there is read before anything goes")
	for _, ev := range e.events[stop:] {
		if command, ok := strings.CutPrefix(ev, "run "); ok && command != listConnectors {
			require.False(t, isRead(command), "nothing is read again after the stop: %s", command)
		}
	}
	order := []int{
		stop,
		at("cf DeleteRecord " + testZone + " rec-ours"),
		at("run systemctl disable --now -- pco-cloudflared@"),
		at("cf DeleteTunnel " + testAccount + " " + tunnelID),
		at("run nft delete"),
		at("run pveum user token remove"),
	}
	require.IsIncreasing(t, order, "the daemon, the records, the connectors, the tunnel, the filter, then Proxmox")
	// What is deleted at Cloudflare is listed before anything is deleted.
	e.requireShown("www.example.com")
	e.requireShown("pco-" + testInstall)
}

func TestUninstallAsksEachQuestion(t *testing.T) {
	withCloudflared := Manifest{InstalledCloudflared: true, AddedAptSource: true, AddedKeyring: true}
	for _, tt := range []struct {
		name          string
		options       UninstallOptions
		answers       []answer
		purged        bool
		cloudflaredGo bool
	}{
		{
			name:    "all asked, only the main question answered yes",
			answers: []answer{{"Remove pco", true}, {"Cloudflare", false}, {"cloudflared", false}},
		},
		{
			name:    "all asked and answered yes",
			answers: []answer{{"Remove pco", true}, {"Cloudflare", true}, {"cloudflared", true}},
			purged:  true, cloudflaredGo: true,
		},
		{
			name:    "--yes answers only the main question, and cloudflared with no",
			options: UninstallOptions{Yes: true, KeepCloudflare: true},
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
			require.NoError(t, os.WriteFile(e.s.host.keyring, []byte(gpgKey), 0o644))
			require.NoError(t, os.WriteFile(e.s.host.sources, sourcesContent(e.s.host.keyring), 0o644))
			e.ask.answers = tt.answers
			script := [][]call{connectorsSeen(), egressSeen(), serviceStopped(), connectorsPruned(), egressDeleted()}
			if tt.cloudflaredGo {
				script = append(script, []call{{line: "apt-get remove -y cloudflared"}})
			}
			e.script(script...)

			require.NoError(t, e.uninstall(tt.options))
			e.done()

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
	e.script(connectorsSeen(), egressSeen(), userRead())

	require.ErrorIs(t, e.uninstall(UninstallOptions{PurgeCloudflare: true}), ErrAborted)
	e.done()

	for _, command := range e.run.ran {
		require.True(t, isRead(command), "only reads before the question: %s", command)
	}
	require.Len(t, e.tunnelNames(), 2)
	require.Len(t, e.recordIDs(), 3)
	e.requireNoDelete()
	e.requireStoreKept()
	// The question says what would go, at Cloudflare too.
	for _, want := range []string{
		"pco@pve", "cf-tunnel-managed", e.paths.Cluster, e.paths.Private, e.paths.Local,
		"www.example.com", "pco-" + testInstall, "connectors of 1 tunnels", "table inet pco_egress",
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
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	err := e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true})

	require.ErrorContains(t, err, "pco uninstall again")
	e.done()
	e.requireShown(tunnelID)
	require.Equal(t, []string{"rec-other-install", "rec-by-hand"}, e.recordIDs(), "the rest went on")
	e.requireStoreKept()

	e.cf.SetConnectors(testAccount, tunnelID, nil)
	e.script(noConnectorsSeen(), noEgressSeen(),
		[]call{
			{line: "pveum user list --output-format json", out: usersWithout},
			{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"a"}`},
		},
		serviceStopped(), noConnectorsSeen())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()
	require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
	e.requireStoreGone()
	e.requireNoSecret()
}

func TestAFailedRemovalKeepsTheStore(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.script(connectorsSeen(), noEgressSeen(),
		[]call{
			{line: "pveum user list --output-format json", out: usersWith},
			{line: "pveum user token list pco@pve --output-format json", out: tokensWith},
			{line: "pvesh get /cluster/options --output-format json", out: `{"registered-tags":"cf-tunnel;cf-tunnel-managed"}`},
		},
		serviceStopped(), connectorsPruned(),
		[]call{
			{line: "pveum user token remove pco@pve pco", err: exitErr(255, "cluster not ready - no quorum?")},
			{line: "pveum user delete pco@pve"},
			{line: "pvesh set /cluster/options --delete registered-tags"},
		})

	err := e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true})

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
		says    string
	}{
		{"as setup made it on 9", privs9, true, ""},
		{"as setup made it on 8", privs8, true, ""},
		{"with a privilege an admin added", privs9 + ",Datastore.Audit", false, "it also grants Datastore.Audit"},
		{"with a privilege an admin took away", "VM.Audit,Sys.Audit,SDN.Audit", false, "it no longer grants VM.GuestAgent.Audit"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{CreatedRole: true})
			script := [][]call{connectorsSeen(), noEgressSeen(),
				{roleWith(tt.privs), {line: "pveum acl list --output-format json", out: `[]`}},
				serviceStopped(), connectorsPruned()}
			if tt.deleted {
				script = append(script, []call{{line: "pveum role delete PCO"}})
			}
			e.script(script...)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
			e.done()
			if !tt.deleted {
				e.requireShown("role PCO is kept: " + tt.says)
			}
		})
	}
}

func TestUninstallRemovesTheEgressFilter(t *testing.T) {
	for _, tt := range []struct {
		name         string
		unit         bool
		seen, remove []call
	}{
		{"without nft", false, []call{{line: "nft list tables", err: notFound("nft")}}, nil},
		{"without the table", false, noEgressSeen(), nil},
		{"with the table", false, egressSeen(), egressDeleted()},
		{"with the unit", true, egressSeen(), append([]call{
			{line: "systemctl disable --now pco-egress.service"},
			{line: "nft list tables", out: "table inet pco_egress\n"},
		}, egressDeleted()...)},
		{"with a unit that takes its table", true, egressSeen(), []call{
			{line: "systemctl disable --now pco-egress.service"},
			{line: "nft list tables"},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{})
			if tt.unit {
				e.installUnit("pco-egress.service")
			}
			e.script(connectorsSeen(), tt.seen, serviceStopped(), connectorsPruned(), tt.remove)

			require.NoError(t, e.uninstall(UninstallOptions{Yes: true, KeepCloudflare: true}))
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
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

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
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	err := e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true})

	require.ErrorContains(t, err, "connection reset by peer")
	require.ErrorContains(t, err, "pco uninstall again")
	e.done()
	e.requireStoreKept()
}

// The reviewer's sequence: on a terminal and without a flag, the listing
// fails once.
func TestAFailedListingKeepsWhatIsNeededToPurge(t *testing.T) {
	for _, tt := range []struct {
		name    string
		second  UninstallOptions
		answers []answer
		purged  bool
	}{
		{"purged on the second run", UninstallOptions{}, []answer{{"Remove pco", true}, {"Cloudflare", true}}, true},
		{"purged with the flag", UninstallOptions{Yes: true, PurgeCloudflare: true}, nil, true},
		{"left with --keep-cloudflare", UninstallOptions{Yes: true, KeepCloudflare: true}, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{})
			e.atCloudflare()
			e.cf.FailNext("zones", 1, errors.New("connection reset by peer"))
			e.ask.answers = []answer{{"Remove pco", true}}
			e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned())

			err := e.uninstall(UninstallOptions{})

			require.ErrorContains(t, err, "connection reset by peer")
			require.ErrorContains(t, err, "pco uninstall --purge-cloudflare again")
			require.ErrorContains(t, err, "pco uninstall --keep-cloudflare")
			e.done()
			e.requireNoDelete()
			e.requireStoreKept()

			calls := len(e.cf.Calls())
			e.ask.answers = tt.answers
			e.script(noConnectorsSeen(), noEgressSeen(), serviceStopped(), noConnectorsSeen())
			require.NoError(t, e.uninstall(tt.second))
			e.done()
			if tt.purged {
				require.Equal(t, []string{"pco-ba9876543210"}, e.tunnelNames())
			} else {
				require.Len(t, e.tunnelNames(), 2)
				require.Len(t, e.cf.Calls(), calls, "--keep-cloudflare does not even look")
			}
			e.requireStoreGone()
		})
	}
}

func TestAFailedDaemonStopRemovesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.script(connectorsSeen(), egressSeen(), userRead(),
		[]call{{line: "systemctl disable --now pco.service", err: exitErr(1, "Job for pco.service canceled.")}})

	err := e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true})

	require.ErrorContains(t, err, "Job for pco.service canceled.")
	require.ErrorContains(t, err, "nothing was removed")
	e.done()
	e.requireNoDelete()
	require.FileExists(t, filepath.Join(e.paths.Local, "tunnels", tunnelID+".token"))
	e.requireStoreKept()
}

func TestUninstallDoesNotLookAtCloudflareForAFixedAnswer(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options UninstallOptions
		answers []answer
	}{
		{"--yes --keep-cloudflare", UninstallOptions{Yes: true, KeepCloudflare: true}, nil},
		{"--keep-cloudflare", UninstallOptions{KeepCloudflare: true}, []answer{{"Remove pco", true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.installed(Manifest{})
			e.atCloudflare()
			e.ask.answers = tt.answers
			e.script(connectorsSeen(), noEgressSeen(), serviceStopped(), connectorsPruned())

			require.NoError(t, e.uninstall(tt.options))
			e.done()

			require.Empty(t, e.cf.Calls())
			e.requireShown("nothing on this node can remove it later")
			e.requireStoreGone()
		})
	}
}

func TestUninstallRefusesPurgeAndKeep(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})

	require.ErrorContains(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, KeepCloudflare: true}), "do not go together")
	require.Empty(t, e.run.ran)
}

func TestASecondUninstallListsOnlyWhatIsThere(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit(serviceUnit)
	h := newFakeHost(t)
	e.onHost(h)
	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}))
	first := len(e.ask.lines)

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true, RemoveCloudflared: true}))

	second := strings.Join(e.ask.lines[first:], "\n")
	for _, gone := range []string{"Proxmox", "role", "user", "registered tags", "the store", "connectors of", "credentials"} {
		require.NotContains(t, second, gone)
	}
	e.requireNothingLeft(h)
}

// A token scoped to one zone may not read the records of the other zones of
// the account. Those this install never served hold none of its records, and
// the purge says nothing of them; one it served it says it could not read.
func TestThePurgeSaysNothingOfAZoneTheInstallNeverServed(t *testing.T) {
	e := newTestEnv(t)
	e.installed(setupsUser)
	e.atCloudflare()
	e.cf.AddZone("zone2", "example.org", testAccount)
	e.cf.AddZone("zone3", "example.net", testAccount)
	e.cf.Deny("dns.read", "zone2", "zone3")
	require.NoError(t, e.st.SaveEngineMemory(store.EngineMemory{InstallID: testInstall, Served: []store.RememberedZone{
		{ID: "zone3", Name: "example.net", AccountID: testAccount, CredentialID: "c0ffee00"},
	}}))
	e.script(connectorsSeen(), egressSeen(), userRead(), serviceStopped(), connectorsPruned(), egressDeleted(), userRemoved())

	require.NoError(t, e.uninstall(UninstallOptions{Yes: true, PurgeCloudflare: true}))
	e.done()

	require.NotContains(t, e.ask.text(), "example.org")
	e.requireShown("credential c0ffee00 may not list the records of zone example.net; what is there is not deleted")
}
