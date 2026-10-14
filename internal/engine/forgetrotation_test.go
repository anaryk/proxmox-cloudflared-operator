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
// armed.
type connectorDropFailing struct {
	cfapi.API
	armed *atomic.Bool
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
	armed := &atomic.Bool{}
	w.useAPI(testToken, connectorDropFailing{API: w.cf, armed: armed})
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.listAt(t, tunnelName, "pve2-"+id, "pve3-"+id)
	w.tick(time.Second)
	armed.Store(true)

	res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	require.Contains(t, strings.Join(res.Pending, "\n"), "its connector pve3-"+id+" could not be dropped")
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
