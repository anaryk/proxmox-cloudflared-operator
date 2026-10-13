//go:build e2e

package e2e

import (
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// The guests of the networking scenarios: on pcot1 and pcot2 the same
// address, reached through a leg each; on the bridge of the run a Direct
// route beside them.
const (
	sameAddr = "10.50.0.10/24"
	s17VMID  = 9351
	s19VMID  = 9352
	s19Dir   = 9353
	s18VMID  = 9354
	s18Tag   = 9355
)

func (s *suite) s17Guest() guest {
	return s.in(guest{vmid: s17VMID, bridge: isolatedBridge, addr: sameAddr, notes: "cf-tunnel: e2e-s17.ZONE -> :8080"})
}

// ensureS17 makes the guest of S17 on pcot1 and attaches the network, unless
// the run has them, and returns the guest with its route served through the
// leg.
func (s *suite) ensureS17(t *testing.T) (guest, engine.RouteView) {
	t.Helper()
	g := s.s17Guest()
	s.bridgeUp(t, isolatedBridge, false)
	if !s.guests[g.vmid] {
		s.create(t, g)
	}
	s.enforce(t)
	if _, ok := networkOf(s.state(t), isolatedBridge); !ok {
		s.attach(t, isolatedBridge, 0, legAddr)
	}
	return g, s.waitVia(t, s.host("s17"), engine.ViaAttach+isolatedBridge)
}

func (s *suite) s19Guest() guest {
	return s.in(guest{vmid: s19VMID, bridge: secondBridge, addr: sameAddr, notes: "cf-tunnel: e2e-s19b.ZONE -> :8080"})
}

// ensureS19 makes the second guest, at the same address on pcot2, and
// attaches that network too.
func (s *suite) ensureS19(t *testing.T) (guest, engine.RouteView) {
	t.Helper()
	g := s.s19Guest()
	s.bridgeUp(t, secondBridge, false)
	if !s.guests[g.vmid] {
		s.create(t, g)
	}
	s.enforce(t)
	if _, ok := networkOf(s.state(t), secondBridge); !ok {
		s.attach(t, secondBridge, 0, legAddr)
	}
	return g, s.waitVia(t, s.host("s19b"), engine.ViaAttach+secondBridge)
}

// target is the service address and port a connector dials for the route.
func target(t testing.TB, r engine.RouteView) string {
	t.Helper()
	return net.JoinHostPort(serviceOf(t, r).String(), "8080")
}

func testAttachIsolatedBridge(t *testing.T, s *suite) {
	s.bridgeUp(t, isolatedBridge, false)
	g := s.s17Guest()
	s.create(t, g)
	s.enforce(t)
	host := s.host("s17")

	// The node has no address on pcot1: until a leg is there the route
	// cannot be proven, and nothing is served.
	held := s.waitState(t, "the route of "+host+" without a way to its guest", 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, "")
		return ok && r.State != planner.StateActive
	})
	r, _ := routeOf(held, host, "")
	t.Logf("before the attach: %s (%s)", r.State, r.Reason)
	s.requireNoTarget(t, g.ip()+":8080")

	attached := time.Now()
	n := s.attach(t, isolatedBridge, 0, legAddr)
	require.Equal(t, store.NetworkAttach, n.Kind)
	require.Equal(t, legPrefix, n.Prefix.String())
	s.requireNetnsKept(t)
	s.requireHostRoutes(t)

	route := s.waitVia(t, host, engine.ViaAttach+isolatedBridge)
	require.Equal(t, "port", route.Level)
	service := serviceOf(t, route)
	tg := target(t, route)
	real := g.ip() + ":8080"
	t.Logf("%s is served at %s for %s", host, tg, real)
	s.waitTarget(t, tg, true, time.Minute, time.Second)
	s.requireNoTarget(t, real)
	s.waitIngress(t, s.tunnel(t), host, "http://"+tg)
	s.requireAnswer(t, tg, g.body(8080), time.Minute)
	require.True(t, s.dnatHas(t, service), "the namespace does not translate %s", service)
	s.waitEvent(t, attached, "network", "service address "+service.String()+" allocated")
	mappings := s.serviceAddrs(t)
	require.Contains(t, mappings, service)
	require.Equal(t, g.ip(), mappings[service].Addr.String())
	require.Equal(t, isolatedBridge, mappings[service].Network)

	t.Log("the route withdrawn: the service address leaves the set, then it is released; the real address is never in the set")
	withdrawn := time.Now()
	s.must(t, "pct", "set", strconv.Itoa(g.vmid), "--delete", "description")
	s.sync(t)
	s.waitGone(t, host)
	s.waitTarget(t, tg, false, 2*time.Minute, time.Second)
	s.requireNoTarget(t, real)
	s.waitEvent(t, withdrawn, "network", "service address "+service.String()+" of "+g.ip()+" on network "+isolatedBridge+" released")
	require.NotContains(t, s.serviceAddrs(t), service)
	require.False(t, s.dnatHas(t, service), "the namespace still translates the released %s", service)

	t.Log("the route back: another service address, never the released one again")
	s.setNotes(t, g.vmid, g.notes)
	route = s.waitVia(t, host, engine.ViaAttach+isolatedBridge)
	again := serviceOf(t, route)
	require.Equal(t, 1, again.Compare(service), "the pool gave %s again, or went backwards: %s", service, again)
	s.requireAnswer(t, target(t, route), g.body(8080), time.Minute)
	s.requireNoTarget(t, real)
}

