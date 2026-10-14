//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The bounds of a takeover the failover scenarios hold the cluster to.
const (
	// powerLossBound is how long after the controller's node lost power the
	// new epoch's first write may come: the lock runs out 120 seconds after
	// its last refresh, and a node polls for it every 5 seconds.
	powerLossBound = 150 * time.Second
	// rebootBound is how long after a clean reboot of the controller's node
	// the next controller may take for its first enforcing cycle, as the
	// stopping daemon releases the lease.
	rebootBound = 20 * time.Second
	// restartBound is how long a daemon killed with SIGKILL may take to
	// finish a cycle again with the same epoch.
	restartBound = 30 * time.Second
	// takeoverHold is how long a new controller writes nothing while an
	// agent has not reported to it.
	takeoverHold = 60 * time.Second
	// staleWrite is how long after a paused machine resumes a call it made
	// before may still land: the freshness window of the lease's write gate.
	staleWrite = 5 * time.Second
	// expiry is past the lock's expiry: another node takes the lease after it.
	expiry = 180 * time.Second
)

// startHomesOn starts the guests of the homes on a node that came back:
// the run makes them without starting at boot.
func (c *clusterRun) startHomesOn(t testing.TB, node string) {
	t.Helper()
	for _, h := range c.homes {
		if h.node == node {
			if out, _ := c.on(node).here("pct", "status", strconv.Itoa(h.g.vmid)); !strings.Contains(out, "running") {
				c.on(node).hereMust(t, "pct", "start", strconv.Itoa(h.g.vmid))
			}
		}
	}
}

// leaseOf is the lease the controller's state shows.
func (c *clusterRun) leaseOf(t testing.TB) engine.LeaseView {
	t.Helper()
	st, _ := c.waitController(t, "a controller", 5*time.Minute, func(engine.State) bool { return true })
	return *st.Lease
}

// settleCluster waits until the controller has a cycle with nothing left to
// do, so that what a scenario takes as before is not still changing.
func (c *clusterRun) settleCluster(t testing.TB) {
	t.Helper()
	asked := time.Now()
	c.ctl(t).sync(t)
	c.waitController(t, "a cycle with nothing to do", 3*time.Minute, func(st engine.State) bool {
		return st.At.After(asked) && st.Complete && len(st.Actions) == 0
	})
}

// waitSentinel waits until the shared tunnel's sentinels carry a
// generation above gen, and returns when the suite saw it.
func (c *clusterRun) waitSentinel(t testing.TB, shared string, gen int, timeout time.Duration) (time.Time, int) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := c.sentinelOf(t, shared); got > gen {
			return time.Now(), got
		}
		if time.Now().After(deadline) {
			t.Fatalf("no sentinel above generation %d in the shared tunnel within %s", gen, timeout)
		}
		time.Sleep(c.every())
	}
}

// ingressHosts is, for each tunnel of the install, the hostnames its rules
// name.
func (c *clusterRun) ingressHosts(t testing.TB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	tunnels := []string{c.sharedTunnel(t)}
	for _, n := range c.names() {
		if id := c.pinnedTunnel(t, n); id != "" {
			tunnels = append(tunnels, id)
		}
	}
	for _, id := range tunnels {
		var hosts []string
		for _, r := range c.cloud.Ingress(t, id) {
			if r.Hostname != "" {
				hosts = append(hosts, r.Hostname)
			}
		}
		slices.Sort(hosts)
		out[id] = hosts
	}
	return out
}

// requireNoWithdrawal checks that every tunnel still has the rules it had,
// and that the fake deleted no record and no tunnel since the index from.
func (c *clusterRun) requireNoWithdrawal(t testing.TB, before map[string][]string, from int) {
	t.Helper()
	now := c.ingressHosts(t)
	for id, hosts := range before {
		for _, h := range hosts {
			require.Contains(t, now[id], h, "the rule of %s left tunnel %s", h, id)
		}
	}
	for _, m := range []string{"DeleteRecord", "DeleteTunnel", "CleanUpConnections"} {
		require.Empty(t, c.callsSince(from, m), "the fake was asked to %s", m)
	}
}

