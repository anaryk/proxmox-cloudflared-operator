//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The appliance on a cluster: the controller container on the node the suite
// runs on, and an agent container on two other nodes, which join it with a
// join token each. The containers sit on the bridge of the guests at the
// offsets 40 (the controller) and 43 and 44 (the agents); the fake listens on
// the driver's address there, and the controller reaches it through the
// relay, as the appliance profile has it.
const (
	controllerOffset = 40
	agentOffset      = 43
	// remoteArtefacts is where the installer, the template and the
	// checksums go on a node that installs an agent.
	remoteArtefacts = "/root/e2e-appliance"
)

// TestApplianceCluster runs the appliance's kinds on a cluster: a controller
// container, agents that join it with a join token, admitted by their
// fingerprint, and the routes of the guests of their nodes.
func TestApplianceCluster(t *testing.T) {
	p, err := chooseProfile()
	require.NoError(t, err)
	if p != profileApplianceCluster {
		t.Skipf("the profile is %s: TestApplianceCluster runs in the profile %s", p, profileApplianceCluster)
	}
	c := newCluster(t, p)
	c.startAppliance(t)
	for _, sc := range applianceClusterScenarios {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, c) })
	}
}

var applianceClusterScenarios = []clusterScenario{
	{"A-C1 the controller container", testControllerContainer},
	{"A-C2 agents join with a token and are admitted by their fingerprint", testAgentsJoin},
	{"A-C3 guests of the agents' nodes on their pinned tunnels", testAgentRoutes},
	{"A-C4 the join token is in no argument, log or event", testJoinTokenNowhere},
	{"A-C5 a join token used again, or for another node", testJoinTokenReused},
}

// agentsOf are the agent containers of the run, by node, and the join tokens
// they enrolled with; seen is what the run saw of the processes of the nodes
// while the agents were installed.
type agentsOf struct {
	mu     sync.Mutex
	vmid   map[string]int
	tokens map[string]string
	seen   map[string][]string
}

// startAppliance installs the controller container on the driver against the
// cloud of the run, with everything undone when t ends.
func (c *clusterRun) startAppliance(t *testing.T) {
	a := &applianceRun{vmid: c.lay.vmid(controllerOffset), guestNet: "net0", guestIface: "eth0", copies: map[int]bool{}, node: c.self}
	c.app = a
	c.at = place{vmid: a.vmid, pco: "pco"}
	if out, _ := c.run("pveum", "user", "list", "--output-format", "json"); strings.Contains(out, `"pco@pve"`) {
		t.Fatal("the user pco@pve exists: the appliance profiles need a cluster without pco's user")
	}
	if _, err := c.run("pvesh", "get", "/pools/"+applianceinstallPool); err == nil {
		t.Fatalf("the pool %s exists: the appliance profiles need a cluster without it", applianceinstallPool)
	}
	c.findArtefacts(t)
	require.NotEmpty(t, a.relay, "the relay: build it with make e2e-binaries and put bin/e2e-relay beside the suite, or name it with PCO_E2E_RELAY")
	a.tags = c.registeredTags(t)
	a.token = c.chooseCloud(t)
	c.secret.add(a.token)
	if c.fake != nil {
		c.at.env = []string{overrideOf + "=" + relayURL}
	} else {
		require.True(t, a.real, "against the real API the containers need a bridge with an uplink: name it with PCO_E2E_APPLIANCE_BRIDGE")
	}
	c.agents = &agentsOf{vmid: map[string]int{}, tokens: map[string]string{}, seen: map[string][]string{}}

	// Cleanups run last first: the agents go first, then the controller,
	// which purges what the install has at Cloudflare, then the guests,
	// and the fake last.
	t.Cleanup(func() {
		if c.fake != nil {
			c.fake.srv.stop()
		}
	})
	t.Cleanup(func() { c.destroyClusterGuests(t) })
	t.Cleanup(func() { c.uninstallControllerContainer(t) })
	t.Cleanup(func() { c.uninstallAgents(t) })
	t.Cleanup(func() { c.diagnoseCluster(t) })

	c.markAppliance(t)
	require.NoError(t, os.WriteFile(tagsSaved, []byte(strings.Join(a.tags, ";")+"\n"), 0o600))
	args := c.containerArgs("controller", a.vmid, controllerOffset, a.template, a.checksums)
	a.installed = true
	out, err := c.nodePcoRun(installTimeout, args...)
	c.logf(t, "pco %s:\n%s", strings.Join(args, " "), out)
	require.NoError(t, err)
	c.waitAppliance(t, a.vmid)
	c.guestCard(t, c.self, a.vmid, controllerOffset)
	c.prepareAppliance(t, a.token)
	c.markAppliance(t)
	c.enforceAppliance(t)
}

