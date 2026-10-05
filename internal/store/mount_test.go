package store

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

func TestOpenCreatesTheLocalRootOnly(t *testing.T) {
	base := t.TempDir()
	p := Paths{
		Cluster: filepath.Join(base, "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "a", "b", "local"),
	}
	_, err := Open(p)
	require.NoError(t, err)

	info, err := os.Stat(p.Local)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	require.Equal(t, fs.FileMode(0o700), info.Mode().Perm())
	requireMissing(t, p.Cluster)
	requireMissing(t, p.Private)

	_, err = Open(p)
	require.NoError(t, err, "an existing store opens again")
}

func TestInitCreatesTheSharedRoots(t *testing.T) {
	p := testPaths(t)
	s, err := Open(p)
	require.NoError(t, err)

	require.NoError(t, s.Init())
	for _, dir := range []string{p.Cluster, p.Private} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.True(t, info.IsDir())
		require.Equal(t, fs.FileMode(0o700), info.Mode().Perm(), dir)
	}
	require.NoError(t, s.SaveInstall(Install{ID: "abc", CreatedAt: t0}))
	require.NoError(t, s.Init(), "roots that exist are fine, and keep what they hold")
	_, found, err := s.Install()
	require.NoError(t, err)
	require.True(t, found)
}

func TestInitCreatesTheLeafOnly(t *testing.T) {
	base := t.TempDir()
	p := Paths{
		Cluster: filepath.Join(base, "missing", "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "local"),
	}
	s, err := Open(p)
	require.NoError(t, err)

	err = s.Init()
	require.Error(t, err, "the parent of the cluster root must exist")
	require.Contains(t, err.Error(), p.Cluster)
	requireMissing(t, filepath.Dir(p.Cluster))
	requireMissing(t, p.Cluster)
}

func TestInitRefusesARootThatIsAFile(t *testing.T) {
	p := testPaths(t)
	writeFile(t, p.Private, "x")
	s, err := Open(p)
	require.NoError(t, err)
	require.Error(t, s.Init())
}

func TestSharedRootsMayBeReadOnly(t *testing.T) {
	skipAsRoot(t)
	p := testPaths(t)
	require.NoError(t, os.MkdirAll(p.Cluster, 0o700))
	require.NoError(t, os.Chmod(p.Cluster, 0o500))
	t.Cleanup(func() { _ = os.Chmod(p.Cluster, 0o700) })

	s, err := Open(p)
	require.NoError(t, err)

	_, found, err := s.Install()
	require.NoError(t, err)
	require.False(t, found)
	settings, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, DefaultSettings(), settings)
	require.ErrorIs(t, s.SaveInstall(Install{ID: "abc", CreatedAt: t0}), fs.ErrPermission)
	require.Empty(t, stored(t, p.Cluster))
}

// afterRemovingTheRoots is a store whose cluster and private roots existed and
// are gone, as they are while pve-cluster restarts and /etc/pve is an empty
// directory of the node's own disk.
func afterRemovingTheRoots(t *testing.T) (*Store, Paths) {
	t.Helper()
	s, p := openStore(t)
	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 1, Nonce: "aa"}))
	require.NoError(t, s.SaveCredential(credentialOf("c1")))
	require.NoError(t, os.RemoveAll(p.Cluster))
	require.NoError(t, os.RemoveAll(p.Private))
	return s, p
}

// requireNoRoot checks that err is the error of a store whose root is gone: it
// is ErrNoRoot, names the root, and is not the error of an unmounted filesystem.
func requireNoRoot(t *testing.T, p Paths, name string, err error) {
	t.Helper()
	require.ErrorIs(t, err, ErrNoRoot, name)
	require.NotErrorIs(t, err, ErrNotMounted, name)
	require.True(t, strings.Contains(err.Error(), p.Cluster) || strings.Contains(err.Error(), p.Private),
		"%s: %v does not say which root", name, err)
}

