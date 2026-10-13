//go:build e2e

package e2e

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// scenario is a step of the run, named by its id.
type scenario struct {
	name string
	run  func(*testing.T, *suite)
}

// The scenarios share one install and run in the order of the profile: S11
// starts from the install that setup leaves, which only observes. Each one
// makes guests of its own; the ones that look at the whole node use those of
// the others.
func TestEndToEnd(t *testing.T) {
	s := newSuite(t)
	run := hostScenarios
	if s.lay.isAppliance() {
		run = applianceScenarios
	}
	for _, sc := range run {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, s) })
	}
}

// The scenarios on the node both profiles have, in the order they run in;
// the appliance runs them between its own (appliance_test.go).
var (
	s11 = scenario{"S11 observe-only until apply, mass-delete guard", testObserveApplyAndGuard}
	s1  = scenario{"S1 basic route", testBasicRoute}
	s2  = scenario{"S2 two ports on one guest", testTwoPorts}
	s3  = scenario{"S3 several hostnames to one port", testSeveralHostnames}
	s5  = scenario{"S5 wildcard after exact", testWildcardOrder}
	s6  = scenario{"S6 static and reported addresses", testAddressSources}
	s7  = scenario{"S7 stop, start, edit, untag, destroy", testLifecycle}
	s8  = scenario{"S8 clone with the notes", testClone}
	s10 = scenario{"S10 foreign record, adopt", testForeignRecord}
	s12 = scenario{"S12 token without dns write", testDNSWriteDenied}
	s13 = scenario{"S13 daemon killed, proxmox and cloudflare down", testOutages}
	s14 = scenario{"S14 ruleset flushed", testRulesetFlushed}
	s15 = scenario{"S15 mac moved", testMACMoved}
	s9  = scenario{"S9 identity of others' addresses", testIdentity}
)

var hostScenarios = []scenario{
	s11, s1, s2, s3, s5, s6, s7, s8, s10, s12, s13, s14, s15, s9,
	// The networking scenarios: guests the node is not on-link with,
	// reached through legs of the gateway namespace, and the managed
	// network. S17 leaves its network attached and S21 the managed network
	// enabled, which S19, S20, S22 and S23 build on.
	{"S17 attach an isolated bridge", testAttachIsolatedBridge},
	{"S18 attach a vlan", testAttachVLAN},
	{"S19 two networks with one subnet", testIdenticalSubnets},
	{"S20 network reload under legs", testNetworkReload},
	{"S21 managed network", testManagedNetwork},
	{"S22 managed network isolation", testManagedIsolation},
	{"S23 gateway restart and reinstall under traffic", testGatewayRestart},
}

func testObserveApplyAndGuard(t *testing.T, s *suite) {
	g := s.g(11, guest{notes: "```cf-tunnel\n" +
		"e2e-s11-1.ZONE e2e-s11-2.ZONE e2e-s11-3.ZONE -> :8080\n" +
		"e2e-s11-4.ZONE e2e-s11-5.ZONE e2e-s11-6.ZONE -> :8080\n```"})
	s.create(t, g)
	hosts := []string{}
	for i := 1; i <= 6; i++ {
		hosts = append(hosts, s.host("s11-"+strconv.Itoa(i)))
	}
	for _, h := range hosts {
		s.waitRoute(t, h, planner.StateActive)
	}

	st := s.state(t)
	require.Equal(t, engine.ModeObserve, st.Mode)
	// The tunnel waits for apply, and the records wait for the tunnel.
	for _, a := range st.Actions {
		require.False(t, a.Applied, "%+v", a)
	}
	require.True(t, slices.ContainsFunc(st.Actions, func(a reconcile.Action) bool {
		return a.Kind == reconcile.CreateTunnel && strings.Contains(a.Held, "observe")
	}), "%+v", st.Actions)
	plan := s.pcoMust(t, "plan")
	require.Contains(t, plan, "observe mode")
	_, found := s.cloud.Tunnel(t, planner.TunnelName(s.install))
	require.False(t, found, "observe mode makes no tunnel")
	for _, h := range hosts {
		require.Empty(t, s.records(t, h), "observe mode makes no record")
	}
	units := s.hereMust(t, "systemctl", "list-units", "--all", "--plain", "--no-legend", "pco-cloudflared@*")
	require.Empty(t, strings.TrimSpace(units), "observe mode starts no connector")

	s.enforce(t)
	tunnel := s.tunnel(t)
	for _, h := range hosts {
		s.waitRecord(t, h, tunnel, 2*time.Minute)
	}

	// Six records of six go at once: the guard holds them until confirmed,
	// also once their grace is over.
	s.setTags(t, g.vmid, "")
	st = s.waitState(t, "the mass-delete guard", 4*time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Problems, func(p string) bool { return strings.HasPrefix(p, reconcile.HeldByGuard) })
	})
	require.NotEmpty(t, st.Waiting)
	time.Sleep(grace + 2*pollInterval)
	for _, h := range hosts {
		require.NotEmpty(t, s.records(t, h), "the guard deletes nothing")
	}
	held := s.pcoMust(t, "plan")
	require.Contains(t, held, reconcile.HeldByGuard)

	out := s.pcoMust(t, "apply", "--confirm-deletes", "--yes")
	t.Logf("pco apply --confirm-deletes:\n%s", out)
	for _, h := range hosts {
		s.waitRecord(t, h, "", 2*time.Minute)
	}
	s.destroy(t, g.vmid)
}

