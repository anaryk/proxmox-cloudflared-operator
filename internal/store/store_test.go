package store

import (
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func testPaths(t *testing.T) Paths {
	t.Helper()
	base := t.TempDir()
	return Paths{
		Cluster: filepath.Join(base, "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "local"),
	}
}

func openStore(t *testing.T) (*Store, Paths) {
	t.Helper()
	p := testPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	return s, p
}

// stored lists the files below dir, by path relative to it.
func stored(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(dir, path)
			require.NoError(t, err)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	require.NoError(t, err)
	return out
}

func TestDefaultPaths(t *testing.T) {
	require.Equal(t, Paths{
		Cluster:    "/etc/pve/pco",
		Private:    "/etc/pve/priv/pco",
		Local:      "/var/lib/pco",
		MountCheck: "/etc/pve/.version",
	}, DefaultPaths())
}

func TestTheNodeLockIsBelowTheLocalRoot(t *testing.T) {
	require.Equal(t, "/var/lib/pco/daemon.lock", DefaultPaths().NodeLock())
	require.Equal(t, "/srv/local/daemon.lock", Paths{Local: "/srv/local"}.NodeLock())
}

func TestTheStoreTellsItsPaths(t *testing.T) {
	p := testPaths(t)
	p.MountCheck = filepath.Join(t.TempDir(), "mounted")
	s, err := Open(p)
	require.NoError(t, err)

	require.Equal(t, p, s.Paths())
}

func TestOpenRefusesAnEmptyPath(t *testing.T) {
	good := testPaths(t)
	for name, p := range map[string]Paths{
		"cluster": {Private: good.Private, Local: good.Local},
		"private": {Cluster: good.Cluster, Local: good.Local},
		"local":   {Cluster: good.Cluster, Private: good.Private},
	} {
		_, err := Open(p)
		require.Error(t, err, name)
		require.Contains(t, err.Error(), name)
	}
}

func TestOpenRefusesALocalRootThatIsAFile(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p.Local, "x")
	_, err := Open(p)
	require.Error(t, err)
}

func TestOpenRefusesALocalRootThatIsNotWritable(t *testing.T) {
	skipAsRoot(t)
	p := testPaths(t)
	require.NoError(t, os.MkdirAll(p.Local, 0o700))
	require.NoError(t, os.Chmod(p.Local, 0o500))
	t.Cleanup(func() { _ = os.Chmod(p.Local, 0o700) })

	_, err := Open(p)
	require.ErrorIs(t, err, fs.ErrPermission)
	require.Contains(t, err.Error(), p.Local)
}

func TestOpenExistingMakesNothing(t *testing.T) {
	p := testPaths(t)

	_, err := OpenExisting(p)
	require.ErrorIs(t, err, ErrNoRoot)
	require.Contains(t, err.Error(), p.Local)
	requireMissing(t, p.Local)
	requireMissing(t, p.Cluster)
	requireMissing(t, p.Private)
}

func TestOpenExistingRefusesAnEmptyPathAndALocalRootThatIsAFile(t *testing.T) {
	good := testPaths(t)
	_, err := OpenExisting(Paths{Cluster: good.Cluster, Private: good.Private})
	require.ErrorContains(t, err, "local")

	writeFile(t, good.Local, "x")
	_, err = OpenExisting(good)
	require.ErrorContains(t, err, "not a directory")
}

// A command that looks leaves the roots as they are: no probe, and the
// leftovers of a crashed write stay for the daemon's own Open to remove.
func TestOpenExistingLeavesTheRootsAsTheyAre(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0}))
	old := time.Now().Add(-time.Hour)
	var leftovers []string
	for _, path := range []string{
		filepath.Join(p.Local, "bindings", ".a.json.1.tmp"),
		filepath.Join(p.Cluster, "claims", ".b.json.1.tmp"),
	} {
		writeFile(t, path, "x")
		require.NoError(t, os.Chtimes(path, old, old))
		leftovers = append(leftovers, path)
	}
	local, err := os.Stat(p.Local)
	require.NoError(t, err)

	got, err := OpenExisting(p)
	require.NoError(t, err)
	install, found, err := got.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "0123456789ab", install.ID)

	for _, path := range leftovers {
		_, err := os.Stat(path)
		require.NoError(t, err, path)
	}
	after, err := os.Stat(p.Local)
	require.NoError(t, err)
	require.Equal(t, local.ModTime(), after.ModTime(), "nothing was made in the local root, not even a probe that was removed again")
}

