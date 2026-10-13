package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	managedSubnet = "10.203.118.0/24"
	// The MACs of the first two addresses of the managed subnet.
	leaseMAC2 = "06:50:43:4f:76:02"
	leaseMAC3 = "06:50:43:4f:76:03"
)

// fakeConfigurer records the changes the engine asks of the cards of guests,
// in the log of the other fakes too, and looks at the store when it is
// asked to add a card, so that a test can tell whether the attachment was
// saved first.
type fakeConfigurer struct {
	mu          sync.Mutex
	log         *callLog
	store       *store.Store
	calls       []string
	notLive     map[string]bool  // by owner: AddNIC says the card waits for a restart
	addErr      map[string]error // by owner: what AddNIC fails with
	linkDownErr map[string]error // by owner: what SetLinkDown fails with
	removeErr   map[string]error // by owner: what RemoveNIC fails with
	unwritable  map[string]string
	writeErr    error           // what CanWrite fails with
	savedFirst  map[string]bool // by owner: the attachment was in the store when AddNIC was called
}

func newFakeConfigurer(log *callLog, s *store.Store) *fakeConfigurer {
	return &fakeConfigurer{
		log: log, store: s, notLive: map[string]bool{}, addErr: map[string]error{}, linkDownErr: map[string]error{},
		removeErr: map[string]error{}, unwritable: map[string]string{}, savedFirst: map[string]bool{},
	}
}

func (f *fakeConfigurer) AddNIC(_ context.Context, g model.Guest, index int, spec managednet.NICSpec) (bool, error) {
	owner := g.Ref.String()
	atts, _ := f.store.Attachments()
	_, saved := atts[owner]
	f.mu.Lock()
	defer f.mu.Unlock()
	f.savedFirst[owner] = saved
	value := fmt.Sprintf("virtio=%s,bridge=%s,firewall=%d", spec.MAC, spec.Bridge, onOff(spec.Firewall))
	if g.Ref.Kind == model.KindLXC {
		value = fmt.Sprintf("name=%s,bridge=%s,hwaddr=%s,ip=%s,firewall=%d", spec.Name, spec.Bridge, spec.MAC, spec.Addr, onOff(spec.Firewall))
	}
	call := fmt.Sprintf("add %s net%d %s", owner, index, value)
	f.calls = append(f.calls, call)
	f.log.add("configurer " + call)
	if err := f.addErr[owner]; err != nil {
		return false, err
	}
	return !f.notLive[owner], nil
}

func onOff(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (f *fakeConfigurer) RemoveNIC(_ context.Context, g model.Guest, index int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := fmt.Sprintf("remove %s net%d", g.Ref, index)
	f.calls = append(f.calls, call)
	f.log.add("configurer " + call)
	return f.removeErr[g.Ref.String()]
}

func (f *fakeConfigurer) SetLinkDown(_ context.Context, g model.Guest, index int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := fmt.Sprintf("link-down %s net%d", g.Ref, index)
	f.calls = append(f.calls, call)
	f.log.add("configurer " + call)
	return f.linkDownErr[g.Ref.String()]
}

func (f *fakeConfigurer) CanWrite(_ context.Context, g model.Guest) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return false, "", f.writeErr
	}
	if why, ok := f.unwritable[g.Ref.String()]; ok {
		return false, why, nil
	}
	return true, "", nil
}

func (f *fakeConfigurer) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// changes are the calls that change a guest: everything but CanWrite.
func (f *fakeConfigurer) changes() []string { return f.called() }

func (f *fakeConfigurer) set(change func(f *fakeConfigurer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// managedFacts is what the engine reads of Proxmox for the managed network,
// as a test sets it.
type managedFacts struct {
	mu         sync.Mutex
	registered []string
	regErr     error
	vnet       ManagedVnet
	vnetErr    error
	node       pve.NodeFirewallOptions
	nodeErr    error
	cluster    pve.ClusterFirewallOptions
	clusterErr error
	service    string
	serviceErr error
	guestFW    map[string]bool // by owner: the guest firewall is on
	guestFWErr map[string]error
	// fwReads counts the reads of a guest's firewall options, by owner, and
	// vnetReads those of the vnet.
	fwReads   map[string]int
	vnetReads int
}

func (f *managedFacts) firewallReads(owner string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fwReads[owner]
}

func (f *managedFacts) vnetReadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.vnetReads
}

func (f *managedFacts) set(change func(f *managedFacts)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *managedFacts) deps(d *Deps) {
	d.RegisteredTags = func(context.Context) ([]string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.Clone(f.registered), f.regErr
	}
	d.Vnet = func(context.Context) (ManagedVnet, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.vnetReads++
		return f.vnet, f.vnetErr
	}
	d.NodeFirewall = func(context.Context) (pve.NodeFirewallOptions, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.node, f.nodeErr
	}
	d.ClusterFirewall = func(context.Context) (pve.ClusterFirewallOptions, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.cluster, f.clusterErr
	}
	d.NodeService = func(_ context.Context, name string) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if name != "pve-firewall" {
			return "", fmt.Errorf("service %s is not in this test", name)
		}
		return f.service, f.serviceErr
	}
	d.GuestFirewall = func(_ context.Context, g model.Guest) (pve.GuestFirewallOptions, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fwReads == nil {
			f.fwReads = map[string]int{}
		}
		f.fwReads[g.Ref.String()]++
		if err := f.guestFWErr[g.Ref.String()]; err != nil {
			return pve.GuestFirewallOptions{}, err
		}
		return pve.GuestFirewallOptions{Enabled: f.guestFW[g.Ref.String()]}, nil
	}
}

