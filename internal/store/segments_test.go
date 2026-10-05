package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSegmentID(t *testing.T) {
	require.Equal(t, "vmbr0", SegmentID("vmbr0", 0))
	require.Equal(t, "vmbr1.20", SegmentID("vmbr1", 20))
	require.Equal(t, "pconet.4094", SegmentID("pconet", 4094))
}

func TestSegmentsRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Segments()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	untagged := Segment{Bridge: "vmbr0", AcknowledgedAt: t0, By: "cli"}
	tagged := Segment{Bridge: "vmbr1", VLAN: 20, AcknowledgedAt: t0.Add(time.Hour)}
	require.NoError(t, s.SaveSegment(untagged))
	require.NoError(t, s.SaveSegment(tagged))

	got, err = s.Segments()
	require.NoError(t, err)
	require.Equal(t, map[string]Segment{"vmbr0": untagged, "vmbr1.20": tagged}, got)
	require.Equal(t, []string{"segments/vmbr0.json", "segments/vmbr1.20.json"}, stored(t, p.Cluster))

	require.NoError(t, s.SaveSegment(tagged))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Cluster, "segments", "vmbr1.20.json")), "an unchanged segment is not written")

	require.NoError(t, s.DeleteSegment("vmbr1.20"))
	require.NoError(t, s.DeleteSegment("vmbr1.20"), "a missing one is no error")
	got, err = s.Segments()
	require.NoError(t, err)
	require.Equal(t, map[string]Segment{"vmbr0": untagged}, got)
}

func TestSaveSegmentRefusesWhatIsNoSegment(t *testing.T) {
	for _, tt := range []struct {
		name string
		seg  Segment
		want string
	}{
		{"no bridge", Segment{AcknowledgedAt: t0}, "bridge"},
		{"a bridge with a dot", Segment{Bridge: "vmbr0.20", AcknowledgedAt: t0}, `"vmbr0.20"`},
		{"a bridge with a slash", Segment{Bridge: "a/b", AcknowledgedAt: t0}, `"a/b"`},
		{"a bridge with a space", Segment{Bridge: "vmbr 0", AcknowledgedAt: t0}, `"vmbr 0"`},
		{"a negative VLAN", Segment{Bridge: "vmbr0", VLAN: -1, AcknowledgedAt: t0}, "VLAN -1"},
		{"VLAN 4095", Segment{Bridge: "vmbr0", VLAN: 4095, AcknowledgedAt: t0}, "VLAN 4095"},
		{"no time", Segment{Bridge: "vmbr0"}, "acknowledged"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)

			require.ErrorContains(t, s.SaveSegment(tt.seg), tt.want)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}

func TestSegmentsRefuseAFileThatHoldsAnotherSegment(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Cluster, "segments", "vmbr0.json"),
		envelopeJSON("vmbr0", `{"bridge":"vmbr1","acknowledgedAt":"2026-10-01T12:00:00Z"}`))

	got, err := s.Segments()

	require.ErrorContains(t, err, "vmbr0.json")
	require.ErrorContains(t, err, "vmbr1")
	require.Nil(t, got)
}