func testAttachVLAN(t *testing.T, s *suite) {
	s.enforce(t)
	t.Log("a tag on a bridge that filters VLANs")
	s.bridgeUp(t, vlanBridge, true)
	tagged := s.in(guest{vmid: s18VMID, bridge: vlanBridge, vlan: vlanTag, addr: "10.60.0.10/24", notes: "cf-tunnel: e2e-s18v.ZONE -> :8080"})
	s.create(t, tagged)
	n := s.attach(t, vlanBridge, vlanTag, vlanLegAddr)
	id := store.SegmentID(vlanBridge, vlanTag)
	require.Equal(t, id, n.ID)
	require.Equal(t, vlanTag, n.VLAN)
	port := "vethpco" + strconv.Itoa(n.Mark)
	vlans := s.must(t, "bridge", "-j", "vlan", "show", "dev", port)
	var ports []struct {
		VLANs []struct {
			VLAN  int      `json:"vlan"`
			Flags []string `json:"flags"`
		} `json:"vlans"`
	}
	require.NoError(t, json.Unmarshal([]byte(vlans), &ports), vlans)
	require.Len(t, ports, 1, vlans)
	require.Len(t, ports[0].VLANs, 1, "the leg's port carries other VLANs: %s", vlans)
	require.Equal(t, vlanTag, ports[0].VLANs[0].VLAN, vlans)
	require.Contains(t, ports[0].VLANs[0].Flags, "PVID", vlans)
	route := s.waitVia(t, s.host("s18v"), engine.ViaAttach+id)
	svcVLAN := serviceOf(t, route)
	s.requireAnswer(t, target(t, route), tagged.body(8080), time.Minute)
	s.requireNoTarget(t, tagged.ip()+":8080")

	t.Log("a tag on a bridge that does not filter VLANs: the leg sits on the bridge Proxmox makes for it, and follows it")
	s.bridgeUp(t, isolatedBridge, false)
	// Proxmox makes that bridge from the physical ports of the guest's
	// bridge, and refuses to start the guest on a bridge that has none.
	s.physicalPort(t, isolatedBridge)
	onTag := s.in(guest{vmid: s18Tag, bridge: isolatedBridge, vlan: taggedVLAN, addr: "10.70.0.10/24", notes: "cf-tunnel: e2e-s18t.ZONE -> :8080"})
	s.create(t, onTag)
	vlanDev := isolatedBridge + "v" + strconv.Itoa(taggedVLAN)
	s.must(t, "ip", "link", "show", "dev", vlanDev)
	n = s.attach(t, isolatedBridge, taggedVLAN, taggedLegAddr)
	tid := store.SegmentID(isolatedBridge, taggedVLAN)
	link := s.must(t, "ip", "-o", "link", "show", "dev", "vethpco"+strconv.Itoa(n.Mark))
	require.Contains(t, link, "master "+vlanDev, link)
	route = s.waitVia(t, s.host("s18t"), engine.ViaAttach+tid)
	s.requireAnswer(t, target(t, route), onTag.body(8080), time.Minute)

	// The tagged guest stops and its bridge goes: the leg has nowhere to be.
	// When the guest starts, Proxmox makes the bridge again, and the leg is
	// put back on it without anyone asking.
	s.stop(t, onTag.vmid)
	_, _ = s.run("ip", "link", "del", vlanDev)
	n = s.waitLeg(t, tid, false)
	leg, _ := legOn(n, s.state(t).Node)
	t.Logf("with %s gone: %s", vlanDev, leg.Problem)
	s.start(t, onTag.vmid)
	s.waitLeg(t, tid, true)
	link = s.must(t, "ip", "-o", "link", "show", "dev", "vethpco"+strconv.Itoa(n.Mark))
	require.Contains(t, link, "master "+vlanDev, link)
	route = s.waitVia(t, s.host("s18t"), engine.ViaAttach+tid)
	s.requireAnswer(t, target(t, route), onTag.body(8080), time.Minute)
	s.requireNetnsKept(t)

	t.Log("detached: the legs go, the service addresses are released")
	for _, network := range []struct {
		id  string
		svc netip.Addr
	}{{tid, serviceOf(t, route)}, {id, svcVLAN}} {
		n, ok := networkOf(s.state(t), network.id)
		require.True(t, ok, network.id)
		s.detach(t, network.id)
		_, err := s.netns("ip", "link", "show", "dev", gateway.LegIface(n.Mark))
		require.Error(t, err, "the leg of %s is still there", network.id)
		require.NotContains(t, s.serviceAddrs(t), network.svc)
	}
	s.requireNetnsKept(t)
	s.destroy(t, tagged.vmid)
	s.destroy(t, onTag.vmid)
	s.sync(t)
}

