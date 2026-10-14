package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// joining adds pve4 to the registry as a node that began joining and whose
// agent never started.
func (w *world) joining(t *testing.T) {
	t.Helper()
	require.NoError(t, w.store.SaveNode(store.NodeEntry{Name: "pve4", Since: t0, State: store.NodeJoining, StateSince: t0}))
}

// A silent node holds allocation back for a while, and not for ever: one that
// sent nothing for silentWait stops holding it, and what its last report
// says its gateway maps stays taken. An admitted node that never reported to
// this controller holds it until it reports or is forgotten: what its gateway
// maps is not known.
func TestASilentNodeHoldsAllocationForAWhile(t *testing.T) {
	t.Run("a joining node whose agent never starts", func(t *testing.T) {
		w, _ := legsWorld(t, func(w *world) {
			w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve1": "10.0.0.250", "pve2": "10.0.0.251", "pve3": "10.0.0.252"})
			w.cardOn(102, "vmbr1")
			w.res.goVia("db.example.com", "vmbr1")
			for _, n := range []string{"pve2", "pve3"} {
				w.acts[n].legs = map[string]string{"db.example.com": "vmbr1"}
			}
			w.joining(t)
		})

		st := w.runFor(4 * time.Minute)

		require.Contains(t, st.Problems, held+"node pve4 has not reported to this controller yet")
		all, err := w.store.ServiceAddrs()
		require.NoError(t, err)
		require.Empty(t, all)

		st = w.runFor(2 * time.Minute)

		require.False(t, hasProblem(st, held), st.Problems)
		require.Contains(t, st.Problems, "node pve4 has sent no status to this controller for 5m0s; service addresses and managed cards "+
			"are made without it, past what its last report maps: bring its agent back, or pco node drain pve4 and pco node forget pve4")
		all, err = w.store.ServiceAddrs()
		require.NoError(t, err)
		require.Len(t, all, 1, "made without the silent node")
	})

	t.Run("a dead node", func(t *testing.T) {
		w := threeNodes(t)
		w.mappings(t, 5)
		w.joining(t)
		w.acts["pve3"].edit = func(st *agentproto.Status) { st.ServiceAddrs = []netip.Addr{netip.MustParseAddr("198.18.1.7")} }
		w.tick(time.Second)
		w.acts["pve3"].silent = true

		st := w.runFor(4 * time.Minute)

		require.True(t, hasProblem(st, held), st.Problems)
		require.Equal(t, netip.MustParseAddr("198.18.1.5"), poolNext(t, w), "nothing moves before every node is accounted for")

		st = w.runFor(2 * time.Minute)

		require.False(t, hasProblem(st, held), st.Problems)
		require.True(t, hasProblem(st, "node pve3 has sent no status to this controller for 5m0s"), st.Problems)
		require.Equal(t, netip.MustParseAddr("198.18.1.8"), poolNext(t, w), "what the dead node's last report maps stays taken")
	})

	t.Run("an admitted node that never reported", func(t *testing.T) {
		w := threeNodes(t)
		w.acts["pve3"].silent = true

		st := w.runFor(10 * time.Minute)

		require.Contains(t, st.Problems, held+"node pve3 has not reported to this controller, so what its gateway maps is not known: "+
			"bring its agent back, or pco node forget pve3")
	})
}
