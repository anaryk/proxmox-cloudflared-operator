//go:build e2e

package e2e

import (
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

// The guests of the managed network sit on pcoti, where the node has no
// address: only a card on pconet reaches them.
const (
	managedVMID   = 9361 // consents by tag
	autoVMID      = 9362 // consents by nothing: automatic consent attaches it
	cloneVMID     = 9363 // a clone of the managed guest, tagged
	untaggedClone = 9364
	dhcpVMID      = 9365 // QEMU, DHCP
	noHotplugVMID = 9366 // QEMU without network hotplug
	optOutVMID    = 9368 // the marker from the start

	markerLine = "\ncf-tunnel-managed=off"
)

func (s *suite) managedGuest() guest {
	return s.in(guest{vmid: managedVMID, bridge: managedBridge, addr: "10.90.0.61/24", tags: gateTag + ";" + consentTag,
		notes: "cf-tunnel: e2e-s21a.ZONE -> :8080"})
}

func (s *suite) autoGuest() guest {
	return s.in(guest{vmid: autoVMID, bridge: managedBridge, addr: "10.90.0.62/24", notes: "cf-tunnel: e2e-s21b.ZONE -> :8080"})
}

// ensureManaged enables the managed network and makes the consenting guest,
// unless the run has them, and returns it with its route served through
// its card.
func (s *suite) ensureManaged(t *testing.T) (guest, engine.RouteView) {
	t.Helper()
	s.enforce(t)
	s.bridgeUp(t, managedBridge, false)
	s.enableManaged(t)
	g := s.managedGuest()
	if !s.guests[g.vmid] {
		s.pconetGuests[g.vmid] = true
		s.create(t, g)
	}
	s.waitAttachment(t, g.owner(), engine.AttachmentAttached)
	return g, s.waitVia(t, s.host("s21a"), engine.ViaManaged)
}

// ensureAuto makes the guest automatic consent attaches, under that mode.
func (s *suite) ensureAuto(t *testing.T) (guest, engine.AttachmentView) {
	t.Helper()
	s.setConsent(t, store.ConsentAuto)
	g := s.autoGuest()
	if !s.guests[g.vmid] {
		s.pconetGuests[g.vmid] = true
		s.create(t, g)
	}
	return g, s.waitAttachment(t, g.owner(), engine.AttachmentAttached)
}

// cardInside waits until the guest sees the address of its card.
func (s *suite) cardInside(t testing.TB, vmid int, iface, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, _ := s.try(vmid, "ip -4 -o addr show dev "+iface)
		if strings.Contains(out, " "+addr+"/") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("guest %d does not have %s on %s:\n%s", vmid, addr, iface, out)
		}
		time.Sleep(time.Second)
	}
}

