//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The appliance profile installs pco as an appliance in a container of the
// node, from the snapshot's package unpacked into the run's directory and the
// snapshot's template, and runs the scenarios against it through pct exec.
// The fake Cloudflare stays in the suite's process on the node; pco accepts
// the override of the API for a loopback address only, so a relay in the
// appliance listens on its loopback and forwards to the fake.
const (
	relayListen = "127.0.0.1:8787"
	relayURL    = "http://" + relayListen + "/client/v4"
	relayPath   = "/usr/local/bin/e2e-relay"
	relayUnit   = "e2e-relay.service"
	pcoDropIn   = "/etc/systemd/system/pco.service.d/e2e.conf"
	tokenInside = "/root/e2e-token"
	identityOK  = "/run/pco-appliance/identity-ok"
	stateRoot   = "/var/lib/pco"

	// applianceFile names the appliance the run installed, the range of
	// its guests, the mode of the run and the install, as lines vmid=<id>,
	// range=<first>-<last>, mode=fake or mode=real and install=<id>; it puts
	// cleanup.sh into its appliance mode.
	applianceFile = markerDir + "/appliance"
	// tagsSaved holds the registered tags before the install, which the
	// uninstall puts back; cleanup.sh puts them back from it after a run that
	// was cut off.
	tagsSaved = "/run/pco-e2e-registered-tags"
	// principalsFile says that the run made its test principals, user
	// pcotest-e2e@pve, role PCOTESTNET and pool pcotest-e2e.
	principalsFile = markerDir + "/principals"
	// firewallFile holds the firewall options of the datacenter and the node
	// before the managed scenarios changed them.
	firewallOptsFile = markerDir + "/firewall-options"

	testUser = "pcotest-e2e@pve"
	testRole = "PCOTESTNET"
	testPool = "pcotest-e2e"

	installTimeout = 15 * time.Minute
)

// applianceRun is what the appliance profile has besides the suite.
type applianceRun struct {
	vmid int
	// nodePco is pco of the snapshot's package, unpacked for the run: the
	// installer runs from it on the node, as install.sh hands over to it.
	nodePco   string
	template  string
	checksums string
	relay     string
	// real is the layout against the real API: net0 on apiBridge with an
	// uplink, by DHCP, and the guests' card net1 on the bridge of the run.
	real      bool
	apiBridge string
	// guestNet and guestIface are the appliance's card on the bridge of the
	// run, in its configuration and inside.
	guestNet, guestIface string
	// segNet is the card of the second segment of A-S13.
	segNet string
	node   string
	major  int // of Proxmox VE
	// installed says that the appliance is there to uninstall; copies are
	// the copies of it the scenarios made, which go first.
	installed bool
	copies    map[int]bool
	// stamp is a file made when the run began: what is newer under /etc
	// and /usr was changed by the run.
	stamp  string
	before nodeState
	tags   []string
	// principals says that the run made its test principals; grants are
	// the ACL lines it added for them and for the managed guests, taken
	// back at the end.
	principals bool
	grants     [][]string
	// firewall is what the managed scenarios changed of the node's
	// firewall, to be put back.
	firewall *firewallOptions
	// token is the Cloudflare token of the run, which the repairs of A-S9
	// read from a file on the node, as an admin gives it.
	token string
}

