package store

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestNodeAddrsAreEmptyUntilSaved(t *testing.T) {
	s, _ := openStore(t)

	got, err := s.NodeAddrs()

	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
}

func TestNodeAddrsComeBackSortedWithoutDuplicates(t *testing.T) {
	s, p := openStore(t)

	require.NoError(t, s.SaveNodeAddrs(addrs("10.0.0.3", "10.0.0.2", "10.0.0.3")))

	got, err := s.NodeAddrs()
	require.NoError(t, err)
	require.Equal(t, addrs("10.0.0.2", "10.0.0.3"), got)
	require.Equal(t, []string{"meta/node-addrs.json"}, stored(t, p.Local))
	require.Empty(t, stored(t, p.Cluster), "the addresses are this node's own")
}

func TestSaveNodeAddrsWritesOnlyOnChange(t *testing.T) {
	s, p := openStore(t)
	path := filepath.Join(p.Local, "meta", "node-addrs.json")

	require.NoError(t, s.SaveNodeAddrs(addrs("10.0.0.2", "10.0.0.3")))
	require.NoError(t, s.SaveNodeAddrs(addrs("10.0.0.3", "10.0.0.2")))
	require.EqualValues(t, 1, revOf(t, path), "the same set in another order is no change")

	require.NoError(t, s.SaveNodeAddrs(addrs("10.0.0.2", "10.0.0.3", "10.0.0.4")))
	require.EqualValues(t, 2, revOf(t, path))
}

func TestSaveNodeAddrsRefusesAnInvalidAddress(t *testing.T) {
	s, p := openStore(t)

	err := s.SaveNodeAddrs([]netip.Addr{netip.MustParseAddr("10.0.0.2"), {}})

	require.Error(t, err)
	require.Empty(t, stored(t, p.Local))
}

func TestNodeAddrsWorkWhileTheClusterFilesystemIsNotMounted(t *testing.T) {
	p, marker := mountedPaths(t)
	s, err := Open(p)
	require.NoError(t, err)
	require.NoError(t, s.Init())
	require.NoError(t, os.Remove(marker))

	require.NoError(t, s.SaveNodeAddrs(addrs("10.0.0.2")))
	got, err := s.NodeAddrs()

	require.NoError(t, err)
	require.Equal(t, addrs("10.0.0.2"), got)
}

func TestNodeAddrsThatCannotBeReadAreAnError(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Local, "meta", "node-addrs.json"), `{"schemaVersion":1,"rev":1,"id":"node-addrs","data":["10.0.0.2",`)

	_, err := s.NodeAddrs()

	require.Error(t, err)
}
