//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// clusterScenario is a step of a run of the cluster profile, named by its id.
type clusterScenario struct {
	name string
	run  func(*testing.T, *clusterRun)
}

// TestCluster runs pco on every node of a cluster: one install, set up on
// the node the suite runs on and joined by the others, and the scenarios in
// their order. Each one leaves the cluster with every node active and a
// controller, as it found it, unless it says otherwise.
func TestCluster(t *testing.T) {
	p, err := chooseProfile()
	require.NoError(t, err)
	if p != profileCluster {
		t.Skipf("the profile is %s: TestCluster runs in the profile %s", p, profileCluster)
	}
	c := newCluster(t, p)
	c.start(t)
	for _, sc := range clusterScenarios {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, c) })
	}
}

var clusterScenarios = []clusterScenario{
	{"C1 every node joins, one holds the lease", testNodesJoin},
	{"C2 guests of every node served from every node", testSharedEverywhere},
	{"C3 an agent stopped pins the routes, a pinned guest moves", testCoverageLost},
	{"C4 an agent restarted every 50 seconds", testFlappingAgent},
	{"C5 drain and undrain", testDrain},
	{"C6 a clone of a MAC is blocked on every node", testMACCloneBlocked},
	{"C7 a network attached on every node", testNetworkOnEveryNode},
	{"C8 a managed guest on another node", testManagedOnAnotherNode},
	{"C9 the agent endpoint takes the registered nodes only", testListenerAllowlist},
	{"C10 doctor across the cluster", testDoctorCluster},
	{"C11 hand-off", testHandoff},
	{"C12 daemon of the controller killed", testControllerKilled},
	{"C13 controller's node rebooted", testControllerRebooted},
	{"C14 takeover while an agent is late", testTakeoverHold},
	{"C15 corosync restarted on every node", testQuorumBlip},
	{"C16 controller hung until the watchdog", testWatchdog},
	{"C17 controller's node loses quorum", testControllerNotQuorate},
	{"C18 controller stopped past the expiry", testControllerStopped},
	{"C19 leader.json restored from an earlier tenure", testRestoredLeader},
	{"C20 no controller for a while", testControllerAbsent},
	{"C21 controller's node powered off", testControllerPowerLoss},
	{"C22 controller's node suspended past the expiry", testControllerSuspended},
	{"C23 live migration under requests", testLiveMigration},
	{"C24 a node forgotten while off, and its return", testForgetPoweredOff},
	{"C25 forget as compromised", testForgetCompromised},
	{"C26 upgrade node by node", testRollingUpgrade},
	{"C27 uninstall a node, then the whole cluster", testUninstallCluster},
}

// homed is a guest of a node with its route.
type homed struct {
	g    guest
	host string
	node string
}

// ensureHomes makes a guest on each of three nodes, unless the run has them,
// and waits until their routes are active.
func (c *clusterRun) ensureHomes(t testing.TB) []homed {
	t.Helper()
	if len(c.homes) == 0 {
		for i, n := range c.names()[:3] {
			name := fmt.Sprintf("c-%d", i+1)
			g := c.createOn(t, n, i+1, guest{notes: "cf-tunnel: e2e-" + name + ".ZONE -> :8080"})
			c.homes = append(c.homes, homed{g: g, host: c.host(name), node: n})
		}
	}
	for _, h := range c.homes {
		c.waitController(t, "route "+h.host+" active", 3*time.Minute, func(st engine.State) bool {
			r, ok := routeOf(st, h.host, "")
			return ok && r.State == planner.StateActive
		})
	}
	return c.homes
}

// hostsOf are the hostnames of homes, but those of the nodes named.
func hostsOf(homes []homed, except ...string) []string {
	var out []string
	for _, h := range homes {
		if !slices.Contains(except, h.node) {
			out = append(out, h.host)
		}
	}
	return out
}

// directTarget is the address and port a route's connectors dial.
func directTarget(t testing.TB, r engine.RouteView) string {
	t.Helper()
	u, err := url.Parse(r.Service)
	require.NoError(t, err, r.Service)
	return u.Host
}

// sharedEverywhere says why a route is not what a cluster with every agent
// up serves: active on the shared tunnel, in the egress set of every replica,
// at port on the hosting node and at port (attested) by it on the others.
func sharedEverywhere(st engine.State, h homed, nodes []string) error {
	r, ok := routeOf(st, h.host, "")
	switch {
	case !ok || r.State != planner.StateActive:
		return fmt.Errorf("%s is not active", h.host)
	case !placedOn(r, ""):
		return fmt.Errorf("%s is placed %+v", h.host, r.Placement)
	case r.GuestNode != h.node:
		return fmt.Errorf("%s has its guest on %q, not %s", h.host, r.GuestNode, h.node)
	}
	for _, n := range nodes {
		rep, ok := replicaOf(r, n)
		switch {
		case !ok:
			return fmt.Errorf("%s has no replica on %s", h.host, n)
		case !rep.OK || !rep.InSet:
			return fmt.Errorf("%s on %s: ok %v, in the set %v, %s", h.host, n, rep.OK, rep.InSet, rep.Reason)
		case rep.Level != "port":
			return fmt.Errorf("%s on %s is at %q", h.host, n, rep.Level)
		case n == h.node && rep.Attested:
			return fmt.Errorf("%s is attested on its own node %s", h.host, n)
		case n != h.node && (!rep.Attested || rep.AttestedBy != h.node):
			return fmt.Errorf("%s on %s is not attested by %s: %+v", h.host, n, h.node, rep)
		}
	}
	return nil
}

// waitCheck polls the controller's state until check finds nothing wrong,
// and fails with what it found last.
func (c *clusterRun) waitCheck(t testing.TB, what string, timeout time.Duration, check func(engine.State) error) engine.State {
	t.Helper()
	var why error
	st, _ := c.waitControllerOr(t, what, timeout, func(st engine.State) bool {
		why = check(st)
		return why == nil
	}, func() string { return fmt.Sprint(why) })
	return st
}

// waitControllerOr is waitController that says why the last state did not
// do, in the words of why.
func (c *clusterRun) waitControllerOr(t testing.TB, what string, timeout time.Duration, ok func(engine.State) bool, why func() string) (engine.State, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, holder, err := c.controllerState()
		if err == nil && ok(st) {
			return st, holder
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s: %s (the controller: %v)", timeout, what, why(), err)
		}
		time.Sleep(time.Second)
	}
}

// allShared waits until every home is served from every node as a cluster
// with every agent up serves it.
func (c *clusterRun) allShared(t testing.TB, timeout time.Duration) engine.State {
	t.Helper()
	homes := c.ensureHomes(t)
	nodes := c.names()
	return c.waitCheck(t, "every route shared and attested", timeout, func(st engine.State) error {
		for _, h := range homes {
			if err := sharedEverywhere(st, h, nodes); err != nil {
				return err
			}
		}
		return nil
	})
}