// newAppliance prepares the node and installs the appliance: the node's
// address on the bridge of the run, the fake listening there, the appliance
// installed without a Cloudflare token, the relay and the drop-in that point
// the daemon at it, the credential added inside and the settings of the run.
// Everything is undone when t ends, whatever happened, and recorded under
// markerDir for cleanup.sh until then.
func (s *suite) newAppliance(t *testing.T) {
	a := &applianceRun{vmid: s.lay.appliance, guestNet: "net0", guestIface: "eth0", segNet: "net1", copies: map[int]bool{}}
	s.app = a
	s.disk = envOr("PCO_E2E_STORAGE", "pcotest-zfs")
	s.at = place{vmid: a.vmid, pco: "pco"}
	s.appliancePreflight(t)
	s.findArtefacts(t)

	a.stamp = filepath.Join(s.dir, "began")
	require.NoError(t, os.WriteFile(a.stamp, nil, 0o600))
	// The modification time of the stamp is what -newer compares with: a
	// second back, as find compares whole timestamps.
	past := time.Now().Add(-time.Second)
	require.NoError(t, os.Chtimes(a.stamp, past, past))
	a.before = s.nodeState(t)
	a.tags = s.registeredTags(t)

	// Cleanups run last first: the copies and the appliance go before the
	// guests, so that their going is nothing the daemon acts on; the bridges
	// and the node's address after them.
	t.Cleanup(func() { s.removeBridges(t) })
	t.Cleanup(func() { s.restoreSysctls(t) })
	t.Cleanup(func() { s.stopFirewallService(t) })
	t.Cleanup(func() { s.removeAddress(t) })
	t.Cleanup(func() { s.removeArchives(t) })
	t.Cleanup(func() { s.removePrincipals(t) })
	t.Cleanup(func() { s.destroyGuests(t) })
	t.Cleanup(func() { s.uninstallAppliance(t) })
	t.Cleanup(func() { s.destroyPconetGuests(t) })
	t.Cleanup(func() { s.restoreRegisteredTags(t) })
	t.Cleanup(func() { s.restoreFirewall(t) })
	t.Cleanup(func() { s.removeGrants(t) })
	t.Cleanup(func() { s.diagnose(t) })

	s.addAddress(t)
	a.token = s.chooseCloud(t)
	if s.fake != nil {
		s.at.env = []string{overrideOf + "=" + relayURL}
	} else {
		require.True(t, a.real, "against the real API the appliance needs a bridge with an uplink: name it with PCO_E2E_APPLIANCE_BRIDGE")
	}
	s.markAppliance(t)
	require.NoError(t, os.WriteFile(tagsSaved, []byte(strings.Join(a.tags, ";")+"\n"), 0o600))
	s.installAppliance(t)
	s.prepareAppliance(t, a.token)
}

// appliancePreflight skips where the suite cannot run, and refuses a node
// where it would change what it did not make: pco set up on the node, its
// user, its pool, a guest in the range, a guest with the gate tag, a bridge
// that leads off the node.
func (s *suite) appliancePreflight(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the end-to-end suite runs as root on a Proxmox VE node")
	}
	for _, bin := range []string{"/usr/sbin/pct", "/usr/bin/dpkg-deb", "/usr/bin/pvesh", "/usr/sbin/pveum"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("the appliance profile needs %s: %v", bin, err)
		}
	}
	if entries, err := os.ReadDir(markerDir); err == nil && len(entries) > 0 {
		t.Fatalf("a run of the suite left %s: %s", filepath.Join(markerDir, entries[0].Name()), cleanupHint)
	}
	if _, found, err := installed(); err != nil || found {
		t.Fatalf("pco is set up on this node (or its store cannot be read: %v): the appliance profile needs a node without it", err)
	}
	if exists("/etc/pve/corosync.conf") {
		t.Fatal("this node is in a cluster: the suite runs on a node of its own, where nothing it does reaches another")
	}
	if out, _ := s.run("pveum", "user", "list", "--output-format", "json"); strings.Contains(out, `"pco@pve"`) {
		t.Fatal("the user pco@pve exists: the appliance profile needs a node without pco's user, which its uninstall removes")
	}
	if _, err := s.run("pvesh", "get", "/pools/"+applianceinstallPool); err == nil {
		t.Fatalf("the pool %s exists: the appliance profile needs a node without it", applianceinstallPool)
	}
	for _, list := range []string{"pct", "qm"} {
		for line := range strings.Lines(s.must(t, list, "list")) {
			id, err := strconv.Atoi(firstOf(strings.Fields(line)))
			if err == nil && s.lay.contains(id) {
				t.Fatalf("guest %d exists; the appliance profile needs the ids %d to %d free, and leaves that guest alone",
					id, s.lay.first, s.lay.last)
			}
		}
	}
	s.refuseGateTag(t)
	s.refuseBridge(t)
	if _, err := s.run("ip", "link", "show", "dev", segBridge); err == nil {
		t.Fatalf("the bridge %s exists: the suite makes it for the run and removes it", segBridge)
	}
	s.must(t, "pvesm", "status", "--storage", s.disk)
	s.app.node = strings.TrimSpace(s.must(t, "hostname"))
	ver := s.must(t, "pveversion")
	s.app.major = 9
	if strings.HasPrefix(ver, "pve-manager/8.") {
		s.app.major = 8
	}
}