func TestOpenExistingWorksOnALocalRootThatIsNotWritable(t *testing.T) {
	skipAsRoot(t)
	s, p := openStore(t)
	require.NoError(t, s.SaveInstall(Install{ID: "0123456789ab", CreatedAt: t0}))
	require.NoError(t, os.Chmod(p.Local, 0o500))
	t.Cleanup(func() { _ = os.Chmod(p.Local, 0o700) })

	got, err := OpenExisting(p)
	require.NoError(t, err)
	_, found, err := got.Install()
	require.NoError(t, err)
	require.True(t, found)
}

func TestInstallRoundTrip(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.Install()
	require.NoError(t, err)
	require.False(t, found)

	want := Install{ID: "0123456789ab", CreatedAt: t0}
	require.NoError(t, s.SaveInstall(want))
	got, found, err := s.Install()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)
	require.Equal(t, []string{"meta/install.json"}, stored(t, p.Cluster))

	require.NoError(t, s.SaveInstall(want))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Cluster, "meta", "install.json")), "an unchanged install is not written again")
}

func TestSaveInstallRefusesAnEmptyID(t *testing.T) {
	s, p := openStore(t)
	require.Error(t, s.SaveInstall(Install{CreatedAt: t0}))
	require.Empty(t, stored(t, p.Cluster))
}

func TestInstallThatCannotBeReadIsAnError(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "meta", "install.json"), "{")
	_, found, err := s.Install()
	require.Error(t, err)
	require.False(t, found)
}

func TestWriterRoundTrip(t *testing.T) {
	s, p := openStore(t)
	_, found, err := s.Writer()
	require.NoError(t, err)
	require.False(t, found)

	want := planner.Writer{InstallID: "abc123", Generation: 3, Nonce: "n0nce"}
	require.NoError(t, s.SaveWriter(want))
	got, found, err := s.Writer()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)
	require.Equal(t, []string{"meta/leader.json"}, stored(t, p.Cluster))
	raw, err := os.ReadFile(filepath.Join(p.Cluster, "meta", "leader.json"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "incarnation", "a host writer has none")
}

func TestAClaimKeepsTheMACItWasPinnedTo(t *testing.T) {
	s, _ := openStore(t)
	pinned := claimOf("a.example.com", "qemu/101")
	pinned.MAC = "bc:24:11:00:aa:b5"
	want := map[string]planner.Claim{"a.example.com": pinned, "b.example.com": claimOf("b.example.com", "qemu/102")}

	require.NoError(t, s.SaveClaims(want))

	got, err := s.Claims()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestWriterKeepsItsIncarnation(t *testing.T) {
	s, _ := openStore(t)
	want := planner.Writer{InstallID: "abc123", Generation: 3, Nonce: "n0nce", Incarnation: "5b0d7a2e-31c4-4f6e-9d43-0c1f2a3b4c5d/123456"}

	require.NoError(t, s.SaveWriter(want))

	got, found, err := s.Writer()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got)
}

func TestWriterIsReadFromDiskEveryTime(t *testing.T) {
	s, p := openStore(t)
	other, err := Open(p) // a second process sharing the files
	require.NoError(t, err)

	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 1, Nonce: "aa"}))
	got, _, err := s.Writer()
	require.NoError(t, err)
	require.Equal(t, 1, got.Generation)

	require.NoError(t, other.SaveWriter(planner.Writer{InstallID: "abc", Generation: 2, Nonce: "bb"}))
	got, found, err := s.Writer()
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, planner.Writer{InstallID: "abc", Generation: 2, Nonce: "bb"}, got)

	require.NoError(t, os.Remove(filepath.Join(p.Cluster, "meta", "leader.json")))
	_, found, err = s.Writer()
	require.NoError(t, err)
	require.False(t, found, "a removed file is missing, not stale")
}

func TestWriterThatCannotBeReadIsNotMissing(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "meta", "leader.json")

	writeFile(t, path, `{"schemaVersion":1,"rev":1,"id":"leader","data":{"generation":`)
	_, found, err := s.Writer()
	require.Error(t, err)
	require.False(t, found)

	writeFile(t, path, `{"schemaVersion":9,"rev":1,"id":"leader","data":{}}`)
	_, found, err = s.Writer()
	require.Error(t, err)
	require.False(t, found)

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	_, found, err = s.Writer()
	require.Error(t, err, "a directory in place of the file is unreadable, not missing")
	require.False(t, found)
}

func TestSaveWriterRefusesAnInvalidWriter(t *testing.T) {
	s, p := openStore(t)
	for _, w := range []planner.Writer{
		{InstallID: "", Generation: 1, Nonce: "aa"},
		{InstallID: "abc", Generation: 1, Nonce: ""},
		{InstallID: "ABC", Generation: 1, Nonce: "aa"},
		{InstallID: "abc", Generation: -1, Nonce: "aa"},
	} {
		require.Error(t, s.SaveWriter(w), "%+v", w)
	}
	require.Empty(t, stored(t, p.Cluster))
}

