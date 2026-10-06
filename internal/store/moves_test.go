package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMovesRoundTrip(t *testing.T) {
	s, p := openStore(t)
	got, err := s.Moves()
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)

	guest := Move{Owner: "qemu/101", From: "pco-0123456789ab-pve-a", To: "pco-0123456789ab-pve-b", Step: MoveRulesAdded, At: t0}
	manual := Move{Owner: "web-1", From: "pco-0123456789ab", To: "pco-0123456789ab-pve-a", Step: MoveCNAMEsSwitched,
		At: t0.Add(time.Minute), CNAMEAt: t0.Add(2 * time.Minute)}
	require.NoError(t, s.SaveMove(guest))
	require.NoError(t, s.SaveMove(manual))

	got, err = s.Moves()
	require.NoError(t, err)
	require.Equal(t, map[string]Move{"qemu/101": guest, "web-1": manual}, got)
	require.Equal(t, []string{"moves/qemu_101.json", "moves/web-1.json"}, stored(t, p.Cluster))

	guest.Step, guest.At = MoveRolledOut, t0.Add(time.Hour)
	require.NoError(t, s.SaveMove(guest))
	require.NoError(t, s.DeleteMove("web-1"))
	require.NoError(t, s.DeleteMove("web-1"), "a missing one is no error")

	got, err = s.Moves()
	require.NoError(t, err)
	require.Equal(t, map[string]Move{"qemu/101": guest}, got)
}

func TestSaveMoveRefusesWhatIsNoMove(t *testing.T) {
	ok := Move{Owner: "qemu/101", From: "a", To: "b", Step: MoveRulesAdded, At: t0}
	for _, tt := range []struct {
		name string
		edit func(*Move)
		want string
	}{
		{"no owner", func(m *Move) { m.Owner = "" }, "owner"},
		{"no tunnel to move from", func(m *Move) { m.From = "" }, "from"},
		{"no tunnel to move to", func(m *Move) { m.To = "" }, "to"},
		{"the same tunnel", func(m *Move) { m.To = "a" }, "same"},
		{"another step", func(m *Move) { m.Step = "halfway" }, `"halfway"`},
		{"no time", func(m *Move) { m.At = time.Time{} }, "time"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, p := openStore(t)
			m := ok
			tt.edit(&m)
			require.ErrorContains(t, s.SaveMove(m), tt.want)
			require.Empty(t, stored(t, p.Cluster))
		})
	}
}