func TestMissingSharedRootIsAnErrorNotAnEmptyStore(t *testing.T) {
	s, p := afterRemovingTheRoots(t)

	reads := map[string]func() error{
		"Install":      func() error { _, _, err := s.Install(); return err },
		"Writer":       func() error { _, _, err := s.Writer(); return err },
		"Settings":     func() error { _, err := s.Settings(); return err },
		"Nodes":        func() error { _, err := s.Nodes(); return err },
		"Claims":       func() error { _, err := s.Claims(); return err },
		"ManualRoutes": func() error { _, err := s.ManualRoutes(); return err },
		"Approvals":    func() error { _, err := s.Approvals(); return err },
		"Segments":     func() error { _, err := s.Segments(); return err },
		"Tombstones":   func() error { _, err := s.Tombstones().Load(t.Context()); return err },
		"Credentials":  func() error { _, err := s.Credentials(); return err },
		"PVEToken":     func() error { _, _, err := s.PVEToken(); return err },
	}
	for name, read := range reads {
		requireNoRoot(t, p, name, read())
	}
	requireMissing(t, p.Cluster)
	requireMissing(t, p.Private)
}

func TestMissingSharedRootFailsEveryWriteAndCreatesNothing(t *testing.T) {
	s, p := afterRemovingTheRoots(t)
	claims := map[string]planner.Claim{"a.example.com": claimOf("a.example.com", "qemu/101")}

	writes := map[string]func() error{
		"SaveWriter":      func() error { return s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 2, Nonce: "bb"}) },
		"SaveInstall":     func() error { return s.SaveInstall(Install{ID: "abc", CreatedAt: t0}) },
		"SaveSettings":    func() error { return s.SaveSettings(DefaultSettings()) },
		"SaveNode":        func() error { return s.SaveNode(NodeEntry{Name: "pve1", Since: t0}) },
		"DeleteNode":      func() error { return s.DeleteNode("pve1") },
		"SaveClaims":      func() error { return s.SaveClaims(claims) },
		"SaveManualRoute": func() error { return s.SaveManualRoute(manualRoute("nas")) },
		"SaveApproval":    func() error { return s.SaveApproval(approvalOf("qemu/101", "i")) },
		"SaveSegment":     func() error { return s.SaveSegment(Segment{Bridge: "vmbr0", AcknowledgedAt: t0}) },
		"DeleteSegment":   func() error { return s.DeleteSegment("vmbr0") },
		"Tombstones": func() error {
			return s.Tombstones().Save(t.Context(), map[string]reconcile.Tombstone{"z/a": stoneOf(0, 1)})
		},
		"AppendAdopted":    func() error { return s.AppendAdopted(t0, "example.com", adoptedSample(0)) },
		"SaveCredential":   func() error { return s.SaveCredential(credentialOf("c2")) },
		"DeleteCredential": func() error { return s.DeleteCredential("c1") },
		"SavePVEToken":     func() error { return s.SavePVEToken(PVEToken{TokenID: "pco@pve!pco", Secret: NewSecret("s")}) },
	}
	for name, write := range writes {
		requireNoRoot(t, p, name, write())
	}
	requireMissing(t, p.Cluster)
	requireMissing(t, p.Private)
}

func TestMissingKindOrFileInAnExistingRootIsStillNotFound(t *testing.T) {
	s, _ := openStore(t)
	_, found, err := s.Writer()
	require.NoError(t, err)
	require.False(t, found)
	claims, err := s.Claims()
	require.NoError(t, err)
	require.Empty(t, claims)
	creds, err := s.Credentials()
	require.NoError(t, err)
	require.Empty(t, creds)
	require.NoError(t, s.DeleteNode("pve1"))
	_, found, err = s.PVEToken()
	require.NoError(t, err)
	require.False(t, found)
}

// mountedPaths returns paths whose mount check is a file that the test can
// remove, and that file.
func mountedPaths(t *testing.T) (Paths, string) {
	t.Helper()
	p := testPaths(t)
	p.MountCheck = filepath.Join(filepath.Dir(p.Cluster), ".version")
	writeFile(t, p.MountCheck, "1")
	return p, p.MountCheck
}

