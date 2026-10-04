package store

import (
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

var proven = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func webBinding(verifiedAt time.Time) resolve.Binding {
	return resolve.Binding{
		Owner: "qemu/101", Hostname: "www.example.com", Guest: "qemu/101",
		Addr: netip.MustParseAddr("10.0.0.11"), MAC: "bc:24:11:00:00:01",
		VerifiedAt: verifiedAt, Level: resolve.LevelPort, Since: proven,
		Bridge: "vmbr0", Port: "tap101i0", Ports: map[string]string{"bc:24:11:00:00:01": "tap101i0"},
	}
}

func bindingPath(p Paths) string { return filepath.Join(p.Local, "bindings", "www.example.com.json") }

// A cycle that proves an address again moves only the time of the proof: the
// file is left as it is until that moved by more than a quarter of the age a
// proof may reach, while the store answers with the time it was given.
func TestABindingIsWrittenOnlyWhenItChanged(t *testing.T) {
	quarter := resolve.DefaultMaxProofAge / 4
	tests := []struct {
		name    string
		change  func(b *resolve.Binding)
		written bool
	}{
		{"nothing", func(*resolve.Binding) {}, false},
		{"the proof, by a poll interval", func(b *resolve.Binding) { b.VerifiedAt = proven.Add(10 * time.Second) }, false},
		{"the proof, by a quarter of its age", func(b *resolve.Binding) { b.VerifiedAt = proven.Add(quarter) }, false},
		{"the proof, by more than a quarter of its age", func(b *resolve.Binding) { b.VerifiedAt = proven.Add(quarter + time.Second) }, true},
		{"the proof, back in time", func(b *resolve.Binding) { b.VerifiedAt = proven.Add(-time.Second) }, true},
		{"the level", func(b *resolve.Binding) { b.Level = resolve.LevelObserved }, true},
		{"withdrawn", func(b *resolve.Binding) { b.Withdrawn = true }, true},
		{"failing", func(b *resolve.Binding) { at := proven; b.FailingSince = &at }, true},
		{"the port of a MAC", func(b *resolve.Binding) { b.Ports = map[string]string{"bc:24:11:00:00:01": "fwpr101p0"} }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"www.example.com": webBinding(proven)}))
			next := webBinding(proven)
			next.VerifiedAt = proven.Add(10 * time.Second)
			tt.change(&next)

			require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"www.example.com": next}))

			want := int64(1)
			if tt.written {
				want = 2
			}
			require.Equal(t, want, revOf(t, bindingPath(p)))
			got, err := s.Bindings()
			require.NoError(t, err)
			require.Equal(t, next.VerifiedAt, got["www.example.com"].VerifiedAt.UTC(), "the time of the proof as it was given")
		})
	}
}

// Times that each moved by less than a quarter add up: the file is written
// once they moved by more since it was.
func TestTheQuarterCountsFromTheTimeOnDisk(t *testing.T) {
	s, p := openStore(t)
	for i := range 9 {
		b := webBinding(proven.Add(time.Duration(i) * 10 * time.Second))
		require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"www.example.com": b}))
	}

	require.Equal(t, int64(2), revOf(t, bindingPath(p)), "written at 0s and at 80s")
	require.Equal(t, proven.Add(80*time.Second), readBinding(t, p).VerifiedAt.UTC())
}

// A restart reads the time of the proof the file holds, which is no later
// than the one that was proven.
func TestARestartSeesTheOlderProof(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"www.example.com": webBinding(proven)}))
	require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"www.example.com": webBinding(proven.Add(time.Minute))}))

	again, err := Open(p)
	require.NoError(t, err)
	got, err := again.Bindings()

	require.NoError(t, err)
	require.Equal(t, proven, got["www.example.com"].VerifiedAt.UTC())
}

// What the file says wins over the time the store kept when the two differ
// in anything else, as when the file was written by another hand.
func TestTheFileWinsWhenItHoldsAnotherBinding(t *testing.T) {
	s, p := openStore(t)
	require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"www.example.com": webBinding(proven)}))
	require.NoError(t, s.SaveBindings(map[string]resolve.Binding{"www.example.com": webBinding(proven.Add(time.Minute))}))
	other, err := Open(p)
	require.NoError(t, err)
	withdrawn := webBinding(proven)
	withdrawn.Withdrawn = true
	require.NoError(t, other.SaveBindings(map[string]resolve.Binding{"www.example.com": withdrawn}))

	got, err := s.Bindings()

	require.NoError(t, err)
	require.True(t, got["www.example.com"].Withdrawn)
	require.Equal(t, proven, got["www.example.com"].VerifiedAt.UTC())
}

func readBinding(t *testing.T, p Paths) resolve.Binding {
	t.Helper()
	s, err := Open(p)
	require.NoError(t, err)
	got, err := s.Bindings()
	require.NoError(t, err)
	return got["www.example.com"]
}
