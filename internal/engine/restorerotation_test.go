package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// rotationEventsOf are the messages of the events of the rotations.
func rotationEventsOf(e *env) []string {
	var out []string
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindRotation {
			out = append(out, ev.Message)
		}
	}
	return out
}

// The promotion of a restored install asks for a new secret of every tunnel,
// shared and pinned: the first cycle that may write gives each one, and once
// the nodes run with it, ends every connection of the tunnel, so that a run
// token the disks of the lost install hold works no longer.
func TestARestoredInstallGivesEveryTunnelANewSecret(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	sharedID, pinnedID := w.tunnelID(t, tunnelName), w.tunnelID(t, pinned2)
	w.listAt(t, tunnelName, "old-connector", "pve2-"+sharedID, "pve3-"+sharedID)
	require.NoError(t, w.store.SaveRestoreRotation(store.RestoreRotation{Since: w.clock.now()}))

	var st State
	for range 3 {
		st = w.tick(time.Minute)
	}

	calls := w.cf.Calls()
	require.Equal(t, 1, count(calls, "RotateTunnelSecret acc1 "+sharedID), st.Problems)
	require.Equal(t, 1, count(calls, "RotateTunnelSecret acc1 "+pinnedID))
	require.Contains(t, calls, "CleanUpConnections acc1 "+sharedID)
	require.Contains(t, calls, "CleanUpConnections acc1 "+pinnedID)
	_, found, err := w.store.RestoreRotation()
	require.NoError(t, err)
	require.False(t, found, "done")
	require.Contains(t, rotationEventsOf(w.env), "the install was restored from a bundle: every tunnel has a new secret, "+
		"and the run tokens of the lost install no longer work")

	w.tick(time.Minute)
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+sharedID), "once")
}

// In observe-only mode the rotation waits for pco apply, and says so.
func TestTheRotationOfARestoredInstallWaitsForApply(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.settings(func(s *store.Settings) { s.ObserveOnly = true })
	require.NoError(t, w.store.SaveRestoreRotation(store.RestoreRotation{Since: w.clock.now()}))

	st := w.tick(time.Minute)

	require.NotContains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+id)
	require.True(t, hasProblem(st, "the install was restored from a bundle: the new secrets of its tunnels wait for a cycle that writes: "+
		"pco is in observe-only mode"), st.Problems)
	_, found, err := w.store.RestoreRotation()
	require.NoError(t, err)
	require.True(t, found)

	w.settings(func(s *store.Settings) { s.ObserveOnly = false })
	for range 3 {
		w.tick(time.Minute)
	}
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id))
}
