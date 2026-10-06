package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/upgrade"
)

const (
	releasePath = "/anaryk/proxmox-cloudflared-operator/releases/download/v"
	latestPath  = "/repos/anaryk/proxmox-cloudflared-operator/releases/latest"
	refusedHost = "pco upgrade runs inside the appliance; a host upgrades with apt: " +
		"the installer installs a newer pco package, and apt-get install --only-upgrade cloudflared a newer cloudflared"
)

func sha(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// upgradeHost answers the commands of pco upgrade: dpkg-query with the
// installed versions, and apt-get and apt-mark with success.
type upgradeHost struct {
	mu        sync.Mutex
	installed map[string]string
	ran       []string
	// binVersion is what the pco the package installed says it is.
	binVersion string
	// onRun is what a command does besides answering, such as a daemon
	// that restarts when apt-get installs pco.
	onRun func(line string)
}

func (h *upgradeHost) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	h.mu.Lock()
	h.ran = append(h.ran, line)
	installed, binVersion, onRun := h.installed[args[len(args)-1]], h.binVersion, h.onRun
	h.mu.Unlock()
	if onRun != nil {
		onRun(line)
	}
	switch {
	case name == "dpkg-query":
		return "hold installed ok " + installed, nil
	case name == "apt-get" || name == "apt-mark" || name == "dpkg-deb":
		return "", nil
	case strings.HasSuffix(name, "/usr/bin/pco"):
		return fmt.Sprintf(`{"version":%q,"commit":"abc1234","date":"2026-10-01","schemaVersion":1}`, binVersion), nil
	}
	return "", fmt.Errorf("unexpected command %s", name)
}

// fakeDaemon is a daemon on a socket of the test that answers its version,
// until it stops.
type fakeDaemon struct {
	socket  string
	mu      sync.Mutex
	version string
	srv     *httptest.Server
}

