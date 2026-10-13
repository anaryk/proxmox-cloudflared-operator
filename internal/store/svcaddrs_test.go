package store

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func allocate(t *testing.T, s *Store, network, real string) ServiceAddr {
	t.Helper()
	m, err := s.AllocateServiceAddr(network, addr(real), testOwner, t0)
	require.NoError(t, err)
	return m
}

func requirePool(t *testing.T, s *Store, next string, used int) {
	t.Helper()
	gotNext, gotUsed, size, err := s.ServicePool()
	require.NoError(t, err)
	require.Equal(t, addr(next), gotNext)
	require.Equal(t, used, gotUsed)
	require.Equal(t, 65280, size)
}

func TestServiceAddrsAreAllocatedFromTheBottomOfThePool(t *testing.T) {
	s, p := openStore(t)
	requirePool(t, s, "198.18.1.0", 0)
	require.Empty(t, stored(t, p.Cluster), "a look at the pool writes nothing")

	first, err := s.AllocateServiceAddr("vmbr1.20", addr("10.20.0.5"), testOwner, t0)
	require.NoError(t, err)
	require.Equal(t, ServiceAddr{Network: "vmbr1.20", Addr: addr("10.20.0.5"), Service: addr("198.18.1.0"), AllocatedAt: t0, ServiceOwner: testOwner}, first)
	second := allocate(t, s, "vmbr1.20", "10.20.0.6")
	require.Equal(t, addr("198.18.1.1"), second.Service)
	other := allocate(t, s, "vmbr2", "10.20.0.5")
	require.Equal(t, addr("198.18.1.2"), other.Service, "the same address on another network is another target")

	again, err := s.AllocateServiceAddr("vmbr1.20", addr("10.20.0.5"), testOwner, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, first, again, "a mapping that exists is returned as it is")
	requirePool(t, s, "198.18.1.3", 3)

	got, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []ServiceAddr{first, second, other}, got)
	require.Equal(t, []string{
		"meta/svcpool.json",
		"svcaddrs/vmbr1.20-10.20.0.5.json",
		"svcaddrs/vmbr1.20-10.20.0.6.json",
		"svcaddrs/vmbr2-10.20.0.5.json",
	}, stored(t, p.Cluster))
}

func TestAReleasedServiceAddrIsNeverGivenAgain(t *testing.T) {
	s, p := openStore(t)
	first := allocate(t, s, "vmbr1", "10.20.0.5")
	allocate(t, s, "vmbr1", "10.20.0.6")

	require.NoError(t, s.ReleaseServiceAddr("vmbr1", addr("10.20.0.5")))
	requireMissing(t, filepath.Join(p.Cluster, "svcaddrs", "vmbr1-10.20.0.5.json"))
	require.NoError(t, s.ReleaseServiceAddr("vmbr1", addr("10.20.0.5")), "a mapping that is gone is no error")
	requirePool(t, s, "198.18.1.2", 2)

	back := allocate(t, s, "vmbr1", "10.20.0.5")
	require.Equal(t, addr("198.18.1.2"), back.Service)
	require.NotEqual(t, first.Service, back.Service)

	require.NoError(t, s.ReleaseServiceAddr("vmbr1", addr("10.20.0.5")))
	require.NoError(t, s.ReleaseServiceAddr("vmbr1", addr("10.20.0.6")))
	got, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Empty(t, got)
	requirePool(t, s, "198.18.1.3", 3)
	require.Equal(t, []string{"meta/svcpool.json"}, stored(t, p.Cluster), "no tombstone stays behind")
}

