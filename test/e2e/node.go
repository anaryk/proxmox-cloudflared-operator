//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// What the suite changes on the node is recorded under markerDir, so that
// cleanup.sh can undo it after a run that was cut off, and touches nothing
// the suite did not make.
const (
	markerDir = "/var/lib/pco-e2e"
	// ownedFile names the install the suite set up and the mode of the
	// run, as lines install=<id> and mode=fake or mode=real. It stays after
	// the run, so that cleanup.sh knows the package is the suite's to purge.
	ownedFile = markerDir + "/owned"
	// tokenFile is the token pco setup reads.
	tokenFile = markerDir + "/cf-token"
	// tablesDir holds the tables of others that S14 flushes, until they
	// are back.
	tablesDir = markerDir + "/tables"
	// foreignFile names the record S10 makes in a real zone, until it is
	// deleted or pco adopted it.
	foreignFile   = markerDir + "/foreign"
	addressFile   = markerDir + "/address-added"
	bridgeWasDown = markerDir + "/bridge-was-down"

	cleanupHint = "run test/e2e/cleanup.sh, which removes what a run of the suite left"
)

// marker writes a file of the marker directory.
func marker(t testing.TB, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(markerDir, 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func unmark(t testing.TB, path string) {
	t.Helper()
	if err := os.RemoveAll(path); err != nil {
		t.Errorf("removing %s: %v", path, err)
	}
}

// ownedInstall is the install ownedFile names, or "".
func ownedInstall() string {
	raw, err := os.ReadFile(ownedFile)
	if err != nil {
		return ""
	}
	for line := range strings.Lines(string(raw)) {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "install="); ok {
			return id
		}
	}
	return ""
}

func (s *suite) own(t testing.TB) {
	t.Helper()
	marker(t, ownedFile, fmt.Sprintf("install=%s\nmode=%s\n", s.install, s.mode()))
}

func (s *suite) mode() string {
	if s.fake != nil {
		return "fake"
	}
	return "real"
}

// preflight skips where the suite cannot run, and refuses a node where it
// would change what it did not make. A refusal points at cleanup.sh only for
// what a run of the suite left.
func (s *suite) preflight(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the end-to-end suite runs as root on a Proxmox VE node")
	}
	for _, bin := range []string{"/usr/sbin/pct", "/usr/sbin/qm", s.pco} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("the end-to-end suite needs %s: %v", bin, err)
		}
	}
	s.refuseLeftovers(t)
	if exists("/etc/pve/corosync.conf") {
		t.Fatal("this node is in a cluster: the suite runs on a node of its own, where nothing it does reaches another")
	}
	s.refuseTakenIDs(t)
	s.refuseGateTag(t)
	s.refuseBridge(t)
	s.must(t, "pvesm", "status", "--storage", s.disk)
}

// refuseLeftovers refuses a node with pco set up, and one where a run of the
// suite was cut off.
func (s *suite) refuseLeftovers(t *testing.T) {
	inst, found, err := installed()
	require.NoError(t, err)
	switch {
	case found && inst == ownedInstall():
		t.Fatalf("a run of the suite left its install %s set up: %s", inst, cleanupHint)
	case found:
		t.Fatalf("pco is set up on this node as install %s, which the suite did not make: it needs a node "+
			"without pco set up, and leaves that install alone", inst)
	}
	entries, err := os.ReadDir(markerDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(ownedFile) {
			t.Fatalf("a run of the suite was cut off and left %s: %s", filepath.Join(markerDir, e.Name()), cleanupHint)
		}
	}
}