func testIdenticalSubnets(t *testing.T, s *suite) {
	s.requireFirewallService(t)
	s.setSysctl(t, "net.bridge.bridge-nf-call-iptables", "1")
	a, ra := s.ensureS17(t)
	b, rb := s.ensureS19(t)
	require.Equal(t, a.ip(), b.ip(), "the guests share an address")
	ta, tb := target(t, ra), target(t, rb)
	require.NotEqual(t, ta, tb, "two networks, two service addresses")
	t.Logf("%s at %s, %s at %s, both for %s", s.host("s17"), ta, s.host("s19b"), tb, a.ip())

	t.Log("500 requests in parallel, each hostname answered by its own guest")
	okA, crossedA, failedA := load(ta, a.body(8080), 250, 10)
	okB, crossedB, failedB := load(tb, b.body(8080), 250, 10)
	t.Logf("%s: %d answered, %d crossed, %d failed; %s: %d answered, %d crossed, %d failed", ta, okA, crossedA, failedA, tb, okB, crossedB, failedB)
	require.Zero(t, crossedA+crossedB, "answers crossed between the guests")
	require.Zero(t, failedA+failedB, "requests failed")

	st := s.state(t)
	na, _ := networkOf(st, isolatedBridge)
	nb, _ := networkOf(st, secondBridge)
	loA, hiA := gateway.PortBlock(na.Mark)
	loB, hiB := gateway.PortBlock(nb.Mark)
	if ports, ok := s.conntrackPorts(t, a.ip(), 8080); !ok {
		t.Log("conntrack is not installed on the node: the source ports of the flows are not read")
	} else {
		require.NotEmpty(t, ports, "no flow to %s in the host's conntrack while bridge-nf-call-iptables is on", a.ip())
		inA, inB := 0, 0
		for _, p := range ports {
			switch {
			case p >= int(loA) && p <= int(hiA):
				inA++
			case p >= int(loB) && p <= int(hiB):
				inB++
			default:
				t.Fatalf("source port %d of a flow to %s is in neither leg's block (%d-%d, %d-%d)", p, a.ip(), loA, hiA, loB, hiB)
			}
		}
		t.Logf("source ports in the host's conntrack: %d in %s's block %d-%d, %d in %s's block %d-%d", inA, na.ID, loA, hiA, inB, nb.ID, loB, hiB)
		require.Positive(t, inA)
		require.Positive(t, inB)
	}

	t.Log("one guest's MAC changes: only its service address leaves the set, the other keeps serving")
	own := s.hwaddr(t, b.vmid)
	const moved = "02:e2:e0:00:93:52"
	s.setMAC(t, b.vmid, moved)
	s.exec(t, b.vmid, "ping -c 1 -W 2 "+strings.TrimSuffix(legAddr, "/24")+" >/dev/null || true")
	s.waitTarget(t, tb, false, time.Minute, time.Second)
	summary, targets, ok := s.egress(t)
	require.True(t, ok && summary == filterOn, summary)
	require.Contains(t, targets, ta, "the other guest's service address left the set as well")
	s.requireAnswer(t, ta, a.body(8080), 10*time.Second)
	s.waitRoute(t, s.host("s19b"), planner.StateWithdrawn)
	s.setMAC(t, b.vmid, own)
	s.exec(t, b.vmid, "ping -c 1 -W 2 "+strings.TrimSuffix(legAddr, "/24")+" >/dev/null || true")
	s.sync(t)
	rb = s.waitVia(t, s.host("s19b"), engine.ViaAttach+secondBridge)
	require.Equal(t, tb, target(t, rb), "the route kept its service address while it was bound")
	s.waitTarget(t, tb, true, time.Minute, time.Second)

	t.Log("a Direct route beside the legs: its MAC change is seen within 2 seconds")
	d := s.in(guest{vmid: s19Dir, addr: "10.77.0.53/24", notes: "cf-tunnel: e2e-s19d.ZONE -> :8080"})
	s.create(t, d)
	s.waitRoute(t, s.host("s19d"), planner.StateActive)
	route, _ := routeOf(s.state(t), s.host("s19d"), "")
	require.Equal(t, engine.ViaDirect, route.Path.Via)
	s.requireMACMoveSeen(t, d, "02:e2:e0:00:93:53")
	s.waitRoute(t, s.host("s19d"), planner.StateActive)
}

