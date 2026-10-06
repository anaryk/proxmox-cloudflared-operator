package store

import (
	"fmt"
)

// NodeSegment is a segment acknowledged on one node: vmbr0 on two nodes is
// not necessarily one LAN. Its id is "<node>-<bridge>[.<vlan>]".
//
// The acknowledgements of the previous release, one per segment, stay in the
// kind segments, which that release reads, until the upgrade is finished;
// until then one of them counts on every node, see SegmentsOn.
type NodeSegment struct {
	Node string `json:"node"`
	Segment
}

// NodeSegmentID returns the id of the segment of a bridge and a VLAN on a
// node.
func NodeSegmentID(node, bridge string, vlan int) string {
	return node + "-" + SegmentID(bridge, vlan)
}

// ID returns the id of the acknowledgement.
func (s NodeSegment) ID() string { return NodeSegmentID(s.Node, s.Bridge, s.VLAN) }

func (s NodeSegment) check() error {
	if err := checkNodeName("segment acknowledgement", s.Node); err != nil {
		return err
	}
	return s.Segment.check()
}

// NodeSegments returns the acknowledgements of segments on nodes, by id.
func (s *Store) NodeSegments() (map[string]NodeSegment, error) {
	out := make(map[string]NodeSegment)
	err := eachObject(s.cluster, kindNodeSegments, func(path, id string, seg NodeSegment) error {
		if seg.ID() != id {
			return fmt.Errorf("%s holds the segment %q, which is stored as %q", path, seg.ID(), id)
		}
		out[id] = seg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SaveNodeSegment stores the acknowledgement of a segment on a node. Node
// names and bridges may both hold a dash, so the id of one can be that of
// another: one stored under the id already that is of another node or
// segment is not replaced, and the save is refused.
func (s *Store) SaveNodeSegment(seg NodeSegment) error {
	if err := seg.check(); err != nil {
		return err
	}
	id := seg.ID()
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	var have NodeSegment
	found, err := s.cluster.Get(kindNodeSegments, id, &have)
	switch {
	case err != nil:
		return err
	case found && (have.Node != seg.Node || have.Bridge != seg.Bridge || have.VLAN != seg.VLAN):
		return fmt.Errorf("segment acknowledgement %s holds bridge %s of node %s; that of bridge %s of node %s cannot be kept under the same id",
			id, SegmentID(have.Bridge, have.VLAN), have.Node, SegmentID(seg.Bridge, seg.VLAN), seg.Node)
	}
	return s.cluster.put(kindNodeSegments, id, seg, true)
}

// DeleteNodeSegment removes the acknowledgement of a segment on a node by its
// id; one acknowledged by the previous release stays, see RevokeNodeSegment.
// A missing one is not an error.
func (s *Store) DeleteNodeSegment(id string) error { return s.cluster.Delete(kindNodeSegments, id) }

// SegmentsOn returns the segments acknowledged on a node, by the id of the
// segment: its own acknowledgements, and every one of the previous release.
func (s *Store) SegmentsOn(node string) (map[string]Segment, error) {
	out, err := s.Segments()
	if err != nil {
		return nil, err
	}
	own, err := s.NodeSegments()
	if err != nil {
		return nil, err
	}
	for _, seg := range own {
		if seg.Node == node {
			out[seg.Segment.ID()] = seg.Segment
		}
	}
	return out, nil
}

// RevokeNodeSegment takes the acknowledgement of a segment back on one node.
// An acknowledgement of the previous release would keep the segment
// acknowledged there, so it goes too; every other node of the registry that
// is not removed, and that it counted for, first gets one of its own with the
// same time, so that only this node loses it.
func (s *Store) RevokeNodeSegment(node, bridge string, vlan int) error {
	if err := checkNodeName("segment acknowledgement", node); err != nil {
		return err
	}
	id := SegmentID(bridge, vlan)
	segments, err := s.Segments()
	if err != nil {
		return err
	}
	if old, ok := segments[id]; ok {
		nodes, err := s.Nodes()
		if err != nil {
			return err
		}
		own, err := s.NodeSegments()
		if err != nil {
			return err
		}
		for _, n := range nodes {
			if n.Name == node || n.State == stateRemoved {
				continue
			}
			if _, has := own[NodeSegmentID(n.Name, bridge, vlan)]; has {
				continue
			}
			if err := s.SaveNodeSegment(NodeSegment{Node: n.Name, Segment: old}); err != nil {
				return err
			}
		}
		if err := s.DeleteSegment(id); err != nil {
			return err
		}
	}
	return s.DeleteNodeSegment(NodeSegmentID(node, bridge, vlan))
}
