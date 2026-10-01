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

	require.NoError(t, s.SaveApproval("qemu/101", "ident-a"))
	require.NoError(t, s.SaveApproval("lxc/200", "ident-b"))
	got, err = s.Approvals()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"qemu/101": "ident-a", "lxc/200": "ident-b"}, got)
	require.Equal(t, []string{"approvals/lxc_200.json", "approvals/qemu_101.json"}, stored(t, p.Cluster))

	require.NoError(t, s.SaveApproval("qemu/101", "ident-c"))
	require.EqualValues(t, 2, revOf(t, filepath.Join(p.Cluster, "approvals", "qemu_101.json")))
	require.NoError(t, s.SaveApproval("qemu/101", "ident-c"))
	require.EqualValues(t, 2, revOf(t, filepath.Join(p.Cluster, "approvals", "qemu_101.json")))

	require.NoError(t, s.DeleteApproval("lxc/200"))
	require.NoError(t, s.DeleteApproval("lxc/200"))
	got, err = s.Approvals()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"qemu/101": "ident-c"}, got)
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
	require.Error(t, s.SaveApproval("", "i"))
	require.Error(t, s.SaveApproval("qemu/101", ""))
	require.Error(t, s.SaveApproval("../x", "i"))
	require.Empty(t, stored(t, p.Cluster))
	require.Error(t, s.DeleteApproval(""))
}