func testNetworkReload(t *testing.T, s *suite) {
	a, ra := s.ensureS17(t)
	b, rb := s.ensureS19(t)
	targets := map[string]string{target(t, ra): a.body(8080), target(t, rb): b.body(8080)}
	st := s.state(t)
	marks := map[string]int{}
	for _, id := range []string{isolatedBridge, secondBridge} {
		n, _ := networkOf(st, id)
		marks[id] = n.Mark
	}
	for _, cmd := range [][]string{{"ifreload", "-a"}, {"pvesh", "set", "/cluster/sdn"}} {
		tr := startTraffic(t, targets)
		out, err := s.run(cmd[0], cmd[1:]...)
		t.Logf("%s:\n%s", strings.Join(cmd, " "), out)
		require.NoError(t, err)
		time.Sleep(5 * time.Second)
		total, failed, errs := tr.end()
		t.Logf("%s: %d requests, %d failed %v", strings.Join(cmd, " "), total, failed, errs)
		require.Zero(t, failed, "requests failed around %s", strings.Join(cmd, " "))
		for id, mark := range marks {
			link := s.must(t, "ip", "-o", "link", "show", "dev", "vethpco"+strconv.Itoa(mark))
			require.Contains(t, link, "master "+id, "the leg of %s left its bridge: %s", id, link)
		}
		s.requireNetnsKept(t)
		s.requireHostRoutes(t)
	}

	t.Log("the second network detached: its leg and service address go, the first keeps serving")
	sb := serviceOf(t, rb)
	s.detach(t, secondBridge)
	_, err := s.netns("ip", "link", "show", "dev", gateway.LegIface(marks[secondBridge]))
	require.Error(t, err, "the leg of %s is still there", secondBridge)
	require.NotContains(t, s.serviceAddrs(t), sb)
	s.waitTarget(t, target(t, rb), false, time.Minute, time.Second)
	s.waitState(t, "the route of "+s.host("s19b")+" no longer served", 2*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, s.host("s19b"), "")
		return ok && r.State != planner.StateActive
	})
	s.requireAnswer(t, target(t, ra), a.body(8080), 10*time.Second)
	s.requireNetnsKept(t)
	s.destroy(t, b.vmid)
	s.sync(t)
}

