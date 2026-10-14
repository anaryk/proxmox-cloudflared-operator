//go:build e2e

package e2e

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The cluster profile runs pco on every node of a Proxmox VE cluster of
// three nodes or more, from one of them, the driver: it reaches the others
// with ssh as root, as the nodes of a cluster reach each other. The fake
// Cloudflare stays in the suite's process on the driver, listening on the
// driver's address in the cluster; every node, the driver too, gets the
// suite's relay on its loopback, which pco takes the API of tests at. The
// driver is never powered off, rebooted or cut off: a scenario that does so
// to the controller hands the lease to another node first.
const (
	// The guests of the cluster profiles have the ids 9500 to 9549, on the
	// bridge PCO_E2E_CLUSTER_BRIDGE (an SDN vnet that spans the nodes), at
	// the address <prefix>.<100 + offset>.
	clusterFirst     = 9500
	clusterLast      = 9549
	clusterGuestBase = 100

	clusterBridgeVar = "PCO_E2E_CLUSTER_BRIDGE"
	// powerVar names the command that powers a node off and on, and
	// suspends and resumes it: it is run on the driver as
	// <command> off|on|suspend|resume <node>. The nodes are virtual machines
	// of a host the suite cannot reach, and the command can.
	powerVar = "PCO_E2E_POWER"

	relayUnitPath = "/etc/systemd/system/" + relayUnit

	// clusterFile names the nodes of the run, the install and the mode,
	// as lines node=<name> <address>, install=<id> and mode=fake or
	// mode=real; it puts cleanup-cluster.sh to work.
	clusterFile = markerDir + "/cluster"

	// The run's nftables table on a node, which cuts what a scenario cuts
	// off; it is removed again at the end of the scenario, and by
	// cleanup-cluster.sh.
	cutTable = "pco_e2e_cut"

	clusterTimeout = 10 * time.Minute
)

// clusterNode is a node of the cluster, by the name and the address
// /etc/pve/.members gives it: corosync's address, which pco's agent channel
// uses unless the settings name another network.
type clusterNode struct {
	name string
	addr string
	id   int
}

// clusterRun is the cluster as the suite drives it.
type clusterRun struct {
	// suite is the suite on the driver, with the cloud of the run.
	*suite
	nodes  []clusterNode
	self   string
	bridge string
	// bridgeAddr is each node's address on the bridge of the guests.
	bridgeAddr map[string]string
	power      string
	relayBin   string
	// cloudflaredOn says which nodes have /usr/bin/cloudflared.
	cloudflaredOn map[string]bool

	mu sync.Mutex
	// down are the nodes the run powered off or suspended, by the action
	// that brings each back; cut the nodes with the run's table.
	down map[string]string
	cut  map[string]bool
	// gone are nodes the run took out of the cluster for good, which
	// nothing touches any more.
	gone map[string]bool
	// cguests are the guests the run made on the nodes; homes the guests
	// of the routes most scenarios look at, one on each of three nodes.
	cguests map[int]bool
	homes   []homed
	// agents are the agent containers of the appliance on a cluster.
	agents *agentsOf
	// secret holds every token and key the run handles, which no line it
	// logs may carry.
	secret secrets
	// before is each node's state before pco was set up on it.
	before map[string]string
	// uninstalled says that a scenario uninstalled pco from every node.
	uninstalled bool
}

// members is /etc/pve/.members as the suite reads it.
type members struct {
	NodeName string `json:"nodename"`
	Cluster  *struct {
		Name    string `json:"name"`
		Quorate int    `json:"quorate"`
	} `json:"cluster"`
	NodeList map[string]struct {
		ID     int    `json:"id"`
		Online int    `json:"online"`
		IP     string `json:"ip"`
	} `json:"nodelist"`
}

func readMembers() (members, error) {
	raw, err := os.ReadFile("/etc/pve/.members")
	if err != nil {
		return members{}, err
	}
	var m members
	if err := json.Unmarshal(raw, &m); err != nil {
		return members{}, fmt.Errorf("reading /etc/pve/.members: %w", err)
	}
	return m, nil
}

// newCluster prepares every node and sets pco up: on the driver first, then
// on the others, which join it. Everything is undone when t ends.
func newCluster(t *testing.T, profile string) *clusterRun {
	if os.Geteuid() != 0 {
		t.Skip("the end-to-end suite runs as root on a Proxmox VE node")
	}
	for _, bin := range []string{"/usr/sbin/pct", "/usr/sbin/qm", "/usr/bin/pvesh"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("the end-to-end suite needs %s: %v", bin, err)
		}
	}
	if !exists("/etc/pve/corosync.conf") {
		t.Fatal("this node is in no cluster: the cluster profiles run on a node of a cluster of three nodes or more")
	}
	m, err := readMembers()
	require.NoError(t, err)
	c := &clusterRun{
		self: m.NodeName, bridge: envOr(clusterBridgeVar, "labx"), power: os.Getenv(powerVar),
		bridgeAddr: map[string]string{}, cloudflaredOn: map[string]bool{},
		down: map[string]string{}, cut: map[string]bool{}, gone: map[string]bool{}, cguests: map[int]bool{}, before: map[string]string{},
	}
	for name, n := range m.NodeList {
		require.Equal(t, 1, n.Online, "node %s is not online: the run needs every node of the cluster", name)
		c.nodes = append(c.nodes, clusterNode{name: name, addr: n.IP, id: n.ID})
	}
	slices.SortFunc(c.nodes, func(a, b clusterNode) int { return cmp.Compare(a.id, b.id) })
	require.GreaterOrEqual(t, len(c.nodes), 3, "the cluster profiles need three nodes or more")

	s := &suite{
		pco:     envOr("PCO_E2E_PCO", "/usr/bin/pco"),
		tmpl:    envOr("PCO_E2E_TEMPLATE", "local:vztmpl/alpine-3.24-default_20260714_amd64.tar.xz"),
		disk:    envOr("PCO_E2E_STORAGE", "local-lvm"),
		dir:     t.TempDir(),
		guests:  map[int]bool{},
		began:   time.Now(),
		started: map[string]bool{}, bridges: map[string]bool{}, sysctls: map[string]string{},
		vms: map[int]bool{}, pconetGuests: map[int]bool{},
	}
	s.at = place{pco: s.pco}
	c.suite = s
	c.clusterPreflight(t, profile)
	s.lay = layout{
		profile: profile, first: clusterFirst, last: clusterLast,
		nodeAddr: c.bridgeAddr[c.self], guestNet: prefixOf(c.bridgeAddr[c.self]), guestBase: clusterGuestBase,
		fakeHost: c.node(t, c.self).addr,
	}
	if profile == profileApplianceCluster {
		// The containers reach the node on the bridge of the guests, where
		// they have their addresses.
		s.lay.fakeHost = c.bridgeAddr[c.self]
	}
	c.refuseTakenAddresses(t)
	return c
}