func testBasicRoute(t *testing.T, s *suite) {
	g := s.s1Guest()
	s.create(t, g)
	s.enforce(t)
	host := s.host("s1")
	route := s.waitRoute(t, host, planner.StateActive)
	service := "http://" + g.ip() + ":8080"
	require.Equal(t, service, route.Service)
	require.Equal(t, s.lay.level(), route.Level)
	require.Equal(t, g.owner(), route.Owner)

	tunnel := s.tunnel(t)
	tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install))
	require.True(t, found)
	require.Equal(t, tunnel, tn.ID)
	s.waitIngress(t, tunnel, host, service)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	marker := planner.DNSMarker(s.install)
	for _, rec := range s.runRecords(t) {
		if strings.EqualFold(rec.Name, host) {
			require.True(t, rec.Proxied, "%+v", rec)
			require.True(t, strings.HasPrefix(rec.Comment, marker), "%+v", rec)
		}
	}

	s.requireConnector(t, tunnel)
	s.waitTarget(t, g.ip()+":8080", true, time.Minute, time.Second)
	summary, _, ok := s.egress(t)
	require.True(t, ok, summary)
	require.Equal(t, filterOn, summary)

	if s.fake == nil {
		s.requireServed(t, host, g.body(8080))
	}
}

func testTwoPorts(t *testing.T, s *suite) {
	g := s.g(2, guest{ports: []int{8080, 8081}, notes: "```cf-tunnel\n" +
		"e2e-s2a.ZONE -> :8080\ne2e-s2b.ZONE -> :8081\n```"})
	s.create(t, g)
	s.enforce(t)
	tunnel := s.tunnel(t)
	for i, name := range []string{"s2a", "s2b"} {
		port := g.servedPorts()[i]
		service := fmt.Sprintf("http://%s:%d", g.ip(), port)
		route := s.waitRoute(t, s.host(name), planner.StateActive)
		require.Equal(t, service, route.Service)
		s.waitIngress(t, tunnel, s.host(name), service)
		s.waitRecord(t, s.host(name), tunnel, 2*time.Minute)
		s.waitTarget(t, fmt.Sprintf("%s:%d", g.ip(), port), true, time.Minute, time.Second)
	}
}

func testSeveralHostnames(t *testing.T, s *suite) {
	g := s.g(3, guest{notes: "cf-tunnel: e2e-s3a.ZONE e2e-s3b.ZONE e2e-s3c.ZONE -> :8080"})
	s.create(t, g)
	s.enforce(t)
	tunnel := s.tunnel(t)
	service := "http://" + g.ip() + ":8080"
	for _, name := range []string{"s3a", "s3b", "s3c"} {
		route := s.waitRoute(t, s.host(name), planner.StateActive)
		require.Equal(t, service, route.Service)
		s.waitIngress(t, tunnel, s.host(name), service)
		s.waitRecord(t, s.host(name), tunnel, 2*time.Minute)
	}
}