// deb is the package of the build under test, for the reinstall of S23:
// PCO_E2E_DEB, or the one pco_*_amd64.deb in the working directory.
func (s *suite) deb(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("PCO_E2E_DEB"); path != "" {
		abs, err := filepath.Abs(path)
		require.NoError(t, err)
		return abs
	}
	matches, _ := filepath.Glob("pco_*_amd64.deb")
	if len(matches) != 1 {
		return ""
	}
	abs, err := filepath.Abs(matches[0])
	require.NoError(t, err)
	return abs
}

func testGatewayRestart(t *testing.T, s *suite) {
	a, ra := s.ensureS17(t)
	m, rm := s.ensureManaged(t)
	targets := map[string]string{target(t, ra): a.body(8080), target(t, rm): m.body(8080)}
	s.settle(t)
	rules := s.ruleset(t)
	mappings := s.serviceAddrs(t)
	active := activeRoutes(s.state(t))

	under := func(what string, run func()) {
		t.Helper()
		tr := startTraffic(t, targets)
		run()
		time.Sleep(5 * time.Second)
		total, failed, errs := tr.end()
		t.Logf("%s: %d requests, %d failed %v", what, total, failed, errs)
		require.Zero(t, failed, "requests failed around %s", what)
		require.Equal(t, rules, s.ruleset(t), "the ruleset of the namespace changed with %s", what)
		s.requireHostRoutes(t)
		s.requireNetnsKept(t)
		require.Equal(t, mappings, s.serviceAddrs(t))
	}

	under("systemctl restart pco-netns", func() { s.must(t, "systemctl", "restart", "pco-netns") })
	s.waitState(t, "the routes still active", 2*time.Minute, func(st engine.State) bool { return slices.Equal(activeRoutes(st), active) })

	deb := s.deb(t)
	if deb == "" {
		t.Log("no package to reinstall: name it with PCO_E2E_DEB, or put the one pco_*_amd64.deb in the working directory")
	} else {
		from := 0
		if s.fake != nil {
			from = len(s.fake.f.Calls())
		}
		reinstalled := time.Now()
		under("reinstall of "+filepath.Base(deb), func() {
			out, err := s.run("env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "--reinstall", "-y", deb)
			t.Logf("apt-get install --reinstall:\n%s", out)
			require.NoError(t, err)
		})
		s.waitState(t, "a complete cycle of the reinstalled daemon", 3*time.Minute, func(st engine.State) bool {
			return st.At.After(reinstalled) && st.Complete && !st.FinishedAt.IsZero()
		})
		if s.fake != nil {
			require.Empty(t, s.fake.writes(from), "the reinstall wrote at Cloudflare")
		}
		s.waitState(t, "the routes still active", 2*time.Minute, func(st engine.State) bool { return slices.Equal(activeRoutes(st), active) })
		require.Equal(t, mappings, s.serviceAddrs(t))
	}
	t.Log("the reboot of the node is a step of the lab: README.md says what to check after it")
}
