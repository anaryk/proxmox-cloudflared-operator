package engine

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// maxVLAN is the highest VLAN id; 0 is the untagged segment of a bridge.
const maxVLAN = 4094

// SegmentView is a bridge and VLAN that routes at observed were proven on,
// or that an admin acknowledged.
type SegmentView struct {
	Bridge         string    `json:"bridge"`
	VLAN           int       `json:"vlan,omitempty"`
	Acknowledged   bool      `json:"acknowledged"`
	AcknowledgedAt time.Time `json:"acknowledgedAt,omitzero"`
	Routes         int       `json:"routes"` // routes at observed proven on it in the last cycle
}

// SegmentArg names a segment as pco segment takes it: "vmbr1", "vmbr1:20".
func SegmentArg(bridge string, vlan int) string {
	if vlan == 0 {
		return bridge
	}
	return bridge + ":" + strconv.Itoa(vlan)
}

// SegmentReason is the reason of a route held on a segment nobody
// acknowledged.
func SegmentReason(bridge string, vlan int) string {
	return fmt.Sprintf("segment %s is not acknowledged; pco segment acknowledge %s",
		resolve.Segment{Bridge: bridge, VLAN: vlan}, SegmentArg(bridge, vlan))
}

// segmentViews lists the segments this cycle saw at observed, with how many
// routes each, and those acknowledged, by bridge and VLAN.
func (c *cycleRun) segmentViews(seen map[resolve.Segment]int) []SegmentView {
	return segmentList(seen, c.segments)
}

func segmentList(seen map[resolve.Segment]int, acked map[string]store.Segment) []SegmentView {
	byID := map[string]SegmentView{}
	for seg, n := range seen {
		byID[store.SegmentID(seg.Bridge, seg.VLAN)] = SegmentView{Bridge: seg.Bridge, VLAN: seg.VLAN, Routes: n}
	}
	for id, s := range acked {
		v := byID[id]
		v.Bridge, v.VLAN, v.Acknowledged, v.AcknowledgedAt = s.Bridge, s.VLAN, true, s.AcknowledgedAt
		byID[id] = v
	}
	return slices.SortedFunc(maps.Values(byID), func(a, b SegmentView) int {
		return cmp.Or(strings.Compare(a.Bridge, b.Bridge), cmp.Compare(a.VLAN, b.VLAN))
	})
}

// Segments returns the segments the last cycle saw at observed, with the
// acknowledgements the store has now.
func (e *Engine) Segments() ([]SegmentView, error) {
	acked, err := e.d.Store.Segments()
	if err != nil {
		return nil, fmt.Errorf("reading the acknowledged segments: %w", err)
	}
	seen := map[resolve.Segment]int{}
	for _, v := range e.State().Segments {
		if v.Routes > 0 {
			seen[resolve.Segment{Bridge: v.Bridge, VLAN: v.VLAN}] = v.Routes
		}
	}
	return segmentList(seen, acked), nil
}

// AcknowledgeSegment lets routes at observed be served on a bridge and VLAN
// from the next cycle. A segment acknowledged already stays as it was.
func (e *Engine) AcknowledgeSegment(ctx context.Context, bridge string, vlan int) error {
	if err := checkSegment(bridge, vlan); err != nil {
		return err
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return err
	}
	defer e.release()

	acked, err := e.d.Store.Segments()
	if err != nil {
		return fmt.Errorf("reading the acknowledged segments: %w", err)
	}
	if _, ok := acked[store.SegmentID(bridge, vlan)]; ok {
		return nil
	}
	if err := e.d.Store.SaveSegment(store.Segment{Bridge: bridge, VLAN: vlan, AcknowledgedAt: e.d.Now(), By: "cli"}); err != nil {
		return fmt.Errorf("saving the segment: %w", err)
	}
	e.adminEvent(ctx, SegmentArg(bridge, vlan), fmt.Sprintf("segment %s is acknowledged; the routes at observed on it are served from the next cycle",
		resolve.Segment{Bridge: bridge, VLAN: vlan}))
	e.Trigger()
	return nil
}

// RevokeSegment takes the acknowledgement of a segment back: its routes at
// observed are held from the next cycle.
func (e *Engine) RevokeSegment(ctx context.Context, bridge string, vlan int) error {
	if err := checkSegment(bridge, vlan); err != nil {
		return err
	}
	if err := e.acquireAdmin(ctx); err != nil {
		return err
	}
	defer e.release()

	seg := resolve.Segment{Bridge: bridge, VLAN: vlan}
	acked, err := e.d.Store.Segments()
	if err != nil {
		return fmt.Errorf("reading the acknowledged segments: %w", err)
	}
	id := store.SegmentID(bridge, vlan)
	if _, ok := acked[id]; !ok {
		return fmt.Errorf("%w: segment %s is not acknowledged", ErrNotFound, seg)
	}
	if err := e.d.Store.DeleteSegment(id); err != nil {
		return fmt.Errorf("removing the segment: %w", err)
	}
	e.adminEvent(ctx, SegmentArg(bridge, vlan), fmt.Sprintf("the acknowledgement of segment %s is revoked; the routes at observed on it are held from the next cycle", seg))
	e.Trigger()
	return nil
}

// checkSegment refuses what is not a bridge and a VLAN, as the store keeps
// them.
func checkSegment(bridge string, vlan int) error {
	switch {
	case bridge == "":
		return fmt.Errorf("%w: a segment needs a bridge", ErrInvalid)
	case strings.IndexFunc(bridge, func(r rune) bool { return !isBridgeChar(r) }) >= 0:
		return fmt.Errorf("%w: bridge %q: want letters, digits, - and _; give the VLAN as vmbr1:20", ErrInvalid, bridge)
	case vlan < 0 || vlan > maxVLAN:
		return fmt.Errorf("%w: VLAN %d is not 0 to %d", ErrInvalid, vlan, maxVLAN)
	}
	return nil
}

func isBridgeChar(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}