func TestSaveWriterOfTheSameWriterWritesNothing(t *testing.T) {
	s, p := openStore(t)
	w := planner.Writer{InstallID: "abc", Generation: 1, Nonce: "aa"}
	require.NoError(t, s.SaveWriter(w))
	require.NoError(t, s.SaveWriter(w))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Cluster, "meta", "leader.json")))
	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 2, Nonce: "aa"}))
	require.EqualValues(t, 2, revOf(t, filepath.Join(p.Cluster, "meta", "leader.json")))
}

func TestNodes(t *testing.T) {
	s, p := openStore(t)
	nodes, err := s.Nodes()
	require.NoError(t, err)
	require.NotNil(t, nodes)
	require.Empty(t, nodes)

	pve2 := NodeEntry{Name: "pve2", Version: "0.1.0", Since: t0}
	pve1 := NodeEntry{Name: "pve1", Version: "0.1.0", Since: t0.Add(time.Hour)}
	require.NoError(t, s.SaveNode(pve2))
	require.NoError(t, s.SaveNode(pve1))
	nodes, err = s.Nodes()
	require.NoError(t, err)
	require.Equal(t, []NodeEntry{pve1, pve2}, nodes)
	require.Equal(t, []string{"nodes/pve1.json", "nodes/pve2.json"}, stored(t, p.Cluster))

	pve1.Version = "0.2.0"
	require.NoError(t, s.SaveNode(pve1))
	require.EqualValues(t, 2, revOf(t, filepath.Join(p.Cluster, "nodes", "pve1.json")))
	require.NoError(t, s.SaveNode(pve1))
	require.EqualValues(t, 2, revOf(t, filepath.Join(p.Cluster, "nodes", "pve1.json")))

	require.NoError(t, s.DeleteNode("pve2"))
	require.NoError(t, s.DeleteNode("pve2"))
	nodes, err = s.Nodes()
	require.NoError(t, err)
	require.Equal(t, []NodeEntry{pve1}, nodes)
}

func TestSaveNodeRefusesAnEmptyName(t *testing.T) {
	s, p := openStore(t)
	require.Error(t, s.SaveNode(NodeEntry{Version: "1", Since: t0}))
	require.Error(t, s.SaveNode(NodeEntry{Name: "../x", Version: "1", Since: t0}))
	require.Empty(t, stored(t, p.Cluster))
}

func TestManualRoutes(t *testing.T) {
	s, p := openStore(t)
	routes, err := s.ManualRoutes()
	require.NoError(t, err)
	require.NotNil(t, routes)
	require.Empty(t, routes)

	nas := model.Route{
		Hostname: "nas.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTPS, Addr: netip.MustParseAddr("10.0.0.9"), Port: 5001},
		Options:  model.RouteOptions{NoTLSVerify: true, AllowNode: true},
		Source:   model.SourceManual,
		ManualID: "nas",
	}
	wiki := model.Route{
		Hostname: "wiki.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTP, Port: 80},
		Source:   model.SourceManual,
		Guest:    &model.GuestRef{Kind: model.KindQEMU, VMID: 101},
		ManualID: "wiki",
	}
	require.NoError(t, s.SaveManualRoute(wiki))
	require.NoError(t, s.SaveManualRoute(nas))

	routes, err = s.ManualRoutes()
	require.NoError(t, err)
	require.Equal(t, []model.Route{nas, wiki}, routes)
	require.Equal(t, []string{"routes/nas.json", "routes/wiki.json"}, stored(t, p.Cluster))

	require.NoError(t, s.SaveManualRoute(nas))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Cluster, "routes", "nas.json")))

	require.NoError(t, s.DeleteManualRoute("nas"))
	require.NoError(t, s.DeleteManualRoute("nas"))
	routes, err = s.ManualRoutes()
	require.NoError(t, err)
	require.Equal(t, []model.Route{wiki}, routes)
}

func TestSaveManualRouteRefusesBadInput(t *testing.T) {
	s, p := openStore(t)
	good := model.Route{
		Hostname: "nas.example.com",
		Target:   model.Target{Scheme: model.SchemeHTTP, Port: 80},
		Source:   model.SourceManual,
		ManualID: "nas",
	}
	noID := good
	noID.ManualID = ""
	noHost := good
	noHost.Hostname = ""
	badHost := good
	badHost.Hostname = "not a host"
	oneLabel := good
	oneLabel.Hostname = "nas"
	address := good
	address.Hostname = "10.0.0.5"
	badID := good
	badID.ManualID = "../x"

	for name, r := range map[string]model.Route{
		"empty id": noID, "empty hostname": noHost, "invalid hostname": badHost,
		"one label": oneLabel, "address": address, "unsafe id": badID,
	} {
		require.Error(t, s.SaveManualRoute(r), name)
	}
	require.Empty(t, stored(t, p.Cluster))
	require.Error(t, s.DeleteManualRoute(""))
}