// applianceinstallPool is the pool of the appliances.
const applianceinstallPool = "pco"

// findArtefacts finds the snapshot's package, template and checksums, and the
// relay, and unpacks the package for the installer.
func (s *suite) findArtefacts(t *testing.T) {
	a := s.app
	one := func(env, pattern string, required bool) string {
		if v := os.Getenv(env); v != "" {
			abs, err := filepath.Abs(v)
			require.NoError(t, err)
			return abs
		}
		matches, _ := filepath.Glob(pattern)
		if len(matches) != 1 {
			if required {
				t.Fatalf("name it with %s, or put the one %s into the working directory (found %d)", env, pattern, len(matches))
			}
			return ""
		}
		abs, err := filepath.Abs(matches[0])
		require.NoError(t, err)
		return abs
	}
	deb := one("PCO_E2E_DEB", "pco_*_amd64.deb", true)
	a.template = one("PCO_E2E_APPLIANCE_TEMPLATE", "pco-appliance_*_amd64.tar.zst", true)
	a.checksums = one("PCO_E2E_CHECKSUMS", "checksums.txt", false)
	a.relay = one("PCO_E2E_RELAY", "e2e-relay", false)
	unpacked := filepath.Join(s.dir, "deb")
	s.must(t, "dpkg-deb", "-x", deb, unpacked)
	a.nodePco = filepath.Join(unpacked, "usr", "bin", "pco")
	if _, err := os.Stat(a.nodePco); err != nil {
		t.Fatalf("%s has no usr/bin/pco: %v", deb, err)
	}
	if bridge := os.Getenv("PCO_E2E_APPLIANCE_BRIDGE"); bridge != "" {
		a.real, a.apiBridge = true, bridge
		a.guestNet, a.guestIface, a.segNet = "net1", "eth1", "net2"
	}
	t.Logf("the installer: %s; the template: %s; checksums: %s", a.nodePco, a.template, dashIfEmpty(a.checksums))
}

func dashIfEmpty(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

// nodePcoRun runs the node's pco with the environment of the run, which
// carries the override of the API against the fake: the installer forwards
// it to what it runs in the appliance.
func (s *suite) nodePcoRun(timeout time.Duration, args ...string) (string, error) {
	return s.combinedWithin(timeout, s.app.nodePco, args...)
}

// installAppliance runs the installer as an admin does, without a
// Cloudflare token.
func (s *suite) installAppliance(t *testing.T) {
	a := s.app
	args := []string{"appliance", "install", "--storage", s.disk, "--vmid", strconv.Itoa(a.vmid), "--template", a.template, "--yes"}
	if a.checksums != "" {
		args = append(args, "--checksums", a.checksums)
	}
	if a.real {
		args = append(args, "--bridge", a.apiBridge)
	} else {
		args = append(args, "--bridge", bridge, "--ip", s.lay.applianceAddr+"/24,gw="+s.lay.applianceGW)
	}
	a.installed = true
	out, err := s.nodePcoRun(installTimeout, args...)
	t.Logf("pco %s:\n%s", strings.Join(args, " "), out)
	require.NoError(t, err)
	if a.real {
		// The guests are reached on the bridge of the run, through a second
		// card; net0 is the way to Cloudflare and the API.
		s.must(t, "pct", "set", strconv.Itoa(a.vmid), "--"+a.guestNet,
			"name="+a.guestIface+",bridge="+bridge+",ip="+s.lay.applianceAddr+"/24")
	}
	s.waitAppliance(t, a.vmid)
}

// prepareAppliance points the daemon at the fake through the relay, adds the
// credential inside and gives the install the settings of the run.
func (s *suite) prepareAppliance(t *testing.T, token string) {
	a := s.app
	if s.fake != nil {
		s.pushRelay(t, a.vmid, relayUnit, relayListen, s.fake.srv.address())
		s.pushFile(t, a.vmid, pcoDropIn, "[Service]\nEnvironment="+overrideOf+"="+relayURL+"\n", "0644")
		s.hereMust(t, "systemctl", "daemon-reload")
		s.hereMust(t, "systemctl", "restart", "pco")
	}
	s.pushFile(t, a.vmid, tokenInside, token+"\n", "0600")
	out, err := s.pcoCombined("credential", "add", "--label", "e2e", "--token-file", tokenInside)
	_, _ = s.here("rm", "-f", tokenInside)
	t.Logf("pco credential add:\n%s", out)
	require.NoError(t, err)

	inst := s.applianceInstall(t)
	s.install = inst.ID
	s.markAppliance(t)
	s.editSettings(t, func(v *store.Settings) {
		v.PollInterval = store.Duration(pollInterval)
		v.Grace = store.Duration(grace)
	})
	asked := time.Now()
	s.sync(t)
	s.waitState(t, "a complete cycle with the credential", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(asked) && st.Complete && !st.FinishedAt.IsZero()
	})
}