// managedEnv is an env with the managed network stored (mark 1), a gateway
// that records what it is given, a configurer that records the changes of
// the cards, and the facts of Proxmox as the step reads them: both tags
// registered, the vnet as the host made it, the firewall of the node on
// the iptables backend, the datacenter firewall on and pve-firewall running.
type managedEnv struct {
	*env
	gw    *fakeGateway
	cfg   *fakeConfigurer
	facts *managedFacts
	log   *callLog
}

func newManagedEnv(t *testing.T) *managedEnv {
	t.Helper()
	e, log := orderLogged(t)
	e.enforce()
	m := &managedEnv{env: e, log: log}
	m.gw = &fakeGateway{log: log, down: map[string]string{}}
	m.cfg = newFakeConfigurer(log, e.store)
	m.facts = &managedFacts{
		registered: []string{"cf-tunnel", managednet.Tag},
		vnet:       ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true},
		cluster:    pve.ClusterFirewallOptions{Enabled: true},
		service:    "running",
		guestFW:    map[string]bool{},
		guestFWErr: map[string]error{},
	}
	e.gateway, e.guests, e.managed = m.gw, m.cfg, m.facts.deps
	e.res.hook(func() { log.add("resolve") })
	saveManagedNetwork(t, e.store, 1)
	e.inv.set(snapshot(managedGuest(101, "web-1", "www.example.com -> :8080")))
	e.eng = e.newEngine()
	return m
}

func saveManagedNetwork(t *testing.T, s *store.Store, mark int) {
	t.Helper()
	subnet := netip.MustParsePrefix(managedSubnet)
	require.NoError(t, s.SaveNetwork(store.Network{
		ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge, Prefix: subnet,
		Legs:   map[string]netip.Addr{testNode: subnet.Addr().Next()},
		LegMAC: store.LegMACFor(store.NetworkManaged, mark), Mark: mark, Scope: store.ScopeNode, CreatedAt: t0,
	}, 0))
}

// managedGuest is a container of this node with the gate tag and the consent
// tag, net0 on vmbr1, a bridge the node has no address in.
func managedGuest(vmid int, name string, routes ...string) model.Guest {
	g := guest(vmid, name, routes...)
	g.Ref.Kind = model.KindLXC
	g.Identity = fmt.Sprintf("mac:%d", vmid)
	g.Tags = []string{"cf-tunnel", managednet.Tag}
	g.NICs = []model.NIC{{Index: 0, MAC: fmt.Sprintf("bc:24:11:00:01:%02x", vmid%256), Bridge: "vmbr1", Static: []netip.Addr{netip.MustParseAddr(fmt.Sprintf("10.77.0.%d", vmid%256))}}}
	return g
}

// withCard is g with a card on the managed network at index, with mac and,
// for a container, the address.
func withCard(g model.Guest, mac string, addr string) model.Guest {
	nic := model.NIC{Index: 1, MAC: mac, Bridge: gateway.ManagedBridge}
	if addr != "" {
		nic.Static = []netip.Addr{netip.MustParseAddr(addr)}
	}
	g.NICs = append(slices.Clone(g.NICs), nic)
	return g
}

func unconsented(g model.Guest) model.Guest {
	g.Tags = []string{"cf-tunnel"}
	return g
}

// warm runs the first cycle, in which no card is added: nothing says yet
// that this process is the writer.
func (m *managedEnv) warm() State {
	m.t.Helper()
	st := m.cycle()
	m.clock.advance(20 * time.Second)
	return st
}

func (m *managedEnv) next() State {
	m.t.Helper()
	st := m.cycle()
	m.clock.advance(20 * time.Second)
	return st
}

func (m *managedEnv) attachments() map[string]store.Attachment {
	m.t.Helper()
	atts, err := m.store.Attachments()
	require.NoError(m.t, err)
	return atts
}

func (m *managedEnv) leases() map[string]store.Lease {
	m.t.Helper()
	leases, err := m.store.Leases()
	require.NoError(m.t, err)
	return leases
}

