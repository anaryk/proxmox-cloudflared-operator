//go:build e2e

package e2e

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// The scenarios share one install and run in this order: S11 starts from the
// install that setup leaves, which only observes. Each one makes guests of its
// own; the ones that look at the whole node use those of the others.
func TestEndToEnd(t *testing.T) {
	s := newSuite(t)
	for _, sc := range []struct {
		name string
		run  func(*testing.T, *suite)
	}{
		{"S11 observe-only until apply, mass-delete guard", testObserveApplyAndGuard},
		{"S1 basic route", testBasicRoute},
		{"S2 two ports on one guest", testTwoPorts},
		{"S3 several hostnames to one port", testSeveralHostnames},
		{"S5 wildcard after exact", testWildcardOrder},
		{"S6 static and reported addresses", testAddressSources},
		{"S7 stop, start, edit, untag, destroy", testLifecycle},
		{"S8 clone with the notes", testClone},
		{"S10 foreign record, adopt", testForeignRecord},
		{"S12 token without dns write", testDNSWriteDenied},
		{"S13 daemon killed, proxmox and cloudflare down", testOutages},
		{"S14 ruleset flushed", testRulesetFlushed},
		{"S15 mac moved", testMACMoved},
		{"S9 identity of others' addresses", testIdentity},
	} {
		t.Run(sc.name, func(t *testing.T) { sc.run(t, s) })
	}
}

// in puts the zone of the run into the notes of g.
func (s *suite) in(g guest) guest {
	g.notes = strings.ReplaceAll(g.notes, "ZONE", s.zone)
	return g
}

// s1Guest is the guest of S1, which the scenarios that look at the whole node
// make when S1 did not run.
func (s *suite) s1Guest() guest {
	return s.in(guest{vmid: 9101, notes: "cf-tunnel: e2e-s1.ZONE -> :8080"})
}

// ensureS1 makes the guest of S1 unless the run has it, and waits until it is
// served and until nothing is left to do.
func (s *suite) ensureS1(t *testing.T) guest {
	t.Helper()
	g := s.s1Guest()
	if !s.guests[g.vmid] {
		s.create(t, g)
	}
	s.enforce(t)
	s.waitRoute(t, s.host("s1"), planner.StateActive)
	s.settle(t)
	return g
}

func testObserveApplyAndGuard(t *testing.T, s *suite) {
	g := s.in(guest{vmid: 9111, notes: "```cf-tunnel\n" +
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
	plan := s.must(t, s.pco, "plan")
	require.Contains(t, plan, "observe mode")
	_, found := s.cloud.Tunnel(t, planner.TunnelName(s.install))
	require.False(t, found, "observe mode makes no tunnel")
	for _, h := range hosts {
		require.Empty(t, s.records(t, h), "observe mode makes no record")
	}
	units := s.must(t, "systemctl", "list-units", "--all", "--plain", "--no-legend", "pco-cloudflared@*")
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
		return slices.ContainsFunc(st.Problems, func(p string) bool { return strings.HasPrefix(p, "mass delete guard") })
	})
	require.NotEmpty(t, st.Waiting)
	time.Sleep(grace + 2*pollInterval)
	for _, h := range hosts {
		require.NotEmpty(t, s.records(t, h), "the guard deletes nothing")
	}
	held := s.must(t, s.pco, "plan")
	require.Contains(t, held, "mass delete guard")

	out := s.must(t, s.pco, "apply", "--confirm-deletes", "--yes")
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
	require.Equal(t, "port", route.Level)
	require.Equal(t, g.owner(), route.Owner)

	tunnel := s.tunnel(t)
	tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install))
	require.True(t, found)
	require.Equal(t, tunnel, tn.ID)
	s.waitIngress(t, tunnel, host, service)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	marker := planner.DNSMarker(s.install)
	for _, rec := range s.cloud.Records(t, s.zone) {
		if strings.EqualFold(rec.Name, host) {
			require.True(t, rec.Proxied, "%+v", rec)
			require.True(t, strings.HasPrefix(rec.Comment, marker), "%+v", rec)
		}
	}

	s.requireConnector(t, tunnel)
	s.waitTarget(t, g.ip()+":8080", true, time.Minute, time.Second)
	summary, _, ok := s.egress(t)
	require.True(t, ok, summary)
	require.Equal(t, "The egress filter is on.", summary)

	if s.fake == nil {
		s.requireServed(t, host, g.body(8080))
	}
}