func testHandoff(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.settleCluster(t)
	before := c.leaseOf(t)
	to := c.others(before.Holder)[0]
	shared := c.sharedTunnel(t)
	v := c.visit(t, hostsOf(homes)...)
	began := time.Now()
	st := c.handoff(t, to)
	took := time.Since(began)
	c.logf(t, "%s holds the lease %s after the hand-off from %s", to, took, before.Holder)
	require.Less(t, took, 30*time.Second)
	require.Equal(t, before.Generation+1, st.Lease.Generation)
	c.waitSentinel(t, shared, before.Generation, 2*time.Minute)
	c.allShared(t, 3*time.Minute)
	v.requireNoFailure(t, "the hand-off")
}

func testControllerKilled(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.settleCluster(t)
	before := c.leaseOf(t)
	holder := before.Holder
	v := c.visit(t, hostsOf(homes)...)
	from := c.callIndex()
	killed := time.Now()
	c.systemctl(t, holder, "kill", "--signal=SIGKILL", "pco.service")
	var others []string
	st, _ := c.waitController(t, "the killed controller back with its epoch", 2*time.Minute, func(st engine.State) bool {
		if st.Node != holder {
			others = append(others, st.Node)
		}
		return st.Node == holder && st.At.After(killed) && !st.FinishedAt.IsZero()
	})
	took := st.FinishedAt.Sub(killed)
	c.logf(t, "the first cycle after the kill finished %s after it", took)
	require.Empty(t, others, "another node took the lease")
	require.Equal(t, before.Generation, st.Lease.Generation)
	require.Equal(t, before.Nonce, st.Lease.Nonce)
	require.LessOrEqual(t, took, restartBound)
	c.settleCluster(t)
	require.Empty(t, c.callsSince(from, "PutTunnelConfig"), "the restarted controller rewrote a configuration")
	v.requireNoFailure(t, "the kill")
}

func testControllerRebooted(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	holder := c.awayFromDriver(t)
	c.settleCluster(t)
	v := c.visit(t, hostsOf(homes, holder)...)
	c.markDown(holder, "reboot")
	rebooted := time.Now()
	_, _ = c.on(holder).here("systemctl", "reboot")
	st, next := c.waitController(t, "another controller enforcing", 2*time.Minute, func(st engine.State) bool {
		return st.Node != holder && st.Mode == engine.ModeEnforce && st.At.After(rebooted) && !st.FinishedAt.IsZero()
	})
	took := st.FinishedAt.Sub(rebooted)
	c.logf(t, "%s finished its first enforcing cycle %s after the reboot of %s", next, took, holder)
	require.LessOrEqual(t, took, rebootBound)
	c.waitBack(t, holder)
	c.startHomesOn(t, holder)
	c.waitActive(t, c.names()...)
	c.allShared(t, 14*time.Minute)
	v.requireNoFailure(t, "the reboot of "+holder)
}

func testTakeoverHold(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	holder := c.awayFromDriver(t)
	late := c.others(holder, c.self)[0]
	c.settleCluster(t)
	before := c.ingressHosts(t)
	from := c.callIndex()
	v := c.visit(t, hostsOf(homes, late)...)

	c.logf(t, "the agent of %s stopped, then the controller on %s", late, holder)
	t.Cleanup(func() {
		_, _ = c.on(late).here("systemctl", "start", "pco")
		_, _ = c.on(holder).here("systemctl", "start", "pco")
	})
	c.systemctl(t, late, "stop", "pco")
	c.systemctl(t, holder, "stop", "pco")
	line := fmt.Sprintf("waiting for the agents of %s to report to this controller", late)
	st, next := c.waitController(t, "a new controller", 3*time.Minute, func(st engine.State) bool { return st.Node != holder })
	took := time.Now()
	sawHold := slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, line) }) || strings.Contains(st.Hold, line)
	for time.Since(took) < takeoverHold+30*time.Second {
		c.requireNoWithdrawal(t, before, from)
		if st, _, err := c.controllerState(); err == nil {
			sawHold = sawHold || slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, line) }) || strings.Contains(st.Hold, line)
		}
		time.Sleep(2 * time.Second)
	}
	require.True(t, sawHold, "%s never said it waits for the agents of %s", next, late)
	c.systemctl(t, late, "start", "pco")
	c.systemctl(t, holder, "start", "pco")
	c.waitActive(t, c.names()...)
	c.allShared(t, 14*time.Minute)
	v.requireNoFailure(t, "the takeover")
}

