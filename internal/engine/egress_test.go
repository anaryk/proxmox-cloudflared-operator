package engine

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func egressTargets(ss ...string) []egress.Target {
	out := []egress.Target{}
	for _, s := range ss {
		ap := netip.MustParseAddrPort(s)
		out = append(out, egress.Target{Addr: ap.Addr(), Port: ap.Port()})
	}
	return out
}

// orderLogged is an env whose egress filter and Cloudflare write their calls to
// one log.
func orderLogged(t *testing.T) (*env, *callLog) {
	t.Helper()
	e := newEnv(t)
	log := &callLog{}
	e.egr.log = log
	e.useAPI(testToken, hookedAPI{API: e.cf, before: func(method string) { log.add("cf " + method) }})
	return e, log
}

func TestATargetEntersTheEgressSetBeforeItsRuleIsWritten(t *testing.T) {
	e, log := orderLogged(t)
	e.enforce()

	st := e.cycle()

	require.Empty(t, st.Problems)
	set := log.index("egress set 10.0.0.11:8080", 0)
	put := log.index("cf PutTunnelConfig", 0)
	require.GreaterOrEqual(t, set, 0, log.all())
	require.Greater(t, put, set, log.all())
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
}

func TestTheEgressSetIsGivenInObserveModeToo(t *testing.T) {
	e := newEnv(t)

	st := e.cycle()

	require.Equal(t, ModeObserve, st.Mode)
	got, ok := e.egr.last()
	require.True(t, ok)
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// A route that goes keeps its target in the set while the configuration last
// verified at Cloudflare still sends a connector to it; the cycle after the
// one whose tunnel run verified the configuration without it takes it out.
func TestAWithdrawnTargetLeavesTheEgressSetOnlyAfterItsRuleIsGone(t *testing.T) {
	e, log := orderLogged(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	e.cycle()
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got)

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.clock.advance(20 * time.Second)
	from := len(log.all())
	st := e.cycle()

	require.Empty(t, st.Problems)
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got, "the rule is still there when the set is given")
	put := log.index("cf PutTunnelConfig", from)
	require.Greater(t, put, log.index("egress set", from), log.all())
	require.NotContains(t, e.rules(), planner9090(), "the tunnel run takes the rule out")

	e.clock.advance(20 * time.Second)
	e.cycle()

	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
	require.Greater(t, log.index("egress set 10.0.0.11:8080", put), put, "out of the set after the run that verified the rule gone")
}

func planner9090() planner.IngressRule {
	return hostRuleAt("api.example.com", "http://10.0.0.11:9090")
}

func hostRuleAt(host, service string) planner.IngressRule {
	return planner.IngressRule{Hostname: host, Service: service}
}

// While the tunnel run writes nothing, the configuration verified last still
// sends a connector to a target that goes, and the target stays.
func TestATargetStaysWhileTheTunnelRunDoesNotVerifyItsRuleGone(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	e.cycle()
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))

	for range 2 {
		e.clock.advance(20 * time.Second)
		e.cycle()

		got, _ := e.egr.last()
		require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got)
	}
}

func TestAnAddressThatLostItsProofLeavesTheEgressFilterAtOnce(t *testing.T) {
	e, log := orderLogged(t)
	e.enforce()
	e.cycle()
	e.res.stop("www.example.com", "identity check failed: 10.0.0.11 answered by bc:24:11:ff:ff:01")

	e.clock.advance(20 * time.Second)
	from := len(log.all())
	st := e.cycle()

	require.Equal(t, "withdrawn", string(route(st, "www.example.com").State))
	calls := log.all()[from:]
	require.Equal(t, "egress remove 10.0.0.11", calls[0], "before anything else of the cycle: %v", calls)
	got, _ := e.egr.last()
	require.Empty(t, got, "and out of the set of the cycle, whose rule still is at Cloudflare")

	t.Run("once", func(t *testing.T) {
		e.clock.advance(20 * time.Second)
		e.cycle()

		require.Equal(t, []netip.Addr{guestAddr}, e.egr.removes(), "a binding withdrawn before lost nothing in this cycle")
		got, _ := e.egr.last()
		require.Empty(t, got, "a withdrawn address is no verified target")
	})
}

// A tunnel that is gone at Cloudflare sends no connector anywhere: what its
// configuration held leaves the set even while nothing is written.
func TestTheTargetsOfATunnelThatIsGoneLeaveTheEgressSet(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	e.cycle()
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	require.NoError(t, e.cf.DeleteTunnel(t.Context(), testAccount, e.tunnels()[0].ID))

	for range 2 {
		e.clock.advance(20 * time.Second)
		e.cycle()
	}

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

// An address that lost its proof is taken out of the filter only when the set
// holds it: a route held back by the minimum never put it there.
func TestAnAddressTheSetDoesNotHoldIsNotRemoved(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(
		guest(101, "web-1", "www.example.com -> 10.0.0.20:8080"),
		guest(102, "api", "api.example.com -> :8080"),
	))
	e.res.setLevel("api.example.com", resolve.LevelObserved)
	e.cycle()
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.20:8080"), got)

	e.res.stop("api.example.com", "guest is not running")
	e.clock.advance(20 * time.Second)
	e.cycle()

	require.Empty(t, e.egr.removes())
}