// requireConnector checks the connector of the tunnel: its unit is enabled
// and runs with the token of the tunnel. Against the fake it cannot connect,
// so it is never ready; against Cloudflare it has to be.
func (s *suite) requireConnector(t *testing.T, tunnel string) {
	t.Helper()
	unit := "pco-cloudflared@" + tunnel + ".service"
	enabled, _ := s.run("systemctl", "is-enabled", unit)
	require.Equal(t, "enabled", strings.TrimSpace(enabled), unit)
	token, err := os.ReadFile("/var/lib/pco/tunnels/" + tunnel + ".token")
	require.NoError(t, err)
	require.Equal(t, s.cloud.RunToken(t, tunnel), strings.TrimSpace(string(token)))

	st := s.waitState(t, "the connector in the state", time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == tunnel })
	})
	i := slices.IndexFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == tunnel })
	conn := st.Connectors[i]
	require.Equal(t, s.install, conn.Install)
	if s.fake != nil {
		require.False(t, conn.Ready, "a connector with the token of the fake does not connect")
		return
	}
	s.waitState(t, "the connector to be ready", 2*time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == tunnel && c.Ready })
	})
}

// requireServed fetches the hostname through Cloudflare.
func (s *suite) requireServed(t *testing.T, host, body string) {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + host + "/")
		if err == nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			last = fmt.Sprintf("%d %s", resp.StatusCode, raw)
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(raw)) == body {
				return
			}
		} else {
			last = err.Error()
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("https://%s/ did not answer %q; the last answer: %s", host, body, last)
}

// waitIngress waits until the rule of host in the tunnel's configuration
// sends it to service.
func (s *suite) waitIngress(t testing.TB, tunnel, host, service string) []planner.IngressRule {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		rules := s.cloud.Ingress(t, tunnel)
		if r, _, ok := ruleOf(rules, host); ok && r.Service == service {
			return rules
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ingress does not send %s to %s: %+v", host, service, rules)
		}
		time.Sleep(time.Second)
	}
}