// markAppliance records the appliance for cleanup.sh.
func (s *suite) markAppliance(t testing.TB) {
	t.Helper()
	marker(t, applianceFile, fmt.Sprintf("vmid=%d\nrange=%d-%d\nmode=%s\ninstall=%s\n",
		s.app.vmid, s.lay.first, s.lay.last, s.mode(), s.install))
}

// pushFile writes a file into a running container with the mode given.
func (s *suite) pushFile(t testing.TB, vmid int, path, content, perms string) {
	t.Helper()
	local := filepath.Join(s.dir, fmt.Sprintf("push-%d-%d-%s", vmid, time.Now().UnixNano(), filepath.Base(path)))
	require.NoError(t, os.WriteFile(local, []byte(content), 0o600))
	defer func() { _ = os.Remove(local) }()
	s.must(t, "pct", "exec", strconv.Itoa(vmid), "--keep-env", "0", "--", "mkdir", "-p", filepath.Dir(path))
	s.must(t, "pct", "push", strconv.Itoa(vmid), local, path, "--perms", perms)
}

// pushRelay installs the relay in the container as an enabled unit, so that
// clones and restores of it carry it, listening on listen and forwarding to
// to.
func (s *suite) pushRelay(t testing.TB, vmid int, unit, listen, to string) {
	t.Helper()
	require.NotEmpty(t, s.app.relay, "the relay: build it with make e2e-binaries and put bin/e2e-relay beside the suite, or name it with PCO_E2E_RELAY")
	id := strconv.Itoa(vmid)
	s.must(t, "pct", "push", id, s.app.relay, relayPath, "--perms", "0755")
	s.pushFile(t, vmid, "/etc/systemd/system/"+unit, fmt.Sprintf(`[Unit]
Description=Relay of the end-to-end suite of pco, %s to %s
After=network.target

[Service]
ExecStart=%s -listen %s -to %s
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
`, listen, to, relayPath, listen, to), "0644")
	in := s.inCT(vmid)
	in.hereMust(t, "systemctl", "daemon-reload")
	in.hereMust(t, "systemctl", "enable", "--now", unit)
}

// inCT is the suite as seen from another container: a copy of the appliance.
// It shares the suite's records; it is for what reads and runs commands.
func (s *suite) inCT(vmid int) *suite {
	c := *s
	c.at.vmid = vmid
	return &c
}

