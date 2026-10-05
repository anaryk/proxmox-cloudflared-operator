package store

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxVLAN is the highest VLAN id; 0 is the untagged segment of a bridge.
const maxVLAN = 4094

// Segment is a bridge and VLAN on which routes at observed may be served once
// an admin acknowledged it. Its id is "<bridge>" or "<bridge>.<vlan>".
type Segment struct {
	Bridge         string    `json:"bridge"`
	VLAN           int       `json:"vlan,omitempty"`
	AcknowledgedAt time.Time `json:"acknowledgedAt"`
	By             string    `json:"by,omitempty"` // "cli" for now
}

// SegmentID returns the id of the segment of a bridge and a VLAN, 0 for none.
func SegmentID(bridge string, vlan int) string {
	if vlan == 0 {
		return bridge
	}
	return bridge + "." + strconv.Itoa(vlan)
}

// ID returns the id of the segment.
func (s Segment) ID() string { return SegmentID(s.Bridge, s.VLAN) }

func (s Segment) check() error {
	switch {
	case s.Bridge == "":
		return errors.New("segment has no bridge")
	case strings.IndexFunc(s.Bridge, func(r rune) bool { return !isBridgeChar(r) }) >= 0:
		return fmt.Errorf("segment bridge %q: want letters, digits, - and _", s.Bridge)
	case s.VLAN < 0 || s.VLAN > maxVLAN:
		return fmt.Errorf("segment %s: VLAN %d is not 0 to %d", s.Bridge, s.VLAN, maxVLAN)
	case s.AcknowledgedAt.IsZero():
		return fmt.Errorf("segment %s has no time it was acknowledged at", s.ID())
	}
	return nil
}

// isBridgeChar keeps the id of a segment unambiguous: a bridge name has no dot.
func isBridgeChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

// Segments returns the acknowledged segments by id. They live on the cluster
// root.
func (s *Store) Segments() (map[string]Segment, error) {
	out := make(map[string]Segment)
	err := eachObject(s.cluster, kindSegments, func(path, id string, seg Segment) error {
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

// SaveSegment stores an acknowledged segment under its id. A segment that
// would not change is not written.
func (s *Store) SaveSegment(seg Segment) error {
	if err := seg.check(); err != nil {
		return err
	}
	return s.cluster.put(kindSegments, seg.ID(), seg, true)
}

// DeleteSegment removes a segment by its id. A missing one is not an error.
func (s *Store) DeleteSegment(id string) error { return s.cluster.Delete(kindSegments, id) }
