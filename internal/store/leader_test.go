package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

func TestTheLeaderRecordRoundTrips(t *testing.T) {
	s, _ := openStore(t)
	_, rev, found, err := s.Leader()
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, rev)

	r := someLeader()
	r.ReleasedAt = t0.Add(time.Hour)
	r.Handoff = &Handoff{To: "pve2", Until: t0.Add(time.Hour + 30*time.Second)}
	require.NoError(t, s.SaveLeader(r))
	require.NoError(t, s.SaveLeader(r), "an unchanged record is not written")

	got, rev, found, err := s.Leader()
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 1, rev)
	require.Equal(t, r, got)
}

// The previous release reads leader.json as a planner.Writer, leniently:
// the record must be a writer with fields added, never one that moved.
func TestTheLeaderRecordIsAWriterWithFieldsAdded(t *testing.T) {
	s, p := openStore(t)
	r := someLeader()
	r.Handoff = &Handoff{To: "pve2", Until: t0}
	require.NoError(t, s.SaveLeader(r))

	data := readRaw(t, filepath.Join(p.Cluster, "meta", "leader.json")).Data
	var wire map[string]any
	require.NoError(t, json.Unmarshal(data, &wire))
	require.Equal(t, "0123456789ab", wire["installId"])
	require.InDelta(t, 4, wire["generation"], 0)
	require.Equal(t, "n0nce", wire["nonce"])
	require.Equal(t, "pve1/boot-1", wire["incarnation"])
	require.Equal(t, "pve1", wire["node"])

	var previous planner.Writer
	require.NoError(t, json.Unmarshal(data, &previous))
	require.Equal(t, r.Writer, previous)

	bare := LeaderRecord{Writer: r.Writer}
	b, err := json.Marshal(bare)
	require.NoError(t, err)
	w, err := json.Marshal(r.Writer)
	require.NoError(t, err)
	require.JSONEq(t, string(w), string(b), "a record with nothing added is the writer, byte for byte")
}

func TestSaveLeaderRefusesWhatIsNoWriter(t *testing.T) {
	s, p := openStore(t)
	r := someLeader()
	r.Nonce = "Not Valid"
	require.ErrorContains(t, s.SaveLeader(r), "nonce")

	r = someLeader()
	r.Handoff = &Handoff{Until: t0}
	require.ErrorContains(t, s.SaveLeader(r), "hand-off")
	require.Empty(t, stored(t, p.Cluster))
}

func TestSaveWriterKeepsWhatTheRecordAddsForTheSameWriter(t *testing.T) {
	s, _ := openStore(t)
	r := someLeader()
	require.NoError(t, s.SaveLeader(r))

	w := r.Writer
	w.Incarnation = "pve1/boot-2"
	require.NoError(t, s.SaveWriter(w))
	got, _, _, err := s.Leader()
	require.NoError(t, err)
	want := r
	want.Writer = w
	require.Equal(t, want, got, "the same epoch keeps its holder")

	next := planner.Writer{InstallID: "0123456789ab", Generation: 5, Nonce: "other"}
	require.NoError(t, s.SaveWriter(next))
	got, _, _, err = s.Leader()
	require.NoError(t, err)
	require.Equal(t, LeaderRecord{Writer: next, CASHA256: r.CASHA256}, got,
		"another epoch keeps the CA and drops what was the other holder's")
}

// A holder that was paused through a takeover must not write over the
// record of the epoch that followed it.
func TestSaveLeaderIfWritesOnlyOverTheRecordItRead(t *testing.T) {
	s, p := openStore(t)
	r := someLeader()
	rev, err := s.SaveLeaderIf(r, 0)
	require.NoError(t, err)
	require.EqualValues(t, 1, rev)

	taken := someLeader()
	taken.Generation, taken.Nonce, taken.Node = 5, "newer", "pve2"
	rev, err = s.SaveLeaderIf(taken, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, rev)

	stale := r
	stale.ReleasedAt = t0.Add(time.Hour)
	before := readRaw(t, filepath.Join(p.Cluster, "meta", "leader.json"))
	rev, err = s.SaveLeaderIf(stale, 1)
	require.ErrorIs(t, err, ErrRevision)
	require.EqualValues(t, 2, rev)
	require.Equal(t, before, readRaw(t, filepath.Join(p.Cluster, "meta", "leader.json")))

	bad := taken
	bad.Nonce = "Not Valid"
	_, err = s.SaveLeaderIf(bad, 2)
	require.ErrorContains(t, err, "nonce")
}
