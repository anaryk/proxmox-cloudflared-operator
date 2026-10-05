package store

import (
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func softEntry(addr, why string, seen time.Time) SoftEntry {
	return SoftEntry{Addr: netip.MustParseAddr(addr), Why: why, LastSeen: seen}
}

func TestSoftDenyRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.SoftDeny()
	require.NoError(t, err)
	require.NotNil(t, got.Entries)
	require.Empty(t, got.Entries)

	in := SoftDeny{Entries: []SoftEntry{
		softEntry("192.168.4.1", "gateway of node pve1", t0),
		softEntry("1.1.1.1", "resolver of the appliance", t0),
		softEntry("fd00::1", "gateway of node pve2", t0.Add(-time.Hour)),
	}}
	require.NoError(t, s.SaveSoftDeny(in))

	got, err = s.SoftDeny()
	require.NoError(t, err)
	require.Equal(t, SoftDeny{Entries: []SoftEntry{
		softEntry("1.1.1.1", "resolver of the appliance", t0),
		softEntry("192.168.4.1", "gateway of node pve1", t0),
		softEntry("fd00::1", "gateway of node pve2", t0.Add(-time.Hour)),
	}}, got, "sorted by address")
	require.Equal(t, "192.168.4.1", in.Entries[0].Addr.String(), "the caller's entries are not changed")
	require.Equal(t, []string{"meta/soft-deny.json"}, stored(t, p.Local), "a node-local file")
	require.Empty(t, stored(t, p.Cluster))

	require.NoError(t, s.SaveSoftDeny(got))
	require.EqualValues(t, 1, revOf(t, filepath.Join(p.Local, "meta", "soft-deny.json")), "an unchanged list is not written")
}

func TestSoftDenyKeepsOneEntryPerAddress(t *testing.T) {
	s, _ := openStore(t)
	require.NoError(t, s.SaveSoftDeny(SoftDeny{Entries: []SoftEntry{
		softEntry("10.92.0.1", "gateway of node pve1", t0.Add(-time.Hour)),
		softEntry("10.92.0.1", "resolver of node pve1", t0),
		softEntry("10.92.0.1", "gateway of node pve2", t0.Add(-2*time.Hour)),
		softEntry("1.1.1.1", "resolver of the appliance", t0),
		softEntry("1.1.1.1", "resolver of node pve1", t0),
	}}))

	got, err := s.SoftDeny()
	require.NoError(t, err)
	require.Equal(t, []SoftEntry{
		softEntry("1.1.1.1", "resolver of node pve1", t0),
		softEntry("10.92.0.1", "resolver of node pve1", t0),
	}, got.Entries, "the one seen last; of two seen at once, the first reason in order")
}

func TestSaveSoftDenyRefusesAnEntryThatIsNotValid(t *testing.T) {
	for name, e := range map[string]SoftEntry{
		"no address": {Why: "gateway of node pve1", LastSeen: t0},
		"no reason":  {Addr: netip.MustParseAddr("10.92.0.1"), LastSeen: t0},
	} {
		t.Run(name, func(t *testing.T) {
			s, p := openStore(t)
			ok := softEntry("1.1.1.1", "resolver of the appliance", t0)

			require.Error(t, s.SaveSoftDeny(SoftDeny{Entries: []SoftEntry{ok, e}}))
			require.Empty(t, stored(t, p.Local))
		})
	}
}

func TestSoftDenyReadsAHandEditedFileInOrder(t *testing.T) {
	s, p := openStore(t)
	writeFile(t, filepath.Join(p.Local, "meta", "soft-deny.json"), envelopeJSON("soft-deny", `{"entries":[`+
		`{"addr":"10.0.0.2","why":"b","lastSeen":"2026-10-01T12:00:00Z"},`+
		`{"addr":"10.0.0.1","why":"a","lastSeen":"2026-10-01T12:00:00Z"}]}`))

	got, err := s.SoftDeny()

	require.NoError(t, err)
	require.Equal(t, []SoftEntry{softEntry("10.0.0.1", "a", t0), softEntry("10.0.0.2", "b", t0)}, got.Entries)
}