func testManagedNetwork(t *testing.T, s *suite) {
	s.enforce(t)
	s.bridgeUp(t, managedBridge, false)
	s.enableManaged(t)
	st := s.state(t)
	require.Equal(t, store.ConsentPerGuest, st.Managed.Consent)
	require.True(t, st.Managed.TagHonoured, "the consent tag is registered")
	require.True(t, st.Managed.Vnet.IsolatePorts, "the vnet of a host isolates its ports")
	require.Zero(t, st.Managed.Vnet.Subnets)
	leg := st.Managed.Leg.Addr
	t.Logf("the managed network: %s, the leg at %s, %d addresses free", st.Managed.Subnet, leg, st.Managed.Free)

	t.Log("a container that consents by tag gets a card and nothing else")
	g := s.managedGuest()
	s.pconetGuests[g.vmid] = true
	s.create(t, g)
	before := withoutCard(s.configLines(t, g.vmid))
	att := s.waitAttachment(t, g.owner(), engine.AttachmentAttached)
	require.Equal(t, "tag", att.Consent)
	card := s.managedCard(t, g.vmid)
	require.NotEmpty(t, card, "no card on pconet in the configuration of %d", g.vmid)
	t.Logf("the card: %s", card)
	require.True(t, strings.HasPrefix(card, "net"+strconv.Itoa(att.NIC)+":"), card)
	require.Equal(t, strings.ToLower(att.MAC), cardValue(card, "hwaddr"))
	require.Equal(t, att.Addr.String()+"/24", cardValue(card, "ip"))
	require.Equal(t, "0", cardValue(card, "firewall"))
	require.True(t, st.Managed.Subnet.Contains(att.Addr))
	require.Equal(t, before, withoutCard(s.configLines(t, g.vmid)), "the attach changed more than the card")
	iface := cardValue(card, "name")
	s.cardInside(t, g.vmid, iface, att.Addr.String())

	host := s.host("s21a")
	route := s.waitVia(t, host, engine.ViaManaged)
	require.Equal(t, "port", route.Level)
	tg := target(t, route)
	s.requireAnswer(t, tg, g.body(8080), time.Minute)
	s.requireNoTarget(t, att.Addr.String()+":8080")
	port := lanPort(g.vmid, att.NIC, false)
	require.Eventually(t, func() bool { return s.stickyPort(t, att.MAC) == port }, 30*time.Second, time.Second,
		"the sticky entry of %s is on %q, not on %s", att.MAC, s.stickyPort(t, att.MAC), port)
	require.True(t, s.guardLists(t, port, att.MAC, att.Addr), "the guard does not keep %s to %s", att.MAC, port)
	require.True(t, s.pinnedIn(t, att.Addr, att.MAC), "the namespace does not pin %s to %s", att.Addr, att.MAC)
	s.requireNetnsKept(t)

	t.Run("qemu with dhcp", func(t *testing.T) {
		image := skipWithoutImage(t)
		v := vm{vmid: dhcpVMID, addr: "10.77.0.65", tags: gateTag + ";" + consentTag, notes: s.in(guest{notes: "cf-tunnel: e2e-s21q.ZONE -> :8080 via=managed"}).notes}
		s.pconetGuests[v.vmid] = true
		s.createVM(t, v, image)
		s.serveVM(t, v, 8080)
		att := s.waitAttachment(t, v.owner(), engine.AttachmentAttached, engine.AttachmentAwaitingDHCP)
		line := s.qmConfig(t, v.vmid, "net"+strconv.Itoa(att.NIC))
		require.Contains(t, line, "bridge="+gateway.ManagedBridge, line)
		require.Contains(t, strings.ToLower(line), strings.ToLower(att.MAC), line)
		iface := s.ifaceOf(t, v, att.MAC)
		got := s.dhcp(t, v, iface)
		require.Equal(t, att.Addr.String(), got, "DHCP gave the machine another address than its lease")
		s.waitState(t, "the lease seen by DHCP", 2*time.Minute, func(st engine.State) bool {
			a, ok := attachmentOf(st, v.owner())
			return ok && a.State == engine.AttachmentAttached
		})
		st, err := store.Open(store.DefaultPaths())
		require.NoError(t, err)
		leases, err := st.Leases()
		require.NoError(t, err)
		lease, ok := leases[strings.ReplaceAll(strings.ToLower(att.MAC), ":", "-")]
		if !ok {
			for _, l := range leases {
				if strings.EqualFold(l.MAC, att.MAC) {
					lease, ok = l, true
				}
			}
		}
		require.True(t, ok, "no lease of %s", att.MAC)
		require.NotNil(t, lease.LastDHCP, "the lease does not record the request")
		route := s.waitVia(t, s.host("s21q"), engine.ViaManaged)
		s.requireAnswer(t, target(t, route), v.body(8080), time.Minute)

		t.Log("a renewal, unicast to the server, is answered unicast")
		answer := s.renew(t, v, iface, got, att.MAC, leg.String())
		t.Log(answer)
		require.Contains(t, answer, "answer from "+leg.String()+":67 ")
		require.Contains(t, answer, "yiaddr="+got+" ")
		require.Contains(t, answer, "type=5 ")
		require.Contains(t, answer, "xid=same")
	})

	t.Run("qemu without network hotplug", func(t *testing.T) {
		image := skipWithoutImage(t)
		v := vm{vmid: noHotplugVMID, addr: "10.77.0.66", tags: gateTag + ";" + consentTag, noHotplug: true,
			notes: s.in(guest{notes: "cf-tunnel: e2e-s21p.ZONE -> :8080 via=managed"}).notes}
		s.pconetGuests[v.vmid] = true
		s.createVM(t, v, image)
		att := s.waitAttachment(t, v.owner(), engine.AttachmentPendingReboot)
		t.Logf("%s: %s %s", v.owner(), att.State, att.Detail)
		pending := s.must(t, "qm", "pending", strconv.Itoa(v.vmid))
		require.Contains(t, pending, "net"+strconv.Itoa(att.NIC), pending)
		s.restartVM(t, v)
		att = s.waitAttachment(t, v.owner(), engine.AttachmentAttached, engine.AttachmentAwaitingDHCP)
		line := s.qmConfig(t, v.vmid, "net"+strconv.Itoa(att.NIC))
		require.Contains(t, line, "bridge="+gateway.ManagedBridge, line)
	})

	t.Run("a clone keeps the tag and gets a card of its own", func(t *testing.T) {
		cloned := time.Now()
		s.stop(t, g.vmid)
		s.guests[cloneVMID] = true
		s.pconetGuests[cloneVMID] = true
		s.must(t, "pct", "clone", strconv.Itoa(g.vmid), strconv.Itoa(cloneVMID), "--hostname", "e2e-"+strconv.Itoa(cloneVMID))
		s.start(t, g.vmid)
		s.must(t, "pct", "set", strconv.Itoa(cloneVMID), "--net0", "name=eth0,bridge="+managedBridge+",ip=10.90.0.63/24")
		// Proxmox gives the copied card a MAC of its own and keeps the
		// address of the original's lease: a card no attachment explains.
		copied := s.managedCard(t, cloneVMID)
		require.Equal(t, att.Addr.String()+"/24", cardValue(copied, "ip"), "the clone did not copy the card: %s", copied)
		t.Logf("the copied card: %s", copied)
		require.Contains(t, tagsOf(s.must(t, "pct", "config", strconv.Itoa(cloneVMID))), consentTag, "the clone did not inherit the tag")
		s.start(t, cloneVMID)

		own := s.waitAttachment(t, "lxc/"+strconv.Itoa(cloneVMID), engine.AttachmentAttached)
		require.NotEqual(t, cardValue(copied, "hwaddr"), strings.ToLower(own.MAC), "the clone kept the copied MAC")
		require.NotEqual(t, att.Addr, own.Addr, "the clone kept the copied address")
		card := s.managedCard(t, cloneVMID)
		require.Equal(t, strings.ToLower(own.MAC), cardValue(card, "hwaddr"), card)
		require.Equal(t, own.Addr.String()+"/24", cardValue(card, "ip"), card)
		s.waitEvent(t, cloned, "managed", "was replaced by the guest's own card")
		s.waitOwnerRoute(t, host, "lxc/"+strconv.Itoa(cloneVMID), planner.StateConflict)
		s.waitOwnerRoute(t, host, g.owner(), planner.StateActive)
		kept, ok := attachmentOf(s.state(t), g.owner())
		require.True(t, ok)
		require.Equal(t, att.MAC, kept.MAC)
		require.Equal(t, att.Addr, kept.Addr)
		s.requireAnswer(t, tg, g.body(8080), time.Minute)
	})

	t.Run("an untagged clone's copied card is set link down", func(t *testing.T) {
		s.stop(t, g.vmid)
		s.guests[untaggedClone] = true
		s.pconetGuests[untaggedClone] = true
		s.must(t, "pct", "clone", strconv.Itoa(g.vmid), strconv.Itoa(untaggedClone), "--hostname", "e2e-"+strconv.Itoa(untaggedClone))
		s.start(t, g.vmid)
		s.must(t, "pct", "set", strconv.Itoa(untaggedClone), "--net0", "name=eth0,bridge="+managedBridge+",ip=10.90.0.64/24", "--delete", "tags")
		copied := s.managedCard(t, untaggedClone)
		nic, err := strconv.Atoi(strings.TrimPrefix(strings.SplitN(copied, ":", 2)[0], "net"))
		require.NoError(t, err, copied)
		s.start(t, untaggedClone)
		var f engine.ForeignView
		s.waitState(t, "the copied card of "+strconv.Itoa(untaggedClone)+" dealt with", 3*time.Minute, func(st engine.State) bool {
			var ok bool
			f, ok = foreignOf(st, untaggedClone, nic)
			return ok && (f.Action == engine.ForeignLinkDown || f.Action == engine.ForeignRemoved)
		})
		t.Logf("foreign card net%d of lxc/%d: %s: %s", f.NIC, untaggedClone, f.Action, f.Why)
		card := s.managedCard(t, untaggedClone)
		if f.Action == engine.ForeignLinkDown {
			require.Equal(t, "1", cardValue(card, "link_down"), card)
		} else {
			require.Empty(t, card)
		}
		_, ok := attachmentOf(s.state(t), "lxc/"+strconv.Itoa(untaggedClone))
		require.False(t, ok, "an untagged clone got a card")
		s.waitOwnerRoute(t, host, g.owner(), planner.StateActive)
		s.destroy(t, untaggedClone)
		delete(s.pconetGuests, untaggedClone)
		s.sync(t)
	})

	t.Run("automatic consent and the opt-out marker", func(t *testing.T) {
		s.setConsent(t, store.ConsentAuto)
		st := s.state(t)
		require.True(t, st.Managed.AutoAllowed, "the gate tag is registered, so automatic consent is allowed")
		require.Empty(t, st.Managed.AutoStopped)

		a := s.autoGuest()
		s.pconetGuests[a.vmid] = true
		s.create(t, a)
		att := s.waitAttachment(t, a.owner(), engine.AttachmentAttached)
		require.Equal(t, "auto", att.Consent)
		route := s.waitVia(t, s.host("s21b"), engine.ViaManaged)
		s.requireAnswer(t, target(t, route), a.body(8080), time.Minute)

		off := s.in(guest{vmid: optOutVMID, bridge: managedBridge, addr: "10.90.0.68/24", notes: "cf-tunnel: e2e-s21off.ZONE -> :8080" + markerLine})
		s.pconetGuests[off.vmid] = true
		s.create(t, off)
		s.settle(t)
		for range 3 {
			s.sync(t)
			time.Sleep(pollInterval)
			st := s.state(t)
			_, ok := attachmentOf(st, off.owner())
			require.False(t, ok, "the guest with the marker got a card")
			require.Empty(t, s.managedCard(t, off.vmid))
		}
		s.destroy(t, off.vmid)
		delete(s.pconetGuests, off.vmid)

		t.Log("the marker on an attached guest starts the grace; taken away within it, the card stays")
		s.setNotes(t, a.vmid, a.notes+markerLine)
		det := s.waitAttachment(t, a.owner(), engine.AttachmentDetaching)
		require.NotNil(t, det.DetachAt)
		require.Contains(t, det.DetachWhy, "cf-tunnel-managed=off")
		s.setNotes(t, a.vmid, a.notes)
		back := s.waitAttachment(t, a.owner(), engine.AttachmentAttached)
		require.Nil(t, back.DetachAt)
		require.Equal(t, att.MAC, back.MAC)
		require.NotEmpty(t, s.managedCard(t, a.vmid))
	})

	t.Run("the registration cleared stops automatic attaches and detaches nothing", func(t *testing.T) {
		_, _ = s.ensureAuto(t)
		owners := []string{g.owner(), s.autoGuest().owner(), "lxc/" + strconv.Itoa(cloneVMID)}
		cards := map[string]string{}
		for _, o := range owners {
			if _, ok := attachmentOf(s.state(t), o); ok {
				cards[o] = s.managedCard(t, vmidOf(o))
			}
		}
		require.NotEmpty(t, cards)
		tags := s.registeredTags(t)
		since := time.Now()
		s.setRegisteredTags(t, nil)
		s.sync(t)
		stopped := s.waitState(t, "automatic attaches stopped", 2*time.Minute, func(st engine.State) bool { return st.Managed.AutoStopped != "" })
		t.Logf("%s", stopped.Managed.AutoStopped)
		for until := time.Now().Add(unregisteredFor); time.Now().Before(until); time.Sleep(10 * time.Second) {
			st := s.state(t)
			for o := range cards {
				a, ok := attachmentOf(st, o)
				require.True(t, ok, "%s lost its card while the registration was gone", o)
				require.Equal(t, engine.AttachmentAttached, a.State, "%s: %s %s", o, a.State, a.DetachWhy)
				require.Nil(t, a.DetachAt, "%s: a grace started: %s", o, a.DetachWhy)
				require.Equal(t, cards[o], s.managedCard(t, vmidOf(o)), "the card of %s changed", o)
			}
		}
		s.setRegisteredTags(t, tags)
		s.sync(t)
		s.waitState(t, "automatic attaches resumed", 2*time.Minute, func(st engine.State) bool { return st.Managed.AutoStopped == "" })
		s.waitEvent(t, since, "managed", "automatic attaches resumed")
	})

	t.Run("a restart of the daemon changes nothing in its first cycle", func(t *testing.T) {
		s.settle(t)
		b := s.takeBefore(t)
		cards := map[int]string{}
		for _, a := range s.state(t).Managed.Attachments {
			if strings.HasPrefix(a.Owner, "lxc/") {
				cards[vmidOf(a.Owner)] = s.managedCard(t, vmidOf(a.Owner))
			}
		}
		mappings := s.serviceAddrs(t)
		from := 0
		if s.fake != nil {
			from = len(s.fake.f.Calls())
		}
		restarted := time.Now()
		s.must(t, "systemctl", "restart", "pco")
		s.waitState(t, "the first cycle of the restarted daemon", 3*time.Minute, func(st engine.State) bool {
			return st.At.After(restarted) && st.Complete && !st.FinishedAt.IsZero()
		})
		for vmid, card := range cards {
			require.Equal(t, card, s.managedCard(t, vmid), "the card of %d changed", vmid)
		}
		require.Equal(t, mappings, s.serviceAddrs(t), "a service address changed")
		if s.fake != nil {
			require.Empty(t, s.fake.writes(from), "the first cycle after a restart wrote at Cloudflare")
		}
		s.requireSame(t, b)
	})
}

