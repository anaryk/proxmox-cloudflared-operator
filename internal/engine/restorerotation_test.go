package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
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

// rotatedThenStuck makes the store unwritable as soon as Cloudflare took a
// new secret: the record of the rotation cannot be saved after it.
type rotatedThenStuck struct {
	cfapi.API
	stick func()
}

func (r rotatedThenStuck) RotateTunnelSecret(ctx context.Context, account, tunnel string, secret []byte) error {
	err := r.API.RotateTunnelSecret(ctx, account, tunnel, secret)
	if err == nil {
		r.stick()
	}
	return err
}

// A secret Cloudflare took is never given again because its record could
// not be saved: the rotation keeps it, holds while the store takes no write,
// and saves it first once it does.
func TestARotationWhoseRecordCannotBeSavedIsNotGivenAgain(t *testing.T) {
	w := threeNodes(t)
	meta := filepath.Join(w.paths.Cluster, "meta")
	w.useAPI(testToken, rotatedThenStuck{API: w.cf, stick: func() { require.NoError(t, os.Chmod(meta, 0o500)) }})
	t.Cleanup(func() { _ = os.Chmod(meta, 0o700) })
	w.settle()
	id := w.tunnelID(t, tunnelName)
	require.NoError(t, w.store.SaveRestoreRotation(store.RestoreRotation{Since: w.clock.now()}))

	for range 3 {
		w.tick(time.Minute)
	}
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id))

	require.NoError(t, os.Chmod(meta, 0o700))
	for range 3 {
		w.tick(time.Minute)
	}
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id), "given once")
	_, found, err := w.store.RestoreRotation()
	require.NoError(t, err)
	require.False(t, found, "done")
}

// A connector counts as running with the new secret only when it reports
// the token of it: one that is ready after the secret was given, with the
// old token still, holds the end of the connections.
func TestARotationWaitsForTheConnectorsToRunTheNewToken(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	stale := true
	w.acts["pve2"].edit = func(st *agentproto.Status) {
		for i := range st.Connectors {
			if stale && st.Connectors[i].TunnelID == id {
				st.Connectors[i].TokenSHA256 = connector.TokenDigest("the-old-token")
			}
		}
	}
	require.NoError(t, w.store.SaveRestoreRotation(store.RestoreRotation{Since: w.clock.now()}))

	var st State
	for range 3 {
		st = w.tick(time.Minute)
	}
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id))
	require.True(t, hasProblem(st, "the connectors of "+tunnelName+" do not run with the new secret yet"), st.Problems)
	rec, found, err := w.store.RestoreRotation()
	require.NoError(t, err)
	require.True(t, found, "kept while a connector runs with the old token")
	require.Empty(t, rec.Cleaned)
	cleaned := count(w.cf.Calls(), "CleanUpConnections acc1 "+id)

	stale = false
	for range 2 {
		w.tick(time.Minute)
	}
	require.Equal(t, cleaned+1, count(w.cf.Calls(), "CleanUpConnections acc1 "+id), "every connection ends once all run the new token")
	_, found, err = w.store.RestoreRotation()
	require.NoError(t, err)
	require.False(t, found)
}

// A record of the rotation that cannot be read holds the whole cycle: it
// may list a tunnel whose old secret the lost install still holds, so it is
// never taken for nothing to rotate.
func TestAnUnreadableRestoreRotationHoldsTheCycle(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	file := filepath.Join(w.paths.Cluster, "meta", "restore-rotation.json")
	require.NoError(t, os.WriteFile(file, []byte("{not json"), 0o600))
	n := len(w.cf.Calls())

	st := w.tick(time.Minute)

	require.Contains(t, st.Hold, "reading the rotation of the restored install")
	for _, call := range w.callsSince(n) {
		require.NotContains(t, call, "Put", "nothing is written at Cloudflare")
		require.NotContains(t, call, "Rotate")
	}
	require.FileExists(t, file, "nothing is dropped")
}

// The record lists every tunnel of the install it rotates, as the cycles
// find them, and is done only once each listed one was rotated, taken up
// and cleaned; a controller that takes over goes on from it, and gives no
// secret again.
func TestTheRestoreRotationListsItsTunnelsAndGoesOnAfterATakeOver(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	sharedID, pinnedID := w.tunnelID(t, tunnelName), w.tunnelID(t, pinned2)
	stale := true
	w.acts["pve2"].edit = func(st *agentproto.Status) {
		for i := range st.Connectors {
			if stale {
				st.Connectors[i].TokenSHA256 = connector.TokenDigest("the-old-token")
			}
		}
	}
	require.NoError(t, w.store.SaveRestoreRotation(store.RestoreRotation{Since: w.clock.now()}))

	w.tick(time.Minute)
	rec, found, err := w.store.RestoreRotation()
	require.NoError(t, err)
	require.True(t, found)
	require.ElementsMatch(t, []string{sharedID, pinnedID}, rec.Tunnels)
	require.ElementsMatch(t, []string{sharedID, pinnedID}, rec.Rotated)

	w.restart(t)
	w.tick(time.Minute)
	stale = false
	for range 3 {
		w.tick(time.Minute)
	}
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+sharedID))
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+pinnedID))
	_, found, err = w.store.RestoreRotation()
	require.NoError(t, err)
	require.False(t, found)
}

// A tunnel the record lists is crossed off only once it is done, or once a
// cycle that read every account found it gone: deleted, it has no secret.
func TestARestoreRotationEndsOnlyWithEveryListedTunnelDone(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	gone := "00000000-0000-4000-8000-0000000000ff"
	require.NoError(t, w.store.SaveRestoreRotation(store.RestoreRotation{Since: w.clock.now(), Tunnels: []string{id, gone}}))

	for range 3 {
		w.tick(time.Minute)
	}

	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id))
	_, found, err := w.store.RestoreRotation()
	require.NoError(t, err)
	require.False(t, found, "the tunnel that is gone has no secret left")
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
