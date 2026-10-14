package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// tunnelIn is the tunnel of name in the snapshot last served to node.
func (w *world) tunnelIn(t *testing.T, node, name string) (agentproto.Tunnel, bool) {
	t.Helper()
	s, ok := w.r.lastOf(node)
	require.True(t, ok, node)
	i := slices.IndexFunc(s.Tunnels, func(tun agentproto.Tunnel) bool { return tun.Name == name })
	if i < 0 {
		return agentproto.Tunnel{}, false
	}
	return s.Tunnels[i], true
}

// pinnedDB has db pinned to pve2: pve3 does not reach it.
func pinnedDB(t *testing.T) (*world, string) {
	t.Helper()
	w := threeNodes(t)
	w.acts["pve3"].fail = map[string]string{"db.example.com": "not reached"}
	for range 12 {
		w.tick(5 * time.Second)
	}
	pinned2, err := planner.PinnedTunnelName(testInstall, "pve2")
	require.NoError(t, err)
	require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))
	return w, pinned2
}

// The run token of the shared tunnel goes to every replica, and the token
// of a tunnel pinned to a node to that node alone: no other node, this one
// included, is given it or runs its connector.
func TestAPinnedTunnelsTokenGoesToItsNodeAlone(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	w.tick(time.Second)

	tun, ok := w.tunnelIn(t, "pve2", pinned2)
	require.True(t, ok)
	require.True(t, tun.Run)
	require.NotEmpty(t, tun.Token.Reveal())
	require.Equal(t, agentproto.TunnelPinned, tun.Kind)

	_, ok = w.tunnelIn(t, "pve3", pinned2)
	require.False(t, ok, "pve3 is not even told of it")
	for _, node := range []string{"pve2", "pve3"} {
		shared, ok := w.tunnelIn(t, node, tunnelName)
		require.True(t, ok, node)
		require.True(t, shared.Run, node)
	}
	id := w.tunnelID(t, pinned2)
	for _, call := range w.conn.ensures() {
		require.NotEqual(t, id, call.id, "this node never runs pve2's pinned tunnel")
	}
}

// A node that is not active gets no shared token, and an appliance agent no
// pinned one: only a host agent runs a tunnel pinned to its node.
func TestNoTokenGoesToANodeThatMayNotRunTheTunnel(t *testing.T) {
	t.Run("joining", func(t *testing.T) {
		w, _ := pinnedDB(t)
		n := entryOf(t, w.env, "pve3")
		n.State, n.StateSince = store.NodeJoining, w.clock.now().Add(time.Second)
		require.NoError(t, w.store.SaveNode(n))
		w.acts["pve3"].started = w.clock.now()
		w.tick(time.Second)
		shared, ok := w.tunnelIn(t, "pve3", tunnelName)
		require.False(t, ok && shared.Run)
		require.Empty(t, shared.Token.Reveal())
	})
	t.Run("an appliance agent", func(t *testing.T) {
		w, pinned2 := pinnedDB(t)
		n := entryOf(t, w.env, "pve2")
		n.Kind = "appliance"
		require.NoError(t, w.store.SaveNode(n))
		w.tick(time.Second)
		tun, ok := w.tunnelIn(t, "pve2", pinned2)
		require.False(t, ok && tun.Run)
		require.Empty(t, tun.Token.Reveal())
	})
}

// Placements that cannot be read hold the cycle: no snapshot goes out, and
// with it no token.
func TestUnreadablePlacementsServeNothing(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	served := len(w.r.served)
	require.NoError(t, os.WriteFile(filepath.Join(w.paths.Cluster, "placements", "broken.example.com.json"), []byte("{"), 0o600))

	st := w.tick(time.Second)

	require.Contains(t, st.Hold, "reading the placements of the routes")
	require.Len(t, w.r.served, served)
}