func TestAFailedSetHoldsTheWritesOfTheTunnelRun(t *testing.T) {
	e := published(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	e.egr.failSet(errors.New("nft -f -: exit status 1: Error: Could not process rule"))
	writes, ensures := len(e.writes()), len(e.conn.ensures())

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, []string{"setting the egress filter: nft -f -: exit status 1: Error: Could not process rule; " +
		"no tunnel configuration is written until it is set"}, st.Problems)
	require.Contains(t, actionKinds(st), "put-config pco-abc123 held: the egress filter could not be set")
	require.Len(t, e.writes(), writes, "no rule for a target the connector cannot reach, and no record for it")
	require.Greater(t, len(e.conn.ensures()), ensures, "the connectors go on")
	require.Empty(t, st.Hold, "DNS goes on as after a held tunnel run")
	require.Equal(t, []string{"www.example.com"}, e.recordNames())

	e.egr.failSet(nil)
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Empty(t, st.Problems)
	require.Equal(t, []string{"api.example.com", "www.example.com"}, e.recordNames())
}

func TestASwitchedOffFilterIsNoFailure(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.egr.failSet(egress.ErrOff)

	st := e.cycle()

	require.Empty(t, st.Problems)
	require.Equal(t, withSentinel(hostRule("www.example.com")), e.rules())
}

func TestAnIncompleteInventoryLeavesTheEgressSetAlone(t *testing.T) {
	e := published(t)
	e.inv.set(incomplete("listing the guests of pve1 failed", guest(101, "web-1", "www.example.com -> :8080")))
	sets := e.egr.setCount()

	e.clock.advance(20 * time.Second)
	e.cycle()

	require.Equal(t, sets, e.egr.setCount())
	require.Empty(t, e.egr.removes())
}

func TestTheVanishGuardLeavesTheEgressSetAlone(t *testing.T) {
	e := publishedMany(t, 10)
	e.inv.set(snapshot())
	sets := e.egr.setCount()

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.NotEmpty(t, st.Hold)
	require.Equal(t, sets, e.egr.setCount())
	require.Empty(t, e.egr.removes())
}

func TestAStoreHoldLeavesTheEgressSetAlone(t *testing.T) {
	e := published(t)
	path := filepath.Join(e.paths.Private, "credentials", "cred2.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"schemaVersion":1,"rev":1,"id":"cred2","data":{`), 0o600))
	sets := e.egr.setCount()

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems[0], "reading the credentials")
	require.Equal(t, sets, e.egr.setCount())
}

// A hold for a reason of Cloudflare's still gives the set: what was verified
// is known.
func TestTheEgressSetIsGivenWhileCloudflareIsHeld(t *testing.T) {
	e := published(t)
	require.NoError(t, os.Remove(filepath.Join(e.paths.Cluster, "meta", "leader.json")))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Hold, "no writer identity")
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got)
}

func TestARouteHeldBackByTheMinimumIsNotInTheEgressSet(t *testing.T) {
	e := webAndAPI(t)
	e.res.setLevel("www.example.com", resolve.LevelObserved)
	e.inv.set(snapshot(
		guest(101, "web-1", "www.example.com -> :8080"),
		guest(102, "api", "api.example.com -> :9090"),
	))

	e.cycle()

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:9090"), got)
}

func TestAManualRouteToTheNodeIsAnExactEntry(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot())
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "pve.example.com", ManualID: "pve", Source: model.SourceManual,
		Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: nodeAddr, Port: 8006},
		Options: model.RouteOptions{AllowNode: true, NoTLSVerify: true},
	}))

	e.cycle()

	got, _ := e.egr.last()
	require.Equal(t, []egress.Target{{Addr: nodeAddr, Port: 8006, AllowNode: true}}, got, "accepted before the addresses of the node are refused")

	// Kept as such in the cycles that follow, also from the memory of an
	// earlier process.
	e.eng = e.newEngine()
	e.clock.advance(10 * time.Second)
	e.cycle()
	got, _ = e.egr.last()
	require.Equal(t, []egress.Target{{Addr: nodeAddr, Port: 8006, AllowNode: true}}, got)
}

