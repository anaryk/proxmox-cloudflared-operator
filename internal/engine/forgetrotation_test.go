package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// connectorDropFailing fails the end of one connector's connections while
// armed, and with all also the end of every connection of a tunnel.
type connectorDropFailing struct {
	cfapi.API
	armed, all *atomic.Bool
}

func (c connectorDropFailing) CleanUpConnections(ctx context.Context, account, tunnel string) error {
	if c.armed.Load() && c.all.Load() {
		return errors.New("cloudflare api: HTTP 500: internal")
	}
	return c.API.CleanUpConnections(ctx, account, tunnel)
}

func (c connectorDropFailing) CleanUpConnector(ctx context.Context, account, tunnel, id string) error {
	if c.armed.Load() {
		return errors.New("cloudflare api: HTTP 500: internal")
	}
	return c.API.CleanUpConnector(ctx, account, tunnel, id)
}

// listedIDs are the connectors Cloudflare lists on a tunnel.
func (w *world) listedIDs(t *testing.T, id string) []string {
	t.Helper()
	conns, err := w.cf.Connectors(t.Context(), testAccount, id)
	require.NoError(t, err)
	var out []string
	for _, c := range conns {
		out = append(out, c.ID)
	}
	slices.Sort(out)
	return out
}

// A forget rotates the shared tunnel's secret and drops the connector of
// the forgotten node alone: the remaining replicas keep their connections
// until each restarts with the new token, so that the tunnel never goes
// without a connector.
func TestForgetDropsOnlyTheConnectorOfTheNode(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.conn.setConnectorID(id, "c-pve1")
	w.listAt(t, tunnelName, "c-pve1", "pve2-"+id, "pve3-"+id)
	w.tick(time.Second)
	n := len(w.cf.Calls())

	res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	require.Empty(t, res.Pending)
	calls := w.callsSince(n)
	require.Contains(t, calls, "RotateTunnelSecret acc1 "+id)
	require.Contains(t, calls, "CleanUpConnector acc1 "+id+" pve3-"+id)
	require.NotContains(t, calls, "CleanUpConnections acc1 "+id, "the replicas keep their connections")
	require.Equal(t, []string{"c-pve1", "pve2-" + id}, w.listedIDs(t, id))

	w.tick(time.Second)
	require.Nil(t, entryOf(t, w.env, "pve3").Rotation, "finished by a cycle that saw every account")
	require.Contains(t, nodeEventsOf(w.env), "node pve3 was forgotten: the secret of the shared tunnel is rotated in every account, "+
		"and the run token the node holds no longer works")
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id))
}