func TestSaveManualRouteStoresTheNormalisedHostname(t *testing.T) {
	s, _ := openStore(t)
	r := model.Route{
		Hostname: "NAS.Example.COM.",
		Target:   model.Target{Scheme: model.SchemeHTTP, Port: 80},
		Source:   model.SourceManual,
		ManualID: "nas",
	}
	require.NoError(t, s.SaveManualRoute(r))
	require.Equal(t, "NAS.Example.COM.", r.Hostname, "the caller's route is not changed")
	routes, err := s.ManualRoutes()
	require.NoError(t, err)
	require.Len(t, routes, 1)
	require.Equal(t, "nas.example.com", routes[0].Hostname)
}

func TestApprovals(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Approvals()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	require.NoError(t, s.SaveApproval(approvalOf("qemu/101", "ident-a")))
	require.NoError(t, s.SaveApproval(approvalOf("lxc/200", "ident-b")))
	got, err = s.Approvals()
	require.NoError(t, err)
	require.Equal(t, map[string]Approval{
		"qemu/101": approvalOf("qemu/101", "ident-a"),
		"lxc/200":  approvalOf("lxc/200", "ident-b"),
	}, got)
	require.Equal(t, []string{"approvals/lxc_200.json", "approvals/qemu_101.json"}, stored(t, p.Cluster))

	require.NoError(t, s.SaveApproval(approvalOf("qemu/101", "ident-c")))
	require.EqualValues(t, 2, revOf(t, filepath.Join(p.Cluster, "approvals", "qemu_101.json")))
	require.NoError(t, s.SaveApproval(approvalOf("qemu/101", "ident-c")))
	require.EqualValues(t, 2, revOf(t, filepath.Join(p.Cluster, "approvals", "qemu_101.json")))

	require.NoError(t, s.DeleteApproval("lxc/200"))
	require.NoError(t, s.DeleteApproval("lxc/200"))
	got, err = s.Approvals()
	require.NoError(t, err)
	require.Equal(t, map[string]Approval{"qemu/101": approvalOf("qemu/101", "ident-c")}, got)
}

func approvalOf(owner, identity string) Approval { return Approval{Owner: owner, Identity: identity} }

func TestAnApprovalKeepsItsMACsAndAddresses(t *testing.T) {
	s, p := openStore(t)
	a := Approval{
		Owner:     "qemu/101",
		Identity:  "uuid:101",
		MACs:      []string{"bc:24:11:00:aa:b6", "bc:24:11:00:aa:b5", "bc:24:11:00:aa:b6"},
		Addresses: []netip.Addr{netip.MustParseAddr("fd00::1"), netip.MustParseAddr("10.0.0.9"), netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.9")},
	}
	require.NoError(t, s.SaveApproval(a))
	require.NoError(t, s.SaveApproval(approvalOf("lxc/200", "uuid:200")))

	got, err := s.Approvals()
	require.NoError(t, err)
	require.Equal(t, map[string]Approval{
		"qemu/101": {
			Owner:     "qemu/101",
			Identity:  "uuid:101",
			MACs:      []string{"bc:24:11:00:aa:b5", "bc:24:11:00:aa:b6"},
			Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.9"), netip.MustParseAddr("fd00::1")},
		},
		"lxc/200": approvalOf("lxc/200", "uuid:200"),
	}, got, "sorted, each once")
	require.Equal(t, "bc:24:11:00:aa:b6", a.MACs[0], "the caller's approval is not changed")

	raw, err := os.ReadFile(filepath.Join(p.Cluster, "approvals", "lxc_200.json"))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "macs", "an approval of the identity alone is written as before")
	require.NotContains(t, string(raw), "addresses")
}