// recordCall is the fake's log of calls from an index on, of one method.
func (c *clusterRun) callsSince(from int, method string) []string {
	if c.fake == nil {
		return nil
	}
	calls := c.fake.f.Calls()
	var out []string
	for _, call := range calls[min(from, len(calls)):] {
		if strings.Fields(call)[0] == method {
			out = append(out, call)
		}
	}
	return out
}

// callIndex is where the fake's log of calls is now.
func (c *clusterRun) callIndex() int {
	if c.fake == nil {
		return 0
	}
	return len(c.fake.f.Calls())
}

func testNodesJoin(t *testing.T, c *clusterRun) {
	names := c.names()
	st := c.waitActive(t, names...)
	require.Len(t, st.Nodes, len(names))
	controllers := 0
	indexes := map[int]string{}
	for _, n := range st.Nodes {
		if n.Controller {
			controllers++
		}
		require.Equal(t, "host", n.Kind, "%+v", n)
		require.NotNil(t, n.Index, "node %s has no index", n.Name)
		require.NotContains(t, indexes, *n.Index, "nodes %s and %s share index %d", n.Name, indexes[*n.Index], *n.Index)
		indexes[*n.Index] = n.Name
		require.True(t, n.WatchRunning, "the watch of %s does not run", n.Name)
	}
	require.Equal(t, 1, controllers)
	require.NotNil(t, st.Lease)
	require.True(t, st.Lease.Valid)
	require.Equal(t, st.Node, st.Lease.Holder)
	require.NotNil(t, st.Cluster)
	require.True(t, st.Cluster.Quorate)
	require.ElementsMatch(t, names, st.Cluster.Members)

	for _, n := range names {
		v := c.agentStatus(t, n)
		require.Equal(t, n, v.Node)
		require.True(t, v.Heard, "the agent of %s heard no heartbeat", n)
		require.Empty(t, v.StoodDown, "the agent of %s stood down", n)
		require.Equal(t, n != st.Node, v.Remote, "the agent of %s", n)
		require.Equal(t, st.Lease.Generation, v.Epoch.Generation, "the epoch the agent of %s follows", n)
		require.Equal(t, c.install, v.Epoch.InstallID)
		require.NotEmpty(t, v.CertFingerprint)
	}

	ctl := c.on(st.Node)
	var listed []engine.NodeView
	require.NoError(t, json.Unmarshal([]byte(ctl.pcoMust(t, "node", "list", "--json")), &listed))
	require.Len(t, listed, len(names))
	for _, n := range listed {
		require.Equal(t, store.NodeActive, n.State, "%+v", n)
	}
	_, err := ctl.pcoOut("cluster", "status")
	require.NoError(t, err, "pco cluster status exits 1 with a node unknown, down or contested")
	cs := c.clusterStatus(t)
	require.Equal(t, st.Node, cs.Cluster.Controller)

	t.Log("a node that is not the controller sends what the controller answers there")
	other := c.others(st.Node)[0]
	out, err := c.on(other).pcoCombined("sync")
	require.Error(t, err)
	require.Contains(t, out, "this node is not the controller; run it on "+st.Node)

	evs := ctl.events(t, c.began)
	for _, n := range c.others(c.self) {
		require.True(t, slices.ContainsFunc(evs, func(ev engine.Event) bool {
			return ev.Kind == "node" && strings.Contains(ev.Message, n) && strings.Contains(ev.Message, "is active")
		}), "no node event says %s is active: %+v", n, evs)
	}
}

func testSharedEverywhere(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	st := c.allShared(t, 5*time.Minute)
	shared := c.sharedTunnel(t)
	for _, h := range homes {
		c.waitRecord(t, h.host, shared, 2*time.Minute)
		r, _ := routeOf(st, h.host, "")
		target := directTarget(t, r)
		require.Equal(t, h.g.ip()+":8080", target)
		c.waitTargetEverywhere(t, target, true, time.Minute, c.names()...)
		c.requireReachedFrom(t, "http://"+target+"/", h.g.body(8080), c.names()...)
		if c.fake == nil {
			c.requireServed(t, h.host, h.g.body(8080))
		}
	}
	for _, n := range c.names() {
		v, _ := nodeView(st, n)
		require.True(t, v.Covered, "node %s is not covered: %+v", n, v)
		require.Zero(t, v.Pinned, "routes pinned to %s", n)
	}
	require.True(t, st.Cluster.Coverage.Covered, "%+v", st.Cluster.Coverage)
}

// spareNode is a node that neither holds the lease nor runs the suite, for a
// scenario that stops or cuts off its agent; a node that only does not hold
// the lease when there is no such node.
func (c *clusterRun) spareNode(t testing.TB) string {
	t.Helper()
	holder := c.holder(t)
	if spare := c.others(holder, c.self); len(spare) > 0 {
		return spare[0]
	}
	return c.others(holder)[0]
}

func testCoverageLost(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.allShared(t, 5*time.Minute)
	x := c.spareNode(t)
	others := slices.DeleteFunc(slices.Clone(homes), func(h homed) bool { return h.node == x })
	// A guest of its own that moves while pinned, from a node other than x
	// to another such node.
	y := others[0].node
	z := c.others(x, y)[0]
	mover := c.createOn(t, y, 4, guest{notes: "cf-tunnel: e2e-c3-move.ZONE -> :8080"})
	moverHost := c.host("c3-move")
	c.waitController(t, moverHost+" active", 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, moverHost, "")
		return ok && r.State == planner.StateActive
	})

	v := c.visit(t, hostsOf(homes, x)...)
	calls := c.callIndex()
	c.systemctl(t, x, "stop", "pco")
	t.Cleanup(func() { _, _ = c.on(x).here("systemctl", "start", "pco") })
	line := fmt.Sprintf("node %s has no pco agent with a running watch; routes of other nodes' guests are pinned to their nodes", x)
	c.waitCheck(t, "the routes of other nodes' guests pinned to them", 4*time.Minute, func(st engine.State) error {
		if !slices.Contains(st.Problems, line) {
			return fmt.Errorf("no problem line %q in %q", line, st.Problems)
		}
		for _, h := range others {
			r, ok := routeOf(st, h.host, "")
			if !ok || r.State != planner.StateActive || !placedOn(r, h.node) {
				return fmt.Errorf("%s is not active and pinned to %s: %+v", h.host, h.node, r.Placement)
			}
		}
		return nil
	})
	for _, h := range others {
		c.waitRecord(t, h.host, c.pinnedTunnel(t, h.node), 3*time.Minute)
	}
	require.Empty(t, c.callsSince(calls, "DeleteRecord"), "a record was deleted when the routes were pinned")
	f, ok := findingOf(c.doctorOn(t, c.holder(t), false), "coverage")
	require.True(t, ok, "doctor has no coverage check")
	require.NotEqual(t, doctor.LevelOK, f.Level, "%+v", f)
	require.Contains(t, f.Detail+f.Fix, x)

	c.logf(t, "the guest %d moves from %s to %s while its routes are pinned", mover.vmid, y, z)
	moved := time.Now()
	c.on(y).hereMust(t, "pct", "migrate", strconv.Itoa(mover.vmid), z, "--restart")
	c.waitController(t, moverHost+" pinned to "+z, 5*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, moverHost, "")
		return ok && r.State == planner.StateActive && placedOn(r, z) && r.Placement.Move == nil
	})
	c.waitRecord(t, moverHost, c.pinnedTunnel(t, z), 3*time.Minute)
	require.True(t, slices.ContainsFunc(c.ctl(t).events(t, moved), func(ev engine.Event) bool {
		return ev.Kind == "placement" && strings.Contains(ev.Message, moverHost)
	}), "no placement event names %s", moverHost)
	t.Log("the rule in the old pinned tunnel goes 5 minutes after the record changed")
	oldTunnel := c.pinnedTunnel(t, y)
	deadline := time.Now().Add(9 * time.Minute)
	for oldTunnel != "" {
		if _, _, still := ruleOf(c.cloud.Ingress(t, oldTunnel), moverHost); !still {
			break
		}
		require.True(t, time.Now().Before(deadline), "the tunnel pinned to %s still has a rule for %s", y, moverHost)
		time.Sleep(10 * time.Second)
	}
	c.destroyOn(t, mover.vmid)

	t.Log("coverage back: the routes are shared again after 10 minutes")
	c.systemctl(t, x, "start", "pco")
	c.waitActive(t, c.names()...)
	c.allShared(t, 14*time.Minute)
	v.requireNoFailure(t, "the agent of "+x+" stopped and back")
	require.Empty(t, c.callsSince(calls, "DeleteRecord"), "a record was deleted")
}