func testWildcardOrder(t *testing.T, s *suite) {
	g := s.g(5, guest{ports: []int{8080, 8081}, notes: "```cf-tunnel\n" +
		"*.e2e-s5.ZONE -> :8080\nwww.e2e-s5.ZONE -> :8081\n```"})
	s.create(t, g)
	s.enforce(t)
	tunnel := s.tunnel(t)
	wild, exact := "*.e2e-s5."+s.zone, "www.e2e-s5."+s.zone
	// A wildcard is published only once an allowHosts pattern names it.
	refused := s.waitRoute(t, wild, planner.StateRejected)
	require.Contains(t, refused.Reason, `add "`+wild+`" to allowHosts`)
	s.waitRoute(t, exact, planner.StateActive)
	s.allowHosts(t, "*", wild)
	s.waitRoute(t, wild, planner.StateActive)
	s.waitIngress(t, tunnel, wild, "http://"+g.ip()+":8080")
	rules := s.waitIngress(t, tunnel, exact, "http://"+g.ip()+":8081")
	_, wi, _ := ruleOf(rules, wild)
	_, ei, _ := ruleOf(rules, exact)
	require.Less(t, ei, wi, "the exact name comes before the wildcard that covers it: %+v", rules)
}

func testAddressSources(t *testing.T, s *suite) {
	static := s.g(6, guest{notes: "cf-tunnel: e2e-s6s.ZONE -> :8080"})
	reported := s.g(7, guest{manual: true, notes: "cf-tunnel: e2e-s6r.ZONE -> :8080"})
	s.create(t, static)
	s.create(t, reported)
	cfg := s.must(t, "pct", "config", strconv.Itoa(reported.vmid))
	require.Contains(t, cfg, "ip=manual", "the address of %d is in no configuration", reported.vmid)
	s.enforce(t)
	for _, g := range []guest{static, reported} {
		name := "s6s"
		if g.manual {
			name = "s6r"
		}
		route := s.waitRoute(t, s.host(name), planner.StateActive)
		require.Equal(t, "http://"+g.ip()+":8080", route.Service, name)
		require.Equal(t, s.lay.level(), route.Level, name)
	}
}

func testLifecycle(t *testing.T, s *suite) {
	g := s.g(8, guest{notes: "cf-tunnel: e2e-s7.ZONE -> :8080"})
	s.create(t, g)
	s.enforce(t)
	tunnel := s.tunnel(t)
	host, target := s.host("s7"), g.ip()+":8080"
	s.waitRoute(t, host, planner.StateActive)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	s.waitTarget(t, target, true, time.Minute, time.Second)

	t.Log("stop: the rule answers 503, the record stays, the target leaves the egress set")
	s.stop(t, g.vmid)
	route := s.waitRoute(t, host, planner.StateWithdrawn)
	t.Logf("withdrawn: %s", route.Reason)
	s.waitTarget(t, target, false, time.Minute, time.Second)
	rules := s.waitIngress(t, tunnel, host, planner.BlockedService)
	require.NotEmpty(t, rules)
	require.Equal(t, []string{"CNAME " + tunnel + ".cfargotunnel.com"}, s.records(t, host))

	t.Log("start: served again")
	s.start(t, g.vmid)
	s.waitRoute(t, host, planner.StateActive)
	s.waitIngress(t, tunnel, host, "http://"+target)
	s.waitTarget(t, target, true, time.Minute, time.Second)

	t.Log("edit: the new name is served at once, the old record goes after the grace")
	edited := time.Now()
	s.setNotes(t, g.vmid, "cf-tunnel: "+s.host("s7b")+" -> :8080")
	s.waitRoute(t, s.host("s7b"), planner.StateActive)
	s.waitRecord(t, s.host("s7b"), tunnel, 2*time.Minute)
	require.NotEmpty(t, s.records(t, host), "the old record waits for the grace")
	s.waitRecord(t, host, "", 3*time.Minute)
	require.Greater(t, time.Since(edited), grace, "the old record went before the grace")

	t.Log("untag: the route goes, its record after the grace, the target at once")
	s.setTags(t, g.vmid, "")
	s.waitGone(t, s.host("s7b"))
	s.waitRecord(t, s.host("s7b"), "", 3*time.Minute)
	s.waitTarget(t, target, false, time.Minute, time.Second)

	t.Log("tag again and destroy: the route and its record go")
	s.setTags(t, g.vmid, gateTag)
	s.waitRoute(t, s.host("s7b"), planner.StateActive)
	s.waitRecord(t, s.host("s7b"), tunnel, 2*time.Minute)
	s.destroy(t, g.vmid)
	s.sync(t)
	s.waitGone(t, s.host("s7b"))
	s.waitRecord(t, s.host("s7b"), "", 3*time.Minute)
	s.waitTarget(t, target, false, time.Minute, time.Second)
}