// waitAppliance waits until a container runs commands and its pco answers.
func (s *suite) waitAppliance(t testing.TB, vmid int) {
	t.Helper()
	s.waitExec(t, vmid)
	in := s.inCT(vmid)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if _, err := in.tryState(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			_, err := in.tryState()
			t.Fatalf("pco in lxc/%d does not answer: %v", vmid, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// scratchStore pulls files of a container's store into a directory laid out
// as /var/lib/pco and opens it as a store: what pco keeps there is read and
// written with pco's own code, never by hand. A file the container does not
// have is left out: pct pull would make an empty one.
func (s *suite) scratchStore(t testing.TB, vmid int, files ...string) (*store.Store, string) {
	t.Helper()
	root, err := os.MkdirTemp(s.dir, "store-")
	require.NoError(t, err)
	id := strconv.Itoa(vmid)
	for _, f := range files {
		remote := stateRoot + "/" + f
		if _, err := s.run("pct", "exec", id, "--keep-env", "0", "--", "test", "-f", remote); err != nil {
			continue
		}
		local := filepath.Join(root, f)
		require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o700))
		s.must(t, "pct", "pull", id, remote, local)
	}
	st, err := store.Open(store.Paths{Cluster: filepath.Join(root, "cluster"), Private: filepath.Join(root, "private"), Local: root})
	require.NoError(t, err)
	return st, root
}

// editApplianceSettings pulls the settings into a scratch store, changes
// them there, pushes the file back beside the one in the container and
// renames it over that one, as an atomic write does.
func (s *suite) editApplianceSettings(t testing.TB, edit func(*store.Settings)) {
	t.Helper()
	const file = "cluster/meta/settings.json"
	st, root := s.scratchStore(t, s.at.vmid, file)
	settings, err := st.Settings()
	require.NoError(t, err)
	edit(&settings)
	require.NoError(t, st.SaveSettings(settings))
	remote := stateRoot + "/" + file
	s.must(t, "pct", "push", strconv.Itoa(s.at.vmid), filepath.Join(root, file), remote+".e2e", "--perms", "0600")
	s.hereMust(t, "mv", "-f", remote+".e2e", remote)
}

// applianceSettings reads the settings of the appliance.
func (s *suite) applianceSettings(t testing.TB) store.Settings {
	t.Helper()
	st, _ := s.scratchStore(t, s.at.vmid, "cluster/meta/settings.json")
	v, err := st.Settings()
	require.NoError(t, err)
	return v
}

// applianceInstall reads the install record of the appliance.
func (s *suite) applianceInstall(t testing.TB) store.Install {
	t.Helper()
	st, _ := s.scratchStore(t, s.at.vmid, "cluster/meta/install.json")
	inst, found, err := st.Install()
	require.NoError(t, err)
	require.True(t, found, "lxc/%d has no install record", s.at.vmid)
	return inst
}

// applianceNodeAddrs reads the node addresses the appliance saved.
func (s *suite) applianceNodeAddrs(t testing.TB) []string {
	t.Helper()
	st, _ := s.scratchStore(t, s.at.vmid, "meta/node-addrs.json", "cluster/meta/nodeaddrs.json")
	addrs, err := st.NodeAddrs()
	require.NoError(t, err)
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return out
}

// leader reads the writer of leader.json in the container, found false when
// it has none.
func (s *suite) leader(t testing.TB) (planner.Writer, bool) {
	t.Helper()
	st, _ := s.scratchStore(t, s.at.vmid, "cluster/meta/leader.json")
	r, _, found, err := st.Leader()
	require.NoError(t, err)
	return r.Writer, found
}

// sentinel is the highest generation the sentinels of the install's tunnel
// carry at Cloudflare, 0 without a tunnel or a sentinel.
func (s *suite) sentinel(t testing.TB) int {
	t.Helper()
	tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install))
	if !found {
		return 0
	}
	gen := 0
	for _, r := range s.cloud.Ingress(t, tn.ID) {
		if w, ok := planner.ParseSentinel(r.Hostname); ok && w.InstallID == s.install {
			gen = max(gen, w.Generation)
		}
	}
	return gen
}

// calls is how many calls the fake has seen, 0 against the real API.
func (s *suite) calls() int {
	if s.fake == nil {
		return 0
	}
	return len(s.fake.f.Calls())
}

// cycles asks for n cycles one after the other and waits for each to finish
// complete or not.
func (s *suite) cycles(t testing.TB, n int) engine.State {
	t.Helper()
	var st engine.State
	for range n {
		asked := time.Now()
		s.sync(t)
		st = s.waitState(t, "a cycle", 2*time.Minute, func(st engine.State) bool { return st.At.After(asked) && !st.FinishedAt.IsZero() })
	}
	return st
}

