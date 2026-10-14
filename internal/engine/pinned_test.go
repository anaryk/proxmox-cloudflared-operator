package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
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
		n.Kind, n.VMID = "appliance", 9302
		require.NoError(t, w.store.SaveNode(n))
		// An appliance agent is served only while Proxmox shows its
		// container on its node.
		addGuest(w.env, model.Guest{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 9302}, Name: "pco", Node: "pve2", Running: true, Pool: "pco"})
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

// The connector a removed node still runs of its pinned tunnel loses its
// connections before the tunnel is deleted: Cloudflare deletes no tunnel
// that has connections, and the node keeps none.
func TestARemovedNodesPinnedConnectorIsDroppedBeforeItsTunnelGoes(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	id := w.tunnelID(t, pinned2)
	w.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: "pve2-" + id, Connections: 4}})
	n := entryOf(t, w.env, "pve2")
	n.State = store.NodeRemoved
	require.NoError(t, w.store.SaveNode(n))
	calls := len(w.cf.Calls())

	w.tick(time.Second)

	since := w.callsSince(calls)
	cleaned, deleted := slices.Index(since, "CleanUpConnections acc1 "+id), slices.Index(since, "DeleteTunnel acc1 "+id)
	require.NotEqual(t, -1, cleaned, since)
	require.Less(t, cleaned, deleted, since)
	require.NotContains(t, w.tunnelNames(), pinned2)
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

// A pinned tunnel on its way out has its node told to stop its connector,
// which serves nothing then: Cloudflare deletes no tunnel that has
// connections.
func TestThePinnedTunnelOnItsWayOutHasItsConnectorStopped(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	w.acts["pve3"].fail = nil
	told := false
	for range 50 {
		w.tick(30 * time.Second)
		if tun, ok := w.tunnelIn(t, "pve2", pinned2); ok && tun.Stop {
			require.False(t, tun.Run)
			require.Empty(t, tun.Token.Reveal())
			told = true
			break
		}
	}
	require.True(t, told, "pve2 is told to stop the connector of its pinned tunnel")
	require.Contains(t, w.tunnelNames(), pinned2, "the tunnel itself goes ten minutes later")
	shared, ok := w.tunnelIn(t, "pve2", tunnelName)
	require.True(t, ok)
	require.False(t, shared.Stop)
}

// A connector that keeps connecting, as one of an agent that does not stop
// it, keeps a pinned tunnel from being deleted: its connections are ended
// before every delete, and a delete that failed is tried again later each
// time, not in every cycle.
func TestAPinnedTunnelThatCannotBeDeletedIsTriedAgainLater(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	w.acts["pve3"].fail = nil
	id := w.tunnelID(t, pinned2)
	back := &connectingAgain{API: w.cf, fake: w.cf, tunnel: id}
	back.connect()
	w.useAPI(testToken, back)
	w.eng = w.newEngine()
	w.watchHere(t)
	for range 50 {
		w.tick(30 * time.Second)
		if tun, ok := w.tunnelIn(t, "pve2", pinned2); ok && tun.Stop {
			break
		}
	}
	calls := len(w.cf.Calls())

	// Due ten minutes after it began to serve nothing, then tried again.
	var problems []string
	for range 40 {
		problems = append(problems, w.tick(30*time.Second).Problems...)
	}

	since := w.callsSince(calls)
	var deletes []int
	for i, call := range since {
		if call == "DeleteTunnel "+testAccount+" "+id {
			deletes = append(deletes, i)
			require.Equal(t, "CleanUpConnections "+testAccount+" "+id, since[i-1], "the connections are ended first")
		}
	}
	require.Contains(t, w.tunnelNames(), pinned2)
	require.GreaterOrEqual(t, len(deletes), 2)
	require.LessOrEqual(t, len(deletes), 4, "tried again after one, two and four minutes, not in every cycle")
	require.True(t, slices.ContainsFunc(problems, func(p string) bool {
		return strings.HasPrefix(p, "deleting tunnel "+pinned2+" in account "+testAccount+", which serves nothing: ") && strings.Contains(p, "; tried again at ")
	}), problems)

	back.stop()
	for range 20 {
		w.tick(time.Minute)
	}
	require.NotContains(t, w.tunnelNames(), pinned2)
}

// connectingAgain is a Cloudflare where a connector of a tunnel connects
// again right after its connections are ended, until stopped.
type connectingAgain struct {
	cfapi.API
	fake   *cffake.Fake
	tunnel string

	mu      sync.Mutex
	stopped bool
}