func TestTheServicePoolRunsOutAtTheEndOfThePrefix(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.cluster.put(kindMeta, idSvcPool, servicePool{Next: addr("198.18.255.254")}, true))

	require.Equal(t, addr("198.18.255.254"), allocate(t, s, "vmbr1", "10.20.0.5").Service)
	last := allocate(t, s, "vmbr1", "10.20.0.6")
	require.Equal(t, addr("198.18.255.255"), last.Service)
	requirePool(t, s, "198.19.0.0", 65280)

	_, err := s.AllocateServiceAddr("vmbr1", addr("10.20.0.7"), testOwner, t0)
	require.ErrorIs(t, err, ErrPoolExhausted)
	requireMissing(t, filepath.Join(p.Cluster, "svcaddrs", "vmbr1-10.20.0.7.json"))
	requirePool(t, s, "198.19.0.0", 65280)

	again, err := s.AllocateServiceAddr("vmbr1", addr("10.20.0.6"), testOwner, t0)
	require.NoError(t, err, "a mapping that exists is found all the same")
	require.Equal(t, last, again)
}

func TestALostPoolStartsAboveTheMappings(t *testing.T) {
	s, p := openStore(t)
	allocate(t, s, "vmbr1", "10.20.0.5")
	allocate(t, s, "vmbr1", "10.20.0.6")
	require.NoError(t, s.ReleaseServiceAddr("vmbr1", addr("10.20.0.5")))
	require.NoError(t, os.Remove(filepath.Join(p.Cluster, "meta", "svcpool.json")))

	requirePool(t, s, "198.18.1.2", 2)
	require.Equal(t, addr("198.18.1.2"), allocate(t, s, "vmbr1", "10.20.0.7").Service)
}

func TestAPoolOutsideThePrefixIsAnError(t *testing.T) {
	for _, next := range []string{"10.0.0.1", "198.18.0.200", "198.19.0.1", "::1"} {
		t.Run(next, func(t *testing.T) {
			s, p := openStore(t)
			require.NoError(t, s.cluster.put(kindMeta, idSvcPool, servicePool{Next: addr(next)}, true))
			_, _, _, err := s.ServicePool()
			require.ErrorContains(t, err, "outside")
			_, err = s.AllocateServiceAddr("vmbr1", addr("10.20.0.5"), testOwner, t0)
			require.ErrorContains(t, err, "outside")
			require.Equal(t, []string{"meta/svcpool.json"}, stored(t, p.Cluster))
		})
	}
}

func TestAMappingOutsideThePoolIsAnError(t *testing.T) {
	s, p := openStore(t)
	allocate(t, s, "vmbr1", "10.20.0.5")
	writeFile(t, filepath.Join(p.Cluster, "svcaddrs", "vmbr1-10.20.0.6.json"), envelopeJSON("vmbr1-10.20.0.6",
		`{"network":"vmbr1","addr":"10.20.0.6","service":"10.20.0.6","allocatedAt":"2026-10-01T12:00:00Z"}`))

	_, err := s.ServiceAddrs()
	require.ErrorContains(t, err, "10.20.0.6 is not a service address")
	_, err = s.AllocateServiceAddr("vmbr1", addr("10.20.0.6"), testOwner, t0)
	require.ErrorContains(t, err, "is not a service address")
}

func TestAMappingStoredUnderAnotherIDIsAnError(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "svcaddrs", "vmbr1-10.20.0.6.json"), envelopeJSON("vmbr1-10.20.0.6",
		`{"network":"vmbr1","addr":"10.20.0.5","service":"198.18.1.0","allocatedAt":"2026-10-01T12:00:00Z"}`))
	_, err := s.ServiceAddrs()
	require.ErrorContains(t, err, `holds the service address of "vmbr1-10.20.0.5"`)
	_, err = s.AllocateServiceAddr("vmbr1", addr("10.20.0.6"), testOwner, t0)
	require.Error(t, err)
}