func TestSaveApprovalRefusesAMACOrAnAddressThatIsNotValid(t *testing.T) {
	for name, a := range map[string]Approval{
		"a MAC in capitals":     {MACs: []string{"BC:24:11:00:AA:B5"}},
		"something else":        {MACs: []string{"eth0"}},
		"an empty MAC":          {MACs: []string{""}},
		"an address of nothing": {Addresses: []netip.Addr{{}}},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			a.Owner, a.Identity = "qemu/101", "uuid:101"

			require.Error(t, s.SaveApproval(a))
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

func TestApprovalsRefuseAFileUnderTheWrongName(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "approvals", "whatever.json"),
		envelopeJSON("qemu/101", `{"owner":"qemu/101","identity":"i"}`))
	got, err := s.Approvals()
	require.Error(t, err)
	require.Nil(t, got)
	require.Contains(t, err.Error(), "whatever.json")
	require.Contains(t, err.Error(), "qemu/101")
	require.Contains(t, err.Error(), "qemu_101.json", "the name it belongs under")
}

func TestApprovalsRefuseAFileWithoutAnOwner(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "approvals", "nameless.json"),
		envelopeJSON("nameless", `{"identity":"i"}`))
	_, err := s.Approvals()
	require.Error(t, err)
	require.Contains(t, err.Error(), "nameless.json")
}

func TestSaveApprovalRefusesEmptyValues(t *testing.T) {
	s, p := openStore(t)
	require.Error(t, s.SaveApproval(approvalOf("", "i")))
	require.Error(t, s.SaveApproval(approvalOf("qemu/101", "")))
	require.Error(t, s.SaveApproval(approvalOf("../x", "i")))
	require.Empty(t, stored(t, p.Cluster))
	require.Error(t, s.DeleteApproval(""))
}

// recordSyncs makes the roots of s record what they sync.
func recordSyncs(s *Store) (cluster, private, local *syncs) {
	cluster, private, local = &syncs{}, &syncs{}, &syncs{}
	s.cluster.flush, s.private.flush, s.local.flush = cluster.flush, private.flush, local.flush
	return cluster, private, local
}

func TestTheSharedRootsOfADurableStoreSyncEveryChange(t *testing.T) {
	p, marker := appliancePaths(t)
	writeFile(t, marker, "")
	s, err := Open(p)
	require.NoError(t, err)
	cluster, private, local := recordSyncs(s)

	require.NoError(t, s.Init())
	require.Equal(t, []string{p.Local, p.Local}, cluster.take(), "the two roots made below the local one")

	meta, claims := filepath.Join(p.Cluster, "meta"), filepath.Join(p.Cluster, "claims")
	require.NoError(t, s.SaveInstall(applianceInstall()))
	require.Equal(t, []string{p.Cluster, meta}, cluster.take())
	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 1, Nonce: "aa", Incarnation: "boot/1"}))
	require.Equal(t, []string{meta}, cluster.take(), "the write-ahead of an epoch is durable")

	two := map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101"), "b.example.com": claimOf("b.example.com", "qemu/102")}
	require.NoError(t, s.SaveClaims(two))
	require.Equal(t, []string{p.Cluster, claims, claims}, cluster.take())
	delete(two, "b.example.com")
	require.NoError(t, s.SaveClaims(two))
	require.Equal(t, []string{claims}, cluster.take(), "the removal of the claim that is gone")
	require.NoError(t, s.AppendAdopted(t0, "example.com", adoptedSample(0)))
	require.Equal(t, []string{p.Cluster}, cluster.take())

	credentials := filepath.Join(p.Private, "credentials")
	require.NoError(t, s.SaveCredential(credentialOf("c1")))
	require.Equal(t, []string{p.Private, credentials}, private.take())
	require.NoError(t, s.DeleteCredential("c1"))
	require.Equal(t, []string{credentials}, private.take())

	require.NoError(t, s.SaveNodeAddrs([]netip.Addr{netip.MustParseAddr("10.92.0.1")}))
	require.Empty(t, local.take(), "the local root is not a shared one")
}

func TestAStoreOnPmxcfsNeverSyncsADirectory(t *testing.T) {
	p, _ := mountedPaths(t)
	require.False(t, p.Durable)
	s, err := Open(p)
	require.NoError(t, err)
	cluster, private, local := recordSyncs(s)

	require.NoError(t, s.Init())
	require.NoError(t, s.SaveInstall(Install{ID: "abc", CreatedAt: t0}))
	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 1, Nonce: "aa"}))
	require.NoError(t, s.SaveClaims(map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101")}))
	require.NoError(t, s.SaveClaims(nil))
	require.NoError(t, s.AppendAdopted(t0, "example.com", adoptedSample(0)))
	require.NoError(t, s.SaveCredential(credentialOf("c1")))
	require.NoError(t, s.DeleteCredential("c1"))
	require.NoError(t, s.SaveNodeAddrs([]netip.Addr{netip.MustParseAddr("10.92.0.1")}))

	require.Empty(t, cluster.take())
	require.Empty(t, private.take())
	require.Empty(t, local.take())
}