func testTwoPorts(t *testing.T, s *suite) {
	g := s.in(guest{vmid: 9102, ports: []int{8080, 8081}, notes: "```cf-tunnel\n" +
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
	g := s.in(guest{vmid: 9103, notes: "cf-tunnel: e2e-s3a.ZONE e2e-s3b.ZONE e2e-s3c.ZONE -> :8080"})
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
	g := s.in(guest{vmid: 9105, ports: []int{8080, 8081}, notes: "```cf-tunnel\n" +
		"*.e2e-s5.ZONE -> :8080\nwww.e2e-s5.ZONE -> :8081\n```"})
	s.create(t, g)
	s.enforce(t)
	tunnel := s.tunnel(t)
	wild, exact := "*.e2e-s5."+s.zone, "www.e2e-s5."+s.zone
	s.waitRoute(t, wild, planner.StateActive)
	s.waitRoute(t, exact, planner.StateActive)
	s.waitIngress(t, tunnel, wild, "http://"+g.ip()+":8080")
	rules := s.waitIngress(t, tunnel, exact, "http://"+g.ip()+":8081")
	_, wi, _ := ruleOf(rules, wild)
	_, ei, _ := ruleOf(rules, exact)
	require.Less(t, ei, wi, "the exact name comes before the wildcard that covers it: %+v", rules)
}

func testAddressSources(t *testing.T, s *suite) {
	static := s.in(guest{vmid: 9106, notes: "cf-tunnel: e2e-s6s.ZONE -> :8080"})
	reported := s.in(guest{vmid: 9107, manual: true, notes: "cf-tunnel: e2e-s6r.ZONE -> :8080"})
	s.create(t, static)
	s.create(t, reported)
	cfg := s.must(t, "pct", "config", strconv.Itoa(reported.vmid))
	require.Contains(t, cfg, "ip=manual", "the address of 9107 is in no configuration")
	s.enforce(t)
	for _, g := range []guest{static, reported} {
		name := "s6s"
		if g.manual {
			name = "s6r"
		}
		route := s.waitRoute(t, s.host(name), planner.StateActive)
		require.Equal(t, "http://"+g.ip()+":8080", route.Service, name)
		require.Equal(t, "port", route.Level, name)
	}
}

func testLifecycle(t *testing.T, s *suite) {
	g := s.in(guest{vmid: 9108, notes: "cf-tunnel: e2e-s7.ZONE -> :8080"})
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
	original := s.in(guest{vmid: 9140, notes: "cf-tunnel: e2e-s8.ZONE -> :8080"})
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
	clone := guest{vmid: 9141}
	s.stop(t, original.vmid)
	s.guests[clone.vmid] = true
	s.must(t, "pct", "clone", "9140", "9141", "--hostname", "e2e-9141")
	s.start(t, original.vmid)
	s.waitOwnerRoute(t, host, original.owner(), planner.StateActive)
	s.must(t, "pct", "set", "9141", "--net0", clone.net0())
	require.Contains(t, s.must(t, "pct", "config", "9141"), host, "the clone has the notes")
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
	t.Cleanup(func() {
		// An adopted record is the install's, which the purge removes.
		if slices.Contains(s.records(t, host), "CNAME foreign.example.net") {
			s.cloud.Remove(t, s.zone, foreign)
		}
	})
	g := s.in(guest{vmid: 9160, notes: "cf-tunnel: e2e-s10.ZONE -> :8080"})
	s.create(t, g)
	s.enforce(t)
	tunnel := s.tunnel(t)

	s.waitState(t, "a conflict for "+host, 2*time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Conflicts, func(c reconcile.Conflict) bool { return strings.EqualFold(c.Name, host) })
	})
	require.Equal(t, []string{"CNAME foreign.example.net"}, s.records(t, host), "a record of someone else is left alone")

	asked := time.Now()
	out := s.must(t, s.pco, "adopt", host, "--yes")
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
	before := s.cloud.Records(t, s.zone)
	from := len(s.fake.f.Calls())
	s.fake.f.Deny("dns.write")
	t.Cleanup(func() { s.fake.f.Allow("dns.write") })

	g := s.in(guest{vmid: 9170, notes: "cf-tunnel: e2e-s12.ZONE -> :8080"})
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
	require.Subset(t, s.cloud.Records(t, s.zone), before, "every record is still there")

	s.fake.f.Allow("dns.write")
	s.sync(t)
	s.waitRecord(t, host, tunnel, 2*time.Minute)
	s.waitState(t, "the problem to go", time.Minute, func(st engine.State) bool {
		return !slices.ContainsFunc(st.Problems, func(p string) bool { return strings.Contains(p, host) })
	})
}