// A target that moved to another address keeps the service address it was
// published under: the connectors dial the same address, which reaches it
// where it is now, and the old address has none any more.
func TestAServiceAddrFollowsItsTargetToAnotherAddress(t *testing.T) {
	s, p := openStore(t)
	old := allocate(t, s, "vmbr1", "10.20.0.5")
	allocate(t, s, "vmbr2", "10.20.0.9")

	moved, err := s.MoveServiceAddr("vmbr1", addr("10.20.0.5"), "vmbr1", addr("10.20.0.6"), t0.Add(time.Hour))

	require.NoError(t, err)
	require.Equal(t, ServiceAddr{Network: "vmbr1", Addr: addr("10.20.0.6"), Service: old.Service, AllocatedAt: t0.Add(time.Hour), ServiceOwner: testOwner}, moved)
	requireMissing(t, filepath.Join(p.Cluster, "svcaddrs", "vmbr1-10.20.0.5.json"))
	got, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []ServiceAddr{moved, {Network: "vmbr2", Addr: addr("10.20.0.9"), Service: addr("198.18.1.1"), AllocatedAt: t0, ServiceOwner: testOwner}}, got)
	requirePool(t, s, "198.18.1.2", 2)

	again := allocate(t, s, "vmbr1", "10.20.0.5")
	require.Equal(t, addr("198.18.1.2"), again.Service, "the old address gets a new one")

	_, err = s.MoveServiceAddr("vmbr2", addr("10.20.0.9"), "vmbr3", addr("10.20.0.9"), t0)
	require.NoError(t, err, "to another network")
}

func TestMoveServiceAddrRefusesWhatWouldMapAnAddressTwice(t *testing.T) {
	s, _ := openStore(t)
	allocate(t, s, "vmbr1", "10.20.0.5")
	allocate(t, s, "vmbr1", "10.20.0.6")

	_, err := s.MoveServiceAddr("vmbr1", addr("10.20.0.5"), "vmbr1", addr("10.20.0.6"), t0)
	require.ErrorContains(t, err, "10.20.0.6 on vmbr1 has a service address already")
	_, err = s.MoveServiceAddr("vmbr1", addr("10.20.0.7"), "vmbr1", addr("10.20.0.8"), t0)
	require.ErrorContains(t, err, "10.20.0.7 on vmbr1 has no service address")
	_, err = s.MoveServiceAddr("vmbr1", addr("10.20.0.5"), "", addr("10.20.0.8"), t0)
	require.Error(t, err)

	got, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Len(t, got, 2, "nothing changed")
}

func TestAllocateServiceAddrRefusesWhatNamesNoMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		network string
		addr    netip.Addr
	}{
		"no network":   {"", addr("10.20.0.5")},
		"no address":   {"vmbr1", netip.Addr{}},
		"IPv6":         {"vmbr1", addr("fd00::5")},
		"IPv4 in IPv6": {"vmbr1", addr("::ffff:10.20.0.5")},
		"a path":       {"../x", addr("10.20.0.5")},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			_, err := s.AllocateServiceAddr(tc.network, tc.addr, testOwner, t0)
			require.Error(t, err)
			require.Error(t, s.ReleaseServiceAddr(tc.network, tc.addr))
			require.Error(t, s.SaveServiceAddr(ServiceAddr{Network: tc.network, Addr: tc.addr, Service: addr("198.18.1.0"), AllocatedAt: t0}))
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

func TestAServiceAddrIsSavedAsItWasWithoutMovingThePool(t *testing.T) {
	s, p := openStore(t)
	m := ServiceAddr{Network: "vmbr1.20", Addr: addr("10.20.0.5"), Service: addr("198.18.1.7"), AllocatedAt: t0}

	require.NoError(t, s.SaveServiceAddr(m))

	got, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []ServiceAddr{m}, got)
	require.Equal(t, []string{"svcaddrs/vmbr1.20-10.20.0.5.json"}, stored(t, p.Cluster), "the pool is not touched")
	requirePool(t, s, "198.18.1.8", 8)
}

func TestAServiceAddrOutsideThePoolIsNotSaved(t *testing.T) {
	s, p := openStore(t)

	err := s.SaveServiceAddr(ServiceAddr{Network: "vmbr1", Addr: addr("10.20.0.5"), Service: addr("10.20.0.5"), AllocatedAt: t0})

	require.ErrorContains(t, err, "is not a service address")
	require.Empty(t, stored(t, p.Cluster))
}

