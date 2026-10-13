//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"net"
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

// The networking scenarios in the appliance: a leg is a card of the
// container, which the node grants it the network for, and the managed
// network's isolation is the guest firewall of the node, which pco proves as
// the level filtered. The connectors dial from inside the appliance, so the
// service addresses are asked from there.

// fetchHere makes one request to a target from where pco runs, as a
// connector does: from the node, or with bash in the appliance, which has
// neither curl nor wget.
func (s *suite) fetchHere(target string) (string, error) {
	if s.app == nil {
		return fetch(target)
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return "", err
	}
	script := fmt.Sprintf(`exec 3<>/dev/tcp/%s/%s && printf 'GET / HTTP/1.0\r\nHost: %s\r\n\r\n' >&3 && cat <&3`, host, port, host)
	out, err := s.here("timeout", "5", "bash", "-c", script)
	if err != nil {
		return "", err
	}
	head, body, ok := strings.Cut(out, "\r\n\r\n")
	switch {
	case !ok:
		return "", fmt.Errorf("%s answered no HTTP: %q", target, out)
	case !strings.HasPrefix(head, "HTTP/1.1 200 ") && !strings.HasPrefix(head, "HTTP/1.0 200 "):
		return "", errors.New(target + " answered " + strings.SplitN(head, "\r\n", 2)[0])
	}
	return strings.TrimSpace(body), nil
}

// requireAnswerHere fetches a target from where pco runs until it answers
// with body, for at most timeout.
func (s *suite) requireAnswerHere(t testing.TB, target, body string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		got, err := s.fetchHere(target)
		if err == nil && got == body {
			return
		}
		last = got
		if err != nil {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer %q within %s; the last answer: %s", target, body, timeout, last)
		}
		time.Sleep(time.Second)
	}
}

// allowNetwork grants the appliance a bridge on the node, as an admin does
// before the appliance can attach a card to it.
func (s *suite) allowNetwork(t testing.TB, bridge string) {
	t.Helper()
	out, err := s.nodePcoRun(commandTimeout, "appliance", "network", "allow", bridge, "--vmid", strconv.Itoa(s.app.vmid), "--yes")
	t.Logf("pco appliance network allow %s:\n%s", bridge, out)
	require.NoError(t, err)
}

// legCard is the line of the appliance's configuration of its card on a
// bridge, "" when it has none.
func (s *suite) legCard(t testing.TB, on string) string {
	t.Helper()
	for k, v := range s.pctConfig(t, s.app.vmid) {
		if strings.HasPrefix(k, "net") && slices.Contains(strings.Split(v, ","), "bridge="+on) {
			return k + ": " + v
		}
	}
	return ""
}

// ensureApplianceAttached makes a guest on a runtime bridge and attaches its
// network in the appliance, unless the run has them, and returns the route of
// host served through the leg.
func (s *suite) ensureApplianceAttached(t *testing.T, g guest, host string) engine.RouteView {
	t.Helper()
	s.bridgeUp(t, g.bridge, false)
	if !s.guests[g.vmid] {
		s.create(t, g)
	}
	s.enforce(t)
	if _, ok := networkOf(s.state(t), g.bridge); !ok {
		s.allowNetwork(t, g.bridge)
		s.attach(t, g.bridge, 0, legAddr)
	}
	return s.waitVia(t, host, engine.ViaAttach+g.bridge)
}

func (s *suite) applianceS17Guest() guest {
	return s.in(guest{vmid: s.lay.netVMID(s17VMID), bridge: isolatedBridge, addr: sameAddr, notes: "cf-tunnel: e2e-s17.ZONE -> :8080"})
}

func (s *suite) applianceS19Guest() guest {
	return s.in(guest{vmid: s.lay.netVMID(s19VMID), bridge: secondBridge, addr: sameAddr, notes: "cf-tunnel: e2e-s19b.ZONE -> :8080"})
}

