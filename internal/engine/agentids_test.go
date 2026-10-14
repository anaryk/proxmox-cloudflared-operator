package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

// rogueIDs are the ids of the rogue connectors of the last state.
func (w *world) rogueIDs() []string {
	var out []string
	for _, r := range w.eng.State().RogueConnectors {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}

// listAt has Cloudflare list connectors of ids on a tunnel, and lets the
// next cycle list them.
func (w *world) listAt(t *testing.T, name string, ids ...string) {
	t.Helper()
	var conns []cfapi.Connector
	for _, id := range ids {
		conns = append(conns, cfapi.Connector{ID: id, OriginIP: "10.0.0.9"})
	}
	w.cf.SetConnectors(testAccount, w.tunnelID(t, name), conns)
	w.clock.advance(zoneRefreshEvery)
}

// The connectors the agents of the replicas run are pco's: Cloudflare
// listing them on the shared tunnel shows no rogue connector.
func TestTheConnectorsOfEveryAgentArePcos(t *testing.T) {
	w := threeNodes(t)
	w.settle()
	sharedID := w.tunnelID(t, tunnelName)
	w.conn.setConnectorID(sharedID, "c-pve1")

	w.listAt(t, tunnelName, "c-pve1", "pve2-"+sharedID, "pve3-"+sharedID)
	w.tick(time.Second)

	require.Empty(t, w.rogueIDs())
}

// An agent may own one connector per tunnel and the one before it: a
// third id it reports is named and treated as rogue; and a connector a
// node reports of the tunnel pinned to another node is never its own.
func TestAnAgentOwnsOneConnectorPerTunnel(t *testing.T) {
	t.Run("a third id", func(t *testing.T) {
		w := threeNodes(t)
		w.settle()
		sharedID := w.tunnelID(t, tunnelName)
		w.conn.setConnectorID(sharedID, "c-pve1")
		// It ran pve2-<id> while the cluster settled; a is its next, b a third.
		for _, id := range []string{"a", "b"} {
			w.acts["pve2"].edit = func(st *agentproto.Status) {
				for i := range st.Connectors {
					st.Connectors[i].ConnectorID = id
				}
			}
			w.tick(time.Second)
		}
		st := w.tick(time.Second)
		require.True(t, hasProblem(st, "agent of node pve2 reports a second connector b on tunnel "+tunnelName), st.Problems)

		w.listAt(t, tunnelName, "c-pve1", "pve2-"+sharedID, "a", "b", "pve3-"+sharedID)
		w.tick(time.Second)
		require.Equal(t, []string{"b"}, w.rogueIDs())
	})
	t.Run("another node's pinned tunnel", func(t *testing.T) {
		w, pinned2 := pinnedDB(t)
		pinnedID := w.tunnelID(t, pinned2)
		w.acts["pve3"].edit = func(st *agentproto.Status) {
			st.Connectors = append(st.Connectors, connector.Status{TunnelID: pinnedID, Active: true, Ready: true, ConnectorID: "stolen"})
		}
		w.tick(time.Second)

		w.listAt(t, pinned2, "pve2-"+pinnedID, "stolen")
		w.tick(time.Second)
		require.Equal(t, []string{"stolen"}, w.rogueIDs())
	})
}