func testFlappingAgent(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.allShared(t, 5*time.Minute)
	x := c.spareNode(t)
	others := slices.DeleteFunc(slices.Clone(homes), func(h homed) bool { return h.node == x })
	flap := 10 * time.Minute
	if raw := os.Getenv("PCO_E2E_FLAP"); raw != "" {
		d, err := time.ParseDuration(raw)
		require.NoError(t, err, "PCO_E2E_FLAP")
		flap = d
	}
	v := c.visit(t, hostsOf(homes, x)...)
	calls := c.callIndex()

	// Where each route is placed, every few seconds: a change of place is
	// a move.
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	var mu sync.Mutex
	places := map[string][]string{}
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for ctx.Err() == nil {
			if st, _, err := c.controllerState(); err == nil {
				mu.Lock()
				for _, h := range others {
					if r, ok := routeOf(st, h.host, ""); ok && r.Placement != nil {
						p := r.Placement.Kind + " " + r.Placement.Node
						if l := places[h.host]; len(l) == 0 || l[len(l)-1] != p {
							places[h.host] = append(l, p)
						}
					}
				}
				mu.Unlock()
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()

	c.logf(t, "the agent of %s restarted every 50 seconds for %s", x, flap)
	for end := time.Now().Add(flap); time.Now().Before(end); {
		c.systemctl(t, x, "restart", "pco")
		time.Sleep(50 * time.Second)
	}
	stop()
	<-polled
	mu.Lock()
	for host, l := range places {
		c.logf(t, "%s was placed: %s", host, strings.Join(l, " -> "))
		require.LessOrEqual(t, len(l), 2, "%s moved more than once: %v", host, l)
	}
	mu.Unlock()
	c.waitActive(t, c.names()...)
	c.allShared(t, 14*time.Minute)
	v.requireNoFailure(t, "the agent of "+x+" flapping")
	require.Empty(t, c.callsSince(calls, "DeleteRecord"), "a record was deleted")
}

func testDrain(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.allShared(t, 5*time.Minute)
	x := c.spareNode(t)
	v := c.visit(t, hostsOf(homes)...)
	out := c.ctl(t).pcoMust(t, "node", "drain", x)
	c.logf(t, "pco node drain %s:\n%s", x, out)
	c.waitCheck(t, x+" drained", 4*time.Minute, func(st engine.State) error {
		n, _ := nodeView(st, x)
		if n.State != store.NodeDraining || n.Drained != "admin" || n.ReplicaReady {
			return fmt.Errorf("%s is %+v", x, n)
		}
		for _, h := range homes {
			r, ok := routeOf(st, h.host, "")
			switch {
			case !ok || r.State != planner.StateActive:
				return fmt.Errorf("%s is not active", h.host)
			case h.node == x && !placedOn(r, x):
				return fmt.Errorf("%s is not pinned to the drained %s: %+v", h.host, x, r.Placement)
			case h.node != x && !placedOn(r, ""):
				return fmt.Errorf("%s is not shared: %+v", h.host, r.Placement)
			}
			if _, on := replicaOf(r, x); on && h.node != x {
				return fmt.Errorf("%s still has a replica on the drained %s", h.host, x)
			}
		}
		return nil
	})
	out = c.ctl(t).pcoMust(t, "node", "undrain", x)
	c.logf(t, "pco node undrain %s:\n%s", x, out)
	c.waitActive(t, x)
	c.allShared(t, 14*time.Minute)
	v.requireNoFailure(t, "the drain of "+x)
}

// macOf is the MAC of net0 of a container, wherever it runs.
func (c *clusterRun) macOf(t testing.TB, vmid int) string {
	t.Helper()
	node := guestNode(vmid)
	require.NotEmpty(t, node, "guest %d", vmid)
	var cfg map[string]any
	require.NoError(t, json.Unmarshal([]byte(c.must(t, "pvesh", "get", fmt.Sprintf("/nodes/%s/lxc/%d/config", node, vmid), "--output-format", "json")), &cfg))
	net0, _ := cfg["net0"].(string)
	for part := range strings.SplitSeq(net0, ",") {
		if mac, ok := strings.CutPrefix(part, "hwaddr="); ok {
			return strings.ToLower(mac)
		}
	}
	t.Fatalf("guest %d has no hwaddr on net0: %q", vmid, net0)
	return ""
}

// lineSeen runs a command on a node and sends the time it first prints a
// line that match says yes to.
func (c *clusterRun) lineSeen(t testing.TB, node string, match func(string) bool, name string, args ...string) <-chan time.Time {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := c.on(node).hereCommand(ctx, name, args...)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	seen := make(chan time.Time, 1)
	go func() {
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			if match(strings.ToLower(lines.Text())) {
				seen <- time.Now()
				break
			}
		}
		for lines.Scan() {
		}
	}()
	return seen
}

func testMACCloneBlocked(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.allShared(t, 5*time.Minute)
	victim := homes[0]
	cloner := c.others(victim.node)[0]
	mac := c.macOf(t, victim.g.vmid)
	target := victim.g.ip() + ":8080"
	addr := netip.MustParseAddr(victim.g.ip())

	// The forwarding table of the cloning node learns the MAC on the
	// clone's port with its first frame: the watch event the bound counts
	// from.
	cloneID := c.lay.vmid(5)
	port := fmt.Sprintf("veth%di0", cloneID)
	seen := c.lineSeen(t, cloner, func(l string) bool { return strings.Contains(l, mac) && strings.Contains(l, port) },
		"timeout", "600", "bridge", "monitor", "fdb")
	// Every node's egress set, asked side by side from before the clone
	// starts, until none has the target.
	var wg sync.WaitGroup
	left := make(map[string]time.Time)
	var mu sync.Mutex
	for _, n := range c.names() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadline := time.Now().Add(4 * time.Minute)
			for time.Now().Before(deadline) {
				if targets := c.targetsOn(n); targets != nil && !slices.Contains(targets, target) {
					mu.Lock()
					left[n] = time.Now()
					mu.Unlock()
					return
				}
				time.Sleep(250 * time.Millisecond)
			}
		}()
	}
	clone := c.createOn(t, cloner, 5, guest{mac: mac, tags: "e2e", notes: "a clone of the MAC of " + victim.g.owner()})
	var at time.Time
	select {
	case at = <-seen:
	case <-time.After(2 * time.Minute):
		t.Fatalf("%s never learnt %s on %s", cloner, mac, port)
	}
	wg.Wait()
	for _, n := range c.names() {
		when, ok := left[n]
		require.True(t, ok, "%s is still in the egress set of %s minutes after the clone", target, n)
		c.logf(t, "%s left the egress set of %s %s after the watch event", target, n, when.Sub(at))
		require.LessOrEqual(t, when.Sub(at), 2*time.Second+500*time.Millisecond, "%s left the egress set of %s too late", target, n)
	}
	cs := c.clusterStatus(t)
	i := slices.IndexFunc(cs.Cluster.Blocks, func(b engine.BlockView) bool { return b.Address == addr })
	require.GreaterOrEqual(t, i, 0, "no block of %s: %+v", addr, cs.Cluster.Blocks)
	require.Equal(t, cloner, cs.Cluster.Blocks[i].Node)

	t.Log("the clone gone: the block holds for 10 minutes after the last report")
	c.destroyOn(t, clone.vmid)
	gone := time.Now()
	for time.Since(gone) < 9*time.Minute {
		for _, n := range c.names() {
			c.on(n).requireNoTarget(t, target)
		}
		time.Sleep(30 * time.Second)
	}
	cs = c.clusterStatus(t)
	require.True(t, slices.ContainsFunc(cs.Cluster.Blocks, func(b engine.BlockView) bool { return b.Address == addr }),
		"the block of %s ended within 10 minutes of the last report", addr)
	out := c.ctl(t).pcoMust(t, "cluster", "unblock", addr.String())
	c.logf(t, "pco cluster unblock %s:\n%s", addr, out)
	c.waitTargetEverywhere(t, target, true, 3*time.Minute, c.names()...)
	c.allShared(t, 14*time.Minute)
}

