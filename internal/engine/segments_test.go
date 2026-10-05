package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestASegmentIsAcknowledgedOnce(t *testing.T) {
	e := newEnv(t)
	since := e.clock.now()

	require.NoError(t, e.eng.AcknowledgeSegment(t.Context(), "vmbr1", 20))
	e.clock.advance(time.Minute)
	require.NoError(t, e.eng.AcknowledgeSegment(t.Context(), "vmbr1", 20))

	segments, err := e.store.Segments()
	require.NoError(t, err)
	require.Equal(t, map[string]store.Segment{"vmbr1.20": {Bridge: "vmbr1", VLAN: 20, AcknowledgedAt: t0, By: "cli"}}, segments,
		"the second time changes nothing")
	require.Equal(t, []string{
		"vmbr1:20: segment vmbr1 VLAN 20 is acknowledged; the routes at observed on it are served from the next cycle",
	}, adminEvents(e, since.Add(-time.Nanosecond)))

	require.NoError(t, e.eng.RevokeSegment(t.Context(), "vmbr1", 20))
	segments, err = e.store.Segments()
	require.NoError(t, err)
	require.Empty(t, segments)
	require.Equal(t, "vmbr1:20: the acknowledgement of segment vmbr1 VLAN 20 is revoked; "+
		"the routes at observed on it are held from the next cycle", adminEvents(e, since.Add(-time.Nanosecond))[1])

	err = e.eng.RevokeSegment(t.Context(), "vmbr1", 20)
	require.ErrorIs(t, err, ErrNotFound)
	require.EqualError(t, err, "not found: segment vmbr1 VLAN 20 is not acknowledged")
}

func TestOnlyASegmentIsAcknowledged(t *testing.T) {
	e := newEnv(t)
	for _, tt := range []struct {
		bridge string
		vlan   int
		want   string
	}{
		{"", 0, "invalid request: a segment needs a bridge"},
		{"vmbr1.20", 0, `invalid request: bridge "vmbr1.20": want letters, digits, - and _; give the VLAN as vmbr1:20`},
		{"vmbr 1", 0, `invalid request: bridge "vmbr 1": want letters, digits, - and _; give the VLAN as vmbr1:20`},
		{"vmbr1", -1, "invalid request: VLAN -1 is not 0 to 4094"},
		{"vmbr1", 4095, "invalid request: VLAN 4095 is not 0 to 4094"},
	} {
		require.EqualError(t, e.eng.AcknowledgeSegment(t.Context(), tt.bridge, tt.vlan), tt.want)
		err := e.eng.RevokeSegment(t.Context(), tt.bridge, tt.vlan)
		require.ErrorIs(t, err, ErrInvalid)
	}
	segments, err := e.store.Segments()
	require.NoError(t, err)
	require.Empty(t, segments)
}

// The segments are those the last cycle saw at observed, with the
// acknowledgements as the store has them now.
func TestTheSegmentsAreThoseSeenAndThoseAcknowledged(t *testing.T) {
	e := observing(t)
	e.res.proveOn(resolve.Segment{Bridge: "vmbr2"})
	e.cycle()
	require.NoError(t, e.eng.AcknowledgeSegment(t.Context(), "vmbr2", 0))

	segments, err := e.eng.Segments()

	require.NoError(t, err)
	require.Equal(t, []SegmentView{
		{Bridge: "vmbr1", Acknowledged: true, AcknowledgedAt: t0},
		{Bridge: "vmbr2", Acknowledged: true, AcknowledgedAt: t0, Routes: 1},
	}, segments)

	require.NoError(t, e.eng.RevokeSegment(t.Context(), "vmbr1", 0))
	segments, err = e.eng.Segments()
	require.NoError(t, err)
	require.Equal(t, []SegmentView{{Bridge: "vmbr2", Acknowledged: true, AcknowledgedAt: t0, Routes: 1}}, segments,
		"a segment no longer acknowledged and seen nowhere is gone")
}

func TestTheReasonASegmentGives(t *testing.T) {
	require.Equal(t, "segment vmbr1 is not acknowledged; pco segment acknowledge vmbr1", SegmentReason("vmbr1", 0))
	require.Equal(t, "segment vmbr1 VLAN 7 is not acknowledged; pco segment acknowledge vmbr1:7", SegmentReason("vmbr1", 7))
	require.Equal(t, "vmbr1:7", SegmentArg("vmbr1", 7))
	require.Equal(t, "vmbr1", SegmentArg("vmbr1", 0))
}
