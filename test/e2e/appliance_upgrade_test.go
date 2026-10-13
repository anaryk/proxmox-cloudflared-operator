//go:build e2e

package e2e

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/upgrade"
)

// release is a release of pco served by the suite on the node, as GitHub
// serves one, for the appliance's pco upgrade behind a second relay.
type release struct {
	root    string // the directory served
	files   string // its directory of the release's files
	keyring string
	srv     *http.Server
	addr    string

	mu       sync.Mutex
	requests []string
}

func (r *release) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.URL.Path)
	r.mu.Unlock()
	http.FileServer(http.Dir(r.root)).ServeHTTP(w, req)
}

func (r *release) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// newRelease writes a release of the package deb of version: the package,
// the manifest of cloudflared it ships, checksums.txt and its signature by
// a key made for the run, and the answer of GitHub about the latest release;
// and serves it on the node's address on the bridge of the run.
func (s *suite) newRelease(t *testing.T, deb, version string) *release {
	t.Helper()
	r := &release{root: filepath.Join(s.dir, "release")}
	r.files = filepath.Join(r.root, upgrade.Repository, "releases", "download", "v"+version)
	require.NoError(t, os.MkdirAll(r.files, 0o755))
	latest := filepath.Join(r.root, "repos", upgrade.Repository, "releases")
	require.NoError(t, os.MkdirAll(latest, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(latest, "latest"), []byte(`{"tag_name":"v`+version+`"}`), 0o600))

	name := "pco_" + version + "_amd64.deb"
	raw, err := os.ReadFile(deb)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(r.files, name), raw, 0o600))
	unpacked := filepath.Join(s.dir, "deb-upgrade")
	s.must(t, "dpkg-deb", "-x", deb, unpacked)
	manifest, err := os.ReadFile(filepath.Join(unpacked, "usr", "share", "pco", upgrade.ManifestName))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(r.files, upgrade.ManifestName), manifest, 0o600))
	var sums strings.Builder
	for _, f := range []string{name, upgrade.ManifestName} {
		b, err := os.ReadFile(filepath.Join(r.files, f))
		require.NoError(t, err)
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(b), f)
	}
	require.NoError(t, os.WriteFile(filepath.Join(r.files, "checksums.txt"), []byte(sums.String()), 0o600))

	home := filepath.Join(s.dir, "gnupg")
	require.NoError(t, os.MkdirAll(home, 0o700))
	gpg := func(args ...string) {
		s.must(t, "gpg", append([]string{"--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...)
	}
	gpg("--quick-gen-key", "pco end-to-end suite <e2e@example.test>", "ed25519", "sign", "1d")
	r.keyring = filepath.Join(s.dir, "release-key.gpg")
	gpg("--output", r.keyring, "--export")
	gpg("--output", filepath.Join(r.files, "checksums.txt.sig"), "--detach-sign", filepath.Join(r.files, "checksums.txt"))

	ln, err := net.Listen("tcp", s.lay.nodeAddr+":0")
	require.NoError(t, err)
	r.addr = ln.Addr().String()
	r.srv = &http.Server{Handler: r, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = r.srv.Serve(ln) }()
	t.Cleanup(func() { _ = r.srv.Close() })
	return r
}

const (
	releaseListen = "127.0.0.1:8788"
	releaseUnit   = "e2e-release.service"
	releaseKey    = "/root/e2e-release-key.gpg"
)

// upgradeHere runs pco upgrade in the appliance against the release the
// suite serves, through the second relay, with its key.
func (s *suite) upgradeHere(args ...string) (string, error) {
	cmd := append([]string{upgrade.BaseVar + "=http://" + releaseListen, upgrade.KeyringVar + "=" + releaseKey, "pco", "upgrade"}, args...)
	argv := s.at.argv("env", cmd...)
	return s.combinedWithin(installTimeout, argv[0], argv[1:]...)
}

// pcoVersion is the version of the package of pco in the appliance.
func (s *suite) pcoVersion(t testing.TB) string {
	t.Helper()
	return strings.TrimSpace(s.hereMust(t, "dpkg-query", "-W", "-f", "${Version}", "pco"))
}

func (s *suite) requireHeld(t testing.TB) {
	t.Helper()
	held := strings.Fields(s.hereMust(t, "apt-mark", "showhold"))
	require.Contains(t, held, "pco")
	require.Contains(t, held, "cloudflared")
}

func testUpgrade(t *testing.T, s *suite) {
	deb := os.Getenv("PCO_E2E_UPGRADE_DEB")
	if deb == "" {
		t.Skip("PCO_E2E_UPGRADE_DEB names no package of a later build: the upgrade is not run")
	}
	a := s.app
	version := strings.TrimSpace(s.must(t, "dpkg-deb", "-f", deb, "Version"))
	first := s.pcoVersion(t)
	t.Logf("from %s to %s", first, version)
	r := s.newRelease(t, deb, version)
	t.Cleanup(func() {
		_, _ = s.here("systemctl", "disable", "--now", releaseUnit)
		_, _ = s.here("rm", "-f", "/etc/systemd/system/"+releaseUnit, releaseKey)
	})
	s.pushRelay(t, a.vmid, releaseUnit, releaseListen, r.addr)
	key, err := os.ReadFile(r.keyring)
	require.NoError(t, err)
	s.pushFile(t, a.vmid, releaseKey, string(key), "0644")

	out, err := s.upgradeHere("pco", "--yes")
	t.Logf("pco upgrade pco:\n%s", out)
	require.NoError(t, err)
	require.Contains(t, out, upgrade.OverrideLine)
	require.Contains(t, out, "Before an upgrade, take a snapshot of this appliance on the node")
	s.waitAppliance(t, a.vmid)
	require.Equal(t, version, s.pcoVersion(t))
	require.Contains(t, s.pcoMust(t, "version"), strings.SplitN(version, "-", 2)[0])
	s.requireHeld(t)
	kept := s.hereMust(t, "ls", upgrade.WorkDir+"/previous/")
	require.Contains(t, kept, "pco_"+first+"_")

	out, err = s.upgradeHere("pco", "--rollback", "--yes")
	t.Logf("pco upgrade pco --rollback:\n%s", out)
	require.NoError(t, err)
	s.waitAppliance(t, a.vmid)
	require.Equal(t, first, s.pcoVersion(t))
	s.requireHeld(t)

	t.Log("a checksums.txt its signature does not match is refused before a package is fetched")
	sums := filepath.Join(r.files, "checksums.txt")
	raw, err := os.ReadFile(sums)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(sums, append(raw, []byte(strings.Repeat("0", 64)+"  pco_9.9.9_amd64.deb\n")...), 0o600))
	from := len(r.asked())
	out, err = s.upgradeHere("pco", "--yes")
	t.Logf("pco upgrade pco with a changed checksums.txt:\n%s", out)
	require.Error(t, err)
	for _, path := range r.asked()[from:] {
		require.False(t, strings.HasSuffix(path, ".deb"), "%s was fetched", path)
	}
	require.Equal(t, first, s.pcoVersion(t))
}