func testClone(t *testing.T, s *suite) {
	original := s.g(12, guest{notes: "cf-tunnel: e2e-s8.ZONE -> :8080"})
	s.create(t, original)
	s.enforce(t)
	tunnel := s.tunnel(t)
	host := s.host("s8")
	s.waitOwnerRoute(t, host, original.owner(), planner.StateActive)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	before := s.records(t, host)

	// A running container is cloned from a snapshot, which not every storage
	// has: the original stops for the copy, which withdraws its route for a
	// moment. The clone keeps the notes and the tag, and gets an address and
	// a MAC of its own, so that it does not take the original's.
	clone := s.g(13, guest{})
	cloneID := strconv.Itoa(clone.vmid)
	s.stop(t, original.vmid)
	s.guests[clone.vmid] = true
	s.must(t, "pct", "clone", strconv.Itoa(original.vmid), cloneID, "--hostname", "e2e-"+cloneID)
	s.start(t, original.vmid)
	s.waitOwnerRoute(t, host, original.owner(), planner.StateActive)
	s.must(t, "pct", "set", cloneID, "--net0", clone.net0())
	require.Contains(t, s.must(t, "pct", "config", cloneID), host, "the clone has the notes")
	require.NotEqual(t, s.hwaddr(t, original.vmid), s.hwaddr(t, clone.vmid))
	s.start(t, clone.vmid)

	route := s.waitOwnerRoute(t, host, clone.owner(), planner.StateConflict)
	t.Logf("the clone: %s", route.Reason)
	for range 3 {
		s.sync(t)
		time.Sleep(pollInterval)
		r, ok := routeOf(s.state(t), host, original.owner())
		require.True(t, ok)
		require.Equal(t, planner.StateActive, r.State, "the original keeps serving")
		require.Equal(t, "http://"+original.ip()+":8080", r.Service)
	}
	require.Equal(t, before, s.records(t, host))
	s.waitIngress(t, tunnel, host, "http://"+original.ip()+":8080")
}

func testForeignRecord(t *testing.T, s *suite) {
	host := s.host("s10")
	foreign := s.cloud.Foreign(t, s.zone, host, "foreign.example.net")
	if s.fake == nil {
		// A run cut off from here on leaves the record in the zone, which
		// cleanup.sh cannot delete; it names it from this file.
		marker(t, foreignFile, fmt.Sprintf("%s %s %s\n", s.zone, foreign.ID, host))
	}
	t.Cleanup(func() {
		// An adopted record is the install's, which the purge removes.
		if slices.Contains(s.records(t, host), "CNAME foreign.example.net") {
			s.cloud.Remove(t, s.zone, foreign)
		}
		unmark(t, foreignFile)
	})
	g := s.g(16, guest{notes: "cf-tunnel: e2e-s10.ZONE -> :8080"})
	s.create(t, g)
	s.enforce(t)
	tunnel := s.tunnel(t)

	s.waitState(t, "a conflict for "+host, 2*time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Conflicts, func(c reconcile.Conflict) bool { return strings.EqualFold(c.Name, host) })
	})
	require.Equal(t, []string{"CNAME foreign.example.net"}, s.records(t, host), "a record of someone else is left alone")

	asked := time.Now()
	out := s.pcoMust(t, "adopt", host, "--yes")
	t.Logf("pco adopt:\n%s", out)
	if s.fake != nil {
		// The adoption waits for a connector that is ready, which one with
		// the token of the fake never is.
		ev := s.waitEvent(t, asked, "admin", "adoption of "+host+" waits")
		t.Logf("%s", ev.Message)
		require.Contains(t, ev.Message, "connector is not ready")
		require.Equal(t, []string{"CNAME foreign.example.net"}, s.records(t, host))
		return
	}
	s.waitRecord(t, host, tunnel, 3*time.Minute)
}