// containerArgs are the installer's arguments for a container of a kind,
// at the offset of the guests' range given.
func (c *clusterRun) containerArgs(kind string, vmid, offset int, template, checksums string) []string {
	args := []string{"appliance", "install", "--kind", kind, "--storage", c.disk, "--vmid", strconv.Itoa(vmid), "--template", template, "--yes"}
	if checksums != "" {
		args = append(args, "--checksums", checksums)
	}
	if c.app.real {
		return append(args, "--bridge", c.app.apiBridge)
	}
	return append(args, "--bridge", c.bridge, "--ip", c.lay.addr(offset)+"/24")
}

// guestCard gives a container a second card on the bridge of the guests
// against the real API, where net0 is on the bridge with an uplink.
func (c *clusterRun) guestCard(t testing.TB, node string, vmid, offset int) {
	t.Helper()
	if !c.app.real {
		return
	}
	c.on(node).hereMust(t, "pct", "set", strconv.Itoa(vmid), "--net1",
		"name=eth1,bridge="+c.bridge+",ip="+c.lay.addr(offset)+"/24")
}

// agentVMID is the agent container of a node, 0 for none.
func (c *clusterRun) agentVMID(node string) int {
	c.agents.mu.Lock()
	defer c.agents.mu.Unlock()
	return c.agents.vmid[node]
}

// enforceAppliance leaves observe-only mode in the controller container.
func (c *clusterRun) enforceAppliance(t testing.TB) {
	t.Helper()
	c.suite.enforce(t)
}

// inAgent is the suite as seen from the agent container of a node.
func (c *clusterRun) inAgent(node string) *suite {
	s := c.on(node)
	s.at.vmid = c.agentVMID(node)
	s.at.pco = "pco"
	s.at.env = nil
	return s
}

// copyArtefacts puts the installer, the template and the checksums of the
// run on a node that installs an agent.
func (c *clusterRun) copyArtefacts(t testing.TB, node string) (installer, template, checksums string) {
	t.Helper()
	if node == c.self {
		return c.app.nodePco, c.app.template, c.app.checksums
	}
	c.on(node).hereMust(t, "mkdir", "-p", remoteArtefacts)
	installer = remoteArtefacts + "/pco"
	template = remoteArtefacts + "/" + filepath.Base(c.app.template)
	c.copyTo(t, node, c.app.nodePco, installer)
	c.copyTo(t, node, c.app.template, template)
	if c.app.checksums != "" {
		checksums = remoteArtefacts + "/checksums.txt"
		c.copyTo(t, node, c.app.checksums, checksums)
	}
	return installer, template, checksums
}

// joinToken has the controller make the join token of a node into a file
// in its container, and returns the token, the file gone again.
func (c *clusterRun) joinToken(t testing.TB, node string) string {
	t.Helper()
	file := "/root/e2e-" + node + ".join"
	out := c.pcoMust(t, "cluster", "join-token", "--node", node, "--file", file)
	c.logf(t, "pco cluster join-token --node %s:\n%s", node, out)
	token, err := c.readHere(file)
	require.NoError(t, err)
	_, _ = c.here("rm", "-f", file)
	token = strings.TrimSpace(token)
	require.True(t, strings.HasPrefix(token, "pco1."), "the join token is not one: %d characters", len(token))
	c.secret.add(joinSecrets(token)...)
	return token
}

// installAgent installs the agent container of a node with a join token,
// and returns what the installer printed and how it ended. While it runs,
// the arguments of every process of the node are read every second.
func (c *clusterRun) installAgent(t testing.TB, node, token string) (string, error) {
	t.Helper()
	vmid := c.lay.vmid(agentOffset + slices.Index(c.others(c.self), node))
	installer, template, checksums := c.copyArtefacts(t, node)
	joinFile := "/root/e2e-agent.join"
	c.writeOn(t, node, joinFile, token+"\n", "0600")
	defer func() { _, _ = c.on(node).here("rm", "-f", joinFile) }()
	c.writeOn(t, node, applianceFile, fmt.Sprintf("vmid=%d\nrange=%d-%d\nmode=%s\ninstall=%s\n", vmid, clusterFirst, clusterLast, c.mode(), c.install), "0600")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
			}
			out, _ := c.on(node).here("ps", "-eo", "args")
			c.agents.mu.Lock()
			c.agents.seen[node] = append(c.agents.seen[node], out)
			c.agents.mu.Unlock()
		}
	}()
	args := []string{installer}
	args = append(args, c.containerArgs("agent", vmid, agentOffset+slices.Index(c.others(c.self), node), template, checksums)...)
	args = append(args, "--join-file", joinFile)
	c.agents.mu.Lock()
	c.agents.vmid[node] = vmid
	c.agents.mu.Unlock()
	out, err := c.onWithin(node, installTimeout, args[0], args[1:]...)
	close(stop)
	wg.Wait()
	c.logf(t, "pco appliance install --kind agent on %s (%v):\n%s", node, err, out)
	if err == nil {
		c.guestCard(t, node, vmid, agentOffset+slices.Index(c.others(c.self), node))
	}
	return out, err
}

