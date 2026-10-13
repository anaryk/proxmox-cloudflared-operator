package engine

import (
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func hw(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	m, err := net.ParseMAC(s)
	require.NoError(t, err)
	return m
}

// attachedVM is a managed env with a running virtual machine whose card on
// the managed network was added live in the last cycle.
func attachedVM(t *testing.T) (*managedEnv, model.Guest) {
	t.Helper()
	m := newManagedEnv(t)
	vm := managedGuest(102, "vm-2", "vm.example.com -> :8080")
	vm.Ref.Kind = model.KindQEMU
	m.inv.set(snapshot(vm))
	m.warm()
	st := m.next()
	require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"))
	m.inv.set(snapshot(withCard(vm, leaseMAC2, "")))
	return m, vm
}

func attachmentOf(st State, owner string) AttachmentView {
	for _, a := range st.Managed.Attachments {
		if a.Owner == owner {
			return a
		}
	}
	return AttachmentView{}
}

func TestAVirtualMachineThatAsksNoAddressAwaitsDHCPAfterAMinute(t *testing.T) {
	m, _ := attachedVM(t)

	st := m.next() // 20 s after the card went live
	require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"))
	st = m.next() // 40 s
	require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"))
	st = m.next() // 60 s

	a := attachmentOf(st, "qemu/102")
	require.Equal(t, AttachmentAwaitingDHCP, a.State)
	require.Equal(t, "no DHCP request seen on the new card; see docs/networking.md for the snippet of your distribution", a.Detail)

	m.eng.DHCPLeases().Seen(hw(t, leaseMAC2), m.clock.now())
	st = m.next()
	require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"), "a request ends the wait")
}

func TestARequestIsWrittenToTheLeaseOncePerHour(t *testing.T) {
	m, _ := attachedVM(t)
	first := m.clock.now()
	m.eng.DHCPLeases().Seen(hw(t, leaseMAC2), first)

	m.next()
	l := m.leases()[leaseMAC2]
	require.NotNil(t, l.LastDHCP)
	require.True(t, l.LastDHCP.Equal(first))

	m.eng.DHCPLeases().Seen(hw(t, leaseMAC2), first.Add(30*time.Minute))
	m.next()
	require.True(t, m.leases()[leaseMAC2].LastDHCP.Equal(first), "a renewal within the hour is not written")

	later := first.Add(2 * time.Hour)
	m.eng.DHCPLeases().Seen(hw(t, leaseMAC2), later)
	m.next()
	require.True(t, m.leases()[leaseMAC2].LastDHCP.Equal(later))
}

// A request the lease records counts after a restart of the daemon too: the
// card is not said to wait.
func TestARequestTheLeaseRecordsEndsTheWait(t *testing.T) {
	m, _ := attachedVM(t)
	leases := m.leases()
	l := leases[leaseMAC2]
	at := t0
	l.LastDHCP = &at
	require.NoError(t, m.store.SaveLease(l, l.Rev))

	var st State
	for range 5 {
		st = m.next()
	}
	require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"))
}

func TestOnlyARunningVirtualMachineAwaitsDHCP(t *testing.T) {
	t.Run("a container has its address in its configuration", func(t *testing.T) {
		m := newManagedEnv(t)
		m.warm()
		var st State
		for range 6 {
			st = m.next()
		}
		require.Equal(t, AttachmentAttached, attachmentState(st, "lxc/101"))
	})
	t.Run("a stopped virtual machine asks nothing", func(t *testing.T) {
		m, vm := attachedVM(t)
		vm = withCard(vm, leaseMAC2, "")
		vm.Running = false
		m.inv.set(snapshot(vm))
		var st State
		for range 5 {
			st = m.next()
		}
		require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"))
		vm.Running = true
		m.inv.set(snapshot(vm))
		for range 3 {
			st = m.next()
		}
		require.Equal(t, AttachmentAttached, attachmentState(st, "qemu/102"), "the minute starts when it runs")
		st = m.next()
		require.Equal(t, AttachmentAwaitingDHCP, attachmentState(st, "qemu/102"))
	})
	t.Run("a card that waits for the restart is not live", func(t *testing.T) {
		m := newManagedEnv(t)
		vm := managedGuest(102, "vm-2", "vm.example.com -> :8080")
		vm.Ref.Kind = model.KindQEMU
		m.inv.set(snapshot(vm))
		m.cfg.set(func(f *fakeConfigurer) { f.notLive["qemu/102"] = true })
		m.warm()
		var st State
		for range 6 {
			st = m.next()
		}
		require.Equal(t, AttachmentPendingReboot, attachmentState(st, "qemu/102"))
	})
}

func TestTheServerIsGivenTheLeases(t *testing.T) {
	m := newManagedEnv(t)
	require.NoError(t, m.store.SaveLease(store.Lease{MAC: leaseMAC3, Addr: netip.MustParseAddr("10.203.118.3"), Owner: "qemu/103", NIC: 1, Since: t0}, 0))
	leases := m.eng.DHCPLeases()

	addr, ok := leases.Lookup(hw(t, leaseMAC3))
	require.True(t, ok, "before the first cycle the store is read")
	require.Equal(t, netip.MustParseAddr("10.203.118.3"), addr)
	_, ok = leases.Lookup(hw(t, "52:54:00:12:34:56"))
	require.False(t, ok)

	m.warm()
	m.next()
	addr, ok = leases.Lookup(hw(t, leaseMAC2))
	require.True(t, ok, "the lease of the card the cycle added")
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), addr)

	require.NoError(t, m.store.DeleteLease(leaseMAC3))
	m.next()
	_, ok = leases.Lookup(hw(t, leaseMAC3))
	require.False(t, ok, "a lease that went is not answered")
}