// node is the node of that name.
func (c *clusterRun) node(t testing.TB, name string) clusterNode {
	t.Helper()
	i := slices.IndexFunc(c.nodes, func(n clusterNode) bool { return n.name == name })
	require.GreaterOrEqual(t, i, 0, "no node %s in the cluster", name)
	return c.nodes[i]
}

// nodeAt is the node whose address in the cluster is addr, "" for none.
func (c *clusterRun) nodeAt(addr string) string {
	for _, n := range c.nodes {
		if n.addr == addr {
			return n.name
		}
	}
	return ""
}

// names are the names of the nodes, by node id.
func (c *clusterRun) names() []string {
	var out []string
	for _, n := range c.nodes {
		if !c.isGone(n.name) {
			out = append(out, n.name)
		}
	}
	return out
}

// others are the nodes that are none of those named.
func (c *clusterRun) others(except ...string) []string {
	return slices.DeleteFunc(c.names(), func(n string) bool { return slices.Contains(except, n) })
}

func (c *clusterRun) isGone(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gone[name]
}

// on is the suite as seen from a node: pco and the commands of the node run
// there.
func (c *clusterRun) on(name string) *suite {
	s := *c.suite
	s.at = place{pco: c.pco, env: c.env}
	if name != c.self {
		for _, n := range c.nodes {
			if n.name == name {
				s.at.node, s.at.remote = n.name, n.addr
			}
		}
	}
	return &s
}

// prefixOf is the first three octets of an IPv4 address.
func prefixOf(addr string) string {
	i := strings.LastIndex(addr, ".")
	if i < 0 {
		return addr
	}
	return addr[:i]
}

// clusterPreflight refuses a cluster where the run would change what it did
// not make, and finds what each node has: its address on the bridge of the
// guests, cloudflared, the template and the storage.
func (c *clusterRun) clusterPreflight(t *testing.T, profile string) {
	if _, found, err := installed(); err != nil || found {
		t.Fatalf("pco is set up in this cluster (or its store cannot be read: %v): the cluster profiles need a cluster without it", err)
	}
	if entries, _ := os.ReadDir(markerDir); len(entries) > 0 {
		t.Fatalf("a run of the suite left %s on this node: %s", markerDir, "run test/e2e/cleanup-cluster.sh, which removes what a run of the cluster profiles left")
	}
	c.refuseClusterGateTag(t)
	c.refuseTakenClusterIDs(t)
	var prefix string
	for _, n := range c.nodes {
		on := c.on(n.name)
		if _, err := on.here("true"); err != nil {
			t.Fatalf("node %s does not answer ssh as root at %s: %v", n.name, n.addr, err)
		}
		if _, err := on.here("test", "-x", c.pco); err != nil && profile == profileCluster {
			t.Fatalf("node %s has no %s: install the package on every node first", n.name, c.pco)
		}
		if _, err := on.here("test", "-e", markerDir); err == nil {
			t.Fatalf("node %s has %s from a run of the suite: run test/e2e/cleanup-cluster.sh", n.name, markerDir)
		}
		addrs := on.hereMust(t, "ip", "-4", "-o", "addr", "show", "dev", c.bridge)
		f := strings.Fields(addrs)
		i := slices.Index(f, "inet")
		if i < 0 || i+1 >= len(f) {
			t.Fatalf("node %s has no IPv4 address on %s: the guests of the run are reached there from every node", n.name, c.bridge)
		}
		addr, _, _ := strings.Cut(f[i+1], "/")
		c.bridgeAddr[n.name] = addr
		switch {
		case prefix == "":
			prefix = prefixOf(addr)
		case prefix != prefixOf(addr):
			t.Fatalf("node %s has %s on %s, outside the /24 of the others (%s.0/24)", n.name, addr, c.bridge, prefix)
		}
		_, err := on.here("test", "-x", cloudflared)
		c.cloudflaredOn[n.name] = err == nil
		if _, err := on.here("pvesm", "path", c.tmpl); err != nil {
			t.Fatalf("node %s does not have the template %s: %v", n.name, c.tmpl, err)
		}
		on.hereMust(t, "pvesm", "status", "--storage", c.disk)
		before, err := c.nodeState(n.name)
		require.NoError(t, err)
		c.before[n.name] = before
	}
}