// managedEvents are the messages of the events of the managed network, in
// order.
func (m *managedEnv) managedEvents() []string {
	var out []string
	for _, ev := range m.eng.Events(time.Time{}) {
		if ev.Kind == kindManaged {
			out = append(out, ev.Message)
		}
	}
	return out
}

func attachmentState(st State, owner string) string {
	for _, a := range st.Managed.Attachments {
		if a.Owner == owner {
			return a.State
		}
	}
	return ""
}

func waitingWhy(st State, owner string) string {
	for _, w := range st.Managed.Waiting {
		if w.String() == owner {
			return w.Why
		}
	}
	return ""
}

func TestATaggedContainerGetsACardBeforeItsAddressIsResolved(t *testing.T) {
	m := newManagedEnv(t)

	st := m.warm()
	require.True(t, st.Managed.Enabled)
	require.Empty(t, m.cfg.changes(), "no card while nothing says that this process is the writer")
	require.Equal(t, gateNoCycle, waitingWhy(st, "lxc/101"))

	st = m.next()

	require.Equal(t, []string{"add lxc/101 net1 name=,bridge=pconet,hwaddr=" + leaseMAC2 + ",ip=10.203.118.2/24,firewall=0"}, m.cfg.changes())
	require.True(t, m.cfg.savedFirst["lxc/101"], "the attachment is saved before the card is added")
	add := m.log.index("configurer add", 0)
	require.GreaterOrEqual(t, add, 0, m.log.all())
	require.Greater(t, m.log.index("resolve", add), add, "the card is added before the addresses are resolved")
	att := m.attachments()["lxc/101"]
	require.Equal(t, store.Attachment{Owner: "lxc/101", Identity: "mac:101", NIC: 1, MAC: leaseMAC2, Addr: netip.MustParseAddr("10.203.118.2"),
		Consent: managednet.ByTag, Since: t0.Add(20 * time.Second)}, att)
	require.Contains(t, m.leases(), leaseMAC2)
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	require.Empty(t, st.Managed.Waiting)
	require.Equal(t, 252, st.Managed.Free)
	require.Equal(t, "per-guest", st.Managed.Consent)
	require.True(t, st.Managed.TagHonoured)
	require.True(t, st.Managed.AutoAllowed)
	require.True(t, st.Managed.Vnet.OK)
	require.Contains(t, m.managedEvents(), "lxc/101 (web-1) is attached to the managed network: net1 at 10.203.118.2, by tag")

	// The resolver of the same cycle saw the lease on the card.
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), m.res.leased("lxc/101"))

	m.inv.set(snapshot(withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")))
	st = m.next()
	require.Len(t, m.cfg.changes(), 1, "a guest with its card is kept")
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), m.res.leased("lxc/101"))
}

func TestAVirtualMachineWithoutHotplugWaitsForItsRestart(t *testing.T) {
	m := newManagedEnv(t)
	vm := managedGuest(102, "vm-2", "vm.example.com -> :8080")
	vm.Ref.Kind = model.KindQEMU
	m.inv.set(snapshot(vm))
	m.cfg.set(func(f *fakeConfigurer) { f.notLive["qemu/102"] = true })
	m.warm()

	st := m.next()

	require.Equal(t, []string{"add qemu/102 net1 virtio=" + leaseMAC2 + ",bridge=pconet,firewall=0"}, m.cfg.changes())
	require.Equal(t, AttachmentPendingReboot, attachmentState(st, "qemu/102"))
	require.Contains(t, m.managedEvents(), "the card net1 of qemu/102 (vm-2) on the managed network waits for the restart of the guest")
	require.False(t, m.res.leased("qemu/102").IsValid(), "a card that is not live is no candidate yet")

	st = m.next()
	require.Len(t, m.cfg.changes(), 2, "the card is asked for again, which the configurer answers without a write, until it is live")
	require.Equal(t, AttachmentPendingReboot, attachmentState(st, "qemu/102"))
	require.Len(t, m.attachments(), 1)
	require.Equal(t, 1, strings.Count(strings.Join(m.managedEvents(), "\n"), "waits for the restart"), "said once")

	// The guest restarted: its configuration shows the card.
	m.inv.set(snapshot(withCard(vm, leaseMAC2, "")))
	st = m.next()
	require.Len(t, m.cfg.changes(), 2, "a card that is there is kept")
	require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"), "the card is live since the restart")
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), m.res.leased("qemu/102"))
}

func TestTheNextFreeIndexIsTaken(t *testing.T) {
	m := newManagedEnv(t)
	g := managedGuest(101, "web-1", "www.example.com -> :8080")
	g.NICs = append(g.NICs, model.NIC{Index: 1, MAC: "bc:24:11:00:02:01", Bridge: "vmbr2"})
	m.inv.set(snapshot(g))
	m.warm()

	m.next()

	require.Equal(t, []string{"add lxc/101 net2 name=,bridge=pconet,hwaddr=" + leaseMAC2 + ",ip=10.203.118.2/24,firewall=0"}, m.cfg.changes())
}