func (c *connectingAgain) connect() {
	c.fake.SetConnectors(testAccount, c.tunnel, []cfapi.Connector{{ID: "pve2-" + c.tunnel, Connections: 4}})
}

func (c *connectingAgain) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
}

func (c *connectingAgain) CleanUpConnections(ctx context.Context, account, id string) error {
	if err := c.API.CleanUpConnections(ctx, account, id); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == c.tunnel && !c.stopped {
		c.connect()
	}
	return nil
}

// A pinned tunnel deleted is not deleted again, nor are its connections
// ended once it is gone, while the listings that named it are still fresh.
func TestADeletedPinnedTunnelIsLeftAlone(t *testing.T) {
	w, pinned2 := pinnedDB(t)
	id := w.tunnelID(t, pinned2)
	n := entryOf(t, w.env, "pve2")
	n.State = store.NodeRemoved
	require.NoError(t, w.store.SaveNode(n))
	calls := len(w.cf.Calls())

	var problems []string
	for range 6 {
		problems = append(problems, w.tick(5*time.Second).Problems...)
	}

	since := w.callsSince(calls)
	require.NotContains(t, w.tunnelNames(), pinned2)
	count := func(call string) int {
		return len(slices.DeleteFunc(slices.Clone(since), func(c string) bool { return c != call }))
	}
	require.Equal(t, 1, count("CleanUpConnections "+testAccount+" "+id), since)
	require.Equal(t, 1, count("DeleteTunnel "+testAccount+" "+id), since)
	require.False(t, slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, pinned2) }), problems)
}

// targetKeys are the hostnames of the targets in the snapshot last served
// to node.
func (w *world) targetKeys(t *testing.T, node string) []string {
	t.Helper()
	s, ok := w.r.lastOf(node)
	require.True(t, ok, node)
	var out []string
	for _, tg := range s.Targets {
		out = append(out, tg.Key)
	}
	slices.Sort(out)
	return out
}

// A node that leaves the placement of routes has their targets leave its
// snapshot in the same cycle, and with them its egress set: drained, it
// keeps only what is pinned to it and the routes of its own guests, which
// no other node resolves, and loses the shared token; while
// coverage is lost, a replica keeps only the routes of its own guests; a
// node that is down, unknown, removed or contested is served nothing more.
func TestANodeThatLeavesAPlacementLosesItsTargets(t *testing.T) {
	t.Run("drained", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		n := entryOf(t, w.env, "pve3")
		n.Drained = &store.NodeDrain{Reason: store.DrainAdmin, Since: w.clock.now()}
		require.NoError(t, w.store.SaveNode(n))
		w.tick(time.Second)
		require.Equal(t, store.NodeDraining, entryOf(t, w.env, "pve3").State)
		require.Equal(t, []string{"app.example.com"}, w.targetKeys(t, "pve3"), "its own guest's route alone")
		require.True(t, w.targetOf(t, "pve3", "app.example.com").Resolve)
		shared, ok := w.tunnelIn(t, "pve3", tunnelName)
		require.False(t, ok && shared.Run)
		require.Empty(t, shared.Token.Reveal())
	})
	t.Run("coverage lost", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.acts["pve2"].noWatch = true
		w.tick(time.Second)
		w.tick(time.Second)
		require.Equal(t, []string{"app.example.com"}, w.targetKeys(t, "pve3"), "its own guest's route alone")
		require.Equal(t, []string{"db.example.com"}, w.targetKeys(t, "pve2"))
	})
	for name, leave := range map[string]func(w *world){
		"down": func(w *world) {
			(*w.listed)[1].Online = false
		},
		"unknown": func(w *world) {
			w.acts["pve2"].silent = true
			w.clock.advance(silentAfter)
		},
		"removed": func(w *world) {
			n := entryOf(w.t, w.env, "pve2")
			n.State = store.NodeRemoved
			require.NoError(w.t, w.store.SaveNode(n))
		},
		"contested": func(w *world) {
			n := entryOf(w.t, w.env, "pve2")
			n.State = store.NodeContested
			require.NoError(w.t, w.store.SaveNode(n))
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := threeNodes(t)
			w.settle()
			leave(w)
			served := len(w.r.served)
			w.tick(time.Second)
			w.tick(time.Second)
			for _, snaps := range w.r.served[served:] {
				require.NotContains(t, snaps, "pve2")
			}
		})
	}
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
