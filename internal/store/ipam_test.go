package store

import (
	"errors"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func leaseOf(a string) Lease {
	addr := netip.MustParseAddr(a)
	return Lease{MAC: ManagedMAC(addr), Addr: addr, Owner: "lxc/105", Identity: "id-105", NIC: 1, Since: t0}
}

func TestManagedMAC(t *testing.T) {
	require.Equal(t, "06:50:43:4f:76:05", ManagedMAC(netip.MustParseAddr("10.203.118.5")))
	require.Equal(t, "06:50:43:4f:ff:fe", ManagedMAC(netip.MustParseAddr("10.50.255.254")))
	require.Equal(t, "06:50:43:4f:00:02", ManagedMAC(netip.MustParseAddr("172.16.0.2")))
}

func TestLeasesRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Leases()
	require.NoError(t, err)
	require.Empty(t, got)

	seen, released := t0.Add(time.Hour), t0.Add(2*time.Hour)
	l := leaseOf("10.203.118.5")
	l.MAC = "06:50:43:4F:76:05"
	l.LastDHCP, l.Released = &seen, &released
	require.NoError(t, s.SaveLease(l, 0))
	require.NoError(t, s.SaveLease(leaseOf("10.203.118.254"), 0))

	got, err = s.Leases()
	require.NoError(t, err)
	l.MAC, l.Rev = "06:50:43:4f:76:05", 1
	other := leaseOf("10.203.118.254")
	other.Rev = 1
	require.Equal(t, map[string]Lease{"06:50:43:4f:76:05": l, "06:50:43:4f:76:fe": other}, got)
	require.Equal(t, []string{"ipam/06-50-43-4f-76-05.json", "ipam/06-50-43-4f-76-fe.json"}, stored(t, p.Cluster))
	require.NotContains(t, string(readRaw(t, filepath.Join(p.Cluster, "ipam", "06-50-43-4f-76-05.json")).Data), `"rev"`,
		"the revision is the envelope's")

	require.NoError(t, s.DeleteLease("06:50:43:4F:76:05"))
	require.NoError(t, s.DeleteLease("06-50-43-4f-76-05"), "a lease that is gone is no error")
	require.Equal(t, []string{"ipam/06-50-43-4f-76-fe.json"}, stored(t, p.Cluster))
	require.Error(t, s.DeleteLease("not a mac"))
}

func TestSaveLeaseRefusesAnAddressAGuestCannotHave(t *testing.T) {
	edit := func(l Lease, f func(*Lease)) Lease {
		f(&l)
		return l
	}
	tests := map[string]struct {
		lease Lease
		msg   string
	}{
		"outside the subnet":        {leaseOf("10.203.119.5"), "not in the managed subnet 10.203.118.0/24"},
		"the subnet itself":         {leaseOf("10.203.118.0"), "the address of the managed subnet itself"},
		"the leg":                   {leaseOf("10.203.118.1"), "the address of pco's leg"},
		"the broadcast address":     {leaseOf("10.203.118.255"), "the broadcast address"},
		"no address":                {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.Addr = netip.Addr{} }), "not in the managed subnet"},
		"IPv4 in IPv6":              {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.Addr = netip.MustParseAddr("::ffff:10.203.118.5") }), "not in the managed subnet"},
		"the MAC of another card":   {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.MAC = "06:50:43:4f:76:06" }), "has the MAC 06:50:43:4f:76:05"},
		"a MAC of another prefix":   {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.MAC = "02:50:43:4f:76:05" }), "has the MAC 06:50:43:4f:76:05"},
		"no MAC":                    {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.MAC = "" }), "MAC"},
		"no owner":                  {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.Owner = "" }), "guest ref"},
		"an owner that is no guest": {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.Owner = "manual/x" }), "guest ref"},
		"a card above net31":        {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.NIC = 32 }), "net32"},
		"a negative card":           {edit(leaseOf("10.203.118.5"), func(l *Lease) { l.NIC = -1 }), "net-1"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			require.ErrorContains(t, s.SaveLease(tc.lease, 0), tc.msg)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

func TestALeaseIsCheckedAgainstTheManagedSubnetOfTheSettings(t *testing.T) {
	s, p := openStore(t)
	settings := DefaultSettings()
	settings.Managed.Subnet = netip.MustParsePrefix("10.50.0.0/16")
	require.NoError(t, s.SaveSettings(settings))

	require.NoError(t, s.SaveLease(leaseOf("10.50.3.4"), 0))
	require.NoError(t, s.SaveLease(leaseOf("10.50.255.254"), 0))
	require.Error(t, s.SaveLease(leaseOf("10.50.0.1"), 0))
	require.Error(t, s.SaveLease(leaseOf("10.50.255.255"), 0))
	require.Error(t, s.SaveLease(leaseOf("10.203.118.5"), 0))

	writeFile(t, filepath.Join(p.Cluster, "meta", "settings.json"), envelopeJSON("settings", `{"admission":"nobody"}`))
	require.ErrorContains(t, s.SaveLease(leaseOf("10.50.3.5"), 0), "admission", "settings that cannot be read refuse every lease")
}

func TestALeaseStoredUnderAnotherMACIsAnError(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "ipam", "06-50-43-4f-76-06.json"), envelopeJSON("06-50-43-4f-76-06",
		`{"mac":"06:50:43:4f:76:05","addr":"10.203.118.5","owner":"lxc/105","identity":"x","nic":1,"since":"2026-10-01T12:00:00Z"}`))
	_, err := s.Leases()
	require.ErrorContains(t, err, `holds the lease of "06:50:43:4f:76:05"`)
}