// pctConfig returns the current configuration of a container, without its
// snapshots and its pending changes, by key.
func (s *suite) pctConfig(t testing.TB, vmid int) map[string]string {
	t.Helper()
	out := s.must(t, "pct", "config", strconv.Itoa(vmid), "--current", "1")
	cfg := map[string]string{}
	for line := range strings.Lines(out) {
		if strings.HasPrefix(line, "[") {
			break
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if ok {
			cfg[k] = v
		}
	}
	return cfg
}

// cardMAC is the MAC of a card of a container.
func (s *suite) cardMAC(t testing.TB, vmid int, card string) string {
	t.Helper()
	mac := cardValue(card+": "+s.pctConfig(t, vmid)[card], "hwaddr")
	require.NotEmpty(t, mac, "lxc/%d has no MAC on %s", vmid, card)
	return mac
}

// applianceMAC is the MAC of the appliance's card on the bridge of the run.
func (s *suite) applianceMAC(t testing.TB) string {
	t.Helper()
	return s.cardMAC(t, s.app.vmid, s.app.guestNet)
}

// running says whether a container runs.
func (s *suite) running(vmid int) bool {
	out, _ := s.run("pct", "status", strconv.Itoa(vmid))
	return strings.Contains(out, "running")
}

// uninstallAppliance removes the copies of the appliance the scenarios left,
// then the appliance with what it has at Cloudflare. The appliance runs for
// the purge, which goes through it.
func (s *suite) uninstallAppliance(t testing.TB) {
	a := s.app
	for vmid := range a.copies {
		out, err := s.nodePcoRun(installTimeout, "appliance", "uninstall", "--vmid", strconv.Itoa(vmid), "--yes", "--keep-cloudflare")
		t.Logf("pco appliance uninstall of the copy lxc/%d:\n%s", vmid, out)
		if err != nil {
			t.Errorf("uninstalling the copy lxc/%d: %v", vmid, err)
			continue
		}
		delete(a.copies, vmid)
	}
	if !a.installed {
		return
	}
	if !s.running(a.vmid) {
		if _, err := s.run("pct", "start", strconv.Itoa(a.vmid)); err != nil {
			t.Logf("starting lxc/%d for the purge: %v", a.vmid, err)
		}
	}
	args := []string{"appliance", "uninstall", "--vmid", strconv.Itoa(a.vmid), "--yes", "--purge-cloudflare"}
	if s.managed {
		args = append(args, "--detach-managed")
	}
	var err error
	for attempt := 1; attempt <= purgeAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(purgeRetry)
		}
		var out string
		out, err = s.nodePcoRun(installTimeout, args...)
		t.Logf("pco %s, attempt %d of %d:\n%s", strings.Join(args, " "), attempt, purgeAttempts, out)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Errorf("pco appliance uninstall: %v; %s names the appliance, and cleanup.sh removes it", err, applianceFile)
		return
	}
	a.installed = false
	if s.managed {
		s.managed = false
		unmark(t, managedMarker)
	}
	unmark(t, applianceFile)
	if err := os.Remove(tagsSaved); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removing %s: %v", tagsSaved, err)
	}
	if s.install == "" {
		return
	}
	if left := s.leftAtCloudflare(t); len(left) > 0 {
		t.Errorf("left at Cloudflare after the purge:\n  %s", strings.Join(left, "\n  "))
	}
}

// nodeState is what the appliance must leave as it found it on the node.
type nodeState struct {
	packages string
	units    string
	ruleset  string
}

var counterValuesText = regexp.MustCompile(`packets \d+ bytes \d+`)

func (s *suite) nodeState(t testing.TB) nodeState {
	t.Helper()
	ruleset, err := s.run("nft", "list", "ruleset")
	require.NoError(t, err)
	return nodeState{
		packages: s.must(t, "dpkg-query", "-W", "-f", "${Package} ${Version} ${Status}\n"),
		units:    s.must(t, "systemctl", "list-unit-files", "--no-legend", "--no-pager"),
		ruleset:  counterValuesText.ReplaceAllString(ruleset, "packets 0 bytes 0"),
	}
}