// refuseClusterGateTag refuses a cluster with a guest that carries the gate
// tag on any node: the daemon would serve it as well.
func (c *clusterRun) refuseClusterGateTag(t *testing.T) {
	confs, err := filepath.Glob("/etc/pve/nodes/*/lxc/*.conf")
	require.NoError(t, err)
	vms, err := filepath.Glob("/etc/pve/nodes/*/qemu-server/*.conf")
	require.NoError(t, err)
	for _, conf := range append(confs, vms...) {
		raw, err := os.ReadFile(conf)
		require.NoError(t, err)
		if slices.Contains(tagsOf(string(raw)), gateTag) {
			t.Fatalf("%s carries the tag %s: the suite would publish that guest as well; it needs a cluster where no guest has it", conf, gateTag)
		}
	}
}

// vmlist is /etc/pve/.vmlist: the node of every guest of the cluster.
func vmlist() (map[int]string, error) {
	raw, err := os.ReadFile("/etc/pve/.vmlist")
	if err != nil {
		return nil, err
	}
	var v struct {
		IDs map[string]struct {
			Node string `json:"node"`
		} `json:"ids"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("reading /etc/pve/.vmlist: %w", err)
	}
	out := map[int]string{}
	for id, g := range v.IDs {
		if n, err := strconv.Atoi(id); err == nil {
			out[n] = g.Node
		}
	}
	return out, nil
}

func (c *clusterRun) refuseTakenClusterIDs(t *testing.T) {
	ids, err := vmlist()
	require.NoError(t, err)
	for id, node := range ids {
		if id >= clusterFirst && id <= clusterLast {
			t.Fatalf("guest %d exists on %s; the cluster profiles need the ids %d to %d free, and leave that guest alone",
				id, node, clusterFirst, clusterLast)
		}
	}
}

// refuseTakenAddresses refuses addresses of the run's guests that are a
// node's, or that something answers ping for.
func (c *clusterRun) refuseTakenAddresses(t *testing.T) {
	for n := 0; n <= clusterLast-clusterFirst; n++ {
		addr := c.lay.addr(n)
		for node, a := range c.bridgeAddr {
			if a == addr {
				t.Fatalf("node %s has %s on %s, an address the guests of the run take", node, addr, c.bridge)
			}
		}
	}
	for _, n := range []int{1, 2, 3} {
		if _, err := c.run("ping", "-c", "1", "-W", "1", c.lay.addr(n)); err == nil {
			t.Fatalf("%s answers ping: the guests of the run take %s.%d to %s.%d on %s",
				c.lay.addr(n), c.lay.guestNet, clusterGuestBase, c.lay.guestNet, clusterGuestBase+clusterLast-clusterFirst, c.bridge)
		}
	}
}

// nodeState is what of a node pco's install and uninstall change: its unit
// files and their states, its local state, its tables and namespaces.
func (c *clusterRun) nodeState(node string) (string, error) {
	return c.on(node).here("sh", "-c", "systemctl list-unit-files --no-legend --no-pager 'pco*' | sort; "+
		"ls -A /var/lib/pco 2>/dev/null | sort; nft list tables | grep -v "+cutTable+" | sort; ip netns list | sort; "+
		"ls /etc/systemd/system | grep -i pco | grep -vx pco.service.d | sort")
}

// start sets pco up on the driver against the cloud of the run, and has the
// other nodes join it, with everything undone when t ends.
func (c *clusterRun) start(t *testing.T) {
	token := c.chooseCloud(t)
	c.secret.add(token, fakeSecondToken)
	if c.fake == nil {
		for _, n := range c.names() {
			if !c.cloudflaredOn[n] {
				t.Fatalf("against the real API the connectors of every node need %s, which %s does not have", cloudflared, n)
			}
		}
	}
	c.markCluster(t)

	// Cleanups run last first: pco goes from every node before the guests,
	// so that their going is nothing the daemon acts on; the relays and the
	// drop-ins after it, the nodes powered on and the cuts removed before
	// anything else.
	t.Cleanup(func() { c.removeRelays(t) })
	t.Cleanup(func() { c.destroyClusterGuests(t) })
	t.Cleanup(func() { c.uninstallCluster(t) })
	t.Cleanup(func() { c.restoreRegisteredTags(t) })
	t.Cleanup(func() { c.uncutAll(t) })
	t.Cleanup(func() { c.powerOnAll(t) })
	t.Cleanup(func() { c.diagnoseCluster(t) })

	if c.fake != nil {
		c.installRelays(t)
	}
	c.setup(t, token)
	c.markCluster(t)
	c.enforce(t)
	for _, n := range c.others(c.self) {
		c.join(t, n)
	}
	c.waitActive(t, c.names()...)
}

// markCluster records the nodes, the install and the mode for
// cleanup-cluster.sh.
func (c *clusterRun) markCluster(t testing.TB) {
	t.Helper()
	var b strings.Builder
	for _, n := range c.nodes {
		fmt.Fprintf(&b, "node=%s %s\n", n.name, n.addr)
	}
	fmt.Fprintf(&b, "bridge=%s\ninstall=%s\nmode=%s\n", c.bridge, c.install, c.mode())
	marker(t, clusterFile, b.String())
}

// installRelays puts the relay on every node as an enabled unit, which
// outlives a reboot of the node, and the drop-in that points the daemon at
// it.
func (c *clusterRun) installRelays(t *testing.T) {
	c.relayBin = envOr("PCO_E2E_RELAY", "e2e-relay")
	abs, err := filepath.Abs(c.relayBin)
	require.NoError(t, err)
	_, err = os.Stat(abs)
	require.NoError(t, err, "the relay: build it with make e2e-binaries and put bin/e2e-relay beside the suite, or name it with PCO_E2E_RELAY")
	c.relayBin = abs
	unit := fmt.Sprintf(`[Unit]
Description=Relay of the end-to-end suite of pco, %s to %s
After=network.target

[Service]
ExecStart=%s -listen %s -to %s
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
`, relayListen, c.fake.srv.address(), relayPath, relayListen, c.fake.srv.address())
	for _, n := range c.names() {
		c.copyTo(t, n, c.relayBin, relayPath)
		c.writeOn(t, n, relayUnitPath, unit, "0644")
		c.writeOn(t, n, dropInFile, "[Service]\nEnvironment="+overrideOf+"="+relayURL+"\n", "0644")
		on := c.on(n)
		on.hereMust(t, "systemctl", "daemon-reload")
		on.hereMust(t, "systemctl", "enable", "--now", relayUnit)
	}
}

func (c *clusterRun) removeRelays(t *testing.T) {
	for _, n := range c.names() {
		if c.isDown(n) {
			t.Errorf("node %s is still down: remove %s, %s and %s there by hand, or run cleanup-cluster.sh", n, relayUnitPath, relayPath, dropInFile)
			continue
		}
		on := c.on(n)
		_, _ = on.here("systemctl", "disable", "--now", relayUnit)
		if _, err := on.here("rm", "-f", relayUnitPath, relayPath, dropInFile); err != nil {
			t.Errorf("removing the relay and the drop-in on %s: %v", n, err)
		}
		_, _ = on.here("rmdir", dropInDir)
		_, _ = on.here("systemctl", "daemon-reload")
	}
	if !t.Failed() {
		unmark(t, clusterFile)
		_ = os.Remove(markerDir)
	}
}

// copyTo copies a file of the driver to a node, executable.
func (c *clusterRun) copyTo(t testing.TB, node, local, remote string) {
	t.Helper()
	if node == c.self {
		c.must(t, "install", "-m", "0755", local, remote)
		return
	}
	n := c.node(t, node)
	c.must(t, "scp", "-q", "-o", "BatchMode=yes", "-o", "HostKeyAlias="+n.name, local, "root@"+n.addr+":"+remote)
}

// writeOn writes a file on a node with the mode given, through a file beside
// it renamed over it.
//
// The content goes through the standard input of the command, never its
// arguments, which any process of the node may read: some of it is a token
// or a key. A directory the write makes is made for root alone.
func (c *clusterRun) writeOn(t testing.TB, node, path, content, mode string) {
	t.Helper()
	script := fmt.Sprintf("set -e; umask 077; mkdir -p -m 0700 %s; cat >%s.e2e; chmod %s %s.e2e; mv -f %s.e2e %s",
		shellLine([]string{filepath.Dir(path)}), path, mode, path, path, path)
	argv := c.on(node).at.argv("sh", "-c", script)
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = c.childEnv()
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "writing %s on %s: %s", path, node, c.secret.scrub(string(out)))
}

// join sets pco up on a further node, which joins the install.
func (c *clusterRun) join(t testing.TB, node string) {
	t.Helper()
	args := []string{"setup", "--yes"}
	if !c.cloudflaredOn[node] {
		args = append(args, "--skip-cloudflared")
	}
	out, err := c.pcoWithin(node, clusterTimeout, args...)
	c.logf(t, "pco %s on %s:\n%s", strings.Join(args, " "), node, out)
	require.NoError(t, err)
}

// pcoWithin runs a pco command on a node and returns stdout and stderr.
func (c *clusterRun) pcoWithin(node string, timeout time.Duration, args ...string) (string, error) {
	on := c.on(node)
	argv := on.at.pcoArgv(args...)
	return on.combinedWithin(timeout, argv[0], argv[1:]...)
}

// onWithin runs a command on a node that may take longer than a command of
// the run, and returns stdout and stderr.
func (c *clusterRun) onWithin(node string, timeout time.Duration, name string, args ...string) (string, error) {
	on := c.on(node)
	argv := on.at.argv(name, args...)
	return on.combinedWithin(timeout, argv[0], argv[1:]...)
}

// holderSeen is the node the driver's last cycle names as the lease's
// holder, "" when it names none.
func (c *clusterRun) holderSeen() string {
	st, err := c.tryState()
	if err != nil || st.Lease == nil {
		return ""
	}
	return st.Lease.Holder
}

// controllerState is the state of the controller: of the node that the
// driver names as the holder, when that node says it holds the lease and it
// is valid.
func (c *clusterRun) controllerState() (engine.State, string, error) {
	holder := c.holderSeen()
	if holder == "" || c.isGone(holder) {
		return engine.State{}, "", fmt.Errorf("no node holds the lease, as %s last read it", c.self)
	}
	st, err := c.on(holder).tryState()
	if err != nil {
		return engine.State{}, holder, err
	}
	if st.Lease == nil || st.Lease.Holder != holder || !st.Lease.Valid {
		return st, holder, fmt.Errorf("%s does not say it holds a valid lease: %+v", holder, st.Lease)
	}
	return st, holder, nil
}

// waitController polls the controller's state until ok, wherever the lease
// is, and fails with the last one after timeout.
func (c *clusterRun) waitController(t testing.TB, what string, timeout time.Duration, ok func(engine.State) bool) (engine.State, string) {
	t.Helper()
	var last engine.State
	failed := "none"
	deadline := time.Now().Add(timeout)
	for {
		st, holder, err := c.controllerState()
		if err == nil {
			last = st
			if ok(st) {
				return st, holder
			}
		} else {
			failed = fmt.Sprintf("%s: %v", time.Now().Format(time.TimeOnly), err)
		}
		if time.Now().After(deadline) {
			raw, _ := json.MarshalIndent(last, "", "  ")
			t.Fatalf("waited %s for %s; the last error: %s; the controller's last state:\n%s", timeout, what, failed, raw)
		}
		time.Sleep(time.Second)
	}
}

// ctl is the suite on the controller, once there is one.
func (c *clusterRun) ctl(t testing.TB) *suite {
	t.Helper()
	_, holder := c.waitController(t, "a controller", 5*time.Minute, func(engine.State) bool { return true })
	return c.on(holder)
}

// holder is the node that holds the lease, once one does.
func (c *clusterRun) holder(t testing.TB) string {
	t.Helper()
	_, holder := c.waitController(t, "a controller", 5*time.Minute, func(engine.State) bool { return true })
	return holder
}

// enforce leaves observe-only mode on the controller.
func (c *clusterRun) enforce(t testing.TB) {
	t.Helper()
	c.ctl(t).enforce(t)
}

// nodeView is the view of a node in a state.
func nodeView(st engine.State, name string) (engine.NodeView, bool) {
	i := slices.IndexFunc(st.Nodes, func(n engine.NodeView) bool { return n.Name == name })
	if i < 0 {
		return engine.NodeView{}, false
	}
	return st.Nodes[i], true
}

// waitActive waits until the controller lists every node named as active.
func (c *clusterRun) waitActive(t testing.TB, names ...string) engine.State {
	t.Helper()
	st, _ := c.waitController(t, "nodes "+strings.Join(names, ", ")+" active", clusterTimeout, func(st engine.State) bool {
		for _, n := range names {
			if v, ok := nodeView(st, n); !ok || v.State != store.NodeActive {
				return false
			}
		}
		return true
	})
	return st
}

// handoff hands the lease to a node and waits until it holds it, with a
// cycle of its own finished.
func (c *clusterRun) handoff(t testing.TB, to string) engine.State {
	t.Helper()
	from := c.holder(t)
	if from == to {
		return c.ctl(t).state(t)
	}
	asked := time.Now()
	out, err := c.pcoWithin(from, commandTimeout, "cluster", "handoff", "--to", to, "--yes")
	c.logf(t, "pco cluster handoff --to %s on %s:\n%s", to, from, out)
	require.NoError(t, err)
	st, _ := c.waitController(t, to+" holding the lease", 2*time.Minute, func(st engine.State) bool {
		return st.Node == to && st.At.After(asked) && !st.FinishedAt.IsZero()
	})
	return st
}

// awayFromDriver makes another node than the driver the controller, and
// returns it.
func (c *clusterRun) awayFromDriver(t testing.TB) string {
	t.Helper()
	holder := c.holder(t)
	if holder != c.self {
		return holder
	}
	to := c.others(c.self)[0]
	c.handoff(t, to)
	return to
}

// clusterStatus is the answer of pco cluster status on the controller.
func (c *clusterRun) clusterStatus(t testing.TB) engine.ClusterStatus {
	t.Helper()
	out, err := c.ctl(t).pcoOut("cluster", "status", "--json")
	require.Contains(t, []int{0, 1}, exitCode(err), "pco cluster status: %v", err)
	var cs engine.ClusterStatus
	require.NoError(t, json.Unmarshal([]byte(out), &cs), out)
	return cs
}

// agentStatus is what the agent of a node says of itself.
func (c *clusterRun) agentStatus(t testing.TB, node string) agentproto.AgentView {
	t.Helper()
	out, err := c.on(node).pcoOut("agent", "status", "--json")
	require.Contains(t, []int{0, 1}, exitCode(err), "pco agent status on %s: %v", node, err)
	var v agentproto.AgentView
	require.NoError(t, json.Unmarshal([]byte(out), &v), out)
	return v
}

// doctorOn is what pco doctor finds on a node, with --cluster if asked.
func (c *clusterRun) doctorOn(t testing.TB, node string, cluster bool) []doctor.Finding {
	t.Helper()
	args := []string{"doctor", "--json"}
	if cluster {
		args = append(args, "--cluster")
	}
	out, err := c.on(node).pcoOut(args...)
	require.Contains(t, []int{0, 1}, exitCode(err), "pco doctor on %s: %v", node, err)
	var findings []doctor.Finding
	require.NoError(t, json.Unmarshal([]byte(out), &findings), out)
	return findings
}

// targetsOn is the egress set of a node, nil when it cannot be read.
func (c *clusterRun) targetsOn(node string) []string { return c.on(node).targetsNow() }

// waitTargetEverywhere waits until the egress set of every node named has
// the target, or has it on none of them.
func (c *clusterRun) waitTargetEverywhere(t testing.TB, target string, in bool, timeout time.Duration, nodes ...string) {
	t.Helper()
	for _, n := range nodes {
		c.on(n).waitTarget(t, target, in, timeout, time.Second)
	}
}

// fetchOn fetches a URL on a node, as its connector would reach a target.
func (c *clusterRun) fetchOn(node, url string) (string, error) {
	out, err := c.on(node).here("curl", "-fsS", "-m", "5", url)
	return strings.TrimSpace(out), err
}

// requireReachedFrom checks that the target answers its body from every node
// named.
func (c *clusterRun) requireReachedFrom(t testing.TB, url, body string, nodes ...string) {
	t.Helper()
	for _, n := range nodes {
		deadline := time.Now().Add(time.Minute)
		for {
			got, err := c.fetchOn(n, url)
			if err == nil && got == body {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s from %s answered %q (%v), not %q", url, n, got, err, body)
			}
			time.Sleep(2 * time.Second)
		}
	}
}

// createOn makes a guest on a node, on the bridge of the guests at the
// address of its offset, tagged with its notes, and starts it.
func (c *clusterRun) createOn(t testing.TB, node string, n int, g guest) guest {
	t.Helper()
	// A guest on a bridge of its own keeps the address it was given.
	addr := g.addr
	g = c.g(n, g)
	if g.bridge == "" {
		g.bridge = c.bridge
	} else if addr != "" {
		g.addr = addr
	}
	c.mu.Lock()
	c.cguests[g.vmid] = true
	c.mu.Unlock()
	on := c.on(node)
	id := strconv.Itoa(g.vmid)
	on.hereMust(t, "pct", "create", id, c.tmpl,
		"--hostname", "e2e-"+id, "--ostype", "alpine", "--unprivileged", "1",
		"--memory", "64", "--swap", "0", "--cores", "1",
		"--rootfs", c.disk+":1", "--net0", g.net0(),
		"--tags", g.tagsOf(), "--description", g.notes)
	on.hereMust(t, "pct", "start", id)
	c.waitGuestExec(t, node, g.vmid)
	files := map[string]string{
		"/etc/local.d/e2e.start":   g.startScript(),
		"/usr/local/bin/e2e-serve": serveScript,
	}
	for _, port := range g.servedPorts() {
		files["/usr/local/bin/e2e-reply-"+strconv.Itoa(port)] = replyScript(g.body(port))
	}
	var script strings.Builder
	script.WriteString("set -e\n")
	for path, content := range files {
		fmt.Fprintf(&script, "mkdir -p %s\nprintf %%s %s | base64 -d >%s\nchmod 0755 %s\n",
			filepath.Dir(path), base64.StdEncoding.EncodeToString([]byte(content)), path, path)
	}
	script.WriteString("rc-update add local default >/dev/null\n/etc/local.d/e2e.start\n")
	on.hereMust(t, "pct", "exec", id, "--", "sh", "-c", script.String())
	c.logf(t, "guest %d is up on %s at %s", g.vmid, node, g.ip())
	return g
}

func (c *clusterRun) waitGuestExec(t testing.TB, node string, vmid int) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		_, err := c.on(node).here("pct", "exec", strconv.Itoa(vmid), "--", "true")
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest %d on %s does not run commands: %v", vmid, node, err)
		}
		time.Sleep(time.Second)
	}
}

// guestNode is the node a guest is on now, "" when it is gone.
func guestNode(vmid int) string {
	ids, err := vmlist()
	if err != nil {
		return ""
	}
	return ids[vmid]
}

// destroyOn removes a guest of the run, wherever it is now.
func (c *clusterRun) destroyOn(t testing.TB, vmid int) {
	t.Helper()
	require.NoError(t, c.destroyClusterGuest(vmid))
	c.mu.Lock()
	delete(c.cguests, vmid)
	c.mu.Unlock()
}

func (c *clusterRun) destroyClusterGuest(vmid int) error {
	node := guestNode(vmid)
	if node == "" {
		return nil
	}
	if c.isDown(node) || c.isGone(node) {
		return fmt.Errorf("guest %d is on %s, which is down or gone", vmid, node)
	}
	on := c.on(node)
	id := strconv.Itoa(vmid)
	kind := "pct"
	if exists(fmt.Sprintf("/etc/pve/nodes/%s/qemu-server/%d.conf", node, vmid)) {
		kind = "qm"
	}
	if out, _ := on.here(kind, "status", id); strings.Contains(out, "running") {
		if _, err := on.here(kind, "stop", id); err != nil {
			return err
		}
	}
	_, err := on.here(kind, "destroy", id, "--purge", "1")
	return err
}

func (c *clusterRun) destroyClusterGuests(t *testing.T) {
	c.mu.Lock()
	ids := make([]int, 0, len(c.cguests))
	for id := range c.cguests {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		if err := c.destroyClusterGuest(id); err != nil {
			t.Errorf("destroying guest %d: %v", id, err)
			continue
		}
		c.mu.Lock()
		delete(c.cguests, id)
		c.mu.Unlock()
	}
}

// uninstallCluster removes pco from every node, unless a scenario did: the
// nodes that do not hold the lease leave the install first, and the last one
// purges what the install has at Cloudflare.
func (c *clusterRun) uninstallCluster(t *testing.T) {
	if !c.setUp || c.uninstalled {
		return
	}
	last := c.holderSeen()
	if last == "" || c.isGone(last) || c.isDown(last) {
		last = c.self
	}
	for _, n := range c.others(last) {
		if c.isDown(n) {
			t.Errorf("node %s is down: pco stays on it; run pco uninstall there once it is back", n)
			continue
		}
		out, err := c.pcoWithin(n, uninstallTimeout, "uninstall", "--yes", "--keep-cloudflare", "--force")
		c.logf(t, "pco uninstall on %s (%v):\n%s", n, err, out)
		if err != nil {
			t.Errorf("pco uninstall on %s: %v", n, err)
		}
	}
	var err error
	for attempt := 1; attempt <= purgeAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(purgeRetry)
		}
		var out string
		out, err = c.pcoWithin(last, uninstallTimeout, "uninstall", "--yes", "--purge-cloudflare", "--force", "--detach-managed")
		c.logf(t, "pco uninstall on %s, attempt %d of %d (%v):\n%s", last, attempt, purgeAttempts, err, out)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Errorf("pco uninstall on %s: %v; cleanup-cluster.sh purges it again", last, err)
		return
	}
	c.uninstalled = true
	if left := c.leftOfInstall(t); len(left) > 0 {
		t.Errorf("left at Cloudflare after the purge:\n  %s", strings.Join(left, "\n  "))
	}
}

// leftOfInstall names what the install has at Cloudflare: its shared
// tunnel, the tunnels pinned to its nodes and its records.
func (c *clusterRun) leftOfInstall(t testing.TB) []string {
	t.Helper()
	if c.install == "" {
		return nil
	}
	left := c.leftAtCloudflare(t)
	for _, n := range c.nodes {
		name, err := planner.PinnedTunnelName(c.install, n.name)
		if err != nil {
			continue
		}
		if tn, found := c.cloud.Tunnel(t, name); found {
			left = append(left, fmt.Sprintf("the tunnel %s (%s)", tn.Name, tn.ID))
		}
	}
	return left
}

// diagnoseCluster logs what helps to understand a failed run: the journal of
// pco on every node that answers, and the last calls of the fake.
func (c *clusterRun) diagnoseCluster(t *testing.T) {
	if !t.Failed() {
		return
	}
	since := c.began.Format("2006-01-02 15:04:05")
	for _, n := range c.names() {
		if c.isDown(n) {
			continue
		}
		out, _ := c.on(n).here("journalctl", "-u", "pco", "--since", since, "--no-pager", "-n", "150")
		c.logf(t, "journal of pco on %s:\n%s", n, out)
	}
	if c.fake != nil {
		calls := c.fake.f.Calls()
		c.logf(t, "the last calls of the fake:\n%s", strings.Join(calls[max(0, len(calls)-80):], "\n"))
	}
}

// powered says whether the run can power nodes off and on.
func (c *clusterRun) powered() bool { return c.power != "" }

// skipWithoutPower skips a scenario that powers a node off or suspends it.
func (c *clusterRun) skipWithoutPower(t *testing.T) {
	t.Helper()
	if !c.powered() {
		t.Skipf("%s names no command that powers the nodes off and on: the scenario is not run", powerVar)
	}
}

// powerAction runs the power command for a node. off and suspend record the
// node as down, on and resume as back.
func (c *clusterRun) powerAction(t testing.TB, action, node string) {
	t.Helper()
	require.NotEqual(t, c.self, node, "the suite never powers off the node it runs on")
	out, err := c.combinedWithin(5*time.Minute, c.power, action, node)
	c.logf(t, "%s %s %s:\n%s", c.power, action, node, out)
	require.NoError(t, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch action {
	case "off":
		c.down[node] = "on"
	case "suspend":
		c.down[node] = "resume"
	default:
		delete(c.down, node)
	}
}

func (c *clusterRun) isDown(node string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.down[node]
	return ok
}

// markDown records a node that went down by a command the scenario ran on
// it, as a reboot, until it is back.
func (c *clusterRun) markDown(node, back string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if back == "" {
		delete(c.down, node)
		return
	}
	c.down[node] = back
}

// waitBack waits until a node answers ssh and its pco answers.
func (c *clusterRun) waitBack(t testing.TB, node string) {
	t.Helper()
	deadline := time.Now().Add(clusterTimeout)
	for {
		if _, err := c.on(node).tryState(); err == nil {
			c.markDown(node, "")
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("node %s is not back within %s", node, clusterTimeout)
		}
		time.Sleep(3 * time.Second)
	}
}

func (c *clusterRun) powerOnAll(t *testing.T) {
	c.mu.Lock()
	down := make(map[string]string, len(c.down))
	for node, back := range c.down {
		down[node] = back
	}
	c.mu.Unlock()
	for node, back := range down {
		if back == "reboot" {
			continue
		}
		if !c.powered() {
			t.Errorf("node %s is down and no %s brings it back", node, powerVar)
			continue
		}
		if out, err := c.combinedWithin(5*time.Minute, c.power, back, node); err != nil {
			t.Errorf("%s %s %s: %v\n%s", c.power, back, node, err, out)
		}
	}
	for node := range down {
		deadline := time.Now().Add(clusterTimeout)
		for time.Now().Before(deadline) {
			if _, err := c.on(node).here("true"); err == nil {
				c.markDown(node, "")
				break
			}
			time.Sleep(3 * time.Second)
		}
		if c.isDown(node) {
			t.Errorf("node %s is not back after the run", node)
		}
	}
}

// cutOff loads the run's table on a node with the rules given, as lines of
// nft, in its input and output chains.
func (c *clusterRun) cutOff(t testing.TB, node string, rules ...string) {
	t.Helper()
	require.NotEqual(t, c.self, node, "the suite never cuts off the node it runs on")
	var b strings.Builder
	fmt.Fprintf(&b, "table inet %s {\n chain input {\n  type filter hook input priority -300; policy accept;\n", cutTable)
	for _, r := range rules {
		fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(r, "$dir", "input"))
	}
	b.WriteString(" }\n chain output {\n  type filter hook output priority -300; policy accept;\n")
	for _, r := range rules {
		fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(r, "$dir", "output"))
	}
	b.WriteString(" }\n}\n")
	file := "/run/" + cutTable + ".nft"
	c.writeOn(t, node, file, b.String(), "0600")
	c.mu.Lock()
	c.cut[node] = true
	c.mu.Unlock()
	c.on(node).hereMust(t, "nft", "-f", file)
}

// uncut removes the run's table from a node.
func (c *clusterRun) uncut(t testing.TB, node string) {
	t.Helper()
	if _, err := c.on(node).here("nft", "delete", "table", "inet", cutTable); err != nil {
		t.Errorf("removing the table %s on %s: %v", cutTable, node, err)
		return
	}
	c.mu.Lock()
	delete(c.cut, node)
	c.mu.Unlock()
}

func (c *clusterRun) uncutAll(t *testing.T) {
	c.mu.Lock()
	var nodes []string
	for n := range c.cut {
		nodes = append(nodes, n)
	}
	c.mu.Unlock()
	for _, n := range nodes {
		c.uncut(t, n)
	}
}

// edge says whether Cloudflare would serve the hostname now: against the
// fake, whether its record leads to a tunnel whose rule for it sends it to a
// service, not to a status; against the real API, whether it answers 200.
func (c *clusterRun) edge(host string) (bool, string) {
	if c.fake == nil {
		return fetchEdge(host)
	}
	var tunnel string
	for _, r := range c.fake.f.RecordsIn(fakeZoneID) {
		if r.Name == host && r.Type == "CNAME" {
			tunnel = strings.TrimSuffix(r.Content, ".cfargotunnel.com")
		}
	}
	if tunnel == "" {
		return false, "no CNAME"
	}
	cfg, err := c.fake.f.TunnelConfig(context.Background(), fakeAccount, tunnel)
	if err != nil {
		return false, "tunnel " + tunnel + ": " + err.Error()
	}
	rule, _, ok := ruleOf(cfg.Ingress, host)
	switch {
	case !ok:
		return false, "no rule in tunnel " + tunnel
	case strings.HasPrefix(rule.Service, "http_status:"):
		return false, rule.Service + " in tunnel " + tunnel
	}
	return true, rule.Service + " in tunnel " + tunnel
}

// fetchEdge fetches the hostname through Cloudflare, once.
func fetchEdge(host string) (bool, string) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	resp, err := client.Get("https://" + host + "/")
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return true, strings.TrimSpace(string(raw))
}

// visitor asks the edge for hostnames every second until ended, and keeps
// the failures.
type visitor struct {
	stop     chan struct{}
	once     sync.Once
	done     chan struct{}
	mu       sync.Mutex
	asked    int
	failures []string
}

func (v *visitor) halt() { v.once.Do(func() { close(v.stop) }) }

func (c *clusterRun) visit(t testing.TB, hosts ...string) *visitor {
	v := &visitor{stop: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(v.halt)
	every := time.Second
	if c.fake == nil {
		every = 3 * time.Second
	}
	go func() {
		defer close(v.done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-v.stop:
				return
			case <-tick.C:
			}
			for _, h := range hosts {
				ok, why := c.edge(h)
				v.mu.Lock()
				v.asked++
				if !ok {
					v.failures = append(v.failures, time.Now().Format(time.TimeOnly)+" "+h+": "+why)
				}
				v.mu.Unlock()
			}
		}
	}()
	return v
}

// end stops the visitor and returns how often it asked and what failed.
func (v *visitor) end() (asked int, failures []string) {
	v.halt()
	<-v.done
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.asked, slices.Clone(v.failures)
}

// requireNoFailure ends the visitor and fails on any request the edge would
// not have served.
func (v *visitor) requireNoFailure(t testing.TB, during string) {
	t.Helper()
	asked, failures := v.end()
	t.Logf("%s: the edge was asked %d times, %d failed", during, asked, len(failures))
	require.Empty(t, failures[:min(len(failures), 30)], "the edge failed during %s", during)
}

// fakeIndex is where the fake's log of requests is now, 0 against the real
// API.
func (c *clusterRun) fakeIndex() int {
	if c.fake == nil {
		return 0
	}
	_, next := c.fake.srv.requests(0)
	return next
}

// writesFrom are the writes the fake answered since the index from, from
// the node named, or from any node when node is empty.
func (c *clusterRun) writesFrom(from int, node string) []servedRequest {
	if c.fake == nil {
		return nil
	}
	reqs, _ := c.fake.srv.requests(from)
	return slices.DeleteFunc(reqs, func(r servedRequest) bool {
		return !r.write() || (node != "" && c.nodeAt(r.From) != node)
	})
}

// sharedTunnel is the id of the install's shared tunnel at Cloudflare.
func (c *clusterRun) sharedTunnel(t testing.TB) string {
	t.Helper()
	tn, found := c.cloud.Tunnel(t, planner.TunnelName(c.install))
	require.True(t, found, "the shared tunnel %s", planner.TunnelName(c.install))
	return tn.ID
}

// pinnedTunnel is the id of the tunnel pinned to a node, "" while there is
// none.
func (c *clusterRun) pinnedTunnel(t testing.TB, node string) string {
	t.Helper()
	name, err := planner.PinnedTunnelName(c.install, node)
	require.NoError(t, err)
	tn, found := c.cloud.Tunnel(t, name)
	if !found {
		return ""
	}
	return tn.ID
}

// sentinelOf is the highest generation a tunnel's sentinels carry.
func (c *clusterRun) sentinelOf(t testing.TB, tunnelID string) int {
	t.Helper()
	gen := 0
	for _, r := range c.cloud.Ingress(t, tunnelID) {
		if w, ok := planner.ParseSentinel(r.Hostname); ok && w.InstallID == c.install {
			gen = max(gen, w.Generation)
		}
	}
	return gen
}

// replicaOf is how a route fares on a node.
func replicaOf(r engine.RouteView, node string) (engine.ReplicaView, bool) {
	i := slices.IndexFunc(r.Replicas, func(v engine.ReplicaView) bool { return v.Node == node })
	if i < 0 {
		return engine.ReplicaView{}, false
	}
	return r.Replicas[i], true
}

// placedOn says whether a route is placed on the shared tunnel, or pinned to
// the node named.
func placedOn(r engine.RouteView, node string) bool {
	p := r.Placement
	if p == nil {
		return false
	}
	if node == "" {
		return p.Kind == engine.TunnelShared
	}
	return p.Kind == engine.TunnelPinned && p.Node == node
}

// systemctl runs systemctl on a node.
func (c *clusterRun) systemctl(t testing.TB, node string, args ...string) {
	t.Helper()
	c.on(node).hereMust(t, "systemctl", args...)
}

// background runs a command on a node until ctx ends, and returns what it
// printed once it ended.
func (c *clusterRun) background(ctx context.Context, node string, name string, args ...string) <-chan string {
	out := make(chan string, 1)
	argv := c.on(node).at.argv(name, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = c.childEnv()
	go func() {
		raw, _ := cmd.CombinedOutput()
		out <- string(raw)
	}()
	return out
}
