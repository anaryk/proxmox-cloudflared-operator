package store

import (
	"fmt"
	"net/netip"
	"slices"
)

const idNodeAddrs = "node-addrs"

// nodeAddrs are the node addresses: on the node-local root, and as they are
// on the cluster root.
var nodeAddrs = copied[[]netip.Addr]{localID: idNodeAddrs, clusterID: "nodeaddrs", what: "node addresses", shared: same[[]netip.Addr]}

// NodeAddrs returns the addresses of the cluster nodes this node has learned,
// sorted, and an empty list when none were saved. See LoadNodeAddrs.
func (s *Store) NodeAddrs() ([]netip.Addr, error) {
	got, _, err := s.LoadNodeAddrs()
	return got, err
}

// LoadNodeAddrs is NodeAddrs with notes. The addresses are kept on the
// node-local root, so that after a restart a node that is offline still
// counts with more than its cluster address, and, once SetNode named the
// node, on the cluster root, where a controller on another node finds them.
// A note says that the copy there was made again, as LoadEngineMemory does.
func (s *Store) LoadNodeAddrs() ([]netip.Addr, []string, error) {
	_, got, notes, err := nodeAddrs.read(s)
	if err != nil {
		return nil, nil, err
	}
	return sortedAddrs(got), notes, nil
}

// SaveNodeAddrs stores the node addresses, sorted and without duplicates,
// unless they are what is stored already. An address that is not valid is
// refused and nothing is written.
func (s *Store) SaveNodeAddrs(addrs []netip.Addr) error {
	for _, a := range addrs {
		if !a.IsValid() {
			return fmt.Errorf("node address %v is not valid", a)
		}
	}
	return nodeAddrs.write(s, sortedAddrs(addrs))
}

func sortedAddrs(addrs []netip.Addr) []netip.Addr {
	out := slices.Clone(addrs)
	slices.SortFunc(out, netip.Addr.Compare)
	out = slices.Compact(out)
	if out == nil {
		out = []netip.Addr{}
	}
	return out
}