// A lease another node's cycle wrote, as for a guest that moved here, is
// answered before this node's next cycle: a MAC the table lacks has the
// store read again, at most every few seconds.
func TestALeaseTheTableLacksIsReadFromTheStore(t *testing.T) {
	m := newManagedEnv(t)
	m.warm()
	m.next()
	leases := m.eng.DHCPLeases()
	_, ok := leases.Lookup(hw(t, leaseMAC3))
	require.False(t, ok)

	moved := store.Lease{MAC: leaseMAC3, Addr: netip.MustParseAddr("10.203.118.3"), Owner: "qemu/103", NIC: 1, Since: t0}
	require.NoError(t, m.store.SaveLease(moved, 0))
	_, ok = leases.Lookup(hw(t, leaseMAC3))
	require.False(t, ok, "the store was read for the miss a moment ago")
	m.clock.advance(leaseReadEvery)
	addr, ok := leases.Lookup(hw(t, leaseMAC3))
	require.True(t, ok, "read again for the next miss")
	require.Equal(t, moved.Addr, addr)
	addr, ok = leases.Lookup(hw(t, leaseMAC2))
	require.True(t, ok, "the leases of the cycle are still there")
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), addr)
}

func TestADeclineAndAReleaseAreEvents(t *testing.T) {
	m, _ := attachedVM(t)
	m.next()
	leases := m.eng.DHCPLeases()
	now := m.clock.now()

	leases.Declined(hw(t, leaseMAC2), now)
	leases.Declined(hw(t, leaseMAC2), now.Add(time.Minute))
	leases.Released(hw(t, leaseMAC2), now)
	leases.Declined(hw(t, "52:54:00:12:34:56"), now)
	leases.Declined(hw(t, leaseMAC2), now.Add(6*time.Minute))

	var got []string
	for _, ev := range m.eng.Events(time.Time{}) {
		if ev.Kind == kindManaged && ev.Subject == "qemu/102" && (strings.Contains(ev.Message, "declined") || strings.Contains(ev.Message, "released")) {
			got = append(got, ev.Level+" "+ev.Message)
		}
	}
	require.Equal(t, []string{
		"info qemu/102 declined its managed address 10.203.118.2: a client on pconet says it is in use; the sender is not verified, and the card keeps its lease",
		"info qemu/102 released its managed address 10.203.118.2: a client on pconet gave it up; the sender is not verified, and the card keeps its lease",
		"info qemu/102 declined its managed address 10.203.118.2: a client on pconet says it is in use; the sender is not verified, and the card keeps its lease",
	}, got)
}

func TestTheServersStateIsInTheView(t *testing.T) {
	m := newManagedEnv(t)
	st := m.warm()
	require.Nil(t, st.Managed.DHCP, "nothing said yet")

	m.eng.NoteDHCP(nil, errors.New("opening the sockets on leg1: no such device"))
	st = m.next()
	require.Nil(t, st.Managed.DHCP)
	require.Contains(t, st.Problems, "the DHCP server is not listening on the managed leg: opening the sockets on leg1: no such device")

	m.eng.NoteDHCP(&DHCPView{Bound: true, Since: t0, Unknown: 3, Forged: 1}, nil)
	st = m.next()
	require.Equal(t, &DHCPView{Bound: true, Since: t0, Unknown: 3, Forged: 1}, st.Managed.DHCP)
	for _, p := range st.Problems {
		require.NotContains(t, p, "DHCP")
	}
}

func TestADisabledManagedNetworkHasNoServerState(t *testing.T) {
	e := newEnv(t)
	e.eng.NoteDHCP(&DHCPView{Bound: true}, nil)
	st := e.cycle()
	require.False(t, st.Managed.Enabled)
	require.Nil(t, st.Managed.DHCP)
}
