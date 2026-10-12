package managednet_test

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// newStore opens a store in a temporary directory with the managed subnet
// given.
func newStore(t *testing.T, subnet string) *store.Store {
	t.Helper()
	base := t.TempDir()
	s, err := store.Open(store.Paths{
		Cluster: filepath.Join(base, "cluster"),
		Private: filepath.Join(base, "private"),
		Local:   filepath.Join(base, "local"),
	})
	require.NoError(t, err)
	require.NoError(t, s.Init())
	settings := store.DefaultSettings()
	settings.Managed.Subnet = netip.MustParsePrefix(subnet)
	require.NoError(t, s.SaveSettings(settings))
	return s
}

func newIPAM(t *testing.T, s managednet.LeaseStore, subnet string) *managednet.IPAM {
	t.Helper()
	return managednet.NewIPAM(s, netip.MustParsePrefix(subnet), func() time.Time { return now })
}

func TestAllocateGivesTheLowestFreeAddressWithItsMAC(t *testing.T) {
	s := newStore(t, "10.203.118.0/24")
	ipam := newIPAM(t, s, "10.203.118.0/24")

	first, err := ipam.Allocate("lxc/101", "mac:a", 1)
	require.NoError(t, err)
	require.Equal(t, store.Lease{
		MAC: "06:50:43:4f:76:02", Addr: netip.MustParseAddr("10.203.118.2"),
		Owner: "lxc/101", Identity: "mac:a", NIC: 1, Since: now, Rev: first.Rev,
	}, first, ".0 is the subnet and .1 the leg")

	second, err := ipam.Allocate("qemu/102", "uuid:b", 2)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.203.118.3"), second.Addr)
	require.Equal(t, "06:50:43:4f:76:03", second.MAC)

	// A gap below the highest lease is filled first.
	require.NoError(t, s.DeleteLease(first.MAC))
	third, err := ipam.Allocate("lxc/103", "mac:c", 1)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), third.Addr)

	leases, err := s.Leases()
	require.NoError(t, err)
	require.Len(t, leases, 2)
	require.Equal(t, "lxc/103", leases["06:50:43:4f:76:02"].Owner)
}

func TestAllocateIsIdempotentForOneCard(t *testing.T) {
	s := newStore(t, "10.203.118.0/24")
	ipam := newIPAM(t, s, "10.203.118.0/24")
	first, err := ipam.Allocate("lxc/101", "mac:a", 1)
	require.NoError(t, err)
	again, err := ipam.Allocate("lxc/101", "mac:a", 1)
	require.NoError(t, err)
	require.Equal(t, first, again, "a cycle that failed after the allocation gets the same lease back")

	other, err := ipam.Allocate("lxc/101", "mac:other", 1)
	require.NoError(t, err)
	require.NotEqual(t, first.Addr, other.Addr, "a guest restored over the vmid is another guest")
}

// The MAC names the address by its last two octets, which tell the address
// apart in a subnet as large as a /16.
func TestAllocateDerivesTheMACFromTheLastTwoOctets(t *testing.T) {
	s := newStore(t, "172.16.0.0/16")
	ipam := newIPAM(t, s, "172.16.0.0/16")
	for i := 2; i <= 254; i++ {
		addr := netip.AddrFrom4([4]byte{172, 16, 0, byte(i)})
		require.NoError(t, s.SaveLease(store.Lease{MAC: store.ManagedMAC(addr), Addr: addr, Owner: "lxc/100", Since: now}, 0))
	}
	l, err := ipam.Allocate("lxc/101", "mac:a", 1)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("172.16.0.255"), l.Addr, "in a /16, .255 of the first block is no broadcast address")
	require.Equal(t, "06:50:43:4f:00:ff", l.MAC)
	l, err = ipam.Allocate("lxc/102", "mac:b", 1)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("172.16.1.0"), l.Addr)
	require.Equal(t, "06:50:43:4f:01:00", l.MAC)
}

