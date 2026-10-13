package engine

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func foreignAction(st State, owner string) string {
	for _, f := range st.Managed.Foreign {
		if f.String() == owner {
			return f.Action
		}
	}
	return ""
}

// A tagged clone carries a copy of the original's card. While its own card
// cannot be put in that place, the copy is set link down, and it is replaced
// once it can.
func TestACopyThatCannotBeReplacedYetIsSetLinkDown(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	original := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	clone := withCard(managedGuest(102, "web-1-clone", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	m.inv.set(snapshot(original, clone))
	// A cycle that holds before its tunnel run closes the gate of the next.
	m.inv.enqueue(incomplete("cluster resources not listed: boom", original, clone))
	m.next()

	st := m.next()

	require.Equal(t, "link-down lxc/102 net1", m.cfg.changes()[1], "the gate is closed: the copy goes down at once")
	require.Len(t, m.cfg.changes(), 2)
	require.Equal(t, ForeignLinkDown, foreignAction(st, "lxc/102"))
	require.Equal(t, gateNoRun, waitingWhy(st, "lxc/102"))
	require.NotContains(t, m.attachments(), "lxc/102", "no lease and no attachment while the gate is closed")

	st = m.next()
	require.Equal(t, "add lxc/102 net1 name=,bridge=pconet,hwaddr="+leaseMAC3+",ip=10.203.118.3/24,firewall=0", m.cfg.changes()[2],
		"once the gate is open the copy is replaced")
	require.Equal(t, ForeignReplaced, foreignAction(st, "lxc/102"))
}

// A held managed network replaces nothing either, and sets the copy down.
func TestACopyIsSetLinkDownWhileTheManagedNetworkIsHeld(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	original := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	clone := withCard(managedGuest(102, "web-1-clone", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	m.inv.set(snapshot(original, clone))
	m.facts.set(func(f *managedFacts) { f.vnet.Subnets = 1 })
	m.clock.advance(vnetEvery)

	st := m.next()

	require.Equal(t, []string{"add lxc/101 net1 name=,bridge=pconet,hwaddr=" + leaseMAC2 + ",ip=10.203.118.2/24,firewall=0", "link-down lxc/102 net1"},
		m.cfg.changes())
	require.Equal(t, ForeignLinkDown, foreignAction(st, "lxc/102"))
}

// While the cycle may not write, as with the writer identity of another
// install, the cards are left as they are but for the restrictive change of
// a copy: no grace is saved, no card removed, no lease released, and what
// waits says why.
func TestNothingButALinkDownWhileTheCycleMayNotWrite(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	attached := withCard(managedGuest(101, "web-1", "www.example.com -> :8080"), leaseMAC2, "10.203.118.2")
	stranger := withCard(managedGuest(103, "copy"), leaseMAC2, "10.203.118.2")
	stranger.Tags = nil
	m.inv.set(snapshot(unconsented(attached), stranger, managedGuest(104, "new-4", "four.example.com -> :8080")))
	require.NoError(t, m.store.SaveWriter(planner.Writer{InstallID: "other", Generation: 1, Nonce: "n1"}))

	st := m.next()

	require.Equal(t, "link-down lxc/103 net1", m.cfg.changes()[1], "the restrictive change runs")
	require.Len(t, m.cfg.changes(), 2, "nothing else")
	att := m.attachments()["lxc/101"]
	require.Nil(t, att.DetachAt, "no grace is saved while the cycle may not write")
	require.Equal(t, AttachmentDetaching, attachmentState(st, "lxc/101"))
	require.True(t, strings.HasPrefix(attachmentDetail(st, "lxc/101"), "waits until the cycle may write: "), attachmentDetail(st, "lxc/101"))
	require.True(t, strings.HasPrefix(waitingWhy(st, "lxc/104"), "waits until the cycle may write: "), waitingWhy(st, "lxc/104"))
	require.NotContains(t, m.attachments(), "lxc/104")
	require.Len(t, m.leases(), 1)

	// A grace that ended removes nothing either.
	require.NoError(t, m.store.SaveWriter(testWriter))
	m.next()
	m.clock.advance(11 * time.Minute)
	require.NoError(t, m.store.SaveWriter(planner.Writer{InstallID: "other", Generation: 1, Nonce: "n1"}))
	n := len(m.cfg.changes())
	m.next()
	require.NotContains(t, m.cfg.changes()[n:], "remove lxc/101 net1")
	require.Nil(t, m.attachments()["lxc/101"].RemovedAt)
}

func attachmentDetail(st State, owner string) string {
	for _, a := range st.Managed.Attachments {
		if a.Owner == owner {
			return a.Detail
		}
	}
	return ""
}

// The firewall of a guest is read only for a guest that is to get a card,
// and once per configuration within a minute.
func TestTheFirewallImpactIsReadOnlyForAGuestToBeAttached(t *testing.T) {
	m := newManagedApplianceEnv(t)
	gui := firewalled(managedGuest(101, "web-1", "www.example.com -> :8080"))
	other := firewalled(unconsented(managedGuest(102, "web-2", "two.example.com -> :8080")))
	m.inv.set(snapshot(gui, other, ownGuest(ownMAC)))
	m.warm()

	st := m.next()
	m.next()

	require.Contains(t, waitingWhy(st, "lxc/101"), "needs-firewall-confirmation")
	require.Equal(t, 1, m.facts.firewallReads("lxc/101"), "read once for its configuration")
	require.Zero(t, m.facts.firewallReads("lxc/102"), "a guest without consent is not asked about")

	m.clock.advance(time.Minute)
	m.next()
	require.Equal(t, 2, m.facts.firewallReads("lxc/101"), "read again after a minute")

	changed := gui
	changed.Digest = "another"
	m.inv.set(snapshot(changed, other, ownGuest(ownMAC)))
	m.next()
	require.Equal(t, 3, m.facts.firewallReads("lxc/101"), "and when its configuration changed")
}

// The vnet is read once a minute.
func TestTheVnetIsReadOnceAMinute(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	m.next()
	require.Equal(t, 1, m.facts.vnetReadCount())

	m.clock.advance(vnetEvery)
	m.next()
	require.Equal(t, 2, m.facts.vnetReadCount())
}

// A card that an attachment lost goes back with its lease, which is saved
// again when it was lost too; one that cannot be saved is not put back.
func TestAHealSavesALostLeaseAgain(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	// The card and its lease are gone, as after a restore of the guest and
	// of the store from before the attach.
	m.inv.set(snapshot(managedGuest(101, "web-1", "www.example.com -> :8080")))
	require.NoError(t, m.store.DeleteLease(leaseMAC2))

	m.next()

	require.Equal(t, "add lxc/101 net1 name=,bridge=pconet,hwaddr="+leaseMAC2+",ip=10.203.118.2/24,firewall=0", m.cfg.changes()[1])
	require.Contains(t, m.leases(), leaseMAC2, "the lease is saved again")
	require.Equal(t, "lxc/101", m.leases()[leaseMAC2].Owner)

	t.Run("a lease that cannot be saved again", func(t *testing.T) {
		m.inv.set(snapshot(managedGuest(101, "web-1", "www.example.com -> :8080")))
		require.NoError(t, m.store.DeleteLease(leaseMAC2))
		// The subnet of the settings no longer holds the address.
		m.settings(func(s *store.Settings) { s.Managed.Subnet = netip.MustParsePrefix("10.204.0.0/24") })
		n := len(m.cfg.changes())

		st := m.next()

		require.Len(t, m.cfg.changes(), n, "no card without its lease")
		require.True(t, hasProblem(st, "the lease of 10.203.118.2 is gone and could not be saved again"), st.Problems)
		require.Equal(t, AttachmentUnverified, attachmentState(st, "lxc/101"))
	})
}

// A lease taken for an attachment that could not be saved goes again.
func TestALeaseIsGivenBackWhenItsAttachmentCannotBeSaved(t *testing.T) {
	m := newManagedEnv(t)
	g := managedGuest(101, "web-1", "www.example.com -> :8080")
	g.Identity = ""
	m.inv.set(snapshot(g))
	m.warm()

	st := m.next()

	require.Empty(t, m.cfg.changes())
	require.Empty(t, m.attachments())
	require.Empty(t, m.leases(), "the lease went with the attachment that could not be saved")
	require.Contains(t, waitingWhy(st, "lxc/101"), "the attachment could not be saved")
}

// A card added on a later try is said to be attached then.
func TestACardAddedLaterIsSaidToBeAttached(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.cfg.set(func(f *fakeConfigurer) { f.addErr["lxc/101"] = errors.New("CT is locked (backup)") })
	m.next()
	require.NotContains(t, strings.Join(m.managedEvents(), "\n"), "is attached")

	m.cfg.set(func(f *fakeConfigurer) { delete(f.addErr, "lxc/101") })
	m.next()
	m.next()

	require.Equal(t, 1, strings.Count(strings.Join(m.managedEvents(), "\n"), "lxc/101 (web-1) is attached to the managed network"), "said once, when it went through")
}

// The record of unknown MACs makes room by the entries older than five
// minutes, once it is full.
func TestUnknownMACsArePruned(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()

	for i := range maxUnknownMACs {
		m.eng.UnknownMAC(net.HardwareAddr{0xbc, 0x24, 0x11, 0x01, 0x00, byte(i)}, m.clock.now())
	}
	m.eng.UnknownMAC(hardware(t, "bc:24:11:00:99:98"), m.clock.now().Add(6*time.Minute))

	m.eng.macMu.Lock()
	defer m.eng.macMu.Unlock()
	require.Equal(t, []string{"bc:24:11:00:99:98"}, keys(m.eng.unknownMACs))
}

// On a host the isolation of the appliance is not touched.
func TestAnUnknownMACOnAHostLeavesTheFilteredViewAlone(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.eng.stateMu.Lock()
	m.eng.state.Managed.Filtered = &FilteredView{Holds: true, Backend: "pve-firewall", Offered: true}
	m.eng.stateMu.Unlock()

	m.eng.UnknownMAC(hardware(t, "bc:24:11:00:99:99"), m.clock.now())

	require.True(t, m.eng.State().Managed.Filtered.Holds)
}

func TestAnUnknownMACInTheApplianceDropsTheFilteredView(t *testing.T) {
	m := newManagedApplianceEnv(t)
	m.warm()
	m.eng.stateMu.Lock()
	m.eng.state.Managed.Filtered = &FilteredView{Holds: true, Backend: "pve-firewall", Offered: true}
	m.eng.stateMu.Unlock()

	m.eng.UnknownMAC(hardware(t, "bc:24:11:00:99:99"), m.clock.now())

	require.False(t, m.eng.State().Managed.Filtered.Holds)
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