// With cluster.rotateOnForget off the run token the forgotten node holds
// still works: its connector is dropped at Cloudflare all the same, by its
// id, as often as a listing shows it again, and it is never one of the
// install's, neither for the rogue connectors nor for a rollout.
func TestAForgetWithoutRotationDropsTheConnectorOfTheNode(t *testing.T) {
	w := threeNodes(t)
	no := false
	w.settings(func(s *store.Settings) { s.Cluster.RotateOnForget = &no })
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.conn.setConnectorID(id, "c-pve1")
	w.listAt(t, tunnelName, "c-pve1", "pve2-"+id, "pve3-"+id)
	w.tick(time.Second)
	n := len(w.cf.Calls())

	_, err := w.eng.ForgetNode(t.Context(), "pve3", false)
	require.NoError(t, err)
	st := w.tick(time.Second)

	calls := w.callsSince(n)
	require.NotContains(t, calls, "RotateTunnelSecret acc1 "+id)
	require.Contains(t, calls, "CleanUpConnector acc1 "+id+" pve3-"+id)
	require.NotContains(t, calls, "CleanUpConnections acc1 "+id, "the replicas keep their connections")
	require.Equal(t, []string{"c-pve1", "pve2-" + id}, w.listedIDs(t, id))
	require.Contains(t, nodeEventsOf(w.env), "node pve3 was forgotten: its connector pve3-"+id+" of tunnel "+tunnelName+
		" in account acc1 is dropped; its run token still works, as cluster.rotateOnForget is off")
	require.Empty(t, st.RogueConnectors)

	t.Run("and again when it connects again", func(t *testing.T) {
		w.listAt(t, tunnelName, "c-pve1", "pve2-"+id, "pve3-"+id)
		n := len(w.cf.Calls())
		st := w.tick(time.Second)
		require.Contains(t, w.callsSince(n), "CleanUpConnector acc1 "+id+" pve3-"+id)
		require.Equal(t, []string{"c-pve1", "pve2-" + id}, w.listedIDs(t, id))
		require.False(t, slices.ContainsFunc(st.RogueConnectors, func(r RogueConnector) bool { return r.ID != "pve3-"+id }), st.RogueConnectors)
	})
	stillConnects := "node pve3 still connects to tunnel " + tunnelName + " in account acc1 with the run token it kept; " +
		"rotate the shared secret: pco tunnel rotate --account acc1"
	t.Run("but not listed in every cycle", func(t *testing.T) {
		n := len(w.cf.Calls())
		var st State
		for range 3 {
			st = w.tick(time.Second)
		}
		require.Zero(t, count(w.callsSince(n), "Connectors acc1 "+id))
		require.Contains(t, st.Problems, stillConnects, "dropped twice: said in every cycle, also those that do not list")
	})
	t.Run("said once as an event, then as a problem while it goes on", func(t *testing.T) {
		for range 3 {
			w.listAt(t, tunnelName, "c-pve1", "pve2-"+id, "pve3-"+id)
			st := w.tick(time.Second)
			require.Contains(t, st.Problems, stillConnects)
		}
		require.Equal(t, 1, count(nodeEventsOf(w.env), "node pve3 was forgotten: its connector pve3-"+id+" of tunnel "+tunnelName+
			" in account acc1 is dropped; its run token still works, as cluster.rotateOnForget is off"))
	})
	t.Run("and no more once it is gone", func(t *testing.T) {
		w.listAt(t, tunnelName, "c-pve1", "pve2-"+id)
		st := w.tick(time.Second)
		require.NotContains(t, st.Problems, stillConnects)
	})
}

// A rotation the forget could not do is kept with the node's entry: the
// cycles finish it once Cloudflare takes it, a controller that takes over
// included.
func TestARotationTheForgetCouldNotDoIsFinishedByTheCycles(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.cf.Deny("tunnel.write")

	res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	require.Contains(t, strings.Join(res.Pending, "\n"), "it is tried again in the next cycle")
	rot := entryOf(t, w.env, "pve3").Rotation
	require.NotNil(t, rot)
	require.Empty(t, rot.Rotated)
	st := w.tick(time.Second)
	require.True(t, hasProblem(st, "node pve3 was forgotten: rotating the secret of tunnel pco-abc123 in account acc1 failed"), st.Problems)

	w.restart(t)
	w.cf.Allow("tunnel.write")
	n := len(w.cf.Calls())
	w.tick(time.Second)
	require.Equal(t, 1, count(w.callsSince(n), "RotateTunnelSecret acc1 "+id))
	require.Nil(t, entryOf(t, w.env, "pve3").Rotation)

	t.Run("not in observe-only mode", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		w.settings(func(s *store.Settings) { s.ObserveOnly = true })
		res, err := w.eng.ForgetNode(t.Context(), "pve2", false)
		require.NoError(t, err)
		require.Contains(t, strings.Join(res.Pending, "\n"), "once pco apply ends observe-only mode")
		st := w.tick(time.Second)
		require.NotContains(t, w.cf.Calls(), "RotateTunnelSecret acc1 "+w.tunnelID(t, tunnelName))
		require.True(t, hasProblem(st, "node pve2 was forgotten: the rotation of the shared tunnel's secret waits for a cycle that writes"), st.Problems)
		require.NotNil(t, entryOf(t, w.env, "pve2").Rotation)
	})
}

