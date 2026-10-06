package store

import "fmt"

// A few objects the previous release keeps on a node's own root are what a
// controller must inherit when the lease moves to another node: the engine
// memory, the node addresses and the soft deny list. This release keeps a
// copy of each on the cluster root beside the old object, which stays where
// the previous release reads it until the upgrade is finished, so that the
// previous release, installed again, serves the same routes.
//
// That release writes the old object only. A copy read blindly would then
// drop what it did, so a copy names the old object it was made from, by node
// and revision. A read takes the copy only while the old object on the node
// it names, when that is this node, is not at a higher revision; otherwise it
// makes the copy again from the old object, and says so. A write writes the
// old object first and then the copy.

// DerivedFrom names the old object a cluster copy was made from: the node
// whose own root holds it and the revision it had.
type DerivedFrom struct {
	Node string `json:"node"`
	Rev  int64  `json:"rev"`
}

// clusterCopy is the cluster copy of an object, with what it was made from.
type clusterCopy[T any] struct {
	DerivedFrom DerivedFrom `json:"derivedFrom"`
	Value       T           `json:"value"`
}

// SetNode tells the store the name of the node it runs on. Only a store that
// knows it reads and writes the cluster copies; one that does not keeps the
// old objects alone, as the previous release does. Call it before the store
// is used.
func (s *Store) SetNode(name string) { s.node.Store(&name) }

// Node returns the name SetNode gave, or "".
func (s *Store) Node() string {
	if p := s.node.Load(); p != nil {
		return *p
	}
	return ""
}

// copied is an object kept twice: localID on the local root, as the previous
// release keeps it, and clusterID on the cluster root, the copy. what names
// it in a note, and shared makes the copy of the old value.
type copied[T any] struct {
	localID, clusterID, what string
	shared                   func(T) T
}

// read returns the old object and the value to use: the copy, or the old
// object when the copy is behind it or missing, with a note when a copy had
// to be made again.
func (c copied[T]) read(s *Store) (old, value T, notes []string, err error) {
	rev, found, err := s.local.getRev(kindMeta, c.localID, &old, false)
	if err != nil {
		return old, value, nil, err
	}
	node := s.Node()
	if node == "" {
		return old, old, nil, nil
	}
	var stored clusterCopy[T]
	have, err := s.cluster.Get(kindMeta, c.clusterID, &stored)
	switch {
	case isDamaged(err):
		// Until the upgrade is finished the old object is the authority, and
		// the next write makes the copy anew.
		return old, old, []string{fmt.Sprintf("the cluster copy of the %s cannot be read, and node %s's own is used: %v", c.what, node, err)}, nil
	case err != nil:
		return old, value, nil, err
	case have && (stored.DerivedFrom.Node != node || rev <= stored.DerivedFrom.Rev):
		return old, stored.Value, nil, nil
	case !found:
		return old, old, nil, nil
	}
	err = s.cluster.put(kindMeta, c.clusterID, clusterCopy[T]{DerivedFrom: DerivedFrom{Node: node, Rev: rev}, Value: c.shared(old)}, true)
	switch {
	case have && err != nil:
		notes = append(notes, fmt.Sprintf("the cluster copy of the %s was made from revision %d of node %s's, which is at revision %d now (an earlier release wrote it?); it is read from there, and the copy could not be made again: %v",
			c.what, stored.DerivedFrom.Rev, node, rev, err))
	case have:
		notes = append(notes, fmt.Sprintf("the cluster copy of the %s was made from revision %d of node %s's, which is at revision %d now (an earlier release wrote it?); it was made again from there",
			c.what, stored.DerivedFrom.Rev, node, rev))
	case err != nil:
		notes = append(notes, fmt.Sprintf("the cluster copy of the %s could not be made: %v", c.what, err))
	}
	return old, old, notes, nil
}

// write stores v as the old object and then as the copy, each unless it is
// what is stored already. A copy that cannot be written after the old object
// was is a *CopyNote: what was saved is kept.
func (c copied[T]) write(s *Store, v T) error {
	rev, err := s.local.putRev(kindMeta, c.localID, v, true)
	if err != nil {
		return err
	}
	node := s.Node()
	if node == "" {
		return nil
	}
	if err := s.cluster.put(kindMeta, c.clusterID, clusterCopy[T]{DerivedFrom: DerivedFrom{Node: node, Rev: rev}, Value: c.shared(v)}, true); err != nil {
		return &CopyNote{What: c.what, Node: node, Err: err}
	}
	return nil
}

// CopyNote is what a save of an object kept twice returns when it stored the
// object on this node, where the previous release keeps it, and not its copy
// on the cluster root: what was saved is kept on this node, and is no failure
// to report as one; the copy waits for the next save that reaches it.
type CopyNote struct {
	What string // as "engine memory"
	Node string
	Err  error
}

func (n *CopyNote) Error() string {
	return fmt.Sprintf("the %s is saved on node %s, and its cluster copy is not: %v", n.What, n.Node, n.Err)
}

func (n *CopyNote) Unwrap() error { return n.Err }

// same is the copy of a value that is shared as it is.
func same[T any](v T) T { return v }