func TestNoCardWhileTheGateIsClosed(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	// A cycle that holds before its tunnel run closes the gate of the next.
	m.inv.enqueue(incomplete("cluster resources not listed: boom", managedGuest(101, "web-1", "www.example.com -> :8080")))
	m.next()

	st := m.next()

	require.Empty(t, m.cfg.changes())
	require.Equal(t, gateNoRun, waitingWhy(st, "lxc/101"))
	require.Empty(t, m.attachments())
	require.Empty(t, m.leases(), "no lease is taken for a card that is not added")
}

func TestAnIncompleteInventoryDecidesNothing(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.inv.enqueue(incomplete("cluster resources not listed: boom", managedGuest(101, "web-1", "www.example.com -> :8080")))

	st := m.next()

	require.Empty(t, m.cfg.changes())
	require.Equal(t, gateNoCycle, waitingWhy(st, "lxc/101"), "the view of the last cycle is carried")
	require.Empty(t, m.attachments())
}

func TestTheCardGoesAfterTheGraceAndTheLeaseOnceTheConfigurationDroppedIt(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	attached := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	m.inv.set(snapshot(unconsented(attached)))

	st := m.next()
	att := m.attachments()["lxc/101"]
	require.NotNil(t, att.DetachAt)
	require.Equal(t, t0.Add(40*time.Second+10*time.Minute), *att.DetachAt)
	require.Equal(t, "the guest has neither the tag cf-tunnel-managed nor an approval", att.DetachWhy)
	require.Equal(t, AttachmentDetaching, attachmentState(st, "lxc/101"))
	require.Contains(t, m.managedEvents(), "the card net1 of lxc/101 (web-1) on the managed network goes at "+att.DetachAt.Format(time.RFC3339)+
		": the guest has neither the tag cf-tunnel-managed nor an approval")
	require.Len(t, m.cfg.changes(), 1, "nothing removed yet")

	m.clock.advance(9 * time.Minute)
	st = m.next()
	require.Len(t, m.cfg.changes(), 1, "not before the grace ends")
	require.Equal(t, AttachmentDetaching, attachmentState(st, "lxc/101"))

	m.clock.advance(time.Minute)
	st = m.next()
	require.Equal(t, "remove lxc/101 net1", m.cfg.changes()[1])
	att = m.attachments()["lxc/101"]
	require.NotNil(t, att.RemovedAt)
	require.Equal(t, AttachmentDetachPending, attachmentState(st, "lxc/101"), "the configuration of the snapshot still lists the card")
	require.Contains(t, m.leases(), leaseMAC2, "the lease is kept while the card may still be there")
	require.Contains(t, m.managedEvents(), "the card net1 of lxc/101 (web-1) was removed from the managed network; "+
		"its lease stays until the configuration no longer lists the card")

	st = m.next()
	require.Len(t, m.cfg.changes(), 2, "removed once")
	require.Equal(t, AttachmentDetachPending, attachmentState(st, "lxc/101"))

	m.inv.set(snapshot(unconsented(managedGuest(101, "web-1", "www.example.com -> :8080"))))
	st = m.next()
	require.Empty(t, m.attachments())
	require.Empty(t, m.leases())
	require.Empty(t, st.Managed.Attachments)
	require.Contains(t, m.managedEvents(), "lxc/101 (web-1) is detached from the managed network; 10.203.118.2 is free again")
}

func TestConsentBackWithinTheGraceCancelsTheDetach(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	attached := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	m.inv.set(snapshot(unconsented(attached)))
	m.next()
	require.NotNil(t, m.attachments()["lxc/101"].DetachAt)

	m.inv.set(snapshot(attached))
	st := m.next()

	att := m.attachments()["lxc/101"]
	require.Nil(t, att.DetachAt)
	require.Empty(t, att.DetachWhy)
	require.Equal(t, managednet.ByTag, att.Consent)
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	require.Contains(t, m.managedEvents(), "the card net1 of lxc/101 (web-1) on the managed network stays: its consent is back")
	require.Len(t, m.cfg.changes(), 1)
}