// installed is the id of the install set up on this node. It reads the file
// as cleanup.sh does, and opens no store, which would tidy it.
func installed() (string, bool, error) {
	file := store.DefaultPaths().Cluster + "/meta/install.json"
	raw, err := os.ReadFile(file)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	var envelope struct {
		Data store.Install `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", true, fmt.Errorf("reading %s: %w", file, err)
	}
	return envelope.Data.ID, true, nil
}

// refuseTakenIDs refuses a node with a guest whose id the suite uses.
func (s *suite) refuseTakenIDs(t *testing.T) {
	for _, list := range []string{"pct", "qm"} {
		for line := range strings.Lines(s.must(t, list, "list")) {
			f := strings.Fields(line)
			id, err := strconv.Atoi(firstOf(f))
			if err != nil || id < firstVMID || id > lastVMID {
				continue
			}
			if list == "pct" && slices.Contains(f, "e2e-"+f[0]) && exists(ownedFile) {
				t.Fatalf("container %d of a run of the suite is left: %s", id, cleanupHint)
			}
			t.Fatalf("guest %d exists; the suite needs the ids %d to %d free, and leaves that guest alone", id, firstVMID, lastVMID)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstOf(f []string) string {
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// refuseGateTag refuses a node with a guest that carries the gate tag: the
// daemon would serve it as well, and against Cloudflare publish it.
func (s *suite) refuseGateTag(t *testing.T) {
	for _, dir := range []string{"/etc/pve/lxc", "/etc/pve/qemu-server"} {
		confs, err := filepath.Glob(dir + "/*.conf")
		require.NoError(t, err)
		for _, conf := range confs {
			raw, err := os.ReadFile(conf)
			require.NoError(t, err)
			if slices.Contains(tagsOf(string(raw)), gateTag) {
				t.Fatalf("%s carries the tag %s: the suite would publish that guest as well; it needs a node where no guest has it", conf, gateTag)
			}
		}
	}
}

// tagsOf returns the tags of a guest configuration, without those of its
// snapshots.
func tagsOf(conf string) []string {
	for line := range strings.Lines(conf) {
		if strings.HasPrefix(line, "[") {
			break
		}
		if rest, ok := strings.CutPrefix(line, "tags:"); ok {
			return strings.FieldsFunc(strings.ToLower(rest), func(r rune) bool { return r == ';' || r == ',' || r == ' ' || r == '\n' })
		}
	}
	return nil
}

// refuseBridge refuses a bridge that leads off the node, or that has the
// address of the run already.
func (s *suite) refuseBridge(t *testing.T) {
	link := s.must(t, "ip", "-o", "link", "show", "dev", bridge)
	flags, _, _ := strings.Cut(link[strings.Index(link, "<")+1:], ">")
	s.bridgeDown = !slices.Contains(strings.Split(flags, ","), "UP")
	ports, err := os.ReadDir("/sys/class/net/" + bridge + "/brif")
	require.NoError(t, err)
	for _, p := range ports {
		if !guestPort(p.Name()) {
			t.Fatalf("%s has the port %s, which is not a guest's: the suite gives the bridge an address and its guests "+
				"claim addresses of others, which must not reach a network beyond the node", bridge, p.Name())
		}
	}
	if addrs := s.must(t, "ip", "-4", "-o", "addr", "show", "dev", bridge); strings.Contains(addrs, "inet "+nodeAddr+"/") {
		t.Fatalf("%s has %s already, which the suite did not give it: it gives the bridge %s for the run and takes it away", bridge, nodeAddr, nodeCIDR)
	}
}

// guestPort says whether a bridge port is an interface of a guest, or of the
// firewall of one, as Proxmox names them.
func guestPort(name string) bool {
	for _, prefix := range []string{"veth", "tap", "fwpr", "fwln"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// addAddress gives the node its address on the bridge, and brings the
// bridge up for the run when it is down.
func (s *suite) addAddress(t *testing.T) {
	if s.bridgeDown {
		marker(t, bridgeWasDown, "")
		s.must(t, "ip", "link", "set", "dev", bridge, "up")
	}
	marker(t, addressFile, nodeCIDR+"\n")
	s.must(t, "ip", "addr", "add", nodeCIDR, "dev", bridge)
}

func (s *suite) removeAddress(t *testing.T) {
	if _, err := s.run("ip", "addr", "del", nodeCIDR, "dev", bridge); err != nil {
		t.Errorf("removing %s from %s: %v", nodeCIDR, bridge, err)
		return
	}
	unmark(t, addressFile)
	if !s.bridgeDown {
		return
	}
	if _, err := s.run("ip", "link", "set", "dev", bridge, "down"); err != nil {
		t.Errorf("taking %s down again: %v", bridge, err)
		return
	}
	unmark(t, bridgeWasDown)
}

func (s *suite) writeDropIn(t *testing.T) {
	require.NoError(t, os.MkdirAll(dropInDir, 0o755))
	unit := "[Service]\nEnvironment=" + s.env[0] + "\n"
	require.NoError(t, os.WriteFile(dropInFile, []byte(unit), 0o644))
	s.must(t, "systemctl", "daemon-reload")
}

func (s *suite) removeDropIn(t *testing.T) {
	err := os.Remove(dropInFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removing the drop-in: %v", err)
	}
	_ = os.Remove(dropInDir) // only when it is empty: it may hold drop-ins of the admin
	if _, err := s.run("systemctl", "daemon-reload"); err != nil {
		t.Errorf("reloading systemd: %v", err)
	}
}

// stopProxmoxAPI stops pveproxy until t ends. The start is registered first,
// and cleanup.sh starts it as well after a run that was cut off.
func (s *suite) stopProxmoxAPI(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := s.run("systemctl", "start", "pveproxy"); err != nil {
			t.Errorf("starting pveproxy again: %v", err)
		}
	})
	s.must(t, "systemctl", "stop", "pveproxy")
}

// saveTables saves the tables of the ruleset that are not pco's, which a
// flush takes as well, and loads those that do not come back by themselves
// again when t ends; cleanup.sh does so after a run that was cut off.
func (s *suite) saveTables(t *testing.T) {
	t.Helper()
	require.NoError(t, os.MkdirAll(tablesDir, 0o700))
	saved := map[string]string{}
	for line := range strings.Lines(s.must(t, "nft", "list", "tables")) {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != "table" || f[2] == "pco_egress" {
			continue
		}
		file := filepath.Join(tablesDir, fmt.Sprintf("table-%d.nft", len(saved)+1))
		require.NoError(t, os.WriteFile(file, []byte(s.must(t, "nft", "list", "table", f[1], f[2])), 0o600))
		saved[f[1]+" "+f[2]] = file
	}
	t.Logf("tables besides pco's: %d", len(saved))
	t.Cleanup(func() { s.restoreTables(t, saved) })
}

// restoreTables waits for whoever owns the tables to load them again, as the
// Proxmox firewall does, and loads the ones still missing then as they were.
func (s *suite) restoreTables(t *testing.T, saved map[string]string) {
	missing := func() []string {
		present, err := s.run("nft", "list", "tables")
		if err != nil {
			t.Errorf("listing the tables: %v", err)
			return nil
		}
		var out []string
		for name := range saved {
			if !strings.Contains(present, "table "+name+"\n") {
				out = append(out, name)
			}
		}
		return out
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(missing()) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Second)
	}
	failed := false
	for _, name := range missing() {
		t.Logf("table %s did not come back by itself; loading it as it was", name)
		if _, err := s.run("nft", "-f", saved[name]); err != nil {
			t.Errorf("loading the table %s again: %v", name, err)
			failed = true
		}
	}
	if !failed {
		unmark(t, tablesDir)
	}
}