// runtimeBridge makes a bridge on a node for the scenario, without a port
// that leads off the node, and removes it when the scenario ends.
func (c *clusterRun) runtimeBridge(t *testing.T, node, name string) {
	t.Helper()
	on := c.on(node)
	if _, err := on.here("ip", "link", "show", "dev", name); err == nil {
		t.Fatalf("the bridge %s exists on %s, and the suite did not make it", name, node)
	}
	on.hereMust(t, "ip", "link", "add", name, "type", "bridge")
	t.Cleanup(func() {
		if _, err := on.here("ip", "link", "del", name); err != nil {
			t.Errorf("removing the bridge %s on %s: %v", name, node, err)
		}
	})
	_, _ = on.here("sysctl", "-qw", "net.ipv6.conf."+name+".disable_ipv6=1")
	on.hereMust(t, "ip", "link", "set", "dev", name, "up")
}

func testNetworkOnEveryNode(t *testing.T, c *clusterRun) {
	const (
		br      = "pcotc1"
		legAddr = "10.51.0.250/24"
		guestIP = "10.51.0.10/24"
	)
	for _, n := range c.names() {
		c.runtimeBridge(t, n, br)
	}
	ctl := c.ctl(t)
	n := ctl.attach(t, br, 0, legAddr)
	t.Cleanup(func() { c.ctl(t).detach(t, n.ID) })
	require.Equal(t, store.ScopeNode, n.ScopeName(), "a bridge without an uplink is a segment of each node")

	// The same address on two nodes: two guests, each reached through its
	// own node's leg.
	nodes := c.names()[:2]
	var gs []guest
	var hosts []string
	for i, node := range nodes {
		name := fmt.Sprintf("c7-%d", i+1)
		g := c.createOn(t, node, 6+i, guest{bridge: br, addr: guestIP, notes: "cf-tunnel: e2e-" + name + ".ZONE -> :8080"})
		t.Cleanup(func() { c.destroyOn(t, g.vmid) })
		gs = append(gs, g)
		hosts = append(hosts, c.host(name))
	}
	st := c.waitCheck(t, "both routes active through their nodes' legs", 5*time.Minute, func(st engine.State) error {
		net, ok := networkOf(st, n.ID)
		if !ok {
			return fmt.Errorf("no network %s", n.ID)
		}
		for i, node := range nodes {
			leg, ok := legOn(net, node)
			if !ok || !leg.Up {
				return fmt.Errorf("the leg on %s: %+v", node, leg)
			}
			r, ok := routeOf(st, hosts[i], "")
			switch {
			case !ok || r.State != planner.StateActive:
				return fmt.Errorf("%s is not active", hosts[i])
			case r.Path == nil || r.Path.Via != engine.ViaAttach+br:
				return fmt.Errorf("%s is not through the leg: %+v", hosts[i], r.Path)
			case !placedOn(r, node):
				return fmt.Errorf("%s is not pinned to %s: %+v", hosts[i], node, r.Placement)
			}
		}
		return nil
	})
	var services []netip.Addr
	for i, node := range nodes {
		r, _ := routeOf(st, hosts[i], "")
		svc := serviceOf(t, r)
		services = append(services, svc)
		c.requireReachedFrom(t, "http://"+svc.String()+":8080/", gs[i].body(8080), node)
		c.on(node).waitTarget(t, svc.String()+":8080", true, time.Minute, time.Second)
	}
	require.NotEqual(t, services[0], services[1], "one service address for two guests")
	entries, err := os.ReadDir(filepath.Join(store.DefaultPaths().Cluster, "nodesvcaddrs"))
	require.NoError(t, err)
	for _, node := range nodes {
		require.True(t, slices.ContainsFunc(entries, func(e os.DirEntry) bool { return strings.Contains(e.Name(), node) }),
			"no mapping of %s in nodesvcaddrs: %v", node, entries)
	}
}