func TestAnApprovalConsentsInItsIdentityAndKeepsTheConsentOfTheAttachment(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	attached := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	m.inv.set(snapshot(unconsented(attached)))
	m.next()
	require.NotNil(t, m.attachments()["lxc/101"].DetachAt)

	c, err := m.eng.ApproveManaged(t.Context(), "lxc/101", "mac:101", false)
	require.NoError(t, err)
	require.Equal(t, store.Consent{Owner: "lxc/101", Identity: "mac:101", At: t0.Add(60 * time.Second)}, c)
	st := m.next()

	att := m.attachments()["lxc/101"]
	require.Nil(t, att.DetachAt)
	require.Equal(t, managednet.ByTag, att.Consent, "what the attachment was made under stays")
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	require.Contains(t, m.eng.Events(time.Time{}), Event{Seq: eventSeq(m.eng, "lxc/101 (web-1) is approved for the managed network in identity mac:101"),
		Boot: m.eng.boot, At: t0.Add(60 * time.Second), Level: levelInfo, Kind: kindAdmin, Subject: "lxc/101",
		Message: "lxc/101 (web-1) is approved for the managed network in identity mac:101"})

	require.NoError(t, m.eng.RevokeManaged(t.Context(), "lxc/101"))
	consents, err := m.store.Consents()
	require.NoError(t, err)
	require.Empty(t, consents)
	st = m.next()
	require.NotNil(t, m.attachments()["lxc/101"].DetachAt, "the grace starts again")
	require.Equal(t, AttachmentDetaching, attachmentState(st, "lxc/101"))
}

func TestAnApprovalIsRefusedForAGuestNotShownAsItIs(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()

	_, err := m.eng.ApproveManaged(t.Context(), "lxc/101", "mac:999", false)
	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "shown in identity mac:999 and has identity mac:101 now")
	_, err = m.eng.ApproveManaged(t.Context(), "lxc/777", "", false)
	require.ErrorIs(t, err, ErrRefused)
	_, err = m.eng.ApproveManaged(t.Context(), "manual/x", "", false)
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorIs(t, m.eng.RevokeManaged(t.Context(), "lxc/101"), ErrNotFound)
}

