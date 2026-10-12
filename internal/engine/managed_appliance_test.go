package engine

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// managedApplianceEnv is an appliance whose daemon runs the managed network:
// the vnet as the node made it for an appliance, without isolated ports,
// the node's firewall on the iptables backend, the datacenter firewall on
// and pve-firewall running.
type managedApplianceEnv struct {
	*applianceEnv
	cfg   *fakeConfigurer
	facts *managedFacts
	log   *callLog
}

func newManagedApplianceEnv(t *testing.T) *managedApplianceEnv {
	t.Helper()
	a := newApplianceEnv(t, incA)
	a.enforce()
	log := &callLog{}
	m := &managedApplianceEnv{applianceEnv: a, log: log, cfg: newFakeConfigurer(log, a.store)}
	m.facts = &managedFacts{
		registered: []string{"cf-tunnel", managednet.Tag},
		vnet:       ManagedVnet{Exists: true, ZoneType: "simple"},
		cluster:    pve.ClusterFirewallOptions{Enabled: true},
		service:    "running",
		guestFW:    map[string]bool{},
		guestFWErr: map[string]error{},
	}
	a.guests, a.managed = m.cfg, m.facts.deps
	a.gateway = &fakeGateway{log: log, down: map[string]string{}}
	saveManagedNetwork(t, a.store, 1)
	a.inv.set(snapshot(managedGuest(101, "web-1", "www.example.com -> :8080"), ownGuest(ownMAC)))
	a.restart(incA)
	return m
}

func (m *managedApplianceEnv) warm() State {
	m.t.Helper()
	st := m.cycle()
	require.True(m.t, st.Identity.OK, st.Identity.Why)
	m.clock.advance(20 * time.Second)
	return st
}

func (m *managedApplianceEnv) next() State {
	m.t.Helper()
	st := m.cycle()
	m.clock.advance(20 * time.Second)
	return st
}

func (m *managedApplianceEnv) managedEvents() []string {
	var out []string
	for _, ev := range m.eng.Events(time.Time{}) {
		if ev.Kind == kindManaged {
			out = append(out, ev.Message)
		}
	}
	return out
}

func (m *managedApplianceEnv) attachments() map[string]store.Attachment {
	m.t.Helper()
	atts, err := m.store.Attachments()
	require.NoError(m.t, err)
	return atts
}

// firewalled is g with firewall=1 on its first card, as the GUI makes a
// card by default.
func firewalled(g model.Guest) model.Guest {
	g.NICs[0].Firewall = true
	return g
}

func TestTheApplianceAddsItsCardsWithTheFirewallOn(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.warm()

	st := m.next()

	require.Equal(t, []string{"add lxc/101 net1 name=,bridge=pconet,hwaddr=" + leaseMAC2 + ",ip=10.203.118.2/24,firewall=1"}, m.cfg.changes())
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	require.True(t, st.Managed.Vnet.OK, st.Managed.Vnet.Why)
	require.False(t, st.Managed.Vnet.IsolatePorts)
	require.Empty(t, st.Managed.Refused)
}