// A target of allowNode leaves the set as any other does: only after a tunnel
// run verified a configuration without its rule, and until then it is still
// one of allowNode, accepted before the addresses of the node are refused.
func TestATargetOfAllowNodeStaysWhileItsRuleIsVerified(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot())
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "pve.example.com", ManualID: "pve", Source: model.SourceManual,
		Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: nodeAddr, Port: 8006},
		Options: model.RouteOptions{AllowNode: true, NoTLSVerify: true},
	}))
	e.cycle()
	require.NoError(t, e.store.DeleteManualRoute("pve"))

	e.clock.advance(time.Minute)
	e.cycle()
	sets := e.egr.setCount()
	got, _ := e.egr.last()
	require.Contains(t, got, egress.Target{Addr: nodeAddr, Port: 8006, AllowNode: true}, "its rule was verified at Cloudflare until this cycle")

	e.clock.advance(time.Minute)
	e.cycle()
	require.Greater(t, e.egr.setCount(), sets)
	got, _ = e.egr.last()
	require.Empty(t, got)
}

// The set and the configuration verified last are in the memory: a restart
// neither takes out a target whose rule is still at Cloudflare nor keeps one
// whose rule is gone.
// The process stops after a cycle saved a withdrawn binding and before it
// saved its memory: the memory still has the address in the set and its rule
// in the configuration verified last. The binding says it is withdrawn, and
// that keeps it out, whatever the memory says.
func TestAWithdrawnAddressDoesNotComeBackThroughAStaleMemory(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	stale, err := e.store.EngineMemory()
	require.NoError(t, err)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:8080")}, stale.Egress)
	// Nothing is written in the cycles that follow: the rule stays at
	// Cloudflare.
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })
	e.res.stop("www.example.com", "identity check failed: 10.0.0.11 answered by bc:24:11:ff:ff:01")
	e.clock.advance(20 * time.Second)
	e.cycle()
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.True(t, bindings["www.example.com"].Withdrawn)

	require.NoError(t, e.store.SaveEngineMemory(stale), "the memory that cycle could not save")
	e.eng = e.newEngine()
	for range 2 {
		e.clock.advance(20 * time.Second)
		st := e.cycle()

		require.Equal(t, "withdrawn", string(route(st, "www.example.com").State))
		got, _ := e.egr.last()
		require.Empty(t, got, "a withdrawn address is fed back")
	}
}

// The same, for a route that is gone from the guest's Notes by the time the
// daemon starts again: nothing resolves it, but its stored binding still says
// the address lost its proof.
func TestAWithdrawnAddressOfARouteThatIsGoneDoesNotComeBack(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	stale, err := e.store.EngineMemory()
	require.NoError(t, err)
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })
	e.res.stop("www.example.com", "identity check failed: 10.0.0.11 answered by bc:24:11:ff:ff:01")
	e.clock.advance(20 * time.Second)
	e.cycle()

	require.NoError(t, e.store.SaveEngineMemory(stale), "the memory that cycle could not save")
	e.inv.set(snapshot(guest(101, "web-1")))
	e.eng = e.newEngine()
	e.clock.advance(20 * time.Second)
	e.cycle()

	got, _ := e.egr.last()
	require.Empty(t, got, "a withdrawn address is fed back")
}

// An address whose proof falls below the minimum is still proven, at a lower
// level: it is held back, and leaves the set the way a withdrawn route's
// target leaves it, after the tunnel run took its rule out. Only a lost proof
// takes it out at once.
func TestATargetThatFallsBelowTheMinimumLeavesAfterItsRule(t *testing.T) {
	e, log := orderLogged(t)
	e.enforce()
	e.cycle()
	e.res.setLevel("www.example.com", resolve.LevelObserved)

	e.clock.advance(20 * time.Second)
	from := len(log.all())
	st := e.cycle()

	require.Equal(t, planner.StateUnreachable, route(st, "www.example.com").State)
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got, "its rule is still at Cloudflare when the set is given")
	require.Empty(t, e.egr.removes())
	put := log.index("cf PutTunnelConfig", from)
	require.Greater(t, put, log.index("egress set", from), log.all())

	e.clock.advance(20 * time.Second)
	e.cycle()

	got, _ = e.egr.last()
	require.Empty(t, got)
}

func TestTheEgressSetSurvivesARestart(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	e.cycle()

	e.eng = e.newEngine()
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	e.clock.advance(20 * time.Second)
	e.cycle()

	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), got)

	e.eng = e.newEngine()
	e.enforce()
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.eng = e.newEngine()
	e.clock.advance(20 * time.Second)
	e.cycle()

	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
}

func TestRuleTargets(t *testing.T) {
	got := ruleTargets(withSentinel(
		hostRuleAt("b.example.com", "https://10.0.0.12:443"),
		hostRuleAt("a.example.com", "http://10.0.0.11:8080"),
		hostRuleAt("c.example.com", "http://10.0.0.11:8080"),
		hostRuleAt("held.example.com", "http_status:503"),
		hostRuleAt("v6.example.com", "http://[fd00::5]:80"),
		hostRuleAt("mapped.example.com", "http://[::ffff:10.0.0.13]:80"),
		hostRuleAt("odd.example.com", "http://web-1:80"),
	))

	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.12:443", "10.0.0.13:80", "[fd00::5]:80"), got)
}