func testManagedIsolation(t *testing.T, s *suite) {
	m, rm := s.ensureManaged(t)
	a, aa := s.ensureAuto(t)
	am, ok := attachmentOf(s.state(t), m.owner())
	require.True(t, ok)
	leg := s.state(t).Managed.Leg.Addr.String()
	ifaceM := cardValue(s.managedCard(t, m.vmid), "name")
	ifaceA := cardValue(s.managedCard(t, a.vmid), "name")
	tm := target(t, rm)
	s.requireAnswer(t, tm, m.body(8080), time.Minute)

	t.Log("guest to guest: nothing")
	require.False(t, s.connects(m.vmid, aa.Addr.String(), "8080"), "%s reaches %s on the managed network", m.owner(), a.owner())
	_, reached := s.try(m.vmid, "ping -c 1 -W 1 "+aa.Addr.String())
	require.False(t, reached, "%s pings %s on the managed network", m.owner(), a.owner())

	t.Log("guest to the leg: nothing but what the leg serves")
	legInBefore := s.counter(t, true, "inet", "pco_gw", "dropped_leg_in")
	for _, port := range []string{"22", "8006", "8080"} {
		require.False(t, s.connects(m.vmid, leg, port), "%s reaches the leg's %s", m.owner(), port)
	}
	legInAfter := s.counter(t, true, "inet", "pco_gw", "dropped_leg_in")
	t.Logf("dropped_leg_in %d -> %d", legInBefore, legInAfter)
	require.Greater(t, legInAfter, legInBefore, "the leg did not drop what the guest sent it")

	t.Log("guest to node: nothing over IPv4, and the node is no router")
	node, gw := s.nodeLAN(t)
	fromBefore := s.counter(t, false, "inet", "pco_host", "from_pconet")
	routedBefore := s.counter(t, false, "inet", "pco_host", "routed_from_pconet")
	s.setSysctl(t, "net.ipv4.ip_forward", "1")
	s.exec(t, m.vmid, "ip route replace "+node+"/32 dev "+ifaceM)
	for _, port := range []string{"22", "8006"} {
		require.False(t, s.connects(m.vmid, node, port), "%s reaches the node's %s from the managed network", m.owner(), port)
	}
	neigh, _ := s.try(m.vmid, "ip neigh show "+node)
	t.Logf("the node's address as the guest sees it: %s", strings.TrimSpace(neigh))
	s.exec(t, m.vmid, "ip route replace default via "+node+" dev "+ifaceM)
	for _, dst := range [][2]string{{gw, "22"}, {gw, "443"}, {"1.1.1.1", "443"}} {
		require.False(t, s.connects(m.vmid, dst[0], dst[1]), "%s reaches %s:%s through the node", m.owner(), dst[0], dst[1])
	}
	_, reached = s.try(m.vmid, "ping -c 1 -W 1 "+gw)
	require.False(t, reached, "%s pings the LAN through the node", m.owner())
	s.exec(t, m.vmid, "ip route del default via "+node+" dev "+ifaceM+"; ip route del "+node+"/32 dev "+ifaceM)
	fromAfter := s.counter(t, false, "inet", "pco_host", "from_pconet")
	routedAfter := s.counter(t, false, "inet", "pco_host", "routed_from_pconet")
	t.Logf("from_pconet %d -> %d, routed_from_pconet %d -> %d", fromBefore, fromAfter, routedBefore, routedAfter)
	require.Greater(t, routedAfter, routedBefore, "the node did not drop what the guest sent it to forward")

	t.Log("no IPv6 on the managed bridge")
	require.Equal(t, "1", strings.TrimSpace(s.must(t, "sysctl", "-n", "net.ipv6.conf."+gateway.ManagedBridge+".disable_ipv6")))
	require.Empty(t, strings.TrimSpace(s.must(t, "ip", "-6", "-o", "addr", "show", "dev", gateway.ManagedBridge)), "the managed bridge has an IPv6 address")
	out, _ := s.try(m.vmid, "ping6 -c 1 -W 1 ff02::1%"+ifaceM+" 2>&1 | tail -2")
	t.Logf("all-nodes multicast from the guest: %s", strings.TrimSpace(out))

	t.Log("a host that forwards for other networks gets nothing into the transit link")
	d := s.in(guest{vmid: s19Dir, addr: "10.77.0.53/24", notes: "cf-tunnel: e2e-s19d.ZONE -> :8080"})
	if !s.guests[d.vmid] {
		s.create(t, d)
	}
	toBefore := s.counter(t, false, "inet", "pco_host", "to_transit")
	s.exec(t, d.vmid, "ip route replace "+gateway.ServicePrefix.String()+" via "+nodeAddr)
	svc, svcPort, _ := strings.Cut(tm, ":")
	require.False(t, s.connects(d.vmid, svc, svcPort), "a guest reaches the service address %s through the node", tm)
	s.exec(t, d.vmid, "ip route del "+gateway.ServicePrefix.String()+" via "+nodeAddr)
	toAfter := s.counter(t, false, "inet", "pco_host", "to_transit")
	t.Logf("to_transit %d -> %d", toBefore, toAfter)
	require.Greater(t, toAfter, toBefore, "the node did not drop what was forwarded into the transit link")

	t.Log("a forged address, then a forged MAC: nothing, and the owner keeps serving")
	spoofed0 := s.counter(t, false, "bridge", "pco_guard", "dropped_spoofed")
	s.exec(t, a.vmid, "ip addr add "+am.Addr.String()+"/24 dev "+ifaceA)
	_, reached = s.try(a.vmid, "ping -c 1 -W 1 -I "+am.Addr.String()+" "+leg)
	require.False(t, reached, "a forged address reaches the leg")
	spoofed1 := s.counter(t, false, "bridge", "pco_guard", "dropped_spoofed")
	require.Greater(t, spoofed1, spoofed0, "the guard did not count the forged address")
	s.exec(t, a.vmid, "ip addr del "+am.Addr.String()+"/24 dev "+ifaceA+" && ip link set dev "+ifaceA+" down && ip link set dev "+ifaceA+" address "+am.MAC+
		" && ip link set dev "+ifaceA+" up && ip addr add "+am.Addr.String()+"/24 dev "+ifaceA+" && ip neigh flush dev "+ifaceA)
	_, reached = s.try(a.vmid, "ping -c 1 -W 1 -I "+am.Addr.String()+" "+leg)
	require.False(t, reached, "a forged MAC and address reach the leg")
	spoofed2 := s.counter(t, false, "bridge", "pco_guard", "dropped_spoofed")
	require.Greater(t, spoofed2, spoofed1, "the guard did not count the forged MAC")
	require.Equal(t, lanPort(m.vmid, am.NIC, false), s.stickyPort(t, am.MAC), "the sticky entry of the owner moved")
	s.requireAnswer(t, tm, m.body(8080), 10*time.Second)
	s.exec(t, a.vmid, "ip addr del "+am.Addr.String()+"/24 dev "+ifaceA+" && ip link set dev "+ifaceA+" down && ip link set dev "+ifaceA+" address "+aa.MAC+
		" && ip link set dev "+ifaceA+" up && ip addr replace "+aa.Addr.String()+"/24 dev "+ifaceA+" && ip neigh flush dev "+ifaceA)
	require.Eventually(t, func() bool {
		_, ok := s.try(a.vmid, "ping -c 1 -W 1 "+leg)
		return ok
	}, 30*time.Second, time.Second, "%s does not reach the leg again with its own card", a.owner())
	t.Logf("dropped_spoofed %d -> %d -> %d", spoofed0, spoofed1, spoofed2)

	t.Log("a card put on pconet by hand is seen within a cycle and set link down")
	const foreignMAC = "12:34:56:78:9a:bc"
	since := time.Now()
	s.must(t, "pct", "set", strconv.Itoa(a.vmid), "--net2", "name=eth2,bridge="+gateway.ManagedBridge+",hwaddr="+foreignMAC)
	s.exec(t, a.vmid, "ip link set eth2 up && ip addr add 10.203.118.240/24 dev eth2 && (arping -c 2 -I eth2 "+leg+" >/dev/null 2>&1; ping -c 1 -W 1 "+leg+" >/dev/null 2>&1; true)")
	var f engine.ForeignView
	s.waitState(t, "the card by hand dealt with", 2*time.Minute, func(st engine.State) bool {
		var ok bool
		f, ok = foreignOf(st, a.vmid, 2)
		return ok && (f.Action == engine.ForeignLinkDown || f.Action == engine.ForeignRemoved)
	})
	t.Logf("foreign card net2 of %s: %s: %s", a.owner(), f.Action, f.Why)
	// On a host the guard of the bridge drops the card's frames before the
	// watch on the leg sees them: the card is found in the configuration.
	// Where the watch did see it, say so.
	for _, ev := range s.events(t, since) {
		if ev.Kind == "managed" && strings.Contains(ev.Message, "no lease knows") {
			t.Logf("the watch saw it too: %s", ev.Message)
		}
	}
	if f.Action == engine.ForeignLinkDown {
		line := ""
		for _, l := range s.configLines(t, a.vmid) {
			if strings.HasPrefix(l, "net2:") {
				line = l
			}
		}
		require.Equal(t, "1", cardValue(line, "link_down"), line)
	}
	_, _ = s.run("pct", "set", strconv.Itoa(a.vmid), "--delete", "net2")
	s.sync(t)
	s.waitState(t, "the card by hand gone", 2*time.Minute, func(st engine.State) bool {
		_, ok := foreignOf(st, a.vmid, 2)
		return !ok
	})
}
