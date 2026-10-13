package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var web101 = model.GuestRef{Kind: model.KindLXC, VMID: 101}

func (m *managedApplianceEnv) installID() string {
	m.t.Helper()
	in, _, err := m.store.Install()
	require.NoError(m.t, err)
	return in.ID
}

// onCard is g with its card on the managed network as the appliance keeps
// it once its link is up: net1 with the lease's MAC and address, and
// firewall=1.
func onCard(g model.Guest) model.Guest {
	g = withCard(g, leaseMAC2, "10.203.118.2")
	g.NICs[len(g.NICs)-1].Firewall = true
	return g
}

// downCard is g with its card as the appliance adds it: link down.
func downCard(g model.Guest) model.Guest {
	g = onCard(g)
	g.NICs[len(g.NICs)-1].LinkDown = true
	return g
}

// attached runs the appliance until lxc/101 has its card, added with its
// link down and brought up once its firewall is enforced, and the inventory
// lists it; www.example.com answers through the managed leg at observed, as
// the appliance's resolver proves it.
func attached(t *testing.T) *managedApplianceEnv {
	t.Helper()
	m := newManagedApplianceEnv(t)
	m.res.mu.Lock()
	m.res.levels = map[string]resolve.Level{"www.example.com": resolve.LevelObserved}
	m.res.vias = map[string]string{"www.example.com": gateway.ManagedBridge}
	m.res.addrs = map[string]netip.Addr{"www.example.com": netip.MustParseAddr("10.203.118.2")}
	m.res.mu.Unlock()
	m.warm()
	st := m.next()
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	m.inv.set(snapshot(downCard(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	m.next()
	require.Contains(t, m.cfg.changes(), "link-up lxc/101 net1")
	m.inv.set(snapshot(onCard(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	return m
}

func TestANewCardComesUpOnlyOnceItsFirewallIsEnforced(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.warm()
	st := m.cycle()
	require.Equal(t, []string{"add lxc/101 net1 name=,bridge=pconet,hwaddr=" + leaseMAC2 + ",ip=10.203.118.2/24,firewall=1,link_down=1"}, m.cfg.changes())
	require.False(t, st.Managed.Filtered.Holds)
	require.Contains(t, st.Managed.Filtered.Why, "guest lxc/101: net1 is not yet enforced: pco wrote its firewall 0s ago")

	m.inv.set(snapshot(downCard(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	m.clock.advance(10 * time.Second)
	m.cycle()
	require.NotContains(t, m.cfg.changes(), "link-up lxc/101 net1", "10 s after the write: not yet")

	m.clock.advance(6 * time.Second)
	st = m.cycle()
	require.Contains(t, m.cfg.changes(), "link-up lxc/101 net1")
	require.Contains(t, m.managedEvents(), "the link of net1 of lxc/101 (web-1) on the managed network is up: its firewall is enforced")
	require.True(t, st.Managed.Filtered.Holds, st.Managed.Filtered.Why)

	t.Run("after a restart the time of the write is not known, and it waits again", func(t *testing.T) {
		n := len(m.cfg.changes())
		m.restart(incA)
		m.cycle()
		m.clock.advance(enforceAfter - time.Second)
		m.cycle()
		require.NotContains(t, m.cfg.changes()[n:], "link-up lxc/101 net1")
		m.clock.advance(time.Second)
		m.cycle()
		require.Contains(t, m.cfg.changes()[n:], "link-up lxc/101 net1")
	})
}

func TestACardIsNotBroughtUpWhileTheCycleMayNotWrite(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.warm()
	m.next()
	m.inv.set(snapshot(downCard(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	require.NoError(t, m.store.SaveWriter(planner.Writer{InstallID: "other", Generation: 1, Nonce: "n1"}))
	m.next()
	m.next()
	require.NotContains(t, m.cfg.changes(), "link-up lxc/101 net1")
}

// An attachment made before pco wrote the firewall gets it, as an attach
// does.
func TestAnAttachmentWithoutItsFirewallGetsIt(t *testing.T) {
	m := attached(t)
	att := m.attachments()["lxc/101"]
	att.Firewall = nil
	require.NoError(t, m.store.SaveAttachment(att))
	m.fw.set(web101, func(g *guestFirewall) { *g = guestFirewall{entries: map[string][]pve.IPSetEntry{}} })

	m.next()

	require.Equal(t, &store.FirewallBefore{}, m.attachments()["lxc/101"].Firewall)
	fw := m.fw.get(web101)
	require.True(t, fw.opts.Enabled)
	require.Len(t, fw.rules, 6)
}

// A card whose removal waits for the guest's restart is still on the vnet:
// its firewall is read, and filtered judges it.
func TestACardWhoseRemovalWaitsIsStillRead(t *testing.T) {
	m := attached(t)
	m.next()
	m.next()
	att := m.attachments()["lxc/101"]
	now := m.clock.now()
	att.DetachAt, att.RemovedAt, att.DetachWhy = &now, &now, "its consent ended"
	require.NoError(t, m.store.SaveAttachment(att))
	m.inv.set(snapshot(unconsented(onCard(managedGuest(101, "web-1", "www.example.com -> :8080"))), ownGuest(ownMAC)))
	m.fw.set(web101, func(g *guestFirewall) { g.opts.MACFilterOff = true })

	st := m.next()

	require.Contains(t, st.Managed.Filtered.Why, "guest lxc/101: macfilter is off")
	require.NotContains(t, strings.Join(st.Managed.Filtered.Why, "\n"), "did not read it")
}

func TestTheApplianceWritesTheGuestFirewallBeforeTheCard(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.warm()
	m.next()

	calls := m.log.all()
	card := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "configurer add lxc/101 net1") })
	require.Positive(t, card)
	var before []string
	for _, c := range calls[:card] {
		if strings.HasPrefix(c, "firewall lxc/101") {
			before = append(before, c)
		}
	}
	require.Equal(t, []string{
		"firewall lxc/101 create ipfilter-net1",
		"firewall lxc/101 add ipfilter-net1 10.203.118.2",
		"firewall lxc/101 add rule", "firewall lxc/101 add rule", "firewall lxc/101 add rule",
		"firewall lxc/101 add rule", "firewall lxc/101 add rule", "firewall lxc/101 add rule",
		"firewall lxc/101 options",
	}, before, "the ipset, the six rules and the firewall turned on, all before the card")

	fw := m.fw.get(web101)
	require.True(t, fw.opts.Enabled)
	require.Equal(t, "ACCEPT", fw.opts.PolicyIn)
	require.Equal(t, "ACCEPT", fw.opts.PolicyOut)
	require.False(t, fw.opts.IPFilter)
	card1 := managednet.FirewallCard{NIC: 1, Leg: netip.MustParseAddr("10.203.118.1"), Lease: netip.MustParseAddr("10.203.118.2"), Ports: []uint16{8080}}
	want := managednet.WantRules(card1, managednet.FirewallComment(m.installID()))
	require.Equal(t, want, fw.rules, "pco's rules carry the comment of the install")
	require.Equal(t, &store.FirewallBefore{}, m.attachments()["lxc/101"].Firewall, "what the firewall was is recorded")
}

func TestAGuestWhoseFirewallWasOnKeepsItsOptions(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.fw.set(web101, func(g *guestFirewall) { g.opts = pve.GuestFirewallOptions{Enabled: true, PolicyIn: "DROP"} })
	m.warm()
	m.next()
	require.NotContains(t, m.log.all(), "firewall lxc/101 options")
	require.Equal(t, "DROP", m.fw.get(web101).opts.PolicyIn)
	require.Equal(t, &store.FirewallBefore{Enabled: true, Rules: 0}, m.attachments()["lxc/101"].Firewall)
}

func TestNoCardIsAddedWithoutTheFirewall(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.fw.fail = fmt.Errorf("permission check failed")
	m.warm()
	st := m.next()
	require.Empty(t, m.cfg.changes())
	require.Equal(t, AttachmentUnverified, attachmentState(st, "lxc/101"))
	require.Contains(t, strings.Join(st.Problems, "\n"), "the firewall of the card could not be written")
}

func TestFilteredHoldsAndRaisesTheRoutesOnTheManagedNetwork(t *testing.T) {
	m := attached(t)

	st := m.next()

	require.NotNil(t, st.Managed.Filtered)
	require.Equal(t, FilteredView{Holds: true, Backend: managednet.BackendIPTables, Offered: true}, *st.Managed.Filtered)
	require.Equal(t, string(resolve.LevelFiltered), route(st, "www.example.com").Level)
	b, err := m.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, resolve.LevelFiltered, b["www.example.com"].Level, "the binding keeps filtered")
	require.Contains(t, m.log.all(), "canary leg1 10.203.118.2:1023", "one probe from the leg, on a port no route publishes")
}

func TestAMinimumOfFilteredServesTheManagedRoutesOnly(t *testing.T) {
	m := attached(t)
	m.settings(func(s *store.Settings) { s.IdentityMinimum = string(resolve.LevelFiltered) })
	m.inv.set(snapshot(onCard(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC),
		unconsented(managedGuest(102, "db-1", "db.example.com -> :5432"))))
	m.res.mu.Lock()
	m.res.levels["db.example.com"] = resolve.LevelObserved
	m.res.mu.Unlock()

	st := m.next()

	require.Equal(t, string(resolve.LevelFiltered), route(st, "www.example.com").Level)
	require.NotEmpty(t, route(st, "www.example.com").Rule)
	require.Contains(t, strings.Join(st.Problems, "\n"), "1 route is held back: its identity level is observed, below the required filtered")

	t.Run("lost, the managed routes are held back too", func(t *testing.T) {
		m.fw.set(web101, func(g *guestFirewall) { g.opts.MACFilterOff = true })
		st := m.next()
		require.False(t, st.Managed.Filtered.Holds)
		require.Equal(t, string(resolve.LevelObserved), route(st, "www.example.com").Level)
		require.Contains(t, strings.Join(st.Problems, "\n"),
			"2 routes are held back: their identity level is observed, below the required filtered; "+
				"guests on other nodes and trusted static addresses are proven at observed only: lower identityMinimum in the settings to serve them; "+
				"filtered lost: guest lxc/101: macfilter is off")
	})
}

func TestTheCanaryFailsFilteredWhenTheGuestAnswers(t *testing.T) {
	m := attached(t)
	m.next()

	m.canary = func(netip.AddrPort) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	said := len(m.managedEvents())
	st := m.next()
	require.False(t, st.Managed.Filtered.Holds)
	require.Equal(t, []string{"guest lxc/101: the firewall is not enforcing on net1: the connection was reset on port 1023"}, st.Managed.Filtered.Why)
	require.Equal(t, string(resolve.LevelObserved), route(st, "www.example.com").Level)
	b, err := m.store.Bindings()
	require.NoError(t, err)
	require.Equal(t, resolve.LevelObserved, b["www.example.com"].Level, "nothing stands on a filtered proof that does not hold")
	require.Contains(t, st.Problems, "filtered lost: guest lxc/101: the firewall is not enforcing on net1: the connection was reset on port 1023; the routes on pconet are served at observed")

	m.next()
	lost := 0
	for _, ev := range m.managedEvents()[said:] {
		if strings.HasPrefix(ev, "filtered lost") {
			lost++
		}
	}
	require.Equal(t, 1, lost, "said once")

	m.canary = func(netip.AddrPort) (net.Conn, error) { c, _ := net.Pipe(); return c, nil }
	st = m.next()
	require.Equal(t, []string{"guest lxc/101: the firewall is not enforcing on net1: the connection was accepted on port 1023"}, st.Managed.Filtered.Why)

	m.canary = nil
	st = m.next()
	require.True(t, st.Managed.Filtered.Holds)
	require.Contains(t, m.managedEvents(), "filtered holds on pconet again: every guest on the vnet is isolated by its firewall")
}

func TestTheCanaryPortMovesWhenARoutePublishesIt(t *testing.T) {
	m := attached(t)
	m.next()
	m.inv.set(snapshot(onCard(managedGuest(101, "web-1", "www.example.com -> :8080", "admin.example.com -> :1023")), ownGuest(ownMAC)))
	m.next()
	require.Contains(t, m.log.all(), "canary leg1 10.203.118.2:1022")
}

func TestNoCanaryForAStoppedGuest(t *testing.T) {
	m := attached(t)
	g := onCard(managedGuest(101, "web-1", "www.example.com -> :8080"))
	g.Running = false
	m.inv.set(snapshot(g, ownGuest(ownMAC)))
	before := len(m.log.all())
	st := m.next()
	for _, c := range m.log.all()[before:] {
		require.False(t, strings.HasPrefix(c, "canary"), c)
	}
	require.True(t, st.Managed.Filtered.Holds, "a stopped guest's firewall is checked, not probed")
}

func TestWhatDriftedIsWrittenAgainAndTheAdminsChoiceIsNot(t *testing.T) {
	m := attached(t)
	m.next()
	ipset := "ipfilter-net1"

	m.fw.set(web101, func(g *guestFirewall) {
		g.entries[ipset] = append(g.entries[ipset], pve.IPSetEntry{CIDR: "10.203.118.0/24"})
	})
	st := m.next()
	require.Equal(t, []string{"guest lxc/101: net1 is not yet enforced: pco wrote its firewall 0s ago"}, st.Managed.Filtered.Why,
		"the widened ipset is narrowed again in the same cycle, and enforced a pve-firewall interval later")
	require.Equal(t, []pve.IPSetEntry{{CIDR: "10.203.118.2", Comment: managednet.FirewallComment(m.installID())}}, m.fw.get(web101).entries[ipset])
	require.True(t, m.next().Managed.Filtered.Holds)

	m.fw.set(web101, func(g *guestFirewall) { g.rules[0].DPort = "1-65535" })
	st = m.next()
	require.Equal(t, []string{"guest lxc/101: net1 is not yet enforced: pco wrote its firewall 0s ago"}, st.Managed.Filtered.Why, "a changed rule is put right")
	require.True(t, m.next().Managed.Filtered.Holds)

	m.fw.set(web101, func(g *guestFirewall) { g.opts.Enabled = false })
	before := len(m.log.all())
	st = m.next()
	require.False(t, st.Managed.Filtered.Holds)
	require.Contains(t, st.Managed.Filtered.Why, "guest lxc/101: the guest firewall is off")
	require.NotContains(t, m.log.all()[before:], "firewall lxc/101 options", "the admin turned it off; pco does not overrule")
}

func TestAPortOpensOnlyWhileTheCycleMayWrite(t *testing.T) {
	m := attached(t)
	m.next()
	published := func() []uint16 {
		var out []uint16
		for _, r := range m.fw.get(web101).rules {
			if r.Type == "in" && r.Proto == "tcp" {
				var p uint16
				_, _ = fmt.Sscan(r.DPort, &p)
				out = append(out, p)
			}
		}
		return out
	}
	require.Equal(t, []uint16{8080}, published())

	m.inv.set(snapshot(onCard(managedGuest(101, "web-1", "www.example.com -> :443")), ownGuest(ownMAC)))
	require.NoError(t, m.store.SaveWriter(planner.Writer{InstallID: "other", Generation: 1, Nonce: "n1"}))
	st := m.next()
	require.Empty(t, published(), "8080 is closed at once, 443 waits")
	require.Equal(t, []string{"guest lxc/101: net1 is not yet enforced: pco wrote its firewall 0s ago"}, st.Managed.Filtered.Why,
		"what pco wrote holds, once it is enforced")
	require.True(t, m.next().Managed.Filtered.Holds)
	require.Empty(t, published(), "443 waits while the cycle may not write")
}

// pco's rules read back in another form than written stop being written:
// one round, then nothing until they change.
func TestRulesThatStillDifferAfterAWriteAreNotWrittenEveryCycle(t *testing.T) {
	m := attached(t)
	m.next()
	m.fw.set(web101, func(g *guestFirewall) { g.rules[0].DPort = "1-65535" })
	m.fw.mu.Lock()
	m.fw.normalize = func(r pve.FirewallRule) pve.FirewallRule {
		if r.Source != "" {
			r.Source += "/32"
		}
		return r
	}
	m.fw.mu.Unlock()
	writes := func() int {
		n := 0
		for _, c := range m.log.all() {
			if strings.HasPrefix(c, "firewall lxc/101") {
				n++
			}
		}
		return n
	}
	st := m.next()
	require.Contains(t, strings.Join(st.Problems, "\n"), "still differ from what pco writes")
	n := writes()
	for range 3 {
		st = m.next()
		require.False(t, st.Managed.Filtered.Holds)
	}
	require.Equal(t, n, writes(), "not written again while they read back the same")
	require.LessOrEqual(t, len(m.fw.get(web101).rules), 6)
}

func TestUnknownMACFailsFilteredUntilACyclePasses(t *testing.T) {
	m := attached(t)
	require.True(t, m.next().Managed.Filtered.Holds)
	mac, _ := net.ParseMAC("bc:24:11:aa:bb:cc")
	m.eng.UnknownMAC(mac, m.clock.now())
	require.False(t, m.eng.State().Managed.Filtered.Holds, "at once")
	st := m.next()
	require.Contains(t, st.Managed.Filtered.Why, "a frame from bc:24:11:aa:bb:cc, a MAC no lease knows, was seen on the vnet")
	st = m.next()
	require.True(t, st.Managed.Filtered.Holds, "a cycle later the guests were read again and nothing is foreign")

	m.eng.UnknownMAC(mac, m.clock.now())
	st = m.next()
	require.False(t, st.Managed.Filtered.Holds, "the same card sending again, within the window of its event, fails the next judgment too")
}

func TestAForeignCardFailsFiltered(t *testing.T) {
	m := attached(t)
	clone := onCard(managedGuest(105, "web-2"))
	clone.Tags = nil
	clone.NICs[1].MAC = "bc:24:11:00:99:01"
	m.inv.set(snapshot(onCard(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC), clone))
	st := m.next()
	require.False(t, st.Managed.Filtered.Holds)
	require.Contains(t, st.Managed.Filtered.Why, "guest lxc/105: net1 (bc:24:11:00:99:01) on the vnet is no card pco attached")
}

func TestTheNFTablesBackendOffersNoFiltered(t *testing.T) {
	m := attached(t)
	m.facts.set(func(f *managedFacts) { f.node.NFTables = true })
	st := m.next()
	require.Equal(t, managednet.BackendNFTables, st.Managed.Filtered.Backend)
	require.False(t, st.Managed.Filtered.Offered)
	require.False(t, st.Managed.Filtered.Holds)
	require.Equal(t, string(resolve.LevelObserved), route(st, "www.example.com").Level)
}

// detach takes the consent of lxc/101 away, lets the grace run and the card
// go.
func (m *managedApplianceEnv) detach() {
	m.t.Helper()
	g := unconsented(onCard(managedGuest(101, "web-1", "www.example.com -> :8080")))
	m.inv.set(snapshot(g, ownGuest(ownMAC)))
	m.next()
	m.clock.advance(11 * time.Minute)
	m.next()
	require.Contains(m.t, m.cfg.changes(), "remove lxc/101 net1")
	m.inv.set(snapshot(unconsented(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	m.next()
}

func TestTheDetachPutsTheGuestFirewallBack(t *testing.T) {
	m := attached(t)
	m.next()
	m.detach()

	fw := m.fw.get(web101)
	require.False(t, fw.opts.Enabled, "pco turned it on, so pco turns it off")
	require.Empty(t, fw.opts.PolicyIn)
	require.Empty(t, fw.sets)
	require.Empty(t, fw.rules)
	require.Empty(t, m.attachments())
	require.Contains(t, m.managedEvents(), "the firewall of lxc/101 (web-1) is as it was before its card on the managed network")
}

// An admin who added a rule after pco turned the firewall on has taken it
// up: it stays on, and only pco's rules and ipset go.
func TestTheDetachLeavesAFirewallAnAdminTookUp(t *testing.T) {
	m := attached(t)
	m.next()
	admin := pve.FirewallRule{Type: "in", Action: "ACCEPT", Iface: "net0", Proto: "tcp", DPort: "22", Enable: true, Comment: "admin"}
	m.fw.set(web101, func(g *guestFirewall) { g.rules = append(g.rules, admin) })
	m.detach()

	fw := m.fw.get(web101)
	require.True(t, fw.opts.Enabled)
	require.Empty(t, fw.sets)
	require.Equal(t, []pve.FirewallRule{admin}, fw.rules, "the admin's rule stays")
	require.Empty(t, m.attachments())
	require.Contains(t, m.managedEvents(), "the firewall of lxc/101 (web-1) stays on, which pco turned on for its card on the managed network: "+
		"it has 1 rules more than when pco turned it on; pco's rules and ipset on net1 are removed")
}

func TestTheHostWritesNoGuestFirewall(t *testing.T) {
	m := newManagedEnv(t)
	fw := newFakeFirewalls(m.log)
	m.managed = func(d *Deps) { m.facts.deps(d); d.Firewall = fw }
	m.eng = m.newEngine()
	m.warm()
	st := m.next()
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	require.Nil(t, st.Managed.Filtered)
	require.Zero(t, fw.readCount())
	for _, c := range m.log.all() {
		require.False(t, strings.HasPrefix(c, "firewall") || strings.HasPrefix(c, "canary"), c)
	}
}

func TestAMinimumOfFilteredWithoutTheManagedNetworkSaysWhy(t *testing.T) {
	a := newApplianceEnv(t, incA)
	a.enforce()
	a.settings(func(s *store.Settings) { s.IdentityMinimum = string(resolve.LevelFiltered) })
	a.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC)))
	a.res.mu.Lock()
	a.res.levels = map[string]resolve.Level{"www.example.com": resolve.LevelObserved}
	a.res.mu.Unlock()
	a.restart(incA)
	a.cycle()
	a.clock.advance(20 * time.Second)
	st := a.cycle()
	require.Contains(t, strings.Join(st.Problems, "\n"), "; "+reasonFilteredNotEnabled)
}

func TestEnableManagedAcknowledgesTheVnetInTheAppliance(t *testing.T) {
	m := newManagedApplianceEnv(t)
	nets, err := m.store.Networks()
	require.NoError(t, err)
	require.NoError(t, m.store.DeleteNetwork(gateway.ManagedBridge, nets[gateway.ManagedBridge].Rev))
	m.networks = &fakeNetworks{}
	m.restart(incA)
	m.warm()
	require.NoError(t, m.eng.EnableManaged(WithActor(context.Background(), "root (cli)")))
	segs, err := m.store.Segments()
	require.NoError(t, err)
	require.Contains(t, segs, gateway.ManagedBridge, "routes on pconet serve at observed when filtered is lost")
}

// The cycle time of the appliance with 50 guests on the vnet, each read and
// probed: the reads go four at a time, the canaries all at once. The number
// is logged, not asserted: the fake answers at once.
func TestTheCycleTimeWithFiftyGuestsOnTheVnet(t *testing.T) {
	m := newManagedApplianceEnv(t)
	guests := []model.Guest{ownGuest(ownMAC)}
	for i := range 50 {
		guests = append(guests, managedGuest(101+i, fmt.Sprintf("web-%d", i), fmt.Sprintf("g%d.example.com -> :8080", i)))
	}
	m.inv.set(snapshot(guests...))
	m.warm()
	m.next()
	atts := m.attachments()
	require.Len(t, atts, 50)
	for i := range guests[1:] {
		a := atts[guests[1+i].Ref.String()]
		g := withCard(guests[1+i], a.MAC, a.Addr.String())
		g.NICs[len(g.NICs)-1].Firewall = true
		guests[1+i] = g
	}
	m.inv.set(snapshot(guests...))
	reads := m.fw.readCount()
	start := time.Now()
	st := m.next()
	took, n := time.Since(start), m.fw.readCount()-reads
	// With an API that answers in 20 ms, the reads take n/4 rounds; the
	// canaries, all out at once, one timeout.
	const apiRead = 20 * time.Millisecond
	t.Logf("a cycle with 50 guests on the vnet took %s against the fake, with %d reads of their firewalls; "+
		"at %s a read that is %s for the reads and %s for the canaries", took, n, apiRead,
		time.Duration((n+firewallConcurrency-1)/firewallConcurrency)*apiRead, managednet.CanaryTimeout)
	require.Equal(t, 200, n, "four reads a guest: options, rules, ipsets and the card's ipset")
	require.True(t, st.Managed.Filtered.Holds, st.Managed.Filtered.Why)
}