func testApplianceAttach(t *testing.T, s *suite) {
	s.ensureS1(t)
	s.bridgeUp(t, isolatedBridge, false)
	g := s.applianceS17Guest()
	s.create(t, g)
	host := s.host("s17")
	held := s.waitState(t, "the route of "+host+" without a way to its guest", 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, "")
		return ok && r.State != planner.StateActive
	})
	r, _ := routeOf(held, host, "")
	t.Logf("before the attach: %s (%s)", r.State, r.Reason)
	real := g.ip() + ":8080"
	s.requireNoTarget(t, real)

	s.allowNetwork(t, isolatedBridge)
	attached := time.Now()
	n := s.attach(t, isolatedBridge, 0, legAddr)
	require.Equal(t, store.NetworkAttach, n.Kind)
	card := s.legCard(t, isolatedBridge)
	t.Logf("the leg's card: %s", card)
	require.NotEmpty(t, card, "the appliance has no card on %s", isolatedBridge)
	require.Contains(t, card, "firewall=0")

	route := s.waitVia(t, host, engine.ViaAttach+isolatedBridge)
	t.Logf("%s is served at the level %s", host, route.Level)
	service := serviceOf(t, route)
	tg := target(t, route)
	s.waitTarget(t, tg, true, time.Minute, time.Second)
	s.requireNoTarget(t, real)
	s.waitIngress(t, s.tunnel(t), host, "http://"+tg)
	s.requireAnswerHere(t, tg, g.body(8080), time.Minute)
	s.waitEvent(t, attached, "network", "service address "+service.String()+" allocated")

	t.Log("the route withdrawn: the service address leaves the set and is released; the real address is never in it")
	withdrawn := time.Now()
	s.must(t, "pct", "set", strconv.Itoa(g.vmid), "--delete", "description")
	s.sync(t)
	s.waitGone(t, host)
	s.waitTarget(t, tg, false, 2*time.Minute, time.Second)
	s.requireNoTarget(t, real)
	s.waitEvent(t, withdrawn, "network", "service address "+service.String()+" of "+g.ip()+" on network "+isolatedBridge+" released")

	t.Log("the route back: another service address, never the released one again")
	s.setNotes(t, g.vmid, g.notes)
	route = s.waitVia(t, host, engine.ViaAttach+isolatedBridge)
	again := serviceOf(t, route)
	require.Equal(t, 1, again.Compare(service), "the pool gave %s again, or went backwards: %s", service, again)
	s.requireAnswerHere(t, target(t, route), g.body(8080), time.Minute)
	s.requireNoTarget(t, real)
}

// answersHere asks a target n times from where pco runs and counts the
// answers with body, with another body, and the failures.
func (s *suite) answersHere(target, body string, n int) (ok, crossed, failed int) {
	for range n {
		got, err := s.fetchHere(target)
		switch {
		case err != nil:
			failed++
		case got == body:
			ok++
		default:
			crossed++
		}
	}
	return ok, crossed, failed
}

func testApplianceIdenticalSubnets(t *testing.T, s *suite) {
	a := s.applianceS17Guest()
	b := s.applianceS19Guest()
	ra := s.ensureApplianceAttached(t, a, s.host("s17"))
	rb := s.ensureApplianceAttached(t, b, s.host("s19b"))
	ta, tb := target(t, ra), target(t, rb)
	require.NotEqual(t, ta, tb, "two networks, two service addresses")
	t.Logf("%s at %s, %s at %s, both for %s", s.host("s17"), ta, s.host("s19b"), tb, a.ip())

	t.Log("each hostname answered by its own guest")
	const n = 50
	okA, crossedA, failedA := s.answersHere(ta, a.body(8080), n)
	okB, crossedB, failedB := s.answersHere(tb, b.body(8080), n)
	t.Logf("%s: %d answered, %d crossed, %d failed; %s: %d answered, %d crossed, %d failed", ta, okA, crossedA, failedA, tb, okB, crossedB, failedB)
	require.Zero(t, crossedA+crossedB, "answers crossed between the guests")
	require.Zero(t, failedA+failedB, "requests failed")

	t.Log("one guest's MAC changes: only its service address leaves the set, the other keeps serving")
	own := s.hwaddr(t, b.vmid)
	moved := fmt.Sprintf("02:e2:e0:00:%02d:%02d", b.vmid/100%100, b.vmid%100)
	leg := strings.TrimSuffix(legAddr, "/24")
	s.setMAC(t, b.vmid, moved)
	s.exec(t, b.vmid, "ping -c 1 -W 2 "+leg+" >/dev/null || true")
	s.waitTarget(t, tb, false, time.Minute, time.Second)
	summary, targets, ok := s.egress(t)
	require.True(t, ok && summary == filterOn, summary)
	require.Contains(t, targets, ta, "the other guest's service address left the set as well")
	s.requireAnswerHere(t, ta, a.body(8080), 10*time.Second)
	s.waitRoute(t, s.host("s19b"), planner.StateWithdrawn)
	s.setMAC(t, b.vmid, own)
	s.exec(t, b.vmid, "ping -c 1 -W 2 "+leg+" >/dev/null || true")
	s.sync(t)
	rb = s.waitVia(t, s.host("s19b"), engine.ViaAttach+secondBridge)
	require.Equal(t, tb, target(t, rb), "the route kept its service address while it was bound")
	s.waitTarget(t, tb, true, time.Minute, time.Second)
}