func TestAutomaticConsentAttachesAGuestThatNeedsTheNetwork(t *testing.T) {
	m := newManagedEnv(t)
	m.settings(func(s *store.Settings) { s.Managed.Consent = store.ConsentAuto })
	// web-1 is on vmbr1, which the node has no address in; db-1 is on
	// vmbr0, where the node is.
	db := unconsented(managedGuest(102, "db-1", "db.example.com -> :5432"))
	db.NICs[0].Bridge = "vmbr0"
	snap := snapshot(unconsented(managedGuest(101, "web-1", "www.example.com -> :8080")), db)
	snap.Nodes[0].Ifaces = []pve.NodeIface{{Name: "vmbr0", Type: "bridge", Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/24")}}}
	m.inv.set(snap)
	m.warm()

	st := m.next()

	require.Equal(t, []string{"add lxc/101 net1 name=,bridge=pconet,hwaddr=" + leaseMAC2 + ",ip=10.203.118.2/24,firewall=0"}, m.cfg.changes())
	require.Equal(t, managednet.ByAuto, m.attachments()["lxc/101"].Consent)
	require.Empty(t, st.Managed.Waiting, "a guest that does not need the network is not waiting")
	require.Empty(t, st.Managed.AutoStopped)
}

func TestTheGateTagLeavingTheRegisteredTagsStopsAutomaticAttachesOnly(t *testing.T) {
	m := newManagedEnv(t)
	m.settings(func(s *store.Settings) { s.Managed.Consent = store.ConsentAuto })
	m.inv.set(snapshot(unconsented(managedGuest(101, "web-1", "www.example.com -> :8080")), unconsented(managedGuest(102, "web-2", "two.example.com -> :8080"))))
	m.warm()
	m.next()
	require.Len(t, m.attachments(), 2)
	m.inv.set(snapshot(
		withCard(unconsented(managedGuest(101, "web-1", "www.example.com -> :8080")), leaseMAC2, "10.203.118.2"),
		withCard(unconsented(managedGuest(102, "web-2", "two.example.com -> :8080")), leaseMAC3, "10.203.118.3"),
		unconsented(managedGuest(103, "web-3", "three.example.com -> :8080")),
	))

	m.facts.set(func(f *managedFacts) { f.registered = []string{managednet.Tag} })
	var st State
	for range 60 {
		st = m.next()
	}

	line := "managed.consent is auto but the gate tag cf-tunnel is not registered as admin-only; automatic attaches are stopped; attached guests stay attached"
	require.Equal(t, line, st.Managed.AutoStopped)
	require.False(t, st.Managed.AutoAllowed)
	require.Contains(t, st.Problems, line)
	require.Len(t, m.cfg.changes(), 2, "nothing attached, nothing removed")
	for _, att := range m.attachments() {
		require.Nil(t, att.DetachAt, att.Owner)
	}
	require.Equal(t, 1, strings.Count(strings.Join(m.managedEvents(), "\n"), "automatic attaches are stopped"), "one event")
	require.Equal(t, "automatic attaches are stopped: the gate tag cf-tunnel is not registered as admin-only", waitingWhy(st, "lxc/103"))

	m.facts.set(func(f *managedFacts) { f.registered = nil; f.regErr = errors.New("no Sys.Audit") })
	st = m.next()
	require.Equal(t, "registered-tags could not be read", st.Managed.AutoStopped)
	require.Contains(t, st.Problems, "managed.consent is auto, and registered-tags could not be read; automatic attaches are stopped; attached guests stay attached")
	require.Len(t, m.cfg.changes(), 2)
	for _, att := range m.attachments() {
		require.Nil(t, att.DetachAt, att.Owner)
	}

	m.facts.set(func(f *managedFacts) { f.registered = []string{"cf-tunnel", managednet.Tag}; f.regErr = nil })
	st = m.next()
	require.Empty(t, st.Managed.AutoStopped)
	require.Contains(t, m.managedEvents(), "automatic attaches resumed: the gate tag cf-tunnel is registered as admin-only again")
	require.Len(t, m.cfg.changes(), 3, "web-3 is attached once they resume")
}

func TestTheOptOutMarkerStartsTheGrace(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	g := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	g.Description = "```cf-tunnel\ncf-tunnel-managed=off\nwww.example.com -> :8080\n```"
	m.inv.set(snapshot(g))

	st := m.next()

	att := m.attachments()["lxc/101"]
	require.NotNil(t, att.DetachAt)
	require.Equal(t, "cf-tunnel-managed=off", att.DetachWhy)
	require.Equal(t, AttachmentDetaching, attachmentState(st, "lxc/101"))
}

func TestACopiedCardOnATaggedCloneIsReplacedWithItsOwnLease(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	original := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	clone := withCard(managedGuest(102, "web-1-clone", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	m.inv.set(snapshot(original, clone))

	st := m.next()

	require.Equal(t, "add lxc/102 net1 name=,bridge=pconet,hwaddr="+leaseMAC3+",ip=10.203.118.3/24,firewall=0", m.cfg.changes()[1])
	require.Len(t, m.cfg.changes(), 2, "one write replaces the copy; no second card")
	require.Equal(t, netip.MustParseAddr("10.203.118.3"), m.attachments()["lxc/102"].Addr)
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), m.attachments()["lxc/101"].Addr, "the original keeps its lease")
	require.Len(t, st.Managed.Foreign, 1)
	require.Equal(t, ForeignView{GuestView: GuestView{GuestRef: clone.Ref, Name: "web-1-clone"}, NIC: 1, MAC: leaseMAC2, Action: "replaced",
		Why: "a card on pconet that pco did not attach, as a clone or a restore copies one; pco replaces it with the guest's own"}, st.Managed.Foreign[0])
	require.Contains(t, m.managedEvents(), "foreign card net1 ("+leaseMAC2+") of lxc/102 (web-1-clone) was replaced by the guest's own card with "+leaseMAC3+" at 10.203.118.3")
	require.Contains(t, st.Problems, "foreign card net1 ("+leaseMAC2+") of lxc/102 (web-1-clone): a card on pconet that pco did not attach, as a clone or a restore copies one; pco replaces it with the guest's own")

	m.inv.set(snapshot(original, withCard(managedGuest(102, "web-1-clone", "www.example.com -> :8080"), leaseMAC3, "10.203.118.3")))
	st = m.next()
	require.Len(t, m.cfg.changes(), 2)
	require.Empty(t, st.Managed.Foreign)
	require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/102"))
}

func TestACopiedCardOnAnUntaggedGuestIsSetLinkDown(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	original := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	clone := withCard(managedGuest(102, "web-1-clone"), leaseMAC2, "10.203.118.2")
	clone.Tags = nil
	m.inv.set(snapshot(original, clone))

	st := m.next()

	require.Equal(t, "link-down lxc/102 net1", m.cfg.changes()[1])
	require.Len(t, st.Managed.Foreign, 1)
	require.Equal(t, "link-down", st.Managed.Foreign[0].Action)
	require.Contains(t, m.managedEvents(), "foreign card net1 ("+leaseMAC2+") of lxc/102 (web-1-clone) was set link down: "+
		"a card on pconet that pco did not attach, as a clone or a restore copies one; pco sets its link down")
	require.True(t, hasProblem(st, "foreign card net1 ("+leaseMAC2+") of lxc/102 (web-1-clone)"))

	t.Run("a container that refuses link_down loses the card", func(t *testing.T) {
		m.cfg.set(func(f *fakeConfigurer) {
			f.linkDownErr["lxc/102"] = fmt.Errorf("%w: pct set: link_down: property is not defined in schema", managednet.ErrLinkDownRefused)
		})
		st := m.next()
		changes := m.cfg.changes()
		require.Equal(t, "link-down lxc/102 net1", changes[len(changes)-2])
		require.Equal(t, "remove lxc/102 net1", changes[len(changes)-1])
		require.Equal(t, "removed", st.Managed.Foreign[0].Action)
		require.Contains(t, m.managedEvents(), "foreign card net1 ("+leaseMAC2+") of lxc/102 (web-1-clone) was removed, as the container refused link_down: "+
			"link_down is refused on this card: pct set: link_down: property is not defined in schema")
	})

	t.Run("a guest pco may not write is reported", func(t *testing.T) {
		m.cfg.set(func(f *fakeConfigurer) {
			f.unwritable["lxc/102"] = "pveum acl modify /vms/102 --users pco@pve --roles PCOManaged"
		})
		n := len(m.cfg.changes())
		st := m.next()
		require.Len(t, m.cfg.changes(), n, "nothing changed")
		require.Equal(t, "reported", st.Managed.Foreign[0].Action)
		require.Contains(t, st.Managed.Foreign[0].Why, "pco may not change lxc/102: remove the card")
	})
}

func TestAVnetThatIsNotAsCreatedHoldsTheAttaches(t *testing.T) {
	for _, tt := range []struct {
		name string
		vnet ManagedVnet
		why  string
	}{
		{"a subnet", ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true, Subnets: 1}, "it has 1 subnet, and pco's leg is its only gateway"},
		{"isolate-ports flipped", ManagedVnet{Exists: true, ZoneType: "simple"}, "isolate-ports is off, and on a host it is on"},
		{"another zone type", ManagedVnet{Exists: true, ZoneType: "vxlan", IsolatePorts: true}, "its zone is of type vxlan, not simple"},
		{"gone", ManagedVnet{}, "it does not exist"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := newManagedEnv(t)
			m.warm()
			m.facts.set(func(f *managedFacts) { f.vnet = tt.vnet })
			m.clock.advance(vnetEvery)

			st := m.next()
			m.next()

			require.Empty(t, m.cfg.changes())
			require.False(t, st.Managed.Vnet.OK)
			require.Equal(t, []string{tt.why}, st.Managed.Vnet.Why)
			line := "the vnet pconet was changed outside pco: " + tt.why
			require.Contains(t, st.Problems, line)
			require.Equal(t, line, waitingWhy(st, "lxc/101"))
			require.Equal(t, 1, strings.Count(strings.Join(m.managedEvents(), "\n"), "changed outside pco"), "one event")
		})
	}
}

