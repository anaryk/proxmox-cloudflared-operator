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