// The guests of the managed network in the appliance: one that consents by
// tag on a bridge pco does not reach, and one as the GUI makes it, with the
// firewall of its card on and its own firewall off, on the bridge of the run.
func (s *suite) applianceManagedGuest() guest {
	return s.in(guest{vmid: s.lay.netVMID(managedVMID), bridge: managedBridge, addr: "10.90.0.61/24", tags: gateTag + ";" + consentTag,
		notes: "cf-tunnel: e2e-s21a.ZONE -> :8080"})
}

func (s *suite) applianceGUIGuest() guest {
	n := s.lay.netVMID(autoVMID) - s.lay.first
	g := s.g(n, guest{tags: gateTag + ";" + consentTag, firewall: true, notes: "cf-tunnel: e2e-s21g.ZONE -> :8080"})
	return g
}

// ensureApplianceManaged makes the managed network of the appliance as an
// admin does: the node's firewall as the guest firewall needs it, pco
// appliance managed enable on the node, then the network taken into the
// store inside, with the grace of the run.
func (s *suite) ensureApplianceManaged(t *testing.T) {
	t.Helper()
	if s.managed {
		return
	}
	s.enforce(t)
	s.bridgeUp(t, managedBridge, false)
	s.requireFirewallService(t)
	s.recordFirewall(t)
	node := "/nodes/" + s.app.node + "/firewall/options"
	s.must(t, "pvesh", "set", node, "--enable", "0", "--nftables", "0")
	s.must(t, "pvesh", "set", "/cluster/firewall/options", "--enable", "1")
	s.registerTag(t, consentTag)
	marker(t, managedMarker, "")
	s.managed = true
	out, err := s.nodePcoRun(installTimeout, "appliance", "managed", "enable", "--vmid", strconv.Itoa(s.app.vmid), "--yes")
	t.Logf("pco appliance managed enable:\n%s", out)
	require.NoError(t, err)
	s.editSettings(t, func(v *store.Settings) { v.Managed.DetachGrace = store.Duration(detachGrace) })
	out, err = s.pcoCombined("network", "managed", "enable", "--yes")
	t.Logf("pco network managed enable:\n%s", out)
	require.NoError(t, err)
	s.sync(t)
	s.waitState(t, "the managed network with its leg up and DHCP listening", 3*time.Minute, func(st engine.State) bool {
		m := st.Managed
		return m.Enabled && m.Leg != nil && m.Leg.Up && m.DHCP != nil && m.DHCP.Bound && m.Vnet.OK
	})
}

// grantCommand finds the pveum line pco asks for a guest's card, in its
// attachment or among the guests that wait.
func grantCommand(st engine.State, owner string) string {
	if a, ok := attachmentOf(st, owner); ok && a.GrantCmd != "" {
		return a.GrantCmd
	}
	for _, w := range st.Managed.Waiting {
		if w.String() == owner {
			if i := strings.Index(w.Why, "pveum acl modify "); i >= 0 {
				return w.Why[i:]
			}
		}
	}
	return ""
}

// runGrant runs a line pco printed, pveum acl modify <path> --users <user>
// --tokens '<token>' --roles <role>, and records what it added.
func (s *suite) runGrant(t testing.TB, line string) {
	t.Helper()
	f := strings.Fields(strings.ReplaceAll(line, "'", ""))
	require.GreaterOrEqual(t, len(f), 7, line)
	require.Equal(t, []string{"pveum", "acl", "modify"}, f[:3], line)
	path, role := f[3], ""
	var who [][2]string
	for i := 4; i+1 < len(f); i += 2 {
		switch f[i] {
		case "--roles":
			role = f[i+1]
		case "--users", "--tokens":
			who = append(who, [2]string{f[i], f[i+1]})
		}
	}
	require.NotEmpty(t, role, line)
	for _, w := range who {
		s.grant(t, path, w[0], w[1], role)
	}
}

