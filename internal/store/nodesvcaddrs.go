package store

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"time"
)

// NodeServiceAddr maps a real address behind an isolated bridge of one node
// to its service address: two isolated bridges of one name on two nodes can
// hold the same address, which are two targets. The mappings of networks
// that are one segment on every node, and of the managed network, are
// ServiceAddrs. These live in a kind of their own, since the previous release
// checks every file of svcaddrs against its two-part key. Both kinds take
// their addresses from the one pool, which gives none twice.
type NodeServiceAddr struct {
	Network     string     `json:"network"`
	Node        string     `json:"node"`
	Addr        netip.Addr `json:"addr"`
	Service     netip.Addr `json:"service"`
	AllocatedAt time.Time  `json:"allocatedAt"`
}

// nodeServiceAddrID is the id the mapping of a real address on a network of
// a node is stored under.
func nodeServiceAddrID(network, node string, addr netip.Addr) string {
	return network + "-" + node + "-" + addr.String()
}

// checkNodeServiceKey refuses what names no mapping.
func checkNodeServiceKey(network, node string, addr netip.Addr) error {
	if err := checkNodeName("service address on "+network, node); err != nil {
		return err
	}
	if err := checkServiceKey(network, addr); err != nil {
		return err
	}
	if _, err := FileName(nodeServiceAddrID(network, node, addr)); err != nil {
		return fmt.Errorf("service address on %s of node %s: %w", network, node, err)
	}
	return nil
}

// NodeServiceAddrs returns the mappings of the networks of single nodes, by
// network, then node, then address.
func (s *Store) NodeServiceAddrs() ([]NodeServiceAddr, error) {
	out := []NodeServiceAddr{}
	err := eachObject(s.cluster, kindNodeSvcAddrs, func(path, id string, m NodeServiceAddr) error {
		if want := nodeServiceAddrID(m.Network, m.Node, m.Addr); want != id {
			return fmt.Errorf("%s holds the service address of %q, which is stored as %q", path, want, id)
		}
		if err := checkMapping(path, ServiceAddr{Service: m.Service}); err != nil {
			return err
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b NodeServiceAddr) int {
		return cmp.Or(cmp.Compare(a.Network, b.Network), cmp.Compare(a.Node, b.Node), a.Addr.Compare(b.Addr))
	})
	return out, nil
}

// AllocateNodeServiceAddr returns the mapping of (network, node, addr),
// making it from the pool of AllocateServiceAddr when there is none. A
// mapping stored under its id that is of another network, node or address,
// whose names make the same id, is refused.
func (s *Store) AllocateNodeServiceAddr(network, node string, addr netip.Addr, now time.Time) (NodeServiceAddr, error) {
	if err := checkNodeServiceKey(network, node, addr); err != nil {
		return NodeServiceAddr{}, err
	}
	id := nodeServiceAddrID(network, node, addr)
	s.netMu.Lock()
	defer s.netMu.Unlock()

	var m NodeServiceAddr
	found, err := s.cluster.Get(kindNodeSvcAddrs, id, &m)
	switch {
	case err != nil:
		return NodeServiceAddr{}, err
	case found && (m.Network != network || m.Node != node || m.Addr != addr):
		return NodeServiceAddr{}, fmt.Errorf("service address %s holds the mapping of %v on %s of node %s", id, m.Addr, m.Network, m.Node)
	case found:
		if err := checkMapping("service address "+id, ServiceAddr{Service: m.Service}); err != nil {
			return NodeServiceAddr{}, err
		}
		return m, nil
	}
	service, err := s.takeFromPool()
	if err != nil {
		return NodeServiceAddr{}, err
	}
	m = NodeServiceAddr{Network: network, Node: node, Addr: addr, Service: service, AllocatedAt: now}
	if err := s.cluster.put(kindNodeSvcAddrs, id, m, true); err != nil {
		return NodeServiceAddr{}, err
	}
	return m, nil
}

// SaveNodeServiceAddr stores a mapping as it was, as a restore does: the pool
// does not move. What SaveServiceAddr refuses, it refuses too.
func (s *Store) SaveNodeServiceAddr(m NodeServiceAddr) error {
	if err := checkNodeServiceKey(m.Network, m.Node, m.Addr); err != nil {
		return err
	}
	id := nodeServiceAddrID(m.Network, m.Node, m.Addr)
	if err := checkMapping("service address "+id, ServiceAddr{Service: m.Service}); err != nil {
		return err
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	if err := s.serviceFree(kindNodeSvcAddrs, id, m.Service); err != nil {
		return err
	}
	return s.cluster.put(kindNodeSvcAddrs, id, m, true)
}

// ReleaseNodeServiceAddr deletes the mapping; the pool does not move back. A
// mapping that is not there is no error.
func (s *Store) ReleaseNodeServiceAddr(network, node string, addr netip.Addr) error {
	if err := checkNodeServiceKey(network, node, addr); err != nil {
		return err
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	return s.cluster.Delete(kindNodeSvcAddrs, nodeServiceAddrID(network, node, addr))
}