func TestAGuestWhoseFirewallWouldFilterAnotherCardWaitsForTheConfirmation(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.inv.set(snapshot(firewalled(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	m.warm()

	st := m.next()

	require.Empty(t, m.cfg.changes())
	why := "needs-firewall-confirmation: turning on the firewall of lxc/101 (web-1) also filters net0 (firewall=1); " +
		"run pco managed approve lxc/101 --accept-firewall-impact, or set managed.acceptFirewallImpact for every guest"
	require.Equal(t, why, waitingWhy(st, "lxc/101"))

	t.Run("the setting releases it", func(t *testing.T) {
		ctx := WithActor(t.Context(), "root (cli)")
		view, err := m.eng.SettingsView()
		require.NoError(t, err)
		s := view.Settings
		s.Managed.AcceptFirewallImpact = true
		_, _, err = m.eng.SaveSettings(ctx, view.Rev, s)
		require.NoError(t, err)
		require.Contains(t, m.managedEvents(), "the firewall-impact confirmation for consented guests was switched on by root (cli)")

		st := m.next()
		require.Equal(t, []string{"add lxc/101 net1 name=,bridge=pconet,hwaddr=" + leaseMAC2 + ",ip=10.203.118.2/24,firewall=1"}, m.cfg.changes())
		require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	})
}

func TestThePerGuestConfirmationReleasesTheGuest(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.inv.set(snapshot(firewalled(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	m.warm()
	st := m.next()
	require.Contains(t, waitingWhy(st, "lxc/101"), "needs-firewall-confirmation")

	c, err := m.eng.ApproveManaged(t.Context(), "lxc/101", "mac:101", true)
	require.NoError(t, err)
	require.True(t, c.AcceptFirewallImpact)
	st = m.next()

	require.Len(t, m.cfg.changes(), 1)
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	require.Equal(t, managednet.ByApproval, m.attachments()["lxc/101"].Consent)
}

func TestNoImpactWithTheGuestFirewallOnOrWithoutAFilteredCard(t *testing.T) {
	m := newManagedApplianceEnv(t)
	on := firewalled(managedGuest(101, "web-1", "www.example.com -> :8080"))
	m.facts.set(func(f *managedFacts) { f.guestFW["lxc/101"] = true })
	m.inv.set(snapshot(on, managedGuest(102, "web-2", "two.example.com -> :8080"), ownGuest(ownMAC)))
	m.warm()

	st := m.next()

	require.Len(t, m.cfg.changes(), 2)
	require.Empty(t, st.Managed.Waiting)
}

func TestAGuestWhoseFirewallOptionsCannotBeReadWaits(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.facts.set(func(f *managedFacts) { f.guestFWErr["lxc/101"] = errors.New("403 Forbidden") })
	m.inv.set(snapshot(firewalled(managedGuest(101, "web-1", "www.example.com -> :8080")), ownGuest(ownMAC)))
	m.warm()

	st := m.next()

	require.Empty(t, m.cfg.changes())
	require.Equal(t, "the firewall options of lxc/101 could not be read: 403 Forbidden", waitingWhy(st, "lxc/101"))
}

func TestAGuestPcoMayNotWriteWaitsForTheGrant(t *testing.T) {
	m := newManagedApplianceEnv(t)
	line := "pveum acl modify /vms/101 --users pco@pve --tokens 'pco@pve!vm9250' --roles PCOManaged"
	m.cfg.set(func(f *fakeConfigurer) { f.unwritable["lxc/101"] = line })
	m.warm()

	st := m.next()

	require.Empty(t, m.cfg.changes())
	require.Equal(t, "needs-grant: "+line, waitingWhy(st, "lxc/101"))
}

func TestTheApplianceRefusesWhileTheNodeCannotIsolateTheGuests(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(f *managedFacts)
		line   string
	}{
		{"nftables", func(f *managedFacts) { f.node = pve.NodeFirewallOptions{NFTables: true} },
			"the firewall of node pve1 runs on the nftables backend (nftables: 1), which installs no guest rules; set nftables: 0 in its firewall options"},
		{"datacenter firewall off", func(f *managedFacts) { f.cluster = pve.ClusterFirewallOptions{} },
			"the datacenter firewall is off; no guest rule is in force until it is turned on"},
		{"pve-firewall stopped", func(f *managedFacts) { f.service = "stopped" },
			"pve-firewall is stopped on node pve1; no guest rule is in force until it runs"},
		{"options unreadable", func(f *managedFacts) { f.nodeErr = errors.New("503") },
			"the firewall options of node pve1 could not be read: 503"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newManagedApplianceEnv(t)
			m.warm()
			m.next()
			require.Len(t, m.cfg.changes(), 1)
			attached := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), 1, leaseMAC2, "10.203.118.2")
			m.inv.set(snapshot(attached, managedGuest(102, "web-2", "two.example.com -> :8080"), ownGuest(ownMAC)))
			m.facts.set(tt.change)

			st := m.next()
			m.next()

			require.Equal(t, tt.line, st.Managed.Refused)
			require.Contains(t, st.Problems, "the managed network cannot run on this node: "+tt.line)
			require.Len(t, m.cfg.changes(), 1, "no attach, nothing detached")
			require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
			require.Nil(t, m.attachments()["lxc/101"].DetachAt)
			require.Equal(t, "the managed network cannot run on this node: "+tt.line, waitingWhy(st, "lxc/102"))
			require.Equal(t, 1, strings.Count(strings.Join(m.managedEvents(), "\n"), "cannot run on this node"), "one event")

			m.facts.set(func(f *managedFacts) {
				f.node, f.nodeErr, f.cluster, f.service = pve.NodeFirewallOptions{}, nil, pve.ClusterFirewallOptions{Enabled: true}, "running"
			})
			st = m.next()
			require.Empty(t, st.Managed.Refused)
			require.Len(t, m.cfg.changes(), 2, "web-2 is attached once the node can isolate it again")
		})
	}
}

func TestEnableManagedStoresTheNetworkOfTheAppliance(t *testing.T) {
	m := newManagedApplianceEnv(t)
	require.NoError(t, m.store.DeleteNetwork(gateway.ManagedBridge, 1))
	nets := &fakeNetworks{}
	m.networks = nets
	m.restart(incA)
	m.warm()
	ctx := WithActor(t.Context(), "root (cli)")

	require.NoError(t, m.eng.EnableManaged(ctx))

	stored, err := m.store.Networks()
	require.NoError(t, err)
	n := stored[gateway.ManagedBridge]
	require.Equal(t, store.Network{
		ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge, Prefix: netip.MustParsePrefix(managedSubnet),
		Legs:   map[string]netip.Addr{testNode: netip.MustParseAddr("10.203.118.1")},
		LegMAC: store.LegMACFor(store.NetworkManaged, 1), Mark: 1, Scope: store.ScopeNode, CreatedAt: m.clock.now(), Rev: 1,
	}, n)
	require.Equal(t, []string{"enable"}, nets.called(), "the first network has the namespace unit enabled, which the appliance ignores")
	require.Contains(t, unnumbered(m.eng.Events(time.Time{})), Event{At: m.clock.now(), Level: levelInfo, Kind: kindManaged, Subject: gateway.ManagedBridge, Actor: "root (cli)",
		Message: "the managed network is enabled: pconet with the subnet 10.203.118.0/24, the leg 10.203.118.1 on node pve1, mark 1"})

	require.NoError(t, m.eng.EnableManaged(ctx), "enabled already: nothing to do")
	require.Len(t, nets.called(), 1)
}

func TestEnableManagedIsRefusedWithoutTheVnetAsMade(t *testing.T) {
	m := newManagedApplianceEnv(t)
	require.NoError(t, m.store.DeleteNetwork(gateway.ManagedBridge, 1))
	m.networks = &fakeNetworks{}
	m.restart(incA)
	m.warm()

	for name, vnet := range map[string]ManagedVnet{
		"missing":        {},
		"isolated ports": {Exists: true, ZoneType: "simple", IsolatePorts: true},
		"with a subnet":  {Exists: true, ZoneType: "simple", Subnets: 1},
	} {
		t.Run(name, func(t *testing.T) {
			m.facts.set(func(f *managedFacts) { f.vnet = vnet })
			err := m.eng.EnableManaged(t.Context())
			require.ErrorIs(t, err, ErrRefused)
			require.ErrorContains(t, err, "the vnet pconet")
		})
	}
	stored, err := m.store.Networks()
	require.NoError(t, err)
	require.Empty(t, stored)
}

func TestEnableManagedChecksTheSubnetAndTakesAFreeMark(t *testing.T) {
	e := newNetEnv(t)
	facts := &managedFacts{vnet: ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true}}
	e.managed = facts.deps
	e.eng = e.newEngine()

	t.Run("a guest address in the subnet", func(t *testing.T) {
		e.settings(func(s *store.Settings) { s.Managed.Subnet = netip.MustParsePrefix("10.77.0.0/24") })
		err := e.eng.EnableManaged(t.Context())
		var fe *FieldError
		require.ErrorAs(t, err, &fe)
		require.Equal(t, "managed.subnet", fe.Field)
		require.ErrorContains(t, err, "holds 10.77.0.11, an address of qemu/101 (web-1)")
		e.settings(func(s *store.Settings) { s.Managed.Subnet = netip.MustParsePrefix(managedSubnet) })
	})

	t.Run("a mark freed since the last sync is not taken", func(t *testing.T) {
		_, err := e.eng.Attach(t.Context(), attachVmbr1())
		require.NoError(t, err)
		nets, err := e.store.Networks()
		require.NoError(t, err)
		require.Equal(t, 1, nets["vmbr1"].Mark)
		require.NoError(t, e.eng.Detach(t.Context(), "vmbr1", nets["vmbr1"].Rev, false))

		require.NoError(t, e.eng.EnableManaged(t.Context()))
		nets, err = e.store.Networks()
		require.NoError(t, err)
		require.Equal(t, 2, nets[gateway.ManagedBridge].Mark, "mark 1 was freed by a detach the gateway has not been synced since")
		require.Equal(t, store.NetworkManaged, nets[gateway.ManagedBridge].Kind)
		calls := e.nets.called()
		require.Equal(t, "enable", calls[len(calls)-1], fmt.Sprint(calls))
	})
}
