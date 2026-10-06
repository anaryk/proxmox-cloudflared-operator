package store

import (
	"fmt"
	"regexp"
)

// NodeRev is a node entry with the revision of its file.
type NodeRev struct {
	Node NodeEntry
	Rev  int64
}

// NodesRev is Nodes with the revision of each entry, which SaveNodeIf takes.
func (s *Store) NodesRev() ([]NodeRev, error) {
	out := []NodeRev{}
	err := eachRevision(s.cluster, kindNodes, func(path, id string, rev int64, n NodeEntry) error {
		if n.Name != id {
			return fmt.Errorf("%s holds the node %q, which is stored as %q", path, n.Name, id)
		}
		out = append(out, NodeRev{Node: n, Rev: rev})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SaveNodeIf is SaveNode for an entry read at revision rev, 0 for a new one:
// it is stored only while the stored one still has that revision, else the
// error is ErrRevision, with the revision it has. Two nodes set up at once,
// or a controller and a setup, cannot both write the entry they read. It
// returns the revision the entry has after.
func (s *Store) SaveNodeIf(n NodeEntry, rev int64) (int64, error) {
	if err := n.check(); err != nil {
		return 0, err
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if err := s.indexIsFree(n); err != nil {
		return 0, err
	}
	return s.cluster.putIf(kindNodes, n.Name, n, rev)
}

// indexIsFree refuses an entry whose index another node holds, removed ones
// too: a name keeps its index for as long as the install lives, and two
// nodes of one index would give their legs one MAC. The caller holds keyMu.
func (s *Store) indexIsFree(n NodeEntry) error {
	if n.Index == nil {
		return nil
	}
	nodes, err := s.Nodes()
	if err != nil {
		return err
	}
	for _, other := range nodes {
		if other.Name != n.Name && other.Index != nil && *other.Index == *n.Index {
			return fmt.Errorf("node %s: index %d is the index of node %s", n.Name, *n.Index, other.Name)
		}
	}
	return nil
}

// nodeNamePattern is what Proxmox takes as the name of a node: a host name
// of one label.
var nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// checkNodeName refuses what cannot be the name of a node; what names it in
// an error.
func checkNodeName(what, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%s names no node", what)
	case !nodeNamePattern.MatchString(name):
		return fmt.Errorf("%s: node %q: want letters, digits and dashes", what, name)
	}
	return nil
}