// A tunnel pinned to a node that serves nothing any more is deleted once ten
// minutes passed, also when a controller that started since finds it from
// the placements it stored; a tunnel pinned to a removed node goes at once.
func TestAPinnedTunnelThatServesNothingIsDeleted(t *testing.T) {
	t.Run("after ten minutes, found after a restart", func(t *testing.T) {
		w, pinned2 := pinnedDB(t)
		w.acts["pve3"].fail = nil
		w.eng = w.newEngine()
		w.watchHere(t)
		w.tick(time.Second)
		for range 200 {
			w.tick(10 * time.Second)
		}
		require.Equal(t, tunnelName, w.recordTunnel(t, "db.example.com"))
		require.NotContains(t, w.tunnelNames(), pinned2)
	})
	t.Run("a removed node", func(t *testing.T) {
		w, pinned2 := pinnedDB(t)
		n := entryOf(t, w.env, "pve2")
		n.State = store.NodeRemoved
		require.NoError(t, w.store.SaveNode(n))
		snap := w.inv.snapshot()
		snap.Guests = slices.DeleteFunc(snap.Guests, func(g model.Guest) bool { return g.Node == "pve2" })
		w.inv.set(snap)
		for range 3 {
			w.tick(time.Second)
		}
		require.NotContains(t, w.tunnelNames(), pinned2)
	})
}

// A node removed or contested loses its pinned tunnel at once, whatever its
// guests: the routes leave it without a move, the tunnel is deleted, which
// ends the token the node holds, and no snapshot goes to the node any more.
func TestARevokedNodesPinnedTunnelGoesAtOnce(t *testing.T) {
	for _, state := range []string{store.NodeRemoved, store.NodeContested} {
		t.Run(state, func(t *testing.T) {
			w, pinned2 := pinnedDB(t)
			served := len(w.r.served)
			n := entryOf(t, w.env, "pve2")
			n.State = state
			require.NoError(t, w.store.SaveNode(n))
			for range 3 {
				w.tick(time.Second)
			}
			require.NotContains(t, w.tunnelNames(), pinned2)
			require.NotEqual(t, pinned2, w.recordTunnel(t, "db.example.com"))
			for _, snaps := range w.r.served[served:] {
				require.NotContains(t, snaps, "pve2")
			}
			moves, err := w.store.Moves()
			require.NoError(t, err)
			require.Empty(t, moves)
		})
	}
}

// A node whose pinned routes moved away is no longer handed the token of
// its pinned tunnel: the tunnel leaves its snapshot as soon as no route is
// placed there, and is deleted ten minutes later.
func TestATokenLeavesTheSnapshotOnceTheRoutesMovedAway(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	w.acts["pve3"].fail = nil
	gone := time.Time{}
	for range 220 {
		w.tick(10 * time.Second)
		planned := slices.ContainsFunc(w.eng.State().Routes, func(r RouteView) bool {
			return r.Placement != nil && (r.Placement.Tunnel == pinned2 || r.Placement.Move != nil && (r.Placement.Move.From == pinned2 || r.Placement.Move.To == pinned2))
		})
		tun, listed := w.tunnelIn(t, "pve2", pinned2)
		if !planned {
			require.False(t, listed && tun.Run, "no run token once no route is there")
			if gone.IsZero() {
				gone = w.clock.now()
			}
		}
	}
	require.False(t, gone.IsZero(), "the route moved away")
	require.NotContains(t, w.tunnelNames(), pinned2)
}

// Once a second node is registered, the shared tunnel's unit is no longer
// enabled at boot on this node either: a node coming back starts it only
// when the controller marks it active.
func TestTheSharedUnitIsNotEnabledAtBootInACluster(t *testing.T) {
	w := threeNodes(t)
	w.settle()

	require.Contains(t, w.conn.disabledUnits(), w.tunnelID(t, tunnelName))
}

func (w *world) tunnelNames() []string {
	var out []string
	for _, tun := range w.cf.TunnelsIn(testAccount) {
		out = append(out, tun.Name)
	}
	return out
}

func (w *world) tunnelID(t *testing.T, name string) string {
	t.Helper()
	for _, tun := range w.cf.TunnelsIn(testAccount) {
		if tun.Name == name {
			return tun.ID
		}
	}
	t.Fatalf("no tunnel %s", name)
	return ""
}

func (f *fakeConnectors) disabledUnits() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.disabled)
}