func testManagedOnAnotherNode(t *testing.T, c *clusterRun) {
	const br = "pcotci"
	node := c.spareNode(t)
	c.runtimeBridge(t, node, br)
	c.registerTag(t, consentTag)
	c.editSettings(t, func(v *store.Settings) { v.Managed.DetachGrace = store.Duration(detachGrace) })
	c.managed = true
	out, err := c.ctl(t).pcoCombined("network", "managed", "enable", "--yes")
	c.logf(t, "pco network managed enable:\n%s", out)
	require.NoError(t, err)
	c.waitController(t, "the managed network enabled", 4*time.Minute, func(st engine.State) bool { return st.Managed.Enabled })

	g := c.createOn(t, node, 8, guest{bridge: br, addr: "10.90.0.71/24", tags: gateTag + ";" + consentTag,
		notes: "cf-tunnel: e2e-c8.ZONE -> :8080"})
	// The card on pconet must go before pco is uninstalled, so that the
	// managed network's zone can go with it.
	t.Cleanup(func() { c.destroyOn(t, g.vmid) })
	host := c.host("c8")
	st := c.waitCheck(t, host+" active through the managed network from "+node, 5*time.Minute, func(st engine.State) error {
		a, ok := attachmentOf(st, g.owner())
		if !ok || a.State != engine.AttachmentAttached {
			return fmt.Errorf("the card of %s: %+v", g.owner(), a)
		}
		r, ok := routeOf(st, host, "")
		switch {
		case !ok || r.State != planner.StateActive:
			return fmt.Errorf("%s is not active", host)
		case r.Path == nil || r.Path.Via != engine.ViaManaged:
			return fmt.Errorf("%s is not through the managed network: %+v", host, r.Path)
		case !placedOn(r, node):
			return fmt.Errorf("%s is not pinned to %s: %+v", host, node, r.Placement)
		}
		return nil
	})
	r, _ := routeOf(st, host, "")
	svc := serviceOf(t, r)
	c.requireReachedFrom(t, "http://"+svc.String()+":8080/", g.body(8080), node)
	c.on(node).waitTarget(t, svc.String()+":8080", true, time.Minute, time.Second)
}

// otherAddr is an IPv4 address of a node that is neither its address in the
// cluster nor on the bridge of the guests, "" when it has none.
func (c *clusterRun) otherAddr(t testing.TB, node string) string {
	t.Helper()
	out := c.on(node).hereMust(t, "ip", "-4", "-o", "addr", "show", "scope", "global")
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		i := slices.Index(f, "inet")
		if i < 0 || i+1 >= len(f) {
			continue
		}
		addr, _, _ := strings.Cut(f[i+1], "/")
		if addr != c.node(t, node).addr && addr != c.bridgeAddr[node] {
			return addr
		}
	}
	return ""
}

// agentCall makes a request to the controller's agent endpoint from a node,
// with the certificate files given (none for an empty dir), from the source
// address given (the node's choice for an empty one), and returns the curl
// exit status, the HTTP status and the body.
func (c *clusterRun) agentCall(t testing.TB, from, certDir, source string) (exit int, code, body string) {
	t.Helper()
	holder := c.holder(t)
	server := pki.NodeServerName(c.install, holder)
	args := []string{"-sS", "--max-time", "10", "-w", "\n%{http_code}",
		"--resolve", server + ":8644:" + c.node(t, holder).addr, "--cacert", "/var/lib/pco/agent/ca.pem"}
	if certDir != "" {
		args = append(args, "--cert", certDir+"/cert.pem", "--key", certDir+"/key.pem")
	}
	if source != "" {
		args = append(args, "--interface", source)
	}
	args = append(args, "https://"+server+":8644/v1/agent/e2e")
	out, err := c.on(from).here("curl", args...)
	out = strings.TrimSpace(out)
	i := strings.LastIndex(out, "\n")
	body, code = "", out
	if i >= 0 {
		body, code = out[:i], out[i+1:]
	}
	return exitCode(err), code, body
}

func testListenerAllowlist(t *testing.T, c *clusterRun) {
	holder := c.holder(t)
	others := c.others(holder)
	x, y := others[0], others[1]
	const own = "/var/lib/pco/agent"

	exit, code, body := c.agentCall(t, x, own, "")
	c.logf(t, "from %s with its own certificate: exit %d, %s %s", x, exit, code, body)
	require.NotEqual(t, "000", code, "a registered node with its own certificate gets an answer")

	if other := c.otherAddr(t, x); other == "" {
		c.logf(t, "%s has no address besides its address in the cluster and on %s: the source that is not a node is not tried", x, c.bridge)
	} else {
		began := time.Now()
		exit, code, _ = c.agentCall(t, x, own, other)
		took := time.Since(began)
		c.logf(t, "from %s at %s: exit %d, %s, after %s", x, other, exit, code, took)
		require.Equal(t, "000", code, "a source that is not a registered node got an answer")
		require.NotEqual(t, 28, exit, "the connection was not dropped but timed out")
		require.Less(t, took, 8*time.Second)
	}

	c.logf(t, "the certificate of %s used from %s", x, y)
	dir := "/run/pco-e2e-cert"
	t.Cleanup(func() { _, _ = c.on(y).here("rm", "-rf", dir) })
	for _, f := range []string{"cert.pem", "key.pem"} {
		content, err := c.on(x).readHere(own + "/" + f)
		c.secret.add(content)
		require.NoError(t, err)
		c.writeOn(t, y, dir+"/"+f, content, "0600")
	}
	exit, code, body = c.agentCall(t, y, dir, "")
	c.logf(t, "from %s with the certificate of %s: exit %d, %s %s", y, x, exit, code, body)
	require.Equal(t, "403", code)
	require.Contains(t, body, "other-source")
}

func testDoctorCluster(t *testing.T, c *clusterRun) {
	holder := c.holder(t)
	findings := c.doctorOn(t, holder, true)
	for _, f := range findings {
		c.logf(t, "%s: %s: %s", f.Level, f.Check, f.Detail)
	}
	for _, n := range c.others(holder) {
		f, ok := findingOf(findings, "answer on "+n)
		require.True(t, ok, "no answer of %s", n)
		require.Equal(t, doctor.LevelOK, f.Level, "%+v", f)
	}
	for _, check := range []string{"lease", "cluster members", "agent channel", "coverage", "certificates", "ca rotation", "upgrade", "node versions"} {
		f, ok := findingOf(findings, check)
		require.True(t, ok, "no %s check", check)
		require.NotEqual(t, doctor.LevelFail, f.Level, "%+v", f)
	}

	x := c.spareNode(t)
	c.logf(t, "the agent of %s stopped", x)
	c.systemctl(t, x, "stop", "pco")
	t.Cleanup(func() { _, _ = c.on(x).here("systemctl", "start", "pco") })
	time.Sleep(75 * time.Second)
	findings = c.doctorOn(t, c.holder(t), true)
	f, ok := findingOf(findings, "agent channel")
	require.True(t, ok)
	require.Equal(t, doctor.LevelFail, f.Level, "%+v", f)
	require.Contains(t, f.Detail, x)
	f, ok = findingOf(findings, "coverage")
	require.True(t, ok)
	require.NotEqual(t, doctor.LevelOK, f.Level, "%+v", f)
	c.systemctl(t, x, "start", "pco")
	c.waitActive(t, c.names()...)

	c.logf(t, "the agent channel of %s cut", x)
	c.cutOff(t, x, "tcp dport 8644 drop", "tcp sport 8644 drop")
	time.Sleep(75 * time.Second)
	f, ok = findingOf(c.doctorOn(t, x, false), "agent dial")
	require.True(t, ok, "no agent dial check on %s", x)
	require.Equal(t, doctor.LevelFail, f.Level, "%+v", f)
	f, ok = findingOf(c.doctorOn(t, c.holder(t), false), "agent channel")
	require.True(t, ok)
	require.Equal(t, doctor.LevelFail, f.Level, "%+v", f)
	c.uncut(t, x)
	c.waitActive(t, c.names()...)
}