func startFakeDaemon(t *testing.T, version string) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{socket: filepath.Join(testutil.ShortDir(t), "pco.sock"), version: version}
	ln, err := net.Listen("unix", d.socket)
	require.NoError(t, err)
	d.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/version" {
			http.NotFound(w, r)
			return
		}
		d.mu.Lock()
		v := d.version
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"version":%q}`, v)
	}))
	d.srv.Listener = ln
	d.srv.Start()
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDaemon) runs(version string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.version = version
}

func (h *upgradeHost) commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for _, line := range h.ran {
		if !strings.HasPrefix(line, "dpkg-query ") {
			names = append(names, line)
		}
	}
	return names
}

// signedBy is a verifier that calls a signature good when it is "signed: "
// and the file, and remembers the keyring it was given.
type signedBy struct {
	mu       sync.Mutex
	keyrings []string
}

func (s *signedBy) Verify(_ context.Context, keyring, sig, file string) (string, error) {
	s.mu.Lock()
	s.keyrings = append(s.keyrings, keyring)
	s.mu.Unlock()
	a, _ := os.ReadFile(sig)
	b, _ := os.ReadFile(file)
	if string(a) != "signed: "+string(b) {
		return "", upgrade.ErrSignature
	}
	return "0123456789ABCDEF0123456789ABCDEF01234567", nil
}

// noConnectors is a systemd without connectors.
type noConnectors struct{ connector.Systemd }

func (noConnectors) ListUnits(context.Context, string) ([]string, error) { return nil, nil }

// upgradeRig is an appliance with pco 1.2.3 and cloudflared 2026.9.3, and a
// release host of the test with the releases 1.2.3 and 1.2.4.
type upgradeRig struct {
	r      *runner
	host   *upgradeHost
	verify *signedBy
	srv    *httptest.Server
	paths  store.Paths
	work   string
}

func newUpgradeRig(t *testing.T) *upgradeRig {
	t.Helper()
	files := map[string]string{latestPath: `{"tag_name":"v1.2.4"}`}
	manifest := map[string]any{"schemaVersion": 1, "updated": "2026-10-05",
		"deny": []any{map[string]any{"version": "2026.9.0", "reason": "cloudflare/cloudflared#1737"}}}
	var versions []any
	for _, v := range []string{"2026.9.3", "2026.10.1"} {
		entry := map[string]any{"version": v}
		for _, arch := range []string{"amd64", "arm64"} {
			path := "/cloudflare/cloudflared/releases/download/" + v + "/cloudflared-linux-" + arch + ".deb"
			files[path] = "cloudflared " + v + " " + arch
			entry[arch] = map[string]any{"url": "https://github.com" + path, "sha256": sha(files[path])}
		}
		versions = append(versions, entry)
	}
	manifest["versions"] = versions
	m, err := json.Marshal(manifest)
	require.NoError(t, err)
	release := func(version string, assets map[string]string) {
		var lines []string
		for name, content := range assets {
			files[releasePath+version+"/"+name] = content
			lines = append(lines, sha(content)+"  "+name)
		}
		slices.Sort(lines)
		sums := strings.Join(lines, "\n") + "\n"
		files[releasePath+version+"/checksums.txt"] = sums
		files[releasePath+version+"/checksums.txt.sig"] = "signed: " + sums
	}
	release("1.2.3", map[string]string{"pco_1.2.3_amd64.deb": "pco 1.2.3", "cloudflared-versions.json": string(m)})
	release("1.2.4", map[string]string{"pco_1.2.4_amd64.deb": "pco 1.2.4", "cloudflared-versions.json": string(m)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(content))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	profile := filepath.Join(dir, "profile")
	require.NoError(t, os.WriteFile(profile, []byte("appliance\n"), 0o644))
	shipped := filepath.Join(dir, "cloudflared-versions.json")
	require.NoError(t, os.WriteFile(shipped, m, 0o644))
	u := &upgradeRig{
		r:      newRunner(t, "/nonexistent/pco/pco.sock"),
		host:   &upgradeHost{installed: map[string]string{"pco": "1.2.3", "cloudflared": "2026.9.3"}},
		verify: &signedBy{},
		srv:    srv,
		work:   filepath.Join(dir, "upgrades"),
		paths:  store.Paths{Cluster: filepath.Join(dir, "pco", "cluster"), Private: filepath.Join(dir, "pco", "private"), Local: filepath.Join(dir, "pco")},
	}
	u.r.env.profileFile = profile
	u.r.env.upgrade = upgradeEnv{
		euid: func() int { return 0 },
		run:  u.host,
		fetcher: func(o upgrade.Overrides, dir string) upgrade.Fetcher {
			base := o.Base
			if base == "" {
				base = srv.URL
			}
			return upgrade.NewFetcher(upgrade.FetchConfig{Dir: dir, Base: base})
		},
		verifier: u.verify,
		systemd:  noConnectors{},
		sleep:    func(context.Context, time.Duration) error { return nil },
		signals:  newFakeSignals(),
		volume:   func() (store.Paths, error) { return u.paths, nil },
		keyring:  "/usr/share/pco/release-key.gpg",
		manifest: shipped,
		workDir:  u.work,
		arch:     "amd64",
	}
	return u
}

func TestUpgradeIsRefusedOnAHost(t *testing.T) {
	u := newUpgradeRig(t)
	u.r.env.profileFile = "/nonexistent/pco/profile"

	res := u.r.run("", "upgrade", "--yes")

	require.EqualError(t, res.err, refusedHost)
	require.Empty(t, u.host.ran)
}

func TestUpgradeRunsAsRoot(t *testing.T) {
	u := newUpgradeRig(t)
	u.r.env.upgrade.euid = func() int { return 1000 }

	res := u.r.run("", "upgrade", "--check")

	require.EqualError(t, res.err, "pco upgrade installs packages: run it as root")
	require.Empty(t, u.host.ran)
}

func TestUpgradeSaysFirstThatTheReleaseIsOverridden(t *testing.T) {
	u := newUpgradeRig(t)
	vars := map[string]string{"PCO_UPGRADE_BASE": u.srv.URL, "PCO_UPGRADE_KEYRING": "/root/test-key.gpg"}
	u.r.env.getenv = func(name string) string { return vars[name] }
	u.r.env.upgrade.fetcher = defaultUpgradeEnv().fetcher

	res := u.r.run("", "upgrade", "--check")

	require.ErrorIs(t, res.err, errReported, "an upgrade is available")
	require.Equal(t, "warning: the release host or key is overridden (PCO_UPGRADE_BASE, PCO_UPGRADE_KEYRING); this is for tests only\n", res.errOut)
	require.Equal(t, []string{"/root/test-key.gpg"}, u.verify.keyrings, "the release is checked with the keyring of the override")

	u.r.env.profileFile = "/nonexistent/pco/profile"
	res = u.r.run("", "upgrade", "--check")
	require.EqualError(t, res.err, refusedHost)
	require.True(t, strings.HasPrefix(res.errOut, "warning: the release host or key is overridden"), "the warning comes first on every run")

	vars["PCO_UPGRADE_BASE"] = "http://10.0.0.1:8788"
	res = u.r.run("", "upgrade", "--check")
	require.ErrorContains(t, res.err, "PCO_UPGRADE_BASE must be an http or https URL of a loopback address")
	require.True(t, strings.HasPrefix(res.errOut, "warning: the release host or key is overridden"))
}

func TestUpgradeRemindsOfTheSnapshotAndOfRecoverBeforeItAsks(t *testing.T) {
	u := newUpgradeRig(t)
	st, err := store.Open(u.paths)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(u.paths.Cluster, 0o700))
	require.NoError(t, os.MkdirAll(u.paths.Private, 0o700))
	require.NoError(t, st.SaveInstall(store.Install{ID: "0123456789ab", CreatedAt: t0, Profile: store.ProfileAppliance, Appliance: &store.ApplianceInstall{
		VMID: 9240, Node: "pve1", MACs: []string{"bc:24:11:00:aa:b5"},
		Endpoints: []store.Endpoint{{Address: "10.92.0.1:8006", ServerName: "pve1"}},
	}}))

	res := u.r.tty().run("n\n", "upgrade")

	require.ErrorIs(t, res.err, errAborted)
	require.Equal(t, `Before an upgrade, take a snapshot of this appliance on the node, as root there:
  pct snapshot 9240 pco-pre-upgrade-20261001