func TestLowestFreeSkipsTheSubnetTheLegAndBroadcast(t *testing.T) {
	subnet := netip.MustParsePrefix("10.203.118.0/30")
	got, ok := managednet.LowestFree(subnet, nil)
	require.True(t, ok)
	require.Equal(t, netip.MustParseAddr("10.203.118.2"), got)
	require.Equal(t, 1, managednet.Available(subnet, nil))

	leases := map[string]store.Lease{"06:50:43:4f:76:02": {MAC: "06:50:43:4f:76:02", Addr: netip.MustParseAddr("10.203.118.2")}}
	_, ok = managednet.LowestFree(subnet, leases)
	require.False(t, ok, ".3 is the broadcast address")
	require.Equal(t, 0, managednet.Available(subnet, leases))

	// A lease outside the subnet takes no address of it.
	outside := map[string]store.Lease{"06:50:43:4f:00:05": {MAC: "06:50:43:4f:00:05", Addr: netip.MustParseAddr("10.9.0.5")}}
	require.Equal(t, 253, managednet.Available(netip.MustParsePrefix("10.203.118.0/24"), outside))
}

func TestAllocateExhausted(t *testing.T) {
	s := newStore(t, "10.203.118.0/28")
	ipam := newIPAM(t, s, "10.203.118.0/28")
	for i := range 13 {
		l, err := ipam.Allocate(model.GuestRef{Kind: model.KindLXC, VMID: 100 + i}.String(), "mac:a", 1)
		require.NoError(t, err)
		require.Equal(t, netip.AddrFrom4([4]byte{10, 203, 118, byte(2 + i)}), l.Addr)
	}
	_, err := ipam.Allocate("lxc/200", "mac:a", 1)
	require.ErrorIs(t, err, managednet.ErrExhausted)
	require.EqualError(t, err, "no free address on the managed network: 10.203.118.0/28 holds 13 leases, from 10.203.118.2 to 10.203.118.14")
	free, err := ipam.Free()
	require.NoError(t, err)
	require.Equal(t, 0, free)
}

// racing takes the address a lease is saved for first, as another writer
// would between the read and the write.
type racing struct {
	*store.Store
	raced bool
}

func (r *racing) SaveLease(l store.Lease, rev int64) error {
	if !r.raced {
		r.raced = true
		other := l
		other.Owner = "lxc/999"
		if err := r.Store.SaveLease(other, 0); err != nil {
			return err
		}
	}
	return r.Store.SaveLease(l, rev)
}

func TestAllocateTakesTheNextAddressWhenAnotherWriterWasFirst(t *testing.T) {
	s := &racing{Store: newStore(t, "10.203.118.0/24")}
	ipam := newIPAM(t, s, "10.203.118.0/24")
	l, err := ipam.Allocate("lxc/101", "mac:a", 1)
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("10.203.118.3"), l.Addr)
	leases, err := s.Leases()
	require.NoError(t, err)
	require.Equal(t, "lxc/999", leases["06:50:43:4f:76:02"].Owner, "the other writer's lease stands")
}

type failing struct{ *store.Store }

func (failing) SaveLease(store.Lease, int64) error { return errors.New("pmxcfs is read-only") }

func TestAllocateSaysWhyTheLeaseWasNotSaved(t *testing.T) {
	ipam := newIPAM(t, failing{newStore(t, "10.203.118.0/24")}, "10.203.118.0/24")
	_, err := ipam.Allocate("lxc/101", "mac:a", 1)
	require.EqualError(t, err, "saving the lease of 10.203.118.2 for lxc/101: pmxcfs is read-only")
}