// uninstallAgent removes the agent container of a node.
func (c *clusterRun) uninstallAgent(t testing.TB, node string) error {
	c.agents.mu.Lock()
	vmid, ok := c.agents.vmid[node]
	c.agents.mu.Unlock()
	if !ok {
		return nil
	}
	installer := remoteArtefacts + "/pco"
	if node == c.self {
		installer = c.app.nodePco
	}
	out, err := c.onWithin(node, installTimeout, installer, "appliance", "uninstall", "--vmid", strconv.Itoa(vmid), "--yes", "--keep-cloudflare")
	c.logf(t, "pco appliance uninstall --vmid %d on %s (%v):\n%s", vmid, node, err, out)
	if err != nil {
		return err
	}
	c.agents.mu.Lock()
	delete(c.agents.vmid, node)
	c.agents.mu.Unlock()
	_, _ = c.on(node).here("rm", "-f", applianceFile)
	_, _ = c.on(node).here("rmdir", markerDir)
	return nil
}

func (c *clusterRun) uninstallAgents(t *testing.T) {
	for _, n := range c.others(c.self) {
		if err := c.uninstallAgent(t, n); err != nil {
			t.Errorf("uninstalling the agent of %s: %v; cleanup.sh in the appliance profile on %s removes it", n, err, n)
			continue
		}
		_, _ = c.on(n).here("rm", "-rf", remoteArtefacts)
	}
}

// uninstallControllerContainer removes the controller, which purges what the
// install has at Cloudflare, and checks that the tunnels pinned to the
// nodes went too.
func (c *clusterRun) uninstallControllerContainer(t *testing.T) {
	if !c.app.installed {
		return
	}
	c.uninstallAppliance(t)
	if c.app.installed || c.install == "" {
		return
	}
	if left := c.leftOfInstall(t); len(left) > 0 {
		t.Errorf("left at Cloudflare after the purge:\n  %s", strings.Join(left, "\n  "))
	}
}

// agentView is what the agent container of a node says of itself.
func (c *clusterRun) agentView(t testing.TB, node string) agentproto.AgentView {
	t.Helper()
	out, err := c.inAgent(node).pcoOut("agent", "status", "--json")
	require.Contains(t, []int{0, 1}, exitCode(err), "pco agent status in the agent of %s: %v", node, err)
	var v agentproto.AgentView
	require.NoError(t, json.Unmarshal([]byte(out), &v), out)
	return v
}

func testControllerContainer(t *testing.T, c *clusterRun) {
	st := c.waitState(t, "the controller's node in the registry", 3*time.Minute, func(st engine.State) bool {
		n, ok := nodeView(st, c.self)
		return ok && n.State == store.NodeActive && n.Controller
	})
	require.Equal(t, engine.ModeEnforce, st.Mode)
	addr := c.lay.addr(controllerOffset)
	if c.app.real {
		addr = ""
	}
	listening := c.hereMust(t, "ss", "-Hltn", "sport = :8644")
	c.logf(t, "the agent endpoint listens on:\n%s", listening)
	require.NotEmpty(t, strings.TrimSpace(listening), "nothing listens on 8644 in the controller")
	for line := range strings.Lines(listening) {
		f := strings.Fields(line)
		require.GreaterOrEqual(t, len(f), 4, line)
		require.NotContains(t, []string{"*:8644", "0.0.0.0:8644", "[::]:8644"}, f[3], "the agent endpoint listens on every address")
		if addr != "" {
			require.Equal(t, addr+":8644", f[3])
		}
	}
	rules := c.must(t, "pvesh", "get", fmt.Sprintf("/nodes/%s/lxc/%d/firewall/rules", c.self, c.app.vmid), "--output-format", "json")
	c.logf(t, "the firewall rules of the controller container: %s", rules)
	require.Contains(t, rules, "8644")
	require.Contains(t, rules, "pco-agents")
}