func TestSaveLeaseComparesAndSwaps(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Cluster, "ipam", "06-50-43-4f-76-05.json")
	l := leaseOf("10.203.118.5")
	require.NoError(t, s.SaveLease(l, 0))
	require.ErrorIs(t, s.SaveLease(l, 0), ErrRevision, "a lease that exists is no new one")

	seen := t0.Add(time.Minute)
	l.LastDHCP = &seen
	require.NoError(t, s.SaveLease(l, 1))
	require.EqualValues(t, 2, revOf(t, path))

	before := readRaw(t, path)
	require.ErrorIs(t, s.SaveLease(leaseOf("10.203.118.5"), 1), ErrRevision)
	require.Equal(t, before, readRaw(t, path), "nothing is written")

	require.ErrorIs(t, s.SaveLease(leaseOf("10.203.118.6"), 3), ErrRevision, "a new lease has no revision yet")
	requireMissing(t, filepath.Join(p.Cluster, "ipam", "06-50-43-4f-76-06.json"))
}

// The engine sets Released when the grace of a detach starts, the DHCP server
// LastDHCP when a guest renews; each reads the lease, changes its field and
// saves. Neither may undo the other's write.
func TestTwoWritersOfALeaseDoNotLoseEachOthersWrite(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveLease(leaseOf("10.203.118.5"), 0))
	const mac = "06:50:43:4f:76:05"
	read := func() Lease {
		t.Helper()
		got, err := s.Leases()
		require.NoError(t, err)
		return got[mac]
	}

	engine, dhcp := read(), read()
	released, renewed := t0.Add(time.Minute), t0.Add(2*time.Minute)
	engine.Released = &released
	require.NoError(t, s.SaveLease(engine, engine.Rev))
	dhcp.LastDHCP = &renewed
	require.ErrorIs(t, s.SaveLease(dhcp, dhcp.Rev), ErrRevision, "the second writer read before the first wrote")
	again := read()
	again.LastDHCP = &renewed
	require.NoError(t, s.SaveLease(again, again.Rev))
	got := read()
	require.Equal(t, &released, got.Released)
	require.Equal(t, &renewed, got.LastDHCP)

	// The same with both at once, each trying again on a stale revision.
	set := func(f func(*Lease)) {
		for {
			l := read()
			f(&l)
			err := s.SaveLease(l, l.Rev)
			if !errors.Is(err, ErrRevision) {
				require.NoError(t, err)
				return
			}
		}
	}
	for i := range 20 {
		at := t0.Add(time.Duration(i) * time.Hour)
		var wg sync.WaitGroup
		wg.Go(func() { set(func(l *Lease) { l.Released = &at }) })
		wg.Go(func() { set(func(l *Lease) { l.LastDHCP = &at }) })
		wg.Wait()
		got := read()
		require.Equal(t, &at, got.Released)
		require.Equal(t, &at, got.LastDHCP)
	}
}

// A lease the engine deleted does not come back with a write made from a
// copy read before.
func TestADeletedLeaseIsNotWrittenAgainFromAnOldCopy(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveLease(leaseOf("10.203.118.5"), 0))
	got, err := s.Leases()
	require.NoError(t, err)
	old := got["06:50:43:4f:76:05"]

	require.NoError(t, s.DeleteLease(old.MAC))
	seen := t0.Add(time.Minute)
	old.LastDHCP = &seen
	require.ErrorIs(t, s.SaveLease(old, old.Rev), ErrRevision)
	require.Empty(t, stored(t, p.Cluster))
}
