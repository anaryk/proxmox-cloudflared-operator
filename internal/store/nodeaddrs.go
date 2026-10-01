package store

import (
	"fmt"
	"net/netip"
	"slices"
)

const idNodeAddrs = "node-addrs"

// NodeAddrs returns the addresses of the cluster nodes this node has learned,
// sorted, and an empty list when none were saved. They are kept on the
// node-local root, so that after a restart a node that is offline still counts
// with more than its cluster address.
func (s *Store) NodeAddrs() ([]netip.Addr, error) {
	got, _, err := getOne[[]netip.Addr](s.local, kindMeta, idNodeAddrs)
	if err != nil {
		return nil, err
	}
	return sortedAddrs(got), nil
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
	return s.local.put(kindMeta, idNodeAddrs, sortedAddrs(addrs), true)
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