// requireNodeAsBefore checks that the node is as the run found it: its
// packages, unit files and ruleset, nothing under /etc or /usr newer than the
// run, and no journal of the installer.
func (s *suite) requireNodeAsBefore(t *testing.T, rulesetToo bool) {
	t.Helper()
	a := s.app
	now := s.nodeState(t)
	require.Equal(t, a.before.packages, now.packages, "the packages of the node changed")
	require.Equal(t, a.before.units, now.units, "the unit files of the node changed")
	if rulesetToo {
		require.Equal(t, a.before.ruleset, now.ruleset, "the ruleset of the node changed")
	}
	for _, dir := range []string{"/etc", "/usr"} {
		newer := strings.TrimSpace(s.must(t, "find", dir, "-xdev", "-newer", a.stamp, "-print"))
		require.Empty(t, newer, "files under %s changed during the run", dir)
	}
	if entries, err := os.ReadDir("/root/.pco-appliance-install"); err == nil {
		require.Empty(t, entries, "the installer left a journal")
	}
	runs, _ := filepath.Glob("/run/pco-appliance-install-*")
	require.Empty(t, runs, "the installer left a directory under /run")
}

// pveJSON runs a pvesh or pveum command with JSON output into v.
func (s *suite) pveJSON(t testing.TB, v any, name string, args ...string) {
	t.Helper()
	out := s.must(t, name, append(args, "--output-format", "json")...)
	require.NoError(t, json.Unmarshal([]byte(out), v), out)
}

// acl is a line of the access control list as pveum lists it.
type acl struct {
	Path      string `json:"path"`
	RoleID    string `json:"roleid"`
	Type      string `json:"type"`
	UGID      string `json:"ugid"`
	Propagate int    `json:"propagate"`
}

func (s *suite) acls(t testing.TB) []acl {
	t.Helper()
	var out []acl
	s.pveJSON(t, &out, "pveum", "acl", "list")
	return out
}

// grant adds an ACL line, which the run takes back at its end.
func (s *suite) grant(t testing.TB, path, flag, who, role string) {
	t.Helper()
	s.must(t, "pveum", "acl", "modify", path, flag, who, "--roles", role)
	s.app.grants = append(s.app.grants, []string{path, flag, who, role})
}

// ungrant deletes an ACL line the run added for its test user.
func (s *suite) ungrant(t testing.TB, path, role string) {
	t.Helper()
	s.must(t, "pveum", "acl", "delete", path, "--users", testUser, "--roles", role)
	line := []string{path, "--users", testUser, role}
	s.app.grants = slices.DeleteFunc(s.app.grants, func(g []string) bool { return slices.Equal(g, line) })
}

func (s *suite) removeGrants(t testing.TB) {
	for _, g := range s.app.grants {
		if _, err := s.run("pveum", "acl", "delete", g[0], g[1], g[2], "--roles", g[3]); err != nil {
			t.Logf("deleting the ACL line %v: %v", g, err)
		}
	}
	s.app.grants = nil
}

// ensurePrincipals makes the test user, its role and its pool, unless the
// run has them.
func (s *suite) ensurePrincipals(t testing.TB) {
	t.Helper()
	if s.app.principals {
		return
	}
	marker(t, principalsFile, testUser+" "+testRole+" "+testPool+"\n")
	s.app.principals = true
	s.must(t, "pveum", "user", "add", testUser, "--comment", "pco end-to-end suite")
	s.must(t, "pveum", "role", "add", testRole, "--privs", "VM.Config.Network")
	s.must(t, "pveum", "pool", "add", testPool, "--comment", "pco end-to-end suite")
}

// removePrincipals removes the test principals: the tokens first, which the
// deletion of their user would leave in the token file, then the user, the
// role and the pool.
func (s *suite) removePrincipals(t testing.TB) {
	if !s.app.principals {
		return
	}
	failed := false
	for _, tok := range []string{"np", "ps"} {
		_, _ = s.run("pveum", "user", "token", "remove", testUser, tok)
	}
	for _, cmd := range [][]string{
		{"pveum", "user", "delete", testUser},
		{"pveum", "role", "delete", testRole},
		{"pveum", "pool", "delete", testPool},
	} {
		if _, err := s.run(cmd[0], cmd[1:]...); err != nil && !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("%s: %v", strings.Join(cmd, " "), err)
			failed = true
		}
	}
	if !failed {
		s.app.principals = false
		unmark(t, principalsFile)
	}
}