func testDNSWriteDenied(t *testing.T, s *suite) {
	if s.fake == nil {
		t.Skip("the token of the owner cannot be made to lose a permission")
	}
	s.ensureS1(t)
	tunnel := s.tunnel(t)
	before := s.runRecords(t)
	from := len(s.fake.f.Calls())
	s.fake.f.Deny("dns.write")
	t.Cleanup(func() { s.fake.f.Allow("dns.write") })

	g := s.g(17, guest{notes: "cf-tunnel: e2e-s12.ZONE -> :8080"})
	s.create(t, g)
	host := s.host("s12")
	s.waitRoute(t, host, planner.StateActive)
	st := s.waitState(t, "a problem with the record of "+host, 2*time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, host) })
	})
	t.Logf("problems: %q", otherProblems(st))
	require.Empty(t, s.records(t, host))
	for _, w := range s.fake.writes(from) {
		require.False(t, strings.HasPrefix(w, "DeleteRecord"), "nothing is deleted: %s", w)
	}
	require.Subset(t, s.runRecords(t), before, "every record is still there")

	s.fake.f.Allow("dns.write")
	s.sync(t)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	s.waitState(t, "the problem to go", time.Minute, func(st engine.State) bool {
		return !slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, host) })
	})
}

func testOutages(t *testing.T, s *suite) {
	s.ensureS1(t)
	b := s.takeBefore(t)
	from := 0
	if s.fake != nil {
		from = len(s.fake.f.Calls())
	}

	t.Log("the daemon is killed in a cycle and comes back")
	killed := s.killInCycle(t)
	s.waitState(t, "a cycle of the new daemon", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(killed) && !st.FinishedAt.IsZero() && st.Complete
	})
	s.requireSame(t, b)

	t.Logf("Proxmox does not answer for %s: the daemon holds", outage)
	s.stopProxmoxAPI(t)
	stopped := time.Now()
	st := s.waitState(t, "a cycle without Proxmox", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(stopped) && !st.FinishedAt.IsZero() && !st.Complete
	})
	t.Logf("problems: %q", otherProblems(st))
	s.holdThrough(t, b)
	s.must(t, "systemctl", "start", "pveproxy")
	back := time.Now()
	s.sync(t)
	s.waitState(t, "a complete cycle", 2*time.Minute, func(st engine.State) bool { return st.At.After(back) && st.Complete })
	s.requireSame(t, b)

	if s.fake == nil {
		return
	}
	t.Logf("Cloudflare does not answer for %s: the daemon holds", outage)
	baseline := otherProblems(s.state(t))
	t.Cleanup(func() { _ = s.fake.srv.start() })
	s.fake.srv.stop()
	down := time.Now()
	st = s.waitState(t, "a problem without Cloudflare", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(down) && slices.ContainsFunc(otherProblems(st), func(p string) bool { return !slices.Contains(baseline, p) })
	})
	t.Logf("problems: %q", otherProblems(st))
	s.holdThrough(t, b)
	require.NoError(t, s.fake.srv.start())
	s.sync(t)
	s.waitState(t, "the problems of the outage to go", 3*time.Minute, func(st engine.State) bool {
		return !slices.ContainsFunc(otherProblems(st), func(p string) bool { return !slices.Contains(baseline, p) })
	})
	s.requireSame(t, b)
	for _, w := range s.fake.writes(from) {
		require.False(t, strings.HasPrefix(w, "Delete") || strings.HasPrefix(w, "CreateTunnel"), "an outage changes nothing: %s", w)
	}
}