// waitManagedAttached waits until the guest has its card, granting it what
// pco asks for on the way.
func (s *suite) waitManagedAttached(t testing.TB, owner string) engine.AttachmentView {
	t.Helper()
	granted := false
	var found engine.AttachmentView
	s.waitState(t, "the card of "+owner, 4*time.Minute, func(st engine.State) bool {
		if line := grantCommand(st, owner); line != "" && !granted {
			t.Logf("pco asks: %s", line)
			s.runGrant(t, line)
			granted = true
		}
		a, ok := attachmentOf(st, owner)
		found = a
		return ok && a.State == engine.AttachmentAttached
	})
	return found
}

// waitFiltered waits until the route of host is active at the level
// filtered.
func (s *suite) waitFiltered(t testing.TB, host string, timeout time.Duration) engine.RouteView {
	t.Helper()
	var found engine.RouteView
	s.waitState(t, "route "+host+" active at filtered", timeout, func(st engine.State) bool {
		r, ok := routeOf(st, host, "")
		found = r
		return ok && r.State == planner.StateActive && r.Level == "filtered"
	})
	return found
}

func testApplianceManaged(t *testing.T, s *suite) {
	s.ensureApplianceManaged(t)
	st := s.state(t)
	t.Logf("the managed network: %s, the leg at %s, filtered %+v", st.Managed.Subnet, st.Managed.Leg.Addr, st.Managed.Filtered)

	t.Log("a container that consents by tag gets a card, once the token may change it")
	g := s.applianceManagedGuest()
	s.pconetGuests[g.vmid] = true
	s.create(t, g)
	att := s.waitManagedAttached(t, g.owner())
	card := s.managedCard(t, g.vmid)
	t.Logf("the card: %s", card)
	require.Equal(t, strings.ToLower(att.MAC), cardValue(card, "hwaddr"))
	require.Equal(t, att.Addr.String()+"/24", cardValue(card, "ip"))
	require.Equal(t, "1", cardValue(card, "firewall"), "in the appliance the guest firewall isolates the card")
	s.cardInside(t, g.vmid, cardValue(card, "name"), att.Addr.String())
	host := s.host("s21a")
	route := s.waitFiltered(t, host, 3*time.Minute)
	require.Equal(t, engine.ViaManaged, route.Path.Via)
	s.requireAnswerHere(t, target(t, route), g.body(8080), time.Minute)
	s.requireNoTarget(t, att.Addr.String()+":8080")

	t.Log("the node cannot isolate the guests for a while: refused, nothing detached, the routes at observed")
	node := "/nodes/" + s.app.node + "/firewall/options"
	for _, r := range []struct {
		what      string
		why       string
		set, back []string
	}{
		{"the nftables backend", "nftables: 1", []string{"pvesh", "set", node, "--nftables", "1"}, []string{"pvesh", "set", node, "--nftables", "0"}},
		{"the datacenter firewall off", "the datacenter firewall is off",
			[]string{"pvesh", "set", "/cluster/firewall/options", "--enable", "0"}, []string{"pvesh", "set", "/cluster/firewall/options", "--enable", "1"}},
		{"pve-firewall stopped", "pve-firewall is", []string{"systemctl", "stop", "pve-firewall"}, []string{"systemctl", "start", "pve-firewall"}},
	} {
		t.Log(r.what)
		s.must(t, r.set[0], r.set[1:]...)
		st := s.waitState(t, "the refusal for "+r.what, 2*time.Minute, func(st engine.State) bool {
			return strings.Contains(st.Managed.Refused, r.why)
		})
		t.Logf("refused: %s", st.Managed.Refused)
		a, ok := attachmentOf(st, g.owner())
		require.True(t, ok, "the card was detached")
		require.Equal(t, att.MAC, a.MAC)
		require.Equal(t, card, s.managedCard(t, g.vmid), "the card changed")
		s.waitState(t, "the route below filtered", 2*time.Minute, func(st engine.State) bool {
			rt, ok := routeOf(st, host, "")
			return ok && rt.Level != "filtered"
		})
		rt, _ := routeOf(s.state(t), host, "")
		t.Logf("with %s: %s at %s", r.what, rt.State, rt.Level)
		if rt.State == planner.StateActive {
			require.Equal(t, "observed", rt.Level)
		}
		s.must(t, r.back[0], r.back[1:]...)
		s.waitState(t, "the refusal gone", 2*time.Minute, func(st engine.State) bool { return st.Managed.Refused == "" })
		s.waitFiltered(t, host, 3*time.Minute)
	}

	t.Log("a guest as the GUI makes it waits for the confirmation of the firewall impact")
	gui := s.applianceGUIGuest()
	s.pconetGuests[gui.vmid] = true
	s.create(t, gui)
	addr := gui.ip() + ":8080"
	s.requireAnswer(t, addr, gui.body(8080), time.Minute)
	granted := false
	s.waitState(t, gui.owner()+" waiting for the confirmation", 4*time.Minute, func(st engine.State) bool {
		if line := grantCommand(st, gui.owner()); line != "" && !granted {
			s.runGrant(t, line)
			granted = true
		}
		return slices.ContainsFunc(st.Managed.Waiting, func(w engine.ManagedWaiting) bool {
			return w.String() == gui.owner() && strings.HasPrefix(w.Why, "needs-firewall-confirmation")
		})
	})
	s.editSettings(t, func(v *store.Settings) { v.Managed.AcceptFirewallImpact = true })
	s.sync(t)
	s.waitManagedAttached(t, gui.owner())
	s.requireAnswer(t, addr, gui.body(8080), time.Minute)
}