// nodeLoad is requests from a node to a target, about 20 a second, until stop
// is called; done gives the time and the outcome of each.
type nodeLoad struct {
	node string
	stop func()
	done <-chan []loadResult
}

type loadResult struct {
	at time.Time
	ok bool
}

const loadStop = "/run/pco-e2e-load-stop"

func (c *clusterRun) startLoad(t testing.TB, node, target string) *nodeLoad {
	t.Helper()
	_, _ = c.on(node).here("rm", "-f", loadStop)
	script := fmt.Sprintf(`while [ ! -e %s ]; do t=$(date +%%s.%%N); if curl -fsS -m 1 -o /dev/null %s; then echo "$t ok"; else echo "$t fail"; fi; sleep 0.05; done`,
		loadStop, "http://"+target+"/")
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	out := c.background(ctx, node, "sh", "-c", script)
	done := make(chan []loadResult, 1)
	go func() {
		raw := <-out
		cancel()
		var results []loadResult
		for line := range strings.Lines(raw) {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			sec, err := strconv.ParseFloat(f[0], 64)
			if err != nil {
				continue
			}
			results = append(results, loadResult{at: time.Unix(0, int64(sec*1e9)), ok: f[1] == "ok"})
		}
		done <- results
	}()
	l := &nodeLoad{node: node, done: done}
	var once sync.Once
	l.stop = func() { once.Do(func() { _, _ = c.on(node).here("touch", loadStop) }) }
	t.Cleanup(l.stop)
	return l
}

// failedFor ends the load and returns how many requests failed and how long
// the failures spanned, from the first to the last.
func (l *nodeLoad) failedFor() (total, failed int, span time.Duration) {
	l.stop()
	results := <-l.done
	var first, last time.Time
	for _, r := range results {
		if r.ok {
			continue
		}
		failed++
		if first.IsZero() {
			first = r.at
		}
		last = r.at
	}
	return len(results), failed, last.Sub(first)
}

func testLiveMigration(t *testing.T, c *clusterRun) {
	image := skipWithoutImage(t)
	c.ensureHomes(t)
	dest := c.others(c.self)[0]
	v := vm{vmid: c.lay.vmid(9), addr: c.lay.addr(9), notes: c.in(guest{notes: "cf-tunnel: e2e-c23.ZONE -> :8080"}).notes}
	c.createClusterVM(t, v, image)
	c.serveVM(t, v, 8080)
	host := c.host("c23")
	c.waitController(t, host+" active", 5*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, "")
		return ok && r.State == planner.StateActive && placedOn(r, "")
	})
	target := v.addr + ":8080"
	loads := map[string]*nodeLoad{}
	for _, n := range c.names() {
		loads[n] = c.startLoad(t, n, target)
	}
	vis := c.visit(t, host)
	time.Sleep(10 * time.Second)
	out, err := c.combinedWithin(30*time.Minute, "qm", "migrate", strconv.Itoa(v.vmid), dest, "--online", "--with-local-disks")
	c.logf(t, "qm migrate %d %s --online --with-local-disks:\n%s", v.vmid, dest, out)
	require.NoError(t, err)
	time.Sleep(20 * time.Second)
	for n, l := range loads {
		total, failed, span := l.failedFor()
		c.logf(t, "from %s: %d requests, %d failed over %s", n, total, failed, span)
		require.NotZero(t, total, "no request was made from %s", n)
		if n == c.self {
			require.LessOrEqual(t, span, 3*time.Second, "requests through the source node failed for too long")
			continue
		}
		require.Zero(t, failed, "requests through %s, which is not the source, failed", n)
	}
	vis.requireNoFailure(t, "the live migration")
	c.waitController(t, host+" on "+dest, 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, "")
		return ok && r.State == planner.StateActive && r.GuestNode == dest
	})
	c.destroyOn(t, v.vmid)
}

// createClusterVM makes a virtual machine on the driver from the cloud
// image, on the bridge of the guests, as the host profile makes one on its
// bridge.
func (c *clusterRun) createClusterVM(t testing.TB, v vm, image string) {
	t.Helper()
	c.mu.Lock()
	c.cguests[v.vmid] = true
	c.mu.Unlock()
	id := strconv.Itoa(v.vmid)
	storage := envOr(vmStorageVar, c.disk)
	c.must(t, "qm", "create", id, "--name", "e2e-"+id, "--memory", "512", "--cores", "1", "--ostype", "l26",
		"--scsihw", "virtio-scsi-single", "--net0", "virtio,bridge="+c.bridge,
		"--serial0", "socket", "--vga", "serial0", "--tags", gateTag, "--description", v.notes)
	c.must(t, "qm", "set", id, "--scsi0", storage+":0,import-from="+image)
	public := c.sshKey(t)
	c.must(t, "qm", "set", id, "--ide2", storage+":cloudinit", "--boot", "order=scsi0",
		"--ciuser", vmUser, "--sshkeys", public, "--ipconfig0", "ip="+v.addr+"/24")
	c.must(t, "qm", "start", id)
	c.waitSSH(t, v)
}