func testAgentsJoin(t *testing.T, c *clusterRun) {
	for _, node := range c.others(c.self)[:2] {
		token := c.joinToken(t, node)
		c.agents.mu.Lock()
		c.agents.tokens[node] = token
		c.agents.mu.Unlock()
		_, err := c.installAgent(t, node, token)
		require.NoError(t, err)
		c.waitAppliance(t, c.agentVMID(node))
		st := c.waitState(t, node+" joining", 3*time.Minute, func(st engine.State) bool {
			n, ok := nodeView(st, node)
			return ok && n.State == store.NodeJoining
		})
		n, _ := nodeView(st, node)
		require.Equal(t, "appliance", n.Kind)
		require.Equal(t, c.agentVMID(node), n.VMID)

		v := c.agentView(t, node)
		require.Len(t, v.CertFingerprint, 64, "the fingerprint of the agent of %s: %q", node, v.CertFingerprint)
		require.Zero(t, v.Connectors, "an agent not admitted runs connectors")
		fp := strings.ToLower(v.CertFingerprint)
		for _, wrong := range []string{fp[:7], wrongPrefix(fp[:8])} {
			out, err := c.pcoCombined("node", "admit", node, "--fingerprint", wrong)
			c.logf(t, "pco node admit %s --fingerprint %s:\n%s", node, wrong, out)
			require.Error(t, err, "admitted with %q", wrong)
		}
		out := c.pcoMust(t, "node", "admit", node, "--fingerprint", fp[:12])
		c.logf(t, "pco node admit %s:\n%s", node, out)
		c.waitState(t, node+" active", 3*time.Minute, func(st engine.State) bool {
			n, ok := nodeView(st, node)
			return ok && n.State == store.NodeActive
		})
		v = c.agentView(t, node)
		require.True(t, v.Heard, "the agent of %s heard no heartbeat", node)
		require.Empty(t, v.StoodDown)
	}
}

// wrongPrefix is a prefix of the same length that is not the fingerprint's.
func wrongPrefix(p string) string {
	if p[0] == '0' {
		return "1" + p[1:]
	}
	return "0" + p[1:]
}

func testAgentRoutes(t *testing.T, c *clusterRun) {
	nodes := c.others(c.self)[:2]
	var gs []guest
	var hosts []string
	for i, node := range nodes {
		name := fmt.Sprintf("ac-%d", i+1)
		g := c.createOn(t, node, i+1, guest{notes: "cf-tunnel: e2e-" + name + ".ZONE -> :8080"})
		gs = append(gs, g)
		hosts = append(hosts, c.host(name))
	}
	// Every segment is acknowledged before a route at observed is served
	// there.
	c.waitState(t, "the routes of the agents' guests seen", 3*time.Minute, func(st engine.State) bool {
		for _, h := range hosts {
			if _, ok := routeOf(st, h, ""); !ok {
				return false
			}
		}
		return true
	})
	out, err := c.pcoCombined("segment", "acknowledge", c.bridge)
	c.logf(t, "pco segment acknowledge %s:\n%s", c.bridge, out)
	require.NoError(t, err)
	for i, node := range nodes {
		st := c.waitState(t, hosts[i]+" active and pinned to "+node, 5*time.Minute, func(st engine.State) bool {
			r, ok := routeOf(st, hosts[i], "")
			return ok && r.State == planner.StateActive && placedOn(r, node)
		})
		r, _ := routeOf(st, hosts[i], "")
		rep, ok := replicaOf(r, node)
		require.True(t, ok, "%s has no replica on %s", hosts[i], node)
		require.Equal(t, "observed", rep.Level)
		require.False(t, rep.Attested, "an appliance agent's attestation was taken")
		c.waitRecord(t, hosts[i], c.pinnedTunnel(t, node), 3*time.Minute)
		c.inAgent(node).waitTarget(t, gs[i].ip()+":8080", true, 2*time.Minute, time.Second)
	}
}

// joinSecrets are the parts of a join token that must not appear anywhere:
// the token, and its payload as the token carries it.
func joinSecrets(token string) []string {
	_, payload, _ := strings.Cut(token, ".")
	return []string{token, payload}
}