func testOutages(t *testing.T, s *suite) {
	s.ensureS1(t)
	tunnel := s.tunnel(t)
	active := activeRoutes(s.state(t))
	records := s.cloud.Records(t, s.zone)
	ingress := s.cloud.Ingress(t, tunnel)
	from := 0
	if s.fake != nil {
		from = len(s.fake.f.Calls())
	}

	t.Log("the daemon is killed in a cycle and comes back")
	s.sync(t)
	time.Sleep(300 * time.Millisecond)
	killed := time.Now()
	s.must(t, "systemctl", "kill", "--signal=SIGKILL", "pco.service")
	s.waitState(t, "a cycle of the new daemon", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(killed) && !st.FinishedAt.IsZero() && st.Complete
	})
	s.requireSame(t, tunnel, active, records, ingress)

	t.Log("Proxmox does not answer: the daemon holds")
	s.must(t, "systemctl", "stop", "pveproxy")
	t.Cleanup(func() { _, _ = s.run("systemctl", "start", "pveproxy") })
	stopped := time.Now()
	st := s.waitState(t, "a cycle without Proxmox", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(stopped) && !st.FinishedAt.IsZero() && !st.Complete
	})
	t.Logf("problems: %q", otherProblems(st))
	time.Sleep(3 * pollInterval)
	require.Equal(t, records, s.cloud.Records(t, s.zone))
	require.Equal(t, ingress, s.cloud.Ingress(t, tunnel))
	s.must(t, "systemctl", "start", "pveproxy")
	s.sync(t)
	s.waitState(t, "a complete cycle", 2*time.Minute, func(st engine.State) bool { return st.Complete })
	s.requireSame(t, tunnel, active, records, ingress)

	if s.fake == nil {
		return
	}
	t.Log("Cloudflare does not answer: the daemon holds")
	baseline := otherProblems(s.state(t))
	s.fake.srv.stop()
	t.Cleanup(func() { _ = s.fake.srv.start() })
	down := time.Now()
	st = s.waitState(t, "a problem without Cloudflare", 2*time.Minute, func(st engine.State) bool {
		return st.At.After(down) && slices.ContainsFunc(otherProblems(st), func(p string) bool { return !slices.Contains(baseline, p) })
	})
	t.Logf("problems: %q", otherProblems(st))
	time.Sleep(3 * pollInterval)
	require.NoError(t, s.fake.srv.start())
	s.sync(t)
	s.waitState(t, "the problems of the outage to go", 3*time.Minute, func(st engine.State) bool {
		return !slices.ContainsFunc(otherProblems(st), func(p string) bool { return !slices.Contains(baseline, p) })
	})
	s.requireSame(t, tunnel, active, records, ingress)
	for _, w := range s.fake.writes(from) {
		require.False(t, strings.HasPrefix(w, "Delete") || strings.HasPrefix(w, "CreateTunnel"), "an outage changes nothing: %s", w)
	}
}

// requireSame checks that the routes, the records and the ingress are as they
// were before an outage.
func (s *suite) requireSame(t *testing.T, tunnel string, active []string, records []cfapi.Record, ingress []planner.IngressRule) {
	t.Helper()
	s.waitState(t, "the routes that were active", 2*time.Minute, func(st engine.State) bool {
		return slices.Equal(activeRoutes(st), active)
	})
	require.Equal(t, records, s.cloud.Records(t, s.zone))
	require.Equal(t, ingress, s.cloud.Ingress(t, tunnel))
	tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install))
	require.True(t, found)
	require.Equal(t, tunnel, tn.ID)
	enabled, _ := s.run("systemctl", "is-enabled", "pco-cloudflared@"+tunnel+".service")
	require.Equal(t, "enabled", strings.TrimSpace(enabled))
}

func activeRoutes(st engine.State) []string {
	var out []string
	for _, r := range st.Routes {
		if r.State == planner.StateActive {
			out = append(out, r.Hostname+" "+r.Service)
		}
	}
	slices.Sort(out)
	return out
}