func TestAnAddFailureIsAProblemLineAndTheAttachmentStays(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.cfg.set(func(f *fakeConfigurer) {
		f.addErr["lxc/101"] = errors.New("adding net1 to lxc/101: net1 is a card on vmbr9; pco replaces only a card on pconet")
	})

	st := m.next()

	require.Contains(t, st.Problems, "managed card of lxc/101 (web-1): adding net1 to lxc/101: net1 is a card on vmbr9; pco replaces only a card on pconet")
	require.Len(t, m.attachments(), 1, "the attachment is kept for the next cycle to add the card again")
	require.Equal(t, AttachmentAttached+" (unverified)", attachmentState(st, "lxc/101"))
}

func TestAnExhaustedSubnetIsSaidOnce(t *testing.T) {
	m := newManagedEnv(t)
	subnet := netip.MustParsePrefix("10.203.118.0/28")
	m.settings(func(s *store.Settings) { s.Managed.Subnet = subnet })
	require.NoError(t, m.store.DeleteNetwork(gateway.ManagedBridge, 1))
	require.NoError(t, m.store.SaveNetwork(store.Network{
		ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge, Prefix: subnet,
		Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.203.118.1")}, LegMAC: store.LegMACFor(store.NetworkManaged, 1), Mark: 1, Scope: store.ScopeNode, CreatedAt: t0,
	}, 0))
	// Every address from .2 to .14 is leased to guests of days past.
	for i := 2; i <= 14; i++ {
		addr := netip.MustParseAddr(fmt.Sprintf("10.203.118.%d", i))
		require.NoError(t, m.store.SaveLease(store.Lease{MAC: store.ManagedMAC(addr), Addr: addr, Owner: fmt.Sprintf("lxc/%d", 900+i), Identity: "x", NIC: 1, Since: t0}, 0))
	}
	m.warm()

	st := m.next()
	m.next()

	require.Empty(t, m.cfg.changes())
	require.Equal(t, 0, st.Managed.Free)
	require.Contains(t, waitingWhy(st, "lxc/101"), "no free address on the managed network")
	require.True(t, hasProblem(st, "no free address on the managed network"))
	require.Equal(t, 1, strings.Count(strings.Join(m.managedEvents(), "\n"), "no free address"), "one event")
}