func testRulesetFlushed(t *testing.T, s *suite) {
	target := s.ensureS1(t).ip() + ":8080"
	s.waitTarget(t, target, true, time.Minute, time.Second)
	if s.app != nil {
		s.testApplianceRulesetFlushed(t)
		return
	}
	_, targets, ok := s.egress(t)
	require.True(t, ok)
	s.saveTables(t)

	flushed := time.Now()
	s.must(t, "nft", "flush", "ruleset")
	t.Log("the ruleset is flushed")
	s.waitEvent(t, flushed, "egress", "was loaded again")
	deadline := time.Now().Add(10 * time.Second)
	for {
		summary, after, ok := s.egress(t)
		if ok && summary == filterOn && slices.Equal(targets, after) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the table did not come back with its targets %v: %s, %v", targets, summary, after)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func testMACMoved(t *testing.T, s *suite) {
	g := s.g(18, guest{notes: "cf-tunnel: e2e-s15.ZONE -> :8080"})
	s.create(t, g)
	s.enforce(t)
	host, target := s.host("s15"), g.ip()+":8080"
	s.waitRoute(t, host, planner.StateActive)
	s.waitTarget(t, target, true, time.Minute, time.Second)
	s.requireMACMoveSeen(t, g, fmt.Sprintf("02:e2:e0:00:%02d:%02d", g.vmid/100%100, g.vmid%100))
	s.waitRoute(t, host, planner.StateActive)
	s.waitTarget(t, target, true, time.Minute, time.Second)
}

// requireMACMoveSeen gives the guest's card another MAC with the link up,
// and checks that its target leaves the egress set within 2 seconds of pco's
// neighbour table seeing the new MAC, the node's or the appliance's, with the
// event and the route withdrawn; then the MAC is put back. The guest is on
// the bridge of the run, where it reaches the address of pco there.
func (s *suite) requireMACMoveSeen(t *testing.T, g guest, moved string) {
	t.Helper()
	target := g.ip() + ":8080"
	host, ok := routeHost(s.state(t), g.owner())
	require.True(t, ok, "no route of %s", g.owner())
	own := s.hwaddr(t, g.vmid)

	// Until the node's neighbour table has the new MAC the target stays in
	// the set; the guest's ping carries it there, or pco's own verification,
	// which asks the guest by ARP, does first. The 2 seconds count from the
	// moment the table has it.
	seen := s.neighbourSeen(t, g.ip(), moved)
	changed := time.Now()
	s.setMAC(t, g.vmid, moved)
	var at time.Time
	seenAlready := func() bool {
		select {
		case at = <-seen:
			t.Logf("the node saw the MAC %s before the guest sent it anything: pco asked for it", moved)
			return true
		default:
			return false
		}
	}
	if !seenAlready() {
		summary, targets, ok := s.egress(t)
		inSet := ok && summary == filterOn && slices.Contains(targets, target)
		if !inSet && !seenAlready() {
			t.Fatalf("the target left the set before the node saw the new MAC: %q, exit 0: %v, the set: %v", summary, ok, targets)
		}
	}
	pinged := make(chan error, 1)
	go func() {
		_, err := s.run("pct", "exec", strconv.Itoa(g.vmid), "--", "sh", "-c", "ping -c 1 -W 2 "+s.lay.pcoAddr()+" >/dev/null || true")
		pinged <- err
	}()
	if at.IsZero() {
		select {
		case at = <-seen:
		case <-time.After(10 * time.Second):
			t.Fatalf("the neighbour table pco watches never gave %s the MAC %s", g.ip(), moved)
		}
	}
	s.waitTarget(t, target, false, 10*time.Second, 50*time.Millisecond)
	took := time.Since(at)
	t.Logf("the target left the egress set at most %s after the node saw the new MAC", took.Round(10*time.Millisecond))
	require.LessOrEqual(t, took, 2*time.Second)
	require.NoError(t, <-pinged)
	ev := s.waitEvent(t, changed, "egress", "the MAC of "+g.ip()+" moved")
	t.Logf("%s", ev.Message)
	route := s.waitRoute(t, host, planner.StateWithdrawn)
	t.Logf("withdrawn: %s", route.Reason)

	s.setMAC(t, g.vmid, own)
	s.exec(t, g.vmid, "ping -c 1 -W 2 "+s.lay.pcoAddr()+" >/dev/null || true")
	s.sync(t)
}

// routeHost is the hostname of the one route of an owner.
func routeHost(st engine.State, owner string) (string, bool) {
	i := slices.IndexFunc(st.Routes, func(r engine.RouteView) bool { return r.Owner == owner })
	if i < 0 {
		return "", false
	}
	return st.Routes[i].Hostname, true
}

func testIdentity(t *testing.T, s *suite) {
	victim := s.g(15, guest{notes: "cf-tunnel: e2e-s9v.ZONE -> :8080"})
	s.create(t, victim)
	s.enforce(t)
	s.waitRoute(t, s.host("s9v"), planner.StateActive)
	tunnel := s.tunnel(t)

	// The attacker answers for the node's address on the bridge and for the
	// victim's, and names both in its notes. In the appliance it names the
	// appliance's address as well, and takes only the victim's: taking the
	// node's would cut the appliance off its API and prove nothing. The
	// node's address there is one only the run gives it, which Proxmox lists
	// nowhere: the installer saved it for the appliance.
	notes := "```cf-tunnel\n" +
		"e2e-s9n.ZONE -> http://" + s.lay.nodeAddr + ":8080\n" +
		"e2e-s9g.ZONE -> http://" + victim.ip() + ":8080\n"
	takes := []string{"ip addr add " + s.lay.nodeAddr + "/32 dev eth0", "ip addr add " + victim.ip() + "/32 dev eth0"}
	denied := []string{s.lay.nodeAddr}
	stolen := []string{s.host("s9n"), s.host("s9g")}
	if s.app != nil {
		notes += "e2e-s9a.ZONE -> http://" + s.lay.applianceAddr + ":8080\n"
		takes = takes[1:]
		denied = append(denied, s.lay.applianceAddr)
		stolen = append(stolen, s.host("s9a"))
	}
	attacker := s.g(14, guest{notes: notes + "```", start: takes})
	s.create(t, attacker)
	// A stolen hostname has no rule but the one of a withdrawn route, which
	// answers 503; no rule sends anything to the node, and the victim's
	// address serves the victim's name only.
	requireIngress := func() {
		t.Helper()
		for _, r := range s.cloud.Ingress(t, tunnel) {
			if slices.Contains(stolen, r.Hostname) {
				require.Equal(t, planner.BlockedService, r.Service, "%+v", r)
			}
			addr := ""
			if u, err := url.Parse(r.Service); err == nil {
				addr = u.Hostname()
			}
			require.NotContains(t, denied, addr, "%+v", r)
			if addr == victim.ip() {
				require.Equal(t, s.host("s9v"), r.Hostname, "%+v", r)
			}
		}
	}
	until := time.Now().Add(6 * pollInterval)
	seen := false
	for time.Now().Before(until) {
		st := s.state(t)
		for _, h := range stolen {
			r, ok := routeOf(st, h, attacker.owner())
			seen = seen || ok
			require.False(t, ok && r.State == planner.StateActive, "%s is served: %+v", h, r)
			require.Empty(t, s.records(t, h), "%s has a record", h)
		}
		for _, d := range denied {
			s.requireNoTarget(t, d+":8080")
		}
		requireIngress()
		time.Sleep(s.every())
	}
	require.True(t, seen, "the routes of the attacker were read")
	st := s.state(t)
	for _, h := range stolen {
		r, _ := routeOf(st, h, attacker.owner())
		t.Logf("%s: %s (%s)", h, r.State, r.Reason)
	}

	s.destroy(t, attacker.vmid)
	s.sync(t)
	s.waitOwnerRoute(t, s.host("s9v"), victim.owner(), planner.StateActive)
}