func testApplianceIsolation(t *testing.T, s *suite) {
	if !s.managed {
		t.Skip("the managed network of the appliance is not enabled: S21 in the appliance did not run")
	}
	m, gui := s.applianceManagedGuest(), s.applianceGUIGuest()
	st := s.state(t)
	am, ok := attachmentOf(st, m.owner())
	require.True(t, ok)
	ag, ok := attachmentOf(st, gui.owner())
	require.True(t, ok)

	t.Log("the node's guest firewall carries pco's rules for the card")
	rules := s.must(t, "iptables-save")
	chain := fmt.Sprintf("veth%di%d-IN", m.vmid, am.NIC)
	var lines []string
	for line := range strings.Lines(rules) {
		if strings.HasPrefix(line, "-A "+chain+" ") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	t.Logf("%s:\n%s", chain, strings.Join(lines, "\n"))
	require.True(t, slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "pco") }), "no rule of pco in %s", chain)

	t.Log("guest to guest: nothing; the GUI guest's own card still answers")
	require.False(t, s.connects(m.vmid, ag.Addr.String(), "8080"), "%s reaches %s on the managed network", m.owner(), gui.owner())
	_, reached := s.try(m.vmid, "ping -c 1 -W 1 "+ag.Addr.String())
	require.False(t, reached, "%s pings %s on the managed network", m.owner(), gui.owner())
	s.requireAnswer(t, gui.ip()+":8080", gui.body(8080), 10*time.Second)

	t.Log("a card put on pconet by hand drops the level to observed and is reported")
	host := s.host("s21a")
	s.waitFiltered(t, host, time.Minute)
	const foreignMAC = "12:34:56:78:9a:bc"
	added := time.Now()
	s.must(t, "pct", "set", strconv.Itoa(gui.vmid), "--net2", "name=eth2,bridge="+gateway.ManagedBridge+",hwaddr="+foreignMAC)
	leg := st.Managed.Leg.Addr.String()
	s.exec(t, gui.vmid, "ip link set eth2 up && ip addr add 10.203.118.240/24 dev eth2 && (ping -c 1 -W 1 "+leg+" >/dev/null 2>&1; true)")
	s.waitState(t, "the level dropped and the card reported", twoCycles+time.Minute, func(st engine.State) bool {
		rt, ok := routeOf(st, host, "")
		_, foreign := foreignOf(st, gui.vmid, 2)
		return ok && rt.Level != "filtered" && foreign
	})
	t.Logf("the level dropped %s after the card was added", time.Since(added).Round(time.Second))
	_, _ = s.run("pct", "set", strconv.Itoa(gui.vmid), "--delete", "net2")
	s.sync(t)
	s.waitFiltered(t, host, 3*time.Minute)
}