// dumps are the backups of the run's containers.
func (s *suite) dumps() []string {
	var out []string
	for vmid := s.lay.first; vmid <= s.lay.last; vmid++ {
		found, _ := filepath.Glob(fmt.Sprintf("/var/lib/vz/dump/vzdump-lxc-%d-*", vmid))
		out = append(out, found...)
	}
	return out
}

func (s *suite) removeArchives(t testing.TB) {
	for _, f := range s.dumps() {
		if err := os.Remove(f); err != nil {
			t.Errorf("removing %s: %v", f, err)
		}
	}
}

// firewallOptions are the firewall options the managed scenarios change, as
// they were: "" when unset, else 0 or 1. The datacenter's firewall is turned
// on for the run with the node's own rules off, so that the node stays open
// to the appliance, to the fake and to whoever runs the suite, while the
// guests' rules are in force.
type firewallOptions struct {
	datacenter string
	nodeEnable string
	nftables   string
}

// recordFirewall reads the firewall options before the run changes them,
// once, and records them for cleanup.sh.
func (s *suite) recordFirewall(t testing.TB) {
	t.Helper()
	if s.app.firewall != nil {
		return
	}
	read := func(path, key string) string {
		var opts map[string]any
		s.pveJSON(t, &opts, "pvesh", "get", path)
		v, ok := opts[key]
		if !ok {
			return ""
		}
		return fmt.Sprint(v)
	}
	node := "/nodes/" + s.app.node + "/firewall/options"
	f := &firewallOptions{
		datacenter: read("/cluster/firewall/options", "enable"),
		nodeEnable: read(node, "enable"),
		nftables:   read(node, "nftables"),
	}
	marker(t, firewallOptsFile, fmt.Sprintf("datacenter=%s\nnode=%s\nnftables=%s\n", f.datacenter, f.nodeEnable, f.nftables))
	s.app.firewall = f
}

// restoreFirewall puts the firewall options back as they were.
func (s *suite) restoreFirewall(t testing.TB) {
	f := s.app.firewall
	if f == nil {
		return
	}
	failed := false
	set := func(path, key, value string) {
		args := []string{"set", path}
		if value == "" {
			args = append(args, "--delete", key)
		} else {
			args = append(args, "--"+key, value)
		}
		if _, err := s.run("pvesh", args...); err != nil {
			t.Errorf("putting %s %s back to %q: %v", path, key, value, err)
			failed = true
		}
	}
	node := "/nodes/" + s.app.node + "/firewall/options"
	set("/cluster/firewall/options", "enable", f.datacenter)
	set(node, "enable", f.nodeEnable)
	set(node, "nftables", f.nftables)
	if !failed {
		s.app.firewall = nil
		unmark(t, firewallOptsFile)
	}
}

// activeConnectors lists the connector units that are active where pco runs.
func (s *suite) activeConnectors(t testing.TB) []string {
	t.Helper()
	out := s.hereMust(t, "systemctl", "list-units", "--state=active", "--plain", "--no-legend", "--no-pager", "pco-cloudflared@*")
	var units []string
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) > 0 {
			units = append(units, f[0])
		}
	}
	return units
}

// flagged says whether the identity flag the connectors start behind is
// there.
func (s *suite) flagged() bool {
	_, err := s.here("test", "-e", identityOK)
	return err == nil
}

// requireServesNothing checks what a copy, or an appliance a principal can
// reach into, must be: no connector active, no flag, no target in the egress
// set.
func (s *suite) requireServesNothing(t testing.TB) {
	t.Helper()
	require.Empty(t, s.activeConnectors(t), "connectors run")
	require.False(t, s.flagged(), "%s is there", identityOK)
	_, targets, _ := s.egress(t)
	require.Empty(t, targets, "the egress set has targets")
}