func TestUnmountedClusterFilesystemFailsEverySharedOperation(t *testing.T) {
	p, marker := mountedPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 1, Nonce: "aa"}))
	require.NoError(t, s.SaveCredential(credentialOf("c1")))
	require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"a.example.com": bindingOf("a.example.com", "qemu/101")}))

	require.NoError(t, os.Remove(marker))

	checks := map[string]func() error{
		"Install":          func() error { _, _, err := s.Install(); return err },
		"SaveInstall":      func() error { return s.SaveInstall(Install{ID: "abc", CreatedAt: t0}) },
		"Writer":           func() error { _, _, err := s.Writer(); return err },
		"SaveWriter":       func() error { return s.SaveWriter(planner.Writer{InstallID: "abc", Generation: 2, Nonce: "bb"}) },
		"Settings":         func() error { _, err := s.Settings(); return err },
		"SaveSettings":     func() error { return s.SaveSettings(DefaultSettings()) },
		"Nodes":            func() error { _, err := s.Nodes(); return err },
		"SaveNode":         func() error { return s.SaveNode(NodeEntry{Name: "pve1", Since: t0}) },
		"DeleteNode":       func() error { return s.DeleteNode("pve1") },
		"Claims":           func() error { _, err := s.Claims(); return err },
		"SaveClaims":       func() error { return s.SaveClaims(nil) },
		"ManualRoutes":     func() error { _, err := s.ManualRoutes(); return err },
		"SaveManualRoute":  func() error { return s.SaveManualRoute(manualRoute("nas")) },
		"Approvals":        func() error { _, err := s.Approvals(); return err },
		"SaveApproval":     func() error { return s.SaveApproval(approvalOf("qemu/101", "i")) },
		"DeleteApproval":   func() error { return s.DeleteApproval("qemu/101") },
		"Segments":         func() error { _, err := s.Segments(); return err },
		"SaveSegment":      func() error { return s.SaveSegment(Segment{Bridge: "vmbr0", AcknowledgedAt: t0}) },
		"DeleteSegment":    func() error { return s.DeleteSegment("vmbr0") },
		"Tombstones Load":  func() error { _, err := s.Tombstones().Load(t.Context()); return err },
		"Tombstones Save":  func() error { return s.Tombstones().Save(t.Context(), nil) },
		"AppendAdopted":    func() error { return s.AppendAdopted(t0, "example.com", adoptedSample(0)) },
		"Credentials":      func() error { _, err := s.Credentials(); return err },
		"SaveCredential":   func() error { return s.SaveCredential(credentialOf("c2")) },
		"DeleteCredential": func() error { return s.DeleteCredential("c1") },
		"PVEToken":         func() error { _, _, err := s.PVEToken(); return err },
		"SavePVEToken":     func() error { return s.SavePVEToken(PVEToken{TokenID: "pco@pve!pco", Secret: NewSecret("s")}) },
		"Init":             func() error { return s.Init() },
	}
	for name, check := range checks {
		err := check()
		require.ErrorIs(t, err, ErrNotMounted, name)
		require.Contains(t, err.Error(), "not mounted", name)
	}
	require.Equal(t, []string{"credentials/c1.json"}, stored(t, p.Private))

	// The local root has nothing to do with the cluster filesystem.
	got, err := s.Bindings()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NoError(t, s.SaveBindings(nil))

	writeFile(t, marker, "1")
	_, found, err := s.Writer()
	require.NoError(t, err)
	require.True(t, found, "the answer comes back with the mount, nothing was remembered")
}

func TestOpenSucceedsWhileTheClusterFilesystemIsNotMounted(t *testing.T) {
	p, marker := mountedPaths(t)
	require.NoError(t, os.Remove(marker))
	s, err := Open(p)
	require.NoError(t, err)
	require.ErrorIs(t, s.Init(), ErrNotMounted)
	requireMissing(t, p.Cluster)
	requireMissing(t, p.Private)
}

func TestOpenDoesNotSweepAnUnmountedClusterFilesystem(t *testing.T) {
	p, marker := mountedPaths(t)
	old := filepath.Join(p.Cluster, "claims", ".a.json.1.tmp")
	writeFile(t, old, "x")
	require.NoError(t, os.Chtimes(old, t0.Add(-time.Hour), t0.Add(-time.Hour)))
	require.NoError(t, os.Remove(marker))

	_, err := open(p, func() time.Time { return t0 })
	require.NoError(t, err)
	_, err = os.Stat(old)
	require.NoError(t, err, "what lies under the mount point is not ours to remove")
}

func TestAnEmptyMountCheckSwitchesTheCheckOff(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveInstall(Install{ID: "abc", CreatedAt: t0}))
}

func TestDirNeverCreatesItsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	d := NewDir(root)

	require.Error(t, d.Put("things", "a", sample{}))
	var got sample
	found, err := d.Get("things", "a", &got)
	require.Error(t, err)
	require.False(t, found)
	require.Contains(t, err.Error(), root)
	_, err = d.List("things")
	require.Error(t, err)
	require.Error(t, d.Delete("things", "a"))
	requireMissing(t, root)
}