func testQuorumBlip(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.settleCluster(t)
	before := c.leaseOf(t)
	from := c.callIndex()
	v := c.visit(t, hostsOf(homes)...)
	var wg sync.WaitGroup
	errs := make(chan error, len(c.nodes))
	for _, n := range c.names() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.on(n).here("systemctl", "restart", "corosync"); err != nil {
				errs <- fmt.Errorf("%s: %w", n, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	line := "waiting for the agents of"
	for end := time.Now().Add(3 * time.Minute); time.Now().Before(end); time.Sleep(2 * time.Second) {
		st, _, err := c.controllerState()
		if err != nil {
			continue
		}
		require.False(t, slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, line) }) || strings.Contains(st.Hold, line),
			"a takeover hold after a quorum loss shorter than the lock's expiry: %q %q", st.Problems, st.Hold)
	}
	after := c.leaseOf(t)
	require.Equal(t, before.Holder, after.Holder)
	require.Equal(t, before.Generation, after.Generation)
	require.Equal(t, before.Nonce, after.Nonce)
	require.Empty(t, c.callsSince(from, "PutTunnelConfig"), "the sentinel was written again")
	v.requireNoFailure(t, "the restart of corosync")
}

func testWatchdog(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	holder := c.awayFromDriver(t)
	c.settleCluster(t)
	before := c.leaseOf(t)
	restarts := func() int {
		out, _ := c.on(holder).here("systemctl", "show", "-p", "NRestarts", "--value", "pco")
		n, _ := strconv.Atoi(strings.TrimSpace(out))
		return n
	}
	was := restarts()
	v := c.visit(t, hostsOf(homes)...)
	t.Cleanup(func() { _, _ = c.on(holder).here("systemctl", "kill", "--signal=SIGCONT", "pco.service") })
	stopped := time.Now()
	c.systemctl(t, holder, "kill", "--signal=SIGSTOP", "pco.service")
	var restarted time.Time
	for time.Since(stopped) < 4*time.Minute {
		if seen := c.holderSeen(); seen != "" && seen != holder {
			t.Fatalf("%s took the lease while the controller on %s was held", seen, holder)
		}
		if restarted.IsZero() && restarts() > was {
			restarted = time.Now()
			c.logf(t, "systemd restarted pco on %s %s after it was held", holder, restarted.Sub(stopped))
		}
		if !restarted.IsZero() {
			if st, err := c.on(holder).tryState(); err == nil && st.Lease != nil && st.Lease.Holder == holder && st.Lease.Valid && st.At.After(restarted) {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	require.False(t, restarted.IsZero(), "the watchdog did not restart pco on %s", holder)
	require.LessOrEqual(t, restarted.Sub(stopped), 90*time.Second+restartBound)
	after := c.leaseOf(t)
	require.Equal(t, holder, after.Holder)
	require.Equal(t, before.Generation, after.Generation, "the restart drew another epoch")
	v.requireNoFailure(t, "the hung controller")
}

// quorate says whether a node's .members says its cluster is quorate.
func (c *clusterRun) quorate(node string) (bool, error) {
	out, err := c.on(node).here("cat", "/etc/pve/.members")
	if err != nil {
		return false, err
	}
	var m members
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return false, err
	}
	return m.Cluster != nil && m.Cluster.Quorate == 1, nil
}

func testControllerNotQuorate(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	holder := c.awayFromDriver(t)
	c.settleCluster(t)
	v := c.visit(t, hostsOf(homes, holder)...)
	from := c.fakeIndex()
	c.logf(t, "corosync of %s cut off", holder)
	c.cutOff(t, holder, "udp dport 5405-5412 drop", "udp sport 5405-5412 drop")
	var lost time.Time
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		if q, err := c.quorate(holder); err == nil && !q {
			lost = time.Now()
			break
		}
	}
	require.False(t, lost.IsZero(), "%s stayed quorate", holder)
	const line = "the cluster is not quorate; nothing is written"
	sawLine := false
	for time.Since(lost) < expiry {
		if st, err := c.on(holder).tryState(); err == nil && (slices.Contains(st.Problems, line) || strings.Contains(st.Hold, line)) {
			sawLine = true
		}
		time.Sleep(5 * time.Second)
	}
	require.True(t, sawLine, "%s never showed the quorum line", holder)
	for _, w := range c.writesFrom(from, holder) {
		require.False(t, w.At.After(lost.Add(staleWrite)), "%s wrote %s %s at %s, %s after it lost quorum", holder, w.Method, w.Path, w.At, w.At.Sub(lost))
	}
	_, next := c.waitController(t, "a controller in the quorate part", 3*time.Minute, func(st engine.State) bool { return st.Node != holder })
	c.logf(t, "%s took over", next)
	c.uncut(t, holder)
	c.waitActive(t, c.names()...)
	c.allShared(t, 14*time.Minute)
	v.requireNoFailure(t, "the quorum loss of "+holder)
}

// withoutWatchdog takes systemd's watchdog off pco on a node until t ends, so
// that a stopped daemon stays stopped past the lock's expiry.
func (c *clusterRun) withoutWatchdog(t *testing.T, node string) {
	t.Helper()
	const dropIn = dropInDir + "/e2e-watchdog.conf"
	c.writeOn(t, node, dropIn, "[Service]\nWatchdogSec=0\n", "0644")
	t.Cleanup(func() {
		on := c.on(node)
		_, _ = on.here("rm", "-f", dropIn)
		_, _ = on.here("systemctl", "daemon-reload")
		_, _ = on.here("systemctl", "restart", "pco")
	})
	c.systemctl(t, node, "daemon-reload")
	c.systemctl(t, node, "restart", "pco")
	c.waitActive(t, c.names()...)
}

func testControllerStopped(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	node := c.others(c.self)[0]
	c.withoutWatchdog(t, node)
	c.handoff(t, node)
	c.settleCluster(t)
	before := c.leaseOf(t)
	shared := c.sharedTunnel(t)
	v := c.visit(t, hostsOf(homes)...)
	from := c.fakeIndex()
	t.Cleanup(func() { _, _ = c.on(node).here("systemctl", "kill", "--signal=SIGCONT", "pco.service") })
	c.systemctl(t, node, "kill", "--signal=SIGSTOP", "pco.service")
	first, gen := c.waitSentinel(t, shared, before.Generation, expiry+time.Minute)
	// The new epoch's first write is the first the fake answered another
	// node since the stop, when it can tell; the time the suite saw the
	// sentinel otherwise.
	for _, w := range c.writesFrom(from, "") {
		if n := c.nodeAt(w.From); n != node && w.Done.Before(first) {
			first = w.Done
			break
		}
	}
	c.logf(t, "the new epoch %d wrote first at %s", gen, first.Format(time.TimeOnly))
	time.Sleep(10 * time.Second)
	c.systemctl(t, node, "kill", "--signal=SIGCONT", "pco.service")
	time.Sleep(90 * time.Second)
	for _, w := range c.writesFrom(from, node) {
		require.False(t, w.Done.After(first), "the stopped controller on %s wrote %s %s at %s, after the new epoch's first write", node, w.Method, w.Path, w.Done)
	}
	require.Equal(t, gen, c.sentinelOf(t, shared), "the old controller wrote its sentinel again")
	require.NotEqual(t, node, c.holder(t))
	c.waitActive(t, c.names()...)
	v.requireNoFailure(t, "the stop of "+node)
}

func testRestoredLeader(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.settleCluster(t)
	file := filepath.Join(store.DefaultPaths().Cluster, "meta", "leader.json")
	saved, err := os.ReadFile(file)
	require.NoError(t, err)
	before := c.leaseOf(t)
	for range 2 {
		c.handoff(t, c.others(c.holder(t))[0])
	}
	c.settleCluster(t)
	shared := c.sharedTunnel(t)
	gen := c.sentinelOf(t, shared)
	require.Equal(t, before.Generation+2, gen)
	v := c.visit(t, hostsOf(homes)...)

	c.logf(t, "leader.json of generation %d put back over generation %d", before.Generation, gen)
	require.NoError(t, os.WriteFile(file+".e2e", saved, 0o600))
	require.NoError(t, os.Rename(file+".e2e", file))
	restored := time.Now()
	from := c.fakeIndex()
	var behind string
	deadline := time.Now().Add(3 * time.Minute)
	for behind == "" {
		for _, n := range c.names() {
			if st, err := c.on(n).tryState(); err == nil && st.Lease != nil && st.Lease.Holder == n && st.Lease.Behind != "" {
				behind = n
				c.logf(t, "%s is behind: %s", n, st.Lease.Behind)
			}
		}
		require.True(t, time.Now().Before(deadline), "no node says it is behind after the restore")
		time.Sleep(2 * time.Second)
	}
	time.Sleep(time.Minute)
	for _, w := range c.writesFrom(from, "") {
		require.False(t, w.At.After(restored.Add(10*time.Second)), "%s %s from %s at %s, before pco promote", w.Method, w.Path, c.nodeAt(w.From), w.At)
	}
	require.Equal(t, gen, c.sentinelOf(t, shared))

	out, err := c.on(behind).pcoCombined("promote", "--yes")
	c.logf(t, "pco promote on %s:\n%s", behind, out)
	require.NoError(t, err)
	c.on(behind).enforce(t)
	c.waitSentinel(t, shared, gen, 3*time.Minute)
	c.allShared(t, 5*time.Minute)
	v.requireNoFailure(t, "the restore of leader.json")
}

// setEligible says in the registry, with pco's own store code, whether the
// nodes may hold the lease: what pco node set does, for a cluster without a
// controller to ask.
func (c *clusterRun) setEligible(t testing.TB, eligible *bool, nodes ...string) {
	t.Helper()
	st, err := store.Open(store.DefaultPaths())
	require.NoError(t, err)
	revs, err := st.NodesRev()
	require.NoError(t, err)
	for _, r := range revs {
		if !slices.Contains(nodes, r.Node.Name) {
			continue
		}
		n := r.Node
		n.ControllerEligible = eligible
		_, err := st.SaveNodeIf(n, r.Rev)
		require.NoError(t, err)
	}
}

func testControllerAbsent(t *testing.T, c *clusterRun) {
	homes := c.ensureHomes(t)
	c.allShared(t, 5*time.Minute)
	absent := 30 * time.Minute
	if raw := os.Getenv("PCO_E2E_ABSENT"); raw != "" {
		d, err := time.ParseDuration(raw)
		require.NoError(t, err, "PCO_E2E_ABSENT")
		absent = d
	}
	holder := c.holder(t)
	t.Cleanup(func() { c.setEligible(t, nil, c.names()...) })
	for _, n := range append(c.others(holder), holder) {
		out, err := c.on(holder).pcoCombined("node", "set", n, "--controller-eligible=false")
		c.logf(t, "pco node set %s --controller-eligible=false:\n%s", n, out)
		require.NoError(t, err)
	}
	c.systemctl(t, holder, "restart", "pco")
	gone := time.Now()
	for c.holderSeen() != "" && time.Since(gone) < time.Minute {
		time.Sleep(time.Second)
	}
	require.Empty(t, c.holderSeen(), "a node holds the lease although none may")
	from := c.fakeIndex()
	v := c.visit(t, hostsOf(homes)...)
	targets := map[string]bool{}
	for _, h := range homes {
		targets[h.g.ip()+":8080"] = true
	}
	reboot := c.others(c.self)[0]
	rebooted := false
	c.logf(t, "no controller for %s; %s reboots in the middle", absent, reboot)
	for end := time.Now().Add(absent); time.Now().Before(end); time.Sleep(10 * time.Second) {
		require.Empty(t, c.writesFrom(from, ""), "something was written at Cloudflare without a controller")
		require.Empty(t, c.holderSeen(), "a node took the lease")
		for _, n := range c.names() {
			if c.isDown(n) {
				continue
			}
			got := c.targetsOn(n)
			for target := range targets {
				require.Contains(t, got, target, "the egress set of %s lost %s without a controller", n, target)
			}
		}
		if !rebooted && time.Until(end) < absent/2 {
			rebooted = true
			// The guest of the node rebooted does not start at boot: its
			// target is no longer one that serves. The shared routes of
			// the others' guests are what the node serves after its boot.
			for _, h := range homes {
				if h.node == reboot {
					delete(targets, h.g.ip()+":8080")
				}
			}
			c.markDown(reboot, "reboot")
			_, _ = c.on(reboot).here("systemctl", "reboot")
			time.Sleep(30 * time.Second)
			c.waitBack(t, reboot)
			for target := range targets {
				c.on(reboot).waitTarget(t, target, true, 2*time.Minute, time.Second)
			}
		}
	}
	v.requireNoFailure(t, "the time without a controller")
	c.setEligible(t, nil, c.names()...)
	c.waitController(t, "a controller again", 5*time.Minute, func(engine.State) bool { return true })
	for _, h := range homes {
		if h.node == reboot {
			_, _ = c.on(reboot).here("pct", "start", strconv.Itoa(h.g.vmid))
		}
	}
	c.waitActive(t, c.names()...)
	c.allShared(t, 14*time.Minute)
}

func testControllerPowerLoss(t *testing.T, c *clusterRun) {
	c.skipWithoutPower(t)
	homes := c.ensureHomes(t)
	runs := 10
	if raw := os.Getenv("PCO_E2E_POWER_RUNS"); raw != "" {
		n, err := strconv.Atoi(raw)
		require.NoError(t, err, "PCO_E2E_POWER_RUNS")
		runs = n
	}
	shared := c.sharedTunnel(t)
	var took []time.Duration
	for run := 1; run <= runs; run++ {
		holder := c.awayFromDriver(t)
		c.settleCluster(t)
		gen := c.leaseOf(t).Generation
		v := c.visit(t, hostsOf(homes, holder)...)
		off := time.Now()
		c.powerAction(t, "off", holder)
		at, got := c.waitSentinel(t, shared, gen, powerLossBound+time.Minute)
		took = append(took, at.Sub(off))
		c.logf(t, "run %d of %d: %s off, generation %d written %s later", run, runs, holder, got, at.Sub(off))
		require.LessOrEqual(t, at.Sub(off), powerLossBound, "run %d", run)
		v.requireNoFailure(t, fmt.Sprintf("run %d", run))
		c.powerAction(t, "on", holder)
		c.waitBack(t, holder)
		for _, h := range homes {
			if h.node == holder {
				_, _ = c.on(holder).here("pct", "start", strconv.Itoa(h.g.vmid))
			}
		}
		c.waitActive(t, c.names()...)
	}
	c.logf(t, "the new epoch's first write after the power loss: %v", took)
	c.allShared(t, 14*time.Minute)
}

func testControllerSuspended(t *testing.T, c *clusterRun) {
	c.skipWithoutPower(t)
	homes := c.ensureHomes(t)
	holder := c.awayFromDriver(t)
	c.settleCluster(t)
	gen := c.leaseOf(t).Generation
	shared := c.sharedTunnel(t)
	v := c.visit(t, hostsOf(homes, holder)...)
	from := c.fakeIndex()
	c.powerAction(t, "suspend", holder)
	_, newGen := c.waitSentinel(t, shared, gen, expiry+time.Minute)
	time.Sleep(30 * time.Second)
	c.powerAction(t, "resume", holder)
	resumed := time.Now()
	time.Sleep(time.Minute)
	for _, w := range c.writesFrom(from, holder) {
		require.False(t, w.At.After(resumed.Add(staleWrite)), "%s wrote %s %s at %s, %s after it resumed", holder, w.Method, w.Path, w.At, w.At.Sub(resumed))
	}
	t.Log("the new controller repairs what landed at its next read")
	c.settleCluster(t)
	require.Equal(t, newGen, c.sentinelOf(t, shared))
	require.NotEqual(t, holder, c.holder(t))
	c.waitActive(t, c.names()...)
	v.requireNoFailure(t, "the suspend of "+holder)
}