pco cannot take it: its token has no right to. A rollback to that snapshot is followed by
pco appliance recover in the appliance.
The connectors restart on the new cloudflared: a tunnel has no connection from this appliance
until its connector is ready again, at most 1m0s.
Upgrade pco from 1.2.3 to 1.2.4 and cloudflared from 2026.9.3 to 2026.10.1? [y/N] `, res.errOut)
	require.Empty(t, u.host.commands(), "nothing is installed")
}

func TestUpgradeWithoutATerminalNeedsYes(t *testing.T) {
	u := newUpgradeRig(t)

	res := u.r.run("", "upgrade", "pco")

	require.ErrorIs(t, res.err, errNoTerminal)
	require.Contains(t, res.errOut, "pct snapshot <vmid> pco-pre-upgrade-20261001")
	require.Empty(t, u.host.commands())
}

func TestUpgradeWithYesUpgradesPcoThenCloudflared(t *testing.T) {
	u := newUpgradeRig(t)

	res := u.r.run("", "upgrade", "--yes")

	require.NoError(t, res.err, res.errOut)
	require.Equal(t, []string{
		"apt-get install -y --allow-change-held-packages " + filepath.Join(u.work, "pco_1.2.4_amd64.deb"),
		"apt-mark hold pco cloudflared",
		"apt-get install -y --allow-change-held-packages " + filepath.Join(u.work, "cloudflared_2026.10.1_amd64.deb"),
		"apt-mark hold pco cloudflared",
	}, u.host.commands())
	require.Contains(t, res.out, "pco 1.2.4 is installed, from release v1.2.4 signed by key 0123456789ABCDEF0123456789ABCDEF01234567\n")
	require.Contains(t, res.out, "pco 1.2.3 is kept as "+filepath.Join(u.work, "previous", "pco_1.2.3_amd64.deb")+" for --rollback\n")
	require.Contains(t, res.out, "cloudflared 2026.10.1 is installed\n")
	require.True(t, strings.HasSuffix(res.out, "pco and cloudflared are held\n"))
	require.Contains(t, res.errOut, "pco-pre-upgrade-20261001", "the reminder is there with --yes too")
	require.NotContains(t, res.errOut, "[y/N]")
	require.Contains(t, res.out, "the daemon did not answer before the upgrade, so it is not waited for\n")
}

// installsPco says whether a command line installs a package of pco.
func installsPco(line string) bool {
	return strings.HasPrefix(line, "apt-get install") && strings.Contains(line, "/pco_")
}

func TestUpgradeWaitsForTheDaemonOfTheNewPco(t *testing.T) {
	u := newUpgradeRig(t)
	d := startFakeDaemon(t, "v1.2.3")
	u.r.socket = d.socket
	u.host.binVersion = "v1.2.4"
	u.host.onRun = func(line string) {
		if installsPco(line) {
			d.runs("v1.2.4")
		}
	}

	res := u.r.run("", "upgrade", "--yes")

	require.NoError(t, res.err, res.errOut)
	require.Contains(t, res.out, "the daemon runs pco v1.2.4\n")
	require.Equal(t, []string{
		"apt-get install -y --allow-change-held-packages " + filepath.Join(u.work, "pco_1.2.4_amd64.deb"),
		"apt-mark hold pco cloudflared",
		"/usr/bin/pco version --json",
		"apt-get install -y --allow-change-held-packages " + filepath.Join(u.work, "cloudflared_2026.10.1_amd64.deb"),
		"apt-mark hold pco cloudflared",
	}, u.host.commands())
}

// A new pco whose daemon does not come back stops the upgrade before
// cloudflared, with the way back.
func TestUpgradeSaysWhenTheDaemonStaysOnTheOldPco(t *testing.T) {
	u := newUpgradeRig(t)
	d := startFakeDaemon(t, "v1.2.3")
	u.r.socket = d.socket
	u.host.binVersion = "v1.2.4"

	res := u.r.run("", "upgrade", "--yes")

	require.EqualError(t, res.err, "pco 1.2.4 is installed, but the daemon still answers as v1.2.3 1m0s later, not as v1.2.4: "+
		"systemctl status pco.service says why, and pco upgrade pco --rollback goes back to 1.2.3")
	require.Equal(t, 1, exitCode(res.err, &strings.Builder{}))
	require.Equal(t, []string{
		"apt-get install -y --allow-change-held-packages " + filepath.Join(u.work, "pco_1.2.4_amd64.deb"),
		"apt-mark hold pco cloudflared",
		"/usr/bin/pco version --json",
	}, u.host.commands(), "cloudflared is left as it is")
}

func TestUpgradeSaysWhenTheDaemonDoesNotComeBack(t *testing.T) {
	u := newUpgradeRig(t)
	d := startFakeDaemon(t, "v1.2.3")
	u.r.socket = d.socket
	u.host.binVersion = "v1.2.4"
	u.host.onRun = func(line string) {
		if installsPco(line) {
			d.srv.Close()
		}
	}

	res := u.r.run("", "upgrade", "pco", "--yes")

	require.ErrorContains(t, res.err, "pco 1.2.4 is installed, but the daemon does not answer 1m0s later (")
	require.ErrorContains(t, res.err, "systemctl status pco.service says why, and pco upgrade pco --rollback goes back to 1.2.3")
}

// A signal while apt-get runs does not end the run before the hold: it ends
// it after, before cloudflared.
func TestASignalStopsTheUpgradeOnlyOnceThePackagesAreHeld(t *testing.T) {
	u := newUpgradeRig(t)
	sigs := newFakeSignals()
	u.r.env.upgrade.signals = sigs
	caughtDuringInstall := false
	u.host.onRun = func(line string) {
		if installsPco(line) {
			caughtDuringInstall = sigs.registered()
			sigs.send(os.Interrupt)
		}
	}

	res := u.r.run("", "upgrade", "--yes")

	require.EqualError(t, res.err, "pco upgrade stopped on interrupt after pco 1.2.4 was installed; pco and cloudflared are held")
	require.True(t, caughtDuringInstall, "the signals are caught while apt-get runs")
	require.False(t, sigs.registered(), "and let go at the end")
	require.Equal(t, []string{
		"apt-get install -y --allow-change-held-packages " + filepath.Join(u.work, "pco_1.2.4_amd64.deb"),
		"apt-mark hold pco cloudflared",
	}, u.host.commands())
	require.Contains(t, res.errOut, "From here a signal stops pco upgrade only once what it installs is held again.\n")
	require.Contains(t, res.out, "pco 1.2.4 is installed, from release v1.2.4")
}

func TestASignalStopsTheWaitForTheConnectors(t *testing.T) {
	u := newUpgradeRig(t)
	sigs := newFakeSignals()
	u.r.env.upgrade.signals = sigs
	sd := &oneConnector{}
	u.r.env.upgrade.systemd = sd
	u.r.env.upgrade.sleep = func(context.Context, time.Duration) error {
		sigs.send(syscall.SIGTERM)
		return nil
	}

	res := u.r.run("", "upgrade", "cloudflared", "--yes")

	require.EqualError(t, res.err, "cloudflared 2026.10.1 is installed, but pco upgrade stopped on terminated; "+
		"pco upgrade cloudflared --rollback goes back to 2026.9.3")
	require.Equal(t, []string{"pco-cloudflared@" + oneTunnel + ".service"}, sd.restarted)
}

const oneTunnel = "00000000-0000-4000-8000-000000000001"

// oneConnector is a systemd with one connector, which runs and never gets
// ready.
type oneConnector struct {
	connector.Systemd
	restarted []string
}

func (*oneConnector) ListUnits(context.Context, string) ([]string, error) {
	return []string{"pco-cloudflared@" + oneTunnel + ".service"}, nil
}

func (*oneConnector) IsActive(context.Context, string) (bool, error) { return true, nil }

func (s *oneConnector) Restart(_ context.Context, unit string) error {
	s.restarted = append(s.restarted, unit)
	return nil
}

func TestARollbackOfPcoWaitsForTheDaemonToo(t *testing.T) {
	u := newUpgradeRig(t)
	u.host.installed["pco"] = "1.2.4"
	require.NoError(t, os.MkdirAll(filepath.Join(u.work, "previous"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(u.work, "previous", "pco_1.2.3_amd64.deb"), []byte("kept"), 0o600))
	d := startFakeDaemon(t, "v1.2.4")
	u.r.socket = d.socket
	u.host.binVersion = "v1.2.3"
	u.host.onRun = func(line string) {
		if strings.HasPrefix(line, "apt-get install") {
			d.runs("v1.2.3")
		}
	}

	res := u.r.run("", "upgrade", "pco", "--rollback", "--yes")

	require.NoError(t, res.err, res.errOut)
	require.Contains(t, res.out, "the daemon runs pco v1.2.3\n")
}

func TestUpgradeOfTheNewestIsNothingToDo(t *testing.T) {
	u := newUpgradeRig(t)
	u.host.installed = map[string]string{"pco": "1.2.4", "cloudflared": "2026.10.1"}

	res := u.r.run("", "upgrade", "--yes")

	require.NoError(t, res.err)
	require.Equal(t, "pco 1.2.4 is installed, the newest there is\ncloudflared 2026.10.1 is installed, the newest there is\n",
		strings.Join(lastLines(res.out, 2), ""))
	require.Empty(t, u.host.commands())
}

func lastLines(s string, n int) []string {
	lines := strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > 0 {
		lines[len(lines)-1] += "\n"
	}
	return lines[max(0, len(lines)-n):]
}

func TestUpgradeCheckSaysWhatIsAvailable(t *testing.T) {
	u := newUpgradeRig(t)

	res := u.r.run("", "upgrade", "--check")

	require.ErrorIs(t, res.err, errReported, "exit status 1: an upgrade is available")
	require.Equal(t, "pco: installed 1.2.3, available 1.2.4\n"+
		"cloudflared: installed 2026.9.3, available 2026.10.1, from the manifest of release v1.2.4 of 2026-10-05\n"+
		"cloudflared denied: 2026.9.0 (cloudflare/cloudflared#1737)\n",
		strings.Join(lastLines(res.out, 3), ""))
	require.Equal(t, 1, exitCode(res.err, &strings.Builder{}))
	require.Empty(t, u.host.commands())

	u.host.installed = map[string]string{"pco": "1.2.4", "cloudflared": "2026.10.1"}
	res = u.r.run("", "upgrade", "--check")
	require.NoError(t, res.err, "exit status 0: nothing is available")
	require.Contains(t, res.out, "pco: installed 1.2.4, the newest release\n")
	require.Contains(t, res.out, "cloudflared: installed 2026.10.1, the newest the manifest of release v1.2.4 of 2026-10-05 allows\n")

	u.host.installed["cloudflared"] = "2026.9.0"
	res = u.r.run("", "upgrade", "cloudflared", "--check")
	require.ErrorIs(t, res.err, errReported)
	require.Contains(t, res.out, "cloudflared: the installed 2026.9.0 is denied: cloudflare/cloudflared#1737\n")
	require.NotContains(t, res.out, "pco:")
}

func TestUpgradeRollbackGoesBackToTheKeptPackages(t *testing.T) {
	u := newUpgradeRig(t)
	u.host.installed = map[string]string{"pco": "1.2.4", "cloudflared": "2026.10.1"}
	require.NoError(t, os.MkdirAll(filepath.Join(u.work, "previous"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(u.work, "previous", "cloudflared_2026.9.3_amd64.deb"), []byte("kept"), 0o600))

	res := u.r.run("", "upgrade", "--rollback", "--yes")

	require.NoError(t, res.err, res.errOut)
	require.Equal(t, []string{
		"apt-get install -y --allow-change-held-packages --allow-downgrades " + filepath.Join(u.work, "previous", "cloudflared_2026.9.3_amd64.deb"),
		"apt-mark hold pco cloudflared",
	}, u.host.commands(), "pco has nothing kept and is left as it is")
	require.Contains(t, res.out, "no package is kept to roll back to: "+filepath.Join(u.work, "previous")+" holds no package of pco\n")
	require.Contains(t, res.out, "cloudflared 2026.9.3 is installed again from the kept package\n")
	require.NotContains(t, res.errOut, "pct snapshot", "a rollback needs no snapshot")

	res = u.r.run("", "upgrade", "pco", "--rollback", "--yes")
	require.ErrorIs(t, res.err, upgrade.ErrNothingKept)
}

func TestUpgradeRefusesWhatItCannotDo(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"nginx"}, `unknown package "nginx": want pco, cloudflared or all`},
		{[]string{"--version", "1.2.5"}, "--version names a version of pco or of cloudflared: name the package, as in pco upgrade pco --version 1.4.0"},
		{[]string{"pco", "--version", "1.2.5", "--rollback"}, "--rollback installs the package the last upgrade replaced; it takes no --version"},
		{[]string{"pco", "--check", "--rollback"}, "--check says what the latest release offers; it takes neither --version nor --rollback"},
		{[]string{"pco", "--check", "--version", "1.2.5"}, "--check says what the latest release offers; it takes neither --version nor --rollback"},
		{[]string{"pco", "cloudflared"}, "accepts at most 1 arg(s), received 2"},
		{[]string{"--json", "--check"}, `--json has no meaning for "pco upgrade": it prints no answer of the daemon`},
		{[]string{"pco", "--version", "1.2.2", "--yes"}, "pco 1.2.3 is installed, and 1.2.2 is not newer than the installed version: " +
			"--rollback goes back to the package the last upgrade kept"},
		{[]string{"cloudflared", "--version", "2026.9.0", "--yes"}, "cloudflared 2026.9.0 is denied: cloudflare/cloudflared#1737"},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			u := newUpgradeRig(t)

			res := u.r.run("", append([]string{"upgrade"}, tt.args...)...)

			require.EqualError(t, res.err, tt.want)
			require.Empty(t, u.host.commands())
		})
	}
}

func TestUpgradeHasItsHelp(t *testing.T) {
	cmd := newRootCmdWith(testEnv())
	up, _, err := cmd.Find([]string{"upgrade"})
	require.NoError(t, err)
	require.Contains(t, up.Long, "pco appliance recover")
	require.Contains(t, up.Long, "pco-pre-upgrade-<YYYYMMDD>")
	examples := 0
	for line := range strings.Lines(up.Example) {
		if strings.HasPrefix(strings.TrimSpace(line), "pco upgrade") {
			examples++
		}
	}
	require.GreaterOrEqual(t, examples, 2)
	require.LessOrEqual(t, examples, 5)
}

func TestTheVersionAsJSONNamesTheSchemaOfTheStore(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")

	res := r.run("", "version", "--json")

	require.NoError(t, res.err)
	require.JSONEq(t, fmt.Sprintf(`{"version":"dev","commit":"none","date":"unknown","schemaVersion":%d}`, store.SchemaVersion()), res.out)
	require.Equal(t, "{\n  \"version\": \"dev\",\n  \"commit\": \"none\",\n  \"date\": \"unknown\",\n  \"schemaVersion\": 1\n}\n", res.out)
}

// A daemon with the overrides of pco upgrade in its environment says so in
// every state: a machine set up for tests.
func TestTheDaemonCarriesTheUpgradeOverrideAsAProblem(t *testing.T) {
	d := startOverriddenDaemon(t, map[string]string{"PCO_UPGRADE_KEYRING": "/root/test-key.gpg"})
	line := "the release host or key is overridden (PCO_UPGRADE_BASE, PCO_UPGRADE_KEYRING); this is for tests only"

	require.Contains(t, d.errOut.String(), line)
	require.Eventually(t, func() bool {
		st, err := d.client.Status(t.Context())
		return err == nil && !st.FinishedAt.IsZero() && containsLine(st, line)
	}, 10*time.Second, 10*time.Millisecond)
}

func TestTheDaemonWithoutTheUpgradeOverrideHasNoSuchProblem(t *testing.T) {
	d := startOverriddenDaemon(t, nil)

	require.Eventually(t, func() bool {
		st, err := d.client.Status(t.Context())
		return err == nil && !st.FinishedAt.IsZero()
	}, 10*time.Second, 10*time.Millisecond)
	st, err := d.client.Status(t.Context())
	require.NoError(t, err)
	require.False(t, containsLine(st, upgrade.OverrideLine))
}