func testJoinTokenNowhere(t *testing.T, c *clusterRun) {
	c.agents.mu.Lock()
	tokens := maps.Clone(c.agents.tokens)
	seen := map[string][]string{}
	for n, l := range c.agents.seen {
		seen[n] = slices.Clone(l)
	}
	c.agents.mu.Unlock()
	require.NotEmpty(t, tokens, "no agent joined")
	since := c.began.Format("2006-01-02 15:04:05")
	events := c.pcoMust(t, "events", "--json", "--since", c.began.UTC().Format(time.RFC3339))
	controllerJournal, _ := c.here("journalctl", "--since", since, "--no-pager", "-o", "cat")
	nodeJournal, _ := c.run("journalctl", "--since", since, "--no-pager", "-o", "cat")
	for node, token := range tokens {
		agentJournal, _ := c.inAgent(node).here("journalctl", "--since", since, "--no-pager", "-o", "cat")
		hostJournal, _ := c.on(node).here("journalctl", "--since", since, "--no-pager", "-o", "cat")
		doctorOut, _ := c.inAgent(node).pcoOut("doctor", "--json")
		where := map[string]string{
			"the events of the controller":              events,
			"the journal of the controller container":   controllerJournal,
			"the journal of " + c.self:                  nodeJournal,
			"the journal of the agent of " + node:       agentJournal,
			"the journal of " + node:                    hostJournal,
			"doctor in the agent of " + node:            doctorOut,
			"the arguments of the processes of " + node: strings.Join(seen[node], "\n"),
		}
		require.NotEmpty(t, seen[node], "the processes of %s were not read during the install", node)
		for name, text := range where {
			for _, secret := range joinSecrets(token) {
				require.False(t, strings.Contains(text, secret), "the join token of %s is in %s", node, name)
			}
		}
	}
}

func testJoinTokenReused(t *testing.T, c *clusterRun) {
	nodes := c.others(c.self)[:2]
	first, second := nodes[0], nodes[1]
	c.agents.mu.Lock()
	used := c.agents.tokens[first]
	c.agents.mu.Unlock()
	require.NotEmpty(t, used)

	c.logf(t, "the agent of %s goes, and its node is forgotten", second)
	require.NoError(t, c.uninstallAgent(t, second))
	c.waitState(t, second+" no longer active", 3*time.Minute, func(st engine.State) bool {
		n, ok := nodeView(st, second)
		return !ok || n.State != store.NodeActive
	})
	if st := c.state(t); func() bool { n, ok := nodeView(st, second); return ok && n.State != store.NodeRemoved }() {
		out, err := c.pcoCombined("node", "forget", second, "--yes")
		c.logf(t, "pco node forget %s:\n%s", second, out)
		require.NoError(t, err)
	}

	c.logf(t, "the used token of %s, on %s", first, second)
	reused := time.Now()
	out, err := c.installAgent(t, second, used)
	require.Error(t, err, "an agent joined with a used token of another node:\n%s", out)
	require.NoError(t, c.uninstallAgentLeft(t, second))
	n, _ := nodeView(c.state(t), first)
	c.logf(t, "%s is %s after its token was used again", first, n.State)
	var sec []engine.Event
	require.NoError(t, json.Unmarshal([]byte(c.pcoMust(t, "events", "--kind", "security", "--json", "--since", reused.UTC().Format(time.RFC3339))), &sec))
	require.NotEmpty(t, sec, "no security event of the refused use of the token of %s", first)
	for _, ev := range sec {
		c.logf(t, "security event: %s %s: %s", ev.Node, ev.Subject, ev.Message)
	}

	c.logf(t, "a token of its own for %s", second)
	token := c.joinToken(t, second)
	_, err = c.installAgent(t, second, token)
	require.NoError(t, err)
	c.waitAppliance(t, c.agentVMID(second))
	v := c.agentView(t, second)
	c.pcoMust(t, "node", "admit", second, "--fingerprint", strings.ToLower(v.CertFingerprint)[:12])
	c.waitState(t, second+" active again", 3*time.Minute, func(st engine.State) bool {
		n, ok := nodeView(st, second)
		return ok && n.State == store.NodeActive
	})
}

// uninstallAgentLeft removes what a refused install of an agent left on a
// node, if it left a container.
func (c *clusterRun) uninstallAgentLeft(t testing.TB, node string) error {
	c.agents.mu.Lock()
	vmid := c.agents.vmid[node]
	c.agents.mu.Unlock()
	if guestNode(vmid) == "" {
		c.agents.mu.Lock()
		delete(c.agents.vmid, node)
		c.agents.mu.Unlock()
		return nil
	}
	return c.uninstallAgent(t, node)
}