func testForgetPoweredOff(t *testing.T, c *clusterRun) {
	c.skipWithoutPower(t)
	homes := c.ensureHomes(t)
	c.allShared(t, 5*time.Minute)
	x := c.spareNode(t)
	require.NotEqual(t, c.self, x)
	shared := c.sharedTunnel(t)
	oldToken := c.runToken(t, shared)
	v := c.visit(t, hostsOf(homes, x)...)

	c.powerAction(t, "off", x)
	c.waitController(t, x+" down", 3*time.Minute, func(st engine.State) bool {
		n, _ := nodeView(st, x)
		return n.State == store.NodeDown || n.State == store.NodeUnknown
	})
	calls := c.callIndex()
	out, err := c.ctl(t).pcoCombined("node", "forget", x, "--yes")
	c.logf(t, "pco node forget %s:\n%s", x, out)
	require.NoError(t, err)
	require.Contains(t, out, "Node "+x+" is forgotten.")
	c.waitController(t, x+" removed", 2*time.Minute, func(st engine.State) bool {
		n, ok := nodeView(st, x)
		return !ok || n.State == store.NodeRemoved
	})
	t.Log("the shared tunnel's secret rotated, and the remaining replicas run the new token")
	var newToken string
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if newToken = c.runToken(t, shared); newToken != oldToken {
			break
		}
		require.True(t, time.Now().Before(deadline), "the secret of the shared tunnel was not rotated")
		time.Sleep(5 * time.Second)
	}
	if c.fake != nil {
		require.NotEmpty(t, c.callsSince(calls, "RotateTunnelSecret"))
	}
	for _, n := range c.others(x) {
		deadline := time.Now().Add(5 * time.Minute)
		for {
			got, _ := c.on(n).readHere("/var/lib/pco/tunnels/" + shared + ".token")
			c.secret.add(got)
			if strings.TrimSpace(got) == newToken {
				break
			}
			require.True(t, time.Now().Before(deadline), "the connector of %s does not run the new token", n)
			time.Sleep(5 * time.Second)
		}
	}
	deadline = time.Now().Add(12 * time.Minute)
	for c.pinnedTunnel(t, x) != "" {
		require.True(t, time.Now().Before(deadline), "the tunnel pinned to %s was not deleted", x)
		time.Sleep(10 * time.Second)
	}
	v.requireNoFailure(t, "the forget of "+x)

	c.logf(t, "%s powered on again: it serves nothing and says it was removed", x)
	c.powerAction(t, "on", x)
	c.waitBack(t, x)
	time.Sleep(5 * time.Minute)
	a := c.agentStatus(t, x)
	require.Contains(t, a.StoodDown, "removed", "%+v", a)
	units := c.on(x).hereMust(t, "systemctl", "list-units", "--state=active", "--plain", "--no-legend", "--no-pager", "pco-cloudflared@*")
	require.Empty(t, strings.TrimSpace(units), "connectors run on the removed %s", x)
	got, _ := c.on(x).readHere("/var/lib/pco/tunnels/" + shared + ".token")
	c.secret.add(got)
	require.True(t, newToken != strings.TrimSpace(got), "the removed %s has the new token", x)

	c.logf(t, "%s joins again", x)
	out, err = c.pcoWithin(x, uninstallTimeout, "uninstall", "--yes", "--keep-cloudflare", "--force")
	c.logf(t, "pco uninstall on %s:\n%s", x, out)
	require.NoError(t, err)
	c.join(t, x)
	c.waitActive(t, c.names()...)
	for _, h := range homes {
		if h.node == x {
			_, _ = c.on(x).here("pct", "start", strconv.Itoa(h.g.vmid))
		}
	}
	c.allShared(t, 14*time.Minute)
}

func testForgetCompromised(t *testing.T, c *clusterRun) {
	holder := c.holder(t)
	x := c.others(holder)[0]
	out, err := c.on(holder).pcoCombined("node", "forget", x, "--compromised", "--yes")
	c.logf(t, "pco node forget %s --compromised while it is in the cluster:\n%s", x, out)
	require.Error(t, err)
	require.Contains(t, out, "pvecm delnode "+x)

	victim := os.Getenv("PCO_E2E_COMPROMISE")
	if victim == "" {
		t.Skip("PCO_E2E_COMPROMISE names no node to take out of the cluster for good: the rotation is not run")
	}
	c.skipWithoutPower(t)
	require.NotEqual(t, c.self, victim, "the node the suite runs on cannot be the compromised one")
	c.node(t, victim)
	newToken := fakeSecondToken
	if c.fake == nil {
		file := os.Getenv("PCO_E2E_CF_NEW_TOKEN_FILE")
		if file == "" {
			t.Skip("against the real API the rotation needs a second token, named with PCO_E2E_CF_NEW_TOKEN_FILE")
		}
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		newToken = strings.TrimSpace(string(raw))
		c.secret.add(newToken)
	}
	if c.holder(t) == victim {
		c.handoff(t, c.others(victim)[0])
	}
	c.allShared(t, 5*time.Minute)
	shared := c.sharedTunnel(t)
	oldToken := c.runToken(t, shared)
	oldCA := c.must(t, "sha256sum", "/etc/pve/priv/pco/ca/current.json")
	fingerprints := map[string]string{}
	remaining := c.others(victim)
	for _, n := range remaining {
		fingerprints[n] = c.agentStatus(t, n).CertFingerprint
	}
	// A client certificate of the old CA, which the new one refuses.
	keep := filepath.Join(c.dir, "old-cert")
	require.NoError(t, os.MkdirAll(keep, 0o700))
	spare := c.others(victim, c.holder(t))[0]
	for _, f := range []string{"cert.pem", "key.pem"} {
		content, err := c.on(spare).readHere("/var/lib/pco/agent/" + f)
		c.secret.add(content)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(keep, f), []byte(content), 0o600))
	}

	c.powerAction(t, "off", victim)
	out = c.must(t, "pvecm", "delnode", victim)
	c.logf(t, "pvecm delnode %s:\n%s", victim, out)
	c.mu.Lock()
	c.gone[victim] = true
	delete(c.down, victim)
	// The guests of the node went with it: no route of theirs comes back.
	c.homes = slices.DeleteFunc(c.homes, func(h homed) bool {
		if h.node == victim {
			delete(c.cguests, h.g.vmid)
			return true
		}
		return false
	})
	c.mu.Unlock()
	ctl := c.ctl(t)
	out, err = ctl.pcoCombined("node", "forget", victim, "--compromised", "--yes")
	c.logf(t, "pco node forget %s --compromised:\n%s", victim, out)
	require.NoError(t, err)

	t.Log("the admin replaces the Cloudflare token and the Proxmox secret")
	tokenFile := "/root/e2e-cf-token"
	c.writeOn(t, c.holder(t), tokenFile, newToken+"\n", "0600")
	out, err = ctl.pcoCombined("credential", "add", "--label", "e2e-rotated", "--token-file", tokenFile)
	_, _ = ctl.here("rm", "-f", tokenFile)
	c.logf(t, "pco credential add:\n%s", out)
	require.NoError(t, err)
	var creds []engine.CredentialView
	require.NoError(t, json.Unmarshal([]byte(ctl.pcoMust(t, "credential", "list", "--json")), &creds))
	for _, cr := range creds {
		if cr.Label != "e2e-rotated" {
			out, err := ctl.pcoCombined("credential", "remove", cr.ID)
			c.logf(t, "pco credential remove %s:\n%s", cr.ID, out)
			require.NoError(t, err)
		}
	}
	out, err = c.pcoWithin(c.holder(t), clusterTimeout, "setup", "--repair", "--yes")
	c.logf(t, "pco setup --repair:\n%s", out)
	require.NoError(t, err)

	t.Log("every tunnel's secret, then the CA; ten minutes later the old CA is refused")
	cs := c.waitClusterStatus(t, "the rotation finished", 30*time.Minute, func(cs engine.ClusterStatus) bool {
		r := cs.Cluster.CARotation
		return r != nil && !r.FinishedAt.IsZero()
	})
	require.Empty(t, cs.Cluster.CARotation.Refused)
	require.True(t, oldToken != c.runToken(t, shared), "the shared tunnel kept its secret")
	require.NotEqual(t, oldCA, c.must(t, "sha256sum", "/etc/pve/priv/pco/ca/current.json"), "the CA was not rotated")
	for _, n := range remaining {
		require.NotEqual(t, fingerprints[n], c.agentStatus(t, n).CertFingerprint, "the agent of %s was not enrolled again", n)
	}
	c.writeOn(t, spare, "/run/pco-e2e-cert/cert.pem", mustRead(t, filepath.Join(keep, "cert.pem")), "0600")
	c.writeOn(t, spare, "/run/pco-e2e-cert/key.pem", mustRead(t, filepath.Join(keep, "key.pem")), "0600")
	t.Cleanup(func() { _, _ = c.on(spare).here("rm", "-rf", "/run/pco-e2e-cert") })
	_, code, _ := c.agentCall(t, spare, "/run/pco-e2e-cert", "")
	require.Equal(t, "000", code, "a certificate of the old CA got an answer")
	c.allShared(t, 14*time.Minute)
}