func TestAServiceAddrAnotherMappingHoldsIsNotSaved(t *testing.T) {
	s, p := openStore(t)
	first := allocate(t, s, "vmbr1", "10.20.0.5")
	require.Equal(t, addr("198.18.1.0"), first.Service)
	onNode, err := s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), testOwner, t0)
	require.NoError(t, err)
	require.Equal(t, addr("198.18.1.1"), onNode.Service)

	err = s.SaveServiceAddr(ServiceAddr{Network: "vmbr2", Addr: addr("10.20.0.5"), Service: addr("198.18.1.0"), AllocatedAt: t0})
	require.ErrorContains(t, err, "is the service address of svcaddrs/vmbr1-10.20.0.5 already")
	err = s.SaveServiceAddr(ServiceAddr{Network: "vmbr2", Addr: addr("10.20.0.5"), Service: addr("198.18.1.1"), AllocatedAt: t0})
	require.ErrorContains(t, err, "is the service address of nodesvcaddrs/vmbr9-pve-a-10.30.0.5 already")
	err = s.SaveNodeServiceAddr(NodeServiceAddr{Network: "vmbr9", Node: "pve-b", Addr: addr("10.30.0.5"), Service: addr("198.18.1.0"), AllocatedAt: t0})
	require.ErrorContains(t, err, "is the service address of svcaddrs/vmbr1-10.20.0.5 already")
	require.NoError(t, s.SaveServiceAddr(first), "the same mapping again is not another")
	require.NoError(t, s.SaveNodeServiceAddr(onNode))

	err = s.SaveServiceAddr(ServiceAddr{Network: "vmbr2", Addr: addr("10.20.0.5"), Service: addr("198.18.1.2"), AllocatedAt: t0})
	require.ErrorContains(t, err, "is not below 198.18.1.2, the next the pool gives out")
	err = s.SaveNodeServiceAddr(NodeServiceAddr{Network: "vmbr9", Node: "pve-b", Addr: addr("10.30.0.5"), Service: addr("198.18.1.9"), AllocatedAt: t0})
	require.ErrorContains(t, err, "is not below 198.18.1.2")
	require.NoError(t, os.Remove(filepath.Join(p.Cluster, "meta", "svcpool.json")))
	require.NoError(t, s.SaveServiceAddr(ServiceAddr{Network: "vmbr2", Addr: addr("10.20.0.5"), Service: addr("198.18.1.9"), AllocatedAt: t0}),
		"without a pool file the mappings alone say what is taken")
	got, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Len(t, got, 2)
}

func TestThePoolIsSavedAboveEveryMappingAndNeverMovesBack(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveServiceAddr(ServiceAddr{Network: "vmbr1", Addr: addr("10.20.0.5"), Service: addr("198.18.1.4"), AllocatedAt: t0}))
	require.NoError(t, s.SaveNodeServiceAddr(NodeServiceAddr{Network: "vmbr9", Node: "pve-a", Addr: addr("10.30.0.5"), Service: addr("198.18.1.6"), AllocatedAt: t0}))

	require.ErrorContains(t, s.SaveServicePool(addr("198.18.1.6")), "not above every service address", "a mapping holds it")
	require.NoError(t, s.SaveServicePool(addr("198.18.1.9")))
	requirePool(t, s, "198.18.1.9", 9)
	require.ErrorContains(t, s.SaveServicePool(addr("198.18.1.8")), "moves only forward")
	require.NoError(t, s.SaveServicePool(addr("198.18.1.9")), "the same again is no move back")
	for _, next := range []string{"10.0.0.1", "198.18.0.200", "198.19.0.1", "::1"} {
		require.ErrorContains(t, s.SaveServicePool(addr(next)), "outside", next)
	}
	requirePool(t, s, "198.18.1.9", 9)
	require.Equal(t, addr("198.18.1.9"), allocate(t, s, "vmbr1", "10.20.0.6").Service)
	require.Contains(t, stored(t, p.Cluster), "meta/svcpool.json")
}