func testRulesetFlushed(t *testing.T, s *suite) {
	target := s.ensureS1(t).ip() + ":8080"
	s.waitTarget(t, target, true, time.Minute, time.Second)
	_, targets, ok := s.egress(t)
	require.True(t, ok)
	others := s.otherTables(t)

	flushed := time.Now()
	s.must(t, "nft", "flush", "ruleset")
	s.waitEvent(t, flushed, "egress", "was loaded again")
	deadline := time.Now().Add(10 * time.Second)
	for {
		summary, after, ok := s.egress(t)
		if ok && slices.Equal(targets, after) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the table did not come back with its targets %v: %s, %v", targets, summary, after)
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.restoreTables(t, others)
}

// otherTables saves the tables of the ruleset that are not pco's, which a
// flush takes as well.
func (s *suite) otherTables(t *testing.T) map[string]string {
	t.Helper()
	tables := map[string]string{}
	for line := range strings.Lines(s.must(t, "nft", "list", "tables")) {
		f := strings.Fields(line)
		if len(f) != 3 || f[0] != "table" || f[2] == "pco_egress" {
			continue
		}
		tables[f[1]+" "+f[2]] = s.must(t, "nft", "list", "table", f[1], f[2])
	}
	t.Logf("tables besides pco's: %d", len(tables))
	return tables
}

// restoreTables waits for whoever owns the tables to load them again, as the
// Proxmox firewall does, and loads the ones still missing then as they were.
func (s *suite) restoreTables(t *testing.T, tables map[string]string) {
	t.Helper()
	if len(tables) == 0 {
		return
	}
	time.Sleep(30 * time.Second)
	present := s.must(t, "nft", "list", "tables")
	for name, def := range tables {
		if strings.Contains(present, "table "+name+"\n") {
			continue
		}
		t.Logf("table %s did not come back by itself; loading it as it was", name)
		file := s.dir + "/table.nft"
		require.NoError(t, os.WriteFile(file, []byte(def), 0o600))
		s.must(t, "nft", "-f", file)
	}
}

func testMACMoved(t *testing.T, s *suite) {
	g := s.in(guest{vmid: 9180, notes: "cf-tunnel: e2e-s15.ZONE -> :8080"})
	s.create(t, g)
	s.enforce(t)
	host, target := s.host("s15"), g.ip()+":8080"
	s.waitRoute(t, host, planner.StateActive)
	s.waitTarget(t, target, true, time.Minute, time.Second)
	own := s.hwaddr(t, g.vmid)

	setMAC := func(mac string) {
		s.exec(t, g.vmid, "ip link set eth0 down && ip link set eth0 address "+mac+" && ip link set eth0 up")
	}
	// The ping makes the guest ask for the node with its new MAC, which the
	// neighbour table of the node takes at once. The 2 seconds count from
	// there.
	const moved = "02:e2:e0:00:91:80"
	seen := s.neighbourSeen(t, g.ip(), moved)
	setMAC(moved)
	pinged := make(chan error, 1)
	go func() {
		_, err := s.run("pct", "exec", strconv.Itoa(g.vmid), "--", "sh", "-c", "ping -c 1 -W 2 "+nodeAddr+" >/dev/null || true")
		pinged <- err
	}()
	var at time.Time
	select {
	case at = <-seen:
	case <-time.After(10 * time.Second):
		t.Fatalf("the neighbour table of the node never gave %s the MAC %s", g.ip(), moved)
	}
	s.waitTarget(t, target, false, 10*time.Second, 50*time.Millisecond)
	took := time.Since(at)
	t.Logf("the target left the egress set at most %s after the node saw the new MAC", took.Round(10*time.Millisecond))
	require.LessOrEqual(t, took, 2*time.Second)
	require.NoError(t, <-pinged)
	route := s.waitRoute(t, host, planner.StateWithdrawn)
	t.Logf("withdrawn: %s", route.Reason)

	setMAC(own)
	s.exec(t, g.vmid, "ping -c 1 -W 2 "+nodeAddr+" >/dev/null || true")
	s.sync(t)
	s.waitRoute(t, host, planner.StateActive)
	s.waitTarget(t, target, true, time.Minute, time.Second)
}

func testIdentity(t *testing.T, s *suite) {
	victim := s.in(guest{vmid: 9151, notes: "cf-tunnel: e2e-s9v.ZONE -> :8080"})
	s.create(t, victim)
	s.enforce(t)
	s.waitRoute(t, s.host("s9v"), planner.StateActive)

	// The attacker answers for the node's address on the bridge and for the
	// victim's, and names both in its notes.
	attacker := s.in(guest{vmid: 9150, notes: "```cf-tunnel\n" +
		"e2e-s9n.ZONE -> http://" + nodeAddr + ":8080\n" +
		"e2e-s9g.ZONE -> http://" + victim.ip() + ":8080\n```",
		start: []string{"ip addr add " + nodeAddr + "/32 dev eth0", "ip addr add " + victim.ip() + "/32 dev eth0"}})
	s.create(t, attacker)
	stolen := []string{s.host("s9n"), s.host("s9g")}
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
		_, targets, _ := s.egress(t)
		require.NotContains(t, targets, nodeAddr+":8080")
		time.Sleep(time.Second)
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