func mustRead(t testing.TB, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(raw)
}

// waitClusterStatus polls pco cluster status on the controller until ok.
func (c *clusterRun) waitClusterStatus(t testing.TB, what string, timeout time.Duration, ok func(engine.ClusterStatus) bool) engine.ClusterStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		cs := c.clusterStatus(t)
		if ok(cs) {
			return cs
		}
		if time.Now().After(deadline) {
			raw, _ := json.MarshalIndent(cs, "", "  ")
			t.Fatalf("waited %s for %s; the last status:\n%s", timeout, what, raw)
		}
		time.Sleep(10 * time.Second)
	}
}

func testRollingUpgrade(t *testing.T, c *clusterRun) {
	deb := os.Getenv("PCO_E2E_UPGRADE_DEB")
	if deb == "" {
		t.Skip("PCO_E2E_UPGRADE_DEB names no package of a later build: the upgrade is not run")
	}
	abs, err := filepath.Abs(deb)
	require.NoError(t, err)
	homes := c.ensureHomes(t)
	c.allShared(t, 5*time.Minute)
	v := c.visit(t, hostsOf(homes)...)
	calls := c.callIndex()
	builds := map[string]string{}
	for _, n := range c.ctl(t).state(t).Nodes {
		builds[n.Name] = n.Build
	}
	const remote = "/root/e2e-upgrade.deb"
	holder := c.holder(t)
	order := append(c.others(holder), holder)
	for i, n := range order {
		c.copyTo(t, n, abs, remote)
		t.Cleanup(func() { _, _ = c.on(n).here("rm", "-f", remote) })
		gen := c.ctl(t).state(t).Lease.Generation
		upgraded := time.Now()
		out, err := c.onWithin(n, clusterTimeout, "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", remote)
		c.logf(t, "the package on %s:\n%s", n, out)
		require.NoError(t, err)
		st := c.waitCheck(t, n+" active with the new build", clusterTimeout, func(st engine.State) error {
			nv, _ := nodeView(st, n)
			if nv.State != store.NodeActive || nv.Build == builds[n] {
				return fmt.Errorf("%s: %s, build %s", n, nv.State, nv.Build)
			}
			if !st.At.After(upgraded) {
				return fmt.Errorf("no cycle since the upgrade")
			}
			return nil
		})
		if n != holder {
			require.Equal(t, gen, st.Lease.Generation, "the upgrade of %s, which does not hold the lease, changed the epoch", n)
		}
		if i == 0 {
			out, err := c.ctl(t).pcoCombined("cluster", "finish-upgrade", "--yes")
			c.logf(t, "pco cluster finish-upgrade with one node upgraded:\n%s", out)
			require.Error(t, err, "finish-upgrade with nodes on the earlier build")
		}
	}
	st := c.waitActive(t, c.names()...)
	first := st.Nodes[0].Build
	for _, n := range st.Nodes {
		require.Equal(t, first, n.Build, "the nodes run different builds")
	}
	c.allShared(t, 14*time.Minute)
	v.requireNoFailure(t, "the upgrade")
	require.Empty(t, c.callsSince(calls, "DeleteRecord"), "a record was deleted during the upgrade")
}

func testUninstallCluster(t *testing.T, c *clusterRun) {
	x := c.spareNode(t)
	c.logf(t, "%s leaves the install", x)
	out, err := c.pcoWithin(x, uninstallTimeout, "uninstall", "--yes", "--keep-cloudflare")
	c.logf(t, "pco uninstall on %s:\n%s", x, out)
	require.NoError(t, err)
	c.waitController(t, x+" removed", 3*time.Minute, func(st engine.State) bool {
		n, ok := nodeView(st, x)
		return !ok || n.State == store.NodeRemoved
	})
	after, err := c.nodeState(x)
	require.NoError(t, err)
	require.Equal(t, c.before[x], after, "%s is not as it was before pco was set up on it", x)
	for _, h := range c.homes {
		if h.node != x {
			c.waitController(t, h.host+" active", 3*time.Minute, func(st engine.State) bool {
				r, ok := routeOf(st, h.host, "")
				return ok && r.State == planner.StateActive
			})
		}
	}

	t.Log("the whole install goes")
	holder := c.holder(t)
	// The registered tags as they were before the run changed them, before
	// the uninstall takes away the tag setup registered.
	c.restoreRegisteredTags(t)
	out, err = c.pcoWithin(holder, uninstallTimeout, "uninstall", "--cluster", "--yes", "--purge-cloudflare", "--detach-managed")
	c.logf(t, "pco uninstall --cluster on %s:\n%s", holder, out)
	require.NoError(t, err)
	c.uninstalled = true
	for _, n := range c.others(holder, x) {
		out, err := c.pcoWithin(n, uninstallTimeout, "uninstall", "--yes", "--keep-cloudflare")
		c.logf(t, "pco uninstall on %s:\n%s", n, out)
		require.NoError(t, err)
	}
	require.Empty(t, c.leftOfInstall(t), "the install left something at Cloudflare")
	for _, n := range c.names() {
		after, err := c.nodeState(n)
		require.NoError(t, err)
		require.Equal(t, c.before[n], after, "%s is not as it was before pco was set up on it", n)
	}
	_, found, err := installed()
	require.NoError(t, err)
	require.False(t, found, "the store still holds the install")
}
