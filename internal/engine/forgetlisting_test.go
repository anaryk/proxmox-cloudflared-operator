package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// listingFailing fails every listing of connectors while armed.
type listingFailing struct {
	cfapi.API
	armed *atomic.Bool
}

func (l listingFailing) Connectors(ctx context.Context, account, tunnel string) ([]cfapi.Connector, error) {
	if l.armed.Load() {
		return nil, errors.New("cloudflare api: HTTP 500: internal")
	}
	return l.API.Connectors(ctx, account, tunnel)
}

// Without the node's connector ids, a listing that fails, or one that shows
// nothing to tell the node's connectors by, ends every connection of the
// tunnel after the rotation.
func TestAForgetThatCannotTellTheNodesConnectorsEndsEveryConnection(t *testing.T) {
	for _, tt := range []struct {
		name string
		prep func(w *world, id string, armed *atomic.Bool)
	}{
		{"an empty listing", func(w *world, id string, _ *atomic.Bool) { w.cf.SetConnectors(testAccount, id, nil) }},
		{"a listing that fails", func(_ *world, _ string, armed *atomic.Bool) { armed.Store(true) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := threeNodes(t)
			armed := &atomic.Bool{}
			w.useAPI(testToken, listingFailing{API: w.cf, armed: armed})
			w.settle()
			id := w.tunnelID(t, tunnelName)
			delete(w.eng.place.ids, "pve3")
			tt.prep(w, id, armed)
			n := len(w.cf.Calls())

			res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

			require.NoError(t, err)
			require.Empty(t, res.Pending)
			require.Contains(t, w.callsSince(n), "CleanUpConnections acc1 "+id)
		})
	}
}

// The rotation stays pending, and the node's connector is dropped again,
// while a later listing still shows it.
func TestAForgetStaysPendingWhileTheNodesConnectorIsListed(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.listAt(t, tunnelName, "pve2-"+id, "pve3-"+id)
	w.tick(time.Second)
	_, err := w.eng.ForgetNode(t.Context(), "pve3", false)
	require.NoError(t, err)
	require.Equal(t, []string{"pve2-" + id}, w.listedIDs(t, id))

	// Cloudflare lists it again, as a listing that lagged does.
	w.cf.SetConnectors(testAccount, id, []cfapi.Connector{{ID: "pve2-" + id}, {ID: "pve3-" + id}})
	n := len(w.cf.Calls())
	w.tick(time.Second)
	require.NotNil(t, entryOf(t, w.env, "pve3").Rotation, "kept while the node's connector is listed")
	require.Contains(t, w.callsSince(n), "CleanUpConnector acc1 "+id+" pve3-"+id)
	require.Equal(t, []string{"pve2-" + id}, w.listedIDs(t, id))

	w.tick(time.Second)
	require.Nil(t, entryOf(t, w.env, "pve3").Rotation)
}

// A connector the node started after it last reported, before the secret
// was given, is no one's: it is dropped with the ones recorded, and keeps
// the rotation pending while it is listed. One that started after the
// secret was given runs with the new token, as a replica that restarted and
// has not reported yet: it stays.
func TestAForgetDropsEveryConnectorNoNodeOwns(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.conn.setConnectorID(id, "c-pve1")
	w.listAt(t, tunnelName, "c-pve1", "pve2-"+id, "pve3-"+id)
	w.tick(time.Second)
	before := w.clock.now().Add(-time.Minute)
	w.cf.SetConnectors(testAccount, id, []cfapi.Connector{
		{ID: "c-pve1"}, {ID: "pve2-" + id}, {ID: "pve3-" + id}, {ID: "pve3-new", RunAt: before},
	})
	n := len(w.cf.Calls())

	res, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	require.Empty(t, res.Pending)
	calls := w.callsSince(n)
	require.Contains(t, calls, "CleanUpConnector acc1 "+id+" pve3-"+id)
	require.Contains(t, calls, "CleanUpConnector acc1 "+id+" pve3-new")
	require.NotContains(t, calls, "CleanUpConnections acc1 "+id)
	require.Equal(t, []string{"c-pve1", "pve2-" + id}, w.listedIDs(t, id))

	// The node's unrecorded connector is listed again, beside a replica's
	// that started with the new token and was not reported yet.
	rotatedAt := entryOf(t, w.env, "pve3").Rotation.At
	w.cf.SetConnectors(testAccount, id, []cfapi.Connector{
		{ID: "c-pve1"}, {ID: "pve2-" + id}, {ID: "pve2-next", RunAt: rotatedAt.Add(time.Second)}, {ID: "pve3-new", RunAt: before},
	})
	n = len(w.cf.Calls())
	w.tick(time.Second)
	require.NotNil(t, entryOf(t, w.env, "pve3").Rotation, "kept while a connector no node owns is listed")
	require.Contains(t, w.callsSince(n), "CleanUpConnector acc1 "+id+" pve3-new")
	require.NotContains(t, w.callsSince(n), "CleanUpConnector acc1 "+id+" pve2-next")
	require.Equal(t, []string{"c-pve1", "pve2-" + id, "pve2-next"}, w.listedIDs(t, id))
	w.tick(time.Second)
	require.Nil(t, entryOf(t, w.env, "pve3").Rotation)
}

// When a replica reported no connector, its own cannot be told from the
// lost ones: every connection of the tunnel is ended.
func TestAForgetEndsEveryConnectionWhenAReplicaIsNotHeardOf(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	id := w.tunnelID(t, tunnelName)
	w.conn.setConnectorID(id, "c-pve1")
	w.listAt(t, tunnelName, "c-pve1", "pve2-"+id, "pve3-"+id, "stray")
	w.tick(time.Second)
	delete(w.eng.place.ids, "pve2")
	n := len(w.cf.Calls())

	_, err := w.eng.ForgetNode(t.Context(), "pve3", false)

	require.NoError(t, err)
	require.Contains(t, w.callsSince(n), "CleanUpConnections acc1 "+id)
}