func TestReleaseOnlyAfterTheCardIsGone(t *testing.T) {
	s := newStore(t, "10.203.118.0/24")
	ipam := newIPAM(t, s, "10.203.118.0/24")
	l, err := ipam.Allocate(web, "mac:a", 1)
	require.NoError(t, err)
	a := store.Attachment{Owner: web, Identity: "mac:a", NIC: 1, MAC: l.MAC, Addr: l.Addr, Consent: managednet.ByTag, Since: now}
	g := onManaged(guest(101), 1, l.MAC)

	err = ipam.Release(a, []model.Guest{g})
	require.ErrorIs(t, err, managednet.ErrCardPresent)
	require.EqualError(t, err, "releasing the lease of lxc/101: its card is still there: its removal was not asked for yet")

	removed := now
	a.RemovedAt = &removed
	err = ipam.Release(a, []model.Guest{g})
	require.ErrorIs(t, err, managednet.ErrCardPresent)
	require.EqualError(t, err, "releasing the lease of lxc/101: its card is still there: lxc/101 lists net1 with the MAC "+l.MAC)

	unreadable := onManaged(guest(101), 1, "")
	require.ErrorIs(t, ipam.Release(a, []model.Guest{unreadable}), managednet.ErrCardPresent, "a card at the index whose MAC could not be read may be the one")

	moved := onManaged(guest(101), 3, l.MAC)
	err = ipam.Release(a, []model.Guest{moved})
	require.ErrorIs(t, err, managednet.ErrCardPresent, "the MAC at another index still answers for the address")
	require.EqualError(t, err, "releasing the lease of lxc/101: its card is still there: lxc/101 lists net3 with the MAC "+l.MAC)

	clone := onManaged(guest(102), 2, l.MAC)
	err = ipam.Release(a, []model.Guest{guest(101), clone})
	require.ErrorIs(t, err, managednet.ErrCardPresent, "a clone that carries the MAC answers for the address as well")
	require.EqualError(t, err, "releasing the lease of lxc/101: its card is still there: lxc/102 lists net2 with the MAC "+l.MAC)

	leases, err := s.Leases()
	require.NoError(t, err)
	require.Contains(t, leases, l.MAC, "nothing was released")

	// The admin's own card at the index, on any bridge, is not the one that
	// went: the lease is released, and released again without complaint.
	elsewhere := guest(101)
	elsewhere.NICs = append(elsewhere.NICs, model.NIC{Index: 1, MAC: "bc:24:11:00:00:09", Bridge: "vmbr1"})
	for name, gone := range map[string][]model.Guest{
		"the admin's own card on the managed network took the index": {onManaged(guest(101), 1, cloneMAC)},
		"the admin's own card on another bridge took the index":      {elsewhere},
		"the index is free":                    {guest(101)},
		"another guest has another card there": {guest(101), onManaged(guest(102), 1, cloneMAC)},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, ipam.Release(a, gone))
			leases, err := s.Leases()
			require.NoError(t, err)
			require.NotContains(t, leases, l.MAC)
		})
	}
}

func TestReleaseOfAGuestThatIsGone(t *testing.T) {
	s := newStore(t, "10.203.118.0/24")
	ipam := newIPAM(t, s, "10.203.118.0/24")
	l, err := ipam.Allocate(web, "mac:a", 1)
	require.NoError(t, err)
	removed := now
	a := store.Attachment{Owner: web, Identity: "mac:a", NIC: 1, MAC: l.MAC, Addr: l.Addr, Consent: managednet.ByTag, Since: now, RemovedAt: &removed}
	require.NoError(t, ipam.Release(a, []model.Guest{guest(102)}))
	leases, err := s.Leases()
	require.NoError(t, err)
	require.Empty(t, leases)
}

// A lease belongs to its MAC: the release of an attachment never takes the
// lease another guest holds under that MAC.
func TestReleaseLeavesTheLeaseOfAnotherGuest(t *testing.T) {
	s := newStore(t, "10.203.118.0/24")
	ipam := newIPAM(t, s, "10.203.118.0/24")
	l, err := ipam.Allocate("lxc/999", "mac:z", 1)
	require.NoError(t, err)
	removed := now
	a := store.Attachment{Owner: web, Identity: "mac:a", NIC: 1, MAC: l.MAC, Addr: l.Addr, Consent: managednet.ByTag, Since: now, RemovedAt: &removed}
	err = ipam.Release(a, nil)
	require.EqualError(t, err, "the lease of 06:50:43:4f:76:02 belongs to lxc/999, not to lxc/101; it is kept")
	leases, err := s.Leases()
	require.NoError(t, err)
	require.Contains(t, leases, l.MAC)
}

// A guest migrated to another node keeps its lease: the lease is the MAC's,
// and the guest's node is no part of it.
func TestALeaseFollowsItsMAC(t *testing.T) {
	s := newStore(t, "10.203.118.0/24")
	ipam := newIPAM(t, s, "10.203.118.0/24")
	l, err := ipam.Allocate(web, "mac:a", 1)
	require.NoError(t, err)
	again, err := ipam.Allocate(web, "mac:a", 1)
	require.NoError(t, err)
	require.Equal(t, l, again)
	free, err := ipam.Free()
	require.NoError(t, err)
	require.Equal(t, 252, free)
}
