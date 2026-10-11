package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNodeServiceAddrsShareThePoolWithTheOthers(t *testing.T) {
	s, p := openStore(t)
	first := allocate(t, s, "vmbr1.20", "10.20.0.5")

	onA, err := s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), t0)
	require.NoError(t, err)
	require.Equal(t, NodeServiceAddr{Network: "vmbr9", Node: "pve-a", Addr: addr("10.30.0.5"), Service: addr("198.18.1.1"), AllocatedAt: t0}, onA)
	onB, err := s.AllocateNodeServiceAddr("vmbr9", "pve-b-2", addr("10.30.0.5"), t0)
	require.NoError(t, err)
	require.Equal(t, addr("198.18.1.2"), onB.Service, "the same address behind an isolated bridge of another node is another target")
	second := allocate(t, s, "vmbr1.20", "10.20.0.6")
	require.Equal(t, addr("198.18.1.3"), second.Service)
	requirePool(t, s, "198.18.1.4", 4)

	again, err := s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), t0.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, onA, again, "a mapping that exists is returned as it is")

	got, err := s.NodeServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []NodeServiceAddr{onA, onB}, got)
	all, err := s.ServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []ServiceAddr{first, second}, all, "the previous release's kind holds only its two-part keys")
	require.Equal(t, []string{
		"meta/svcpool.json",
		"nodesvcaddrs/vmbr9-pve-a-10.30.0.5.json",
		"nodesvcaddrs/vmbr9-pve-b-2-10.30.0.5.json",
		"svcaddrs/vmbr1.20-10.20.0.5.json",
		"svcaddrs/vmbr1.20-10.20.0.6.json",
	}, stored(t, p.Cluster))

	require.NoError(t, s.ReleaseNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5")))
	require.NoError(t, s.ReleaseNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5")), "a missing one is no error")
	got, err = s.NodeServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []NodeServiceAddr{onB}, got)
	requirePool(t, s, "198.18.1.4", 4)
}

func TestAPoolFileThatWasLostCountsTheMappingsOfEveryNode(t *testing.T) {
	s, p := openStore(t)
	_, err := s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), t0)
	require.NoError(t, err)
	_, err = s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.6"), t0)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(p.Cluster, "meta", "svcpool.json")))

	m := allocate(t, s, "vmbr1", "10.20.0.5")

	require.Equal(t, addr("198.18.1.2"), m.Service, "no address is given twice")
}

func TestANodeServiceAddrWhoseIDIsTakenByAnotherIsRefused(t *testing.T) {
	s, _ := openStore(t)
	_, err := s.AllocateNodeServiceAddr("vmbr9", "pve-a", addr("10.30.0.5"), t0)
	require.NoError(t, err)

	_, err = s.AllocateNodeServiceAddr("vmbr9-pve", "a", addr("10.30.0.5"), t0)

	require.ErrorContains(t, err, "vmbr9-pve-a-10.30.0.5")
	requirePool(t, s, "198.18.1.1", 1)
}

func TestAllocateNodeServiceAddrRefusesWhatNamesNoMapping(t *testing.T) {
	s, p := openStore(t)
	for _, tt := range []struct {
		name, network, node, real, want string
	}{
		{"no network", "", "pve1", "10.0.0.5", "network"},
		{"no node", "vmbr9", "", "10.0.0.5", "node"},
		{"a node with a dot", "vmbr9", "pve.lan", "10.0.0.5", `"pve.lan"`},
		{"IPv6", "vmbr9", "pve1", "fd00::5", "IPv4"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.AllocateNodeServiceAddr(tt.network, tt.node, addr(tt.real), t0)
			require.ErrorContains(t, err, tt.want)
			err = s.SaveNodeServiceAddr(NodeServiceAddr{Network: tt.network, Node: tt.node, Addr: addr(tt.real), Service: addr("198.18.1.0"), AllocatedAt: t0})
			require.ErrorContains(t, err, tt.want)
		})
	}
	require.Empty(t, stored(t, p.Cluster))
}

func TestANodeServiceAddrIsSavedAsItWasWithoutMovingThePool(t *testing.T) {
	s, p := openStore(t)
	m := NodeServiceAddr{Network: "vmbr9", Node: "pve-a", Addr: addr("10.30.0.5"), Service: addr("198.18.1.3"), AllocatedAt: t0}

	require.NoError(t, s.SaveNodeServiceAddr(m))

	got, err := s.NodeServiceAddrs()
	require.NoError(t, err)
	require.Equal(t, []NodeServiceAddr{m}, got)
	require.Equal(t, []string{"nodesvcaddrs/vmbr9-pve-a-10.30.0.5.json"}, stored(t, p.Cluster), "the pool is not touched")
	requirePool(t, s, "198.18.1.4", 4)

	err = s.SaveNodeServiceAddr(NodeServiceAddr{Network: "vmbr9", Node: "pve-a", Addr: addr("10.30.0.6"), Service: addr("10.30.0.6"), AllocatedAt: t0})
	require.ErrorContains(t, err, "is not a service address")
}