func TestAGuestThatMigratedKeepsItsCard(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	g := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	g.Node = "pve2"
	m.inv.set(snapshot(g))

	st := m.next()

	require.Len(t, m.cfg.changes(), 1)
	require.Equal(t, AttachmentMigrated, attachmentState(st, "lxc/101"))
	require.Empty(t, st.Managed.Waiting, "an attached guest that migrated is shown as migrated, not as waiting")
	require.Contains(t, m.managedEvents(), "lxc/101 (web-1) took its card on the managed network to node pve2")
	require.Contains(t, m.leases(), leaseMAC2)
}

func TestTheManagedNetworkIsOffWithoutItsNetwork(t *testing.T) {
	m := newManagedEnv(t)
	require.NoError(t, m.store.DeleteNetwork(gateway.ManagedBridge, 1))
	m.warm()

	st := m.next()

	require.False(t, st.Managed.Enabled)
	require.Empty(t, m.cfg.changes())
	require.Empty(t, st.Managed.Waiting)
	require.Equal(t, "per-guest", st.Managed.Consent)
}

func TestUnknownMACsAreRecordedOncePerFiveMinutes(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	mac := hardware(t, "bc:24:11:00:99:99")

	m.eng.UnknownMAC(mac, m.clock.now())
	m.eng.UnknownMAC(mac, m.clock.now().Add(4*time.Minute))
	m.eng.UnknownMAC(hardware(t, "bc:24:11:00:99:98"), m.clock.now())

	require.Equal(t, []string{testNode, testNode}, m.inv.forcedNodes())
	events := m.managedEvents()
	require.Equal(t, []string{
		"a frame from bc:24:11:00:99:99, a MAC no lease knows, was seen on the managed network; the guests of pve1 are read again",
		"a frame from bc:24:11:00:99:98, a MAC no lease knows, was seen on the managed network; the guests of pve1 are read again",
	}, events)

	m.eng.UnknownMAC(mac, m.clock.now().Add(5*time.Minute))
	require.Len(t, m.inv.forcedNodes(), 3, "after five minutes it counts again")
}

func TestTheSettingsRefuseAutoWithoutTheGateTagRegistered(t *testing.T) {
	m := newManagedEnv(t)
	m.facts.set(func(f *managedFacts) { f.registered = []string{managednet.Tag} })

	err := saveManaged(t, m.env, func(s *store.Settings) { s.Managed.Consent = store.ConsentAuto })

	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "managed.consent", fe.Field)
	require.ErrorContains(t, err, "the gate tag cf-tunnel must be registered as admin-only first (pco setup --repair)")

	m.facts.set(func(f *managedFacts) { f.registered = []string{"cf-tunnel", managednet.Tag} })
	require.NoError(t, saveManaged(t, m.env, func(s *store.Settings) { s.Managed.Consent = store.ConsentAuto }))

	err = saveManaged(t, m.env, func(s *store.Settings) { s.GateTag = "other" })
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "gateTag", fe.Field)
	require.ErrorContains(t, err, "the gate tag other must be registered as admin-only first")

	m.facts.set(func(f *managedFacts) { f.regErr = errors.New("no Sys.Audit") })
	err = saveManaged(t, m.env, func(s *store.Settings) { s.GateTag = "other" })
	require.ErrorAs(t, err, &fe)
	require.ErrorContains(t, err, "registered-tags could not be read")

	require.NoError(t, saveManaged(t, m.env, func(s *store.Settings) { s.Managed.Consent = store.ConsentPerGuest; s.GateTag = "other" }),
		"per-guest consent needs no registration")
}

func TestSwitchingTheFirewallImpactConfirmationIsAnEvent(t *testing.T) {
	m := newManagedEnv(t)
	ctx := WithActor(t.Context(), "root (cli)")

	view, err := m.eng.SettingsView()
	require.NoError(t, err)
	s := view.Settings
	s.Managed.AcceptFirewallImpact = true
	_, _, err = m.eng.SaveSettings(ctx, view.Rev, s)
	require.NoError(t, err)

	events := m.eng.Events(time.Time{})
	require.Contains(t, unnumbered(events), Event{At: t0, Level: levelInfo, Kind: kindManaged, Subject: "managed.acceptFirewallImpact", Actor: "root (cli)",
		Message: "the firewall-impact confirmation for consented guests was switched on by root (cli)"})
}

// saveManaged saves the settings with change applied, at the revision they
// were read at.
func saveManaged(t *testing.T, e *env, change func(*store.Settings)) error {
	t.Helper()
	view, err := e.eng.SettingsView()
	require.NoError(t, err)
	s := view.Settings
	change(&s)
	_, _, err = e.eng.SaveSettings(t.Context(), view.Rev, s)
	return err
}

// eventSeq is the number of the first event with the message.
func eventSeq(e *Engine, msg string) uint64 {
	for _, ev := range e.Events(time.Time{}) {
		if ev.Message == msg {
			return ev.Seq
		}
	}
	return 0
}