// A secret Cloudflare took is recorded at once: when what follows it fails,
// only that is tried again, and the tunnel gets no further secret.
func TestASecretIsGivenOnceAndOnlyWhatFollowsIsTriedAgain(t *testing.T) {
	w := threeNodes(t)
	armed, all := &atomic.Bool{}, &atomic.Bool{}
	all.Store(true)
	w.useAPI(testToken, connectorDropFailing{API: w.cf, armed: armed, all: all})
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.listAt(t, tunnelName, "pve2-"+id, "pve3-"+id)
	w.tick(time.Second)
	armed.Store(true)

	res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	require.Contains(t, strings.Join(res.Pending, "\n"), "connector pve3-"+id)
	rot := entryOf(t, w.env, "pve3").Rotation
	require.Equal(t, []string{id}, rot.Rotated)
	require.Empty(t, rot.TakenUp)
	w.tick(time.Second)
	w.tick(time.Second)
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id), "no new secret every cycle")
	require.NotNil(t, entryOf(t, w.env, "pve3").Rotation)

	armed.Store(false)
	w.tick(time.Second)
	require.Equal(t, 1, count(w.cf.Calls(), "RotateTunnelSecret acc1 "+id))
	require.Equal(t, []string{"pve2-" + id}, w.listedIDs(t, id))
	require.Nil(t, entryOf(t, w.env, "pve3").Rotation)
}

// The connector of the forgotten node is dropped however little is known of
// it: one no node still in the install owns goes when its id is not known,
// and when no connector can be dropped alone every connection of the tunnel
// is ended. The rotation is finished only then.
func TestTheForgottenNodeKeepsNoConnection(t *testing.T) {
	t.Run("its connector id not known", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		id := w.tunnelID(t, tunnelName)
		w.conn.setConnectorID(id, "c-pve1")
		w.listAt(t, tunnelName, "c-pve1", "pve2-"+id, "pve3-"+id)
		w.tick(time.Second)
		// As after a takeover the agent of pve3 never reported to.
		delete(w.eng.place.ids, "pve3")
		n := len(w.cf.Calls())

		res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

		require.NoError(t, err)
		require.Empty(t, res.Pending)
		require.Contains(t, w.callsSince(n), "CleanUpConnector acc1 "+id+" pve3-"+id)
		require.NotContains(t, w.callsSince(n), "CleanUpConnections acc1 "+id)
		require.Equal(t, []string{"c-pve1", "pve2-" + id}, w.listedIDs(t, id))
	})
	t.Run("a connector that cannot be dropped alone", func(t *testing.T) {
		w := threeNodes(t)
		armed, all := &atomic.Bool{}, &atomic.Bool{}
		w.useAPI(testToken, connectorDropFailing{API: w.cf, armed: armed, all: all})
		w.settle()
		id := w.tunnelID(t, tunnelName)
		w.listAt(t, tunnelName, "pve2-"+id, "pve3-"+id)
		w.tick(time.Second)
		armed.Store(true)
		n := len(w.cf.Calls())

		res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

		require.NoError(t, err)
		require.Empty(t, res.Pending)
		require.Contains(t, w.callsSince(n), "CleanUpConnections acc1 "+id, "every connection ends instead")
		require.Empty(t, w.listedIDs(t, id))
		w.tick(time.Second)
		require.Nil(t, entryOf(t, w.env, "pve3").Rotation)
	})
	t.Run("kept pending while nothing could be dropped", func(t *testing.T) {
		w := threeNodes(t)
		armed, all := &atomic.Bool{}, &atomic.Bool{}
		all.Store(true)
		w.useAPI(testToken, connectorDropFailing{API: w.cf, armed: armed, all: all})
		w.settle()
		armed.Store(true)
		_, err := w.eng.ForgetNode(t.Context(), "pve3", false)
		require.NoError(t, err)
		for range 2 {
			w.restart(t)
			w.tick(time.Second)
			rot := entryOf(t, w.env, "pve3").Rotation
			require.NotNil(t, rot, "a crash or a failure keeps it pending")
			require.Empty(t, rot.TakenUp)
		}
	})
}
