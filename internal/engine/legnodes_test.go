package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agent"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// onLeg makes the report of a target behind a leg what an agent reports: it
// is verified only once the snapshot gives it its service address, at the
// address that maps to, and the node that resolves it reports the binding
// through the leg.
func (w *world) onLeg(r *agentproto.TargetReport, t agentproto.Target, network string) {
	if !t.Service.IsValid() || t.Addr.IsValid() && t.Addr != r.Addr {
		r.OK, r.InEgressSet, r.Reason = false, false, "service address not allocated"
	}
	if !t.Resolve || t.Guest == nil {
		return
	}
	mac, _ := model.NormalizeMAC(t.Guest.NICs[0].MAC)
	raw, err := agent.EncodeResult(resolve.Result{
		Target: planner.ResolvedTarget{Addr: r.Addr, Reachable: true, Owner: t.Route.Owner()},
		Level:  resolve.LevelPort,
		Binding: &resolve.Binding{Owner: t.Route.Owner(), Hostname: t.Route.Hostname, Guest: t.Route.Guest.String(),
			Addr: r.Addr, MAC: mac, VerifiedAt: w.clock.now(), Level: resolve.LevelPort, Via: network},
	})
	if err != nil {
		panic(err)
	}
	r.Result = raw
}

// attach saves an attached network of the three nodes with a leg address on
// each node named.
func (w *world) attach(t *testing.T, id string, mark int, scope string, prefix string, legs map[string]string) {
	t.Helper()
	n := store.Network{
		ID: id, Kind: store.NetworkAttach, Bridge: id, Prefix: netip.MustParsePrefix(prefix), Legs: map[string]netip.Addr{},
		LegMAC: store.LegMACFor(store.NetworkAttach, mark), Mark: mark, Scope: scope, CreatedAt: t0,
	}
	for node, addr := range legs {
		n.Legs[node] = netip.MustParseAddr(addr)
	}
	require.NoError(t, w.store.SaveNetworkIndexed(n, 0, map[string]int{"pve1": 0, "pve2": 1, "pve3": 2}))
}

// legsWorld is threeNodes with a gateway on this node, made again so that the
// engine drives it.
func legsWorld(t *testing.T, setup func(w *world)) (*world, *fakeGateway) {
	t.Helper()
	w := threeNodes(t)
	gw := &fakeGateway{log: &callLog{}, down: map[string]string{}}
	w.gateway = gw
	setup(w)
	w.eng = w.newEngine()
	w.watchHere(t)
	return w, gw
}

// runFor runs cycles for d, every five seconds.
func (w *world) runFor(d time.Duration) State {
	var st State
	for end := w.clock.now().Add(d); w.clock.now().Before(end); {
		st = w.tick(5 * time.Second)
	}
	return st
}

// A network that is one segment on every node, with a leg on each: the
// guest's node resolves its target through its own leg, every node verifies
// it through its own, and all of them dial the one service address, each
// gateway forwarding it to the guest through its own leg. The route is
// shared.
func TestATargetOnANetworkOfTheClusterHasOneServiceAddressOnEveryNode(t *testing.T) {
	w, gw := legsWorld(t, func(w *world) {
		w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve1": "10.0.0.250", "pve2": "10.0.0.251", "pve3": "10.0.0.252"})
		w.cardOn(102, "vmbr1")
		w.res.goVia("db.example.com", "vmbr1")
		for _, n := range []string{"pve2", "pve3"} {
			w.acts[n].legs = map[string]string{"db.example.com": "vmbr1"}
		}
	})

	st := w.runFor(time.Minute)

	all, err := w.store.ServiceAddrs()
	require.NoError(t, err)
	require.Len(t, all, 1)
	m := all[0]
	require.Equal(t, "vmbr1", m.Network)
	require.Equal(t, dbAddr, m.Addr)
	nodes, err := w.store.NodeServiceAddrs()
	require.NoError(t, err)
	require.Empty(t, nodes)

	for _, n := range []string{"pve2", "pve3"} {
		tg := w.targetOf(t, n, "db.example.com")
		require.Equal(t, m.Service, tg.Service, "node %s dials the one service address", n)
		require.Equal(t, dbAddr, tg.Addr)
	}
	require.Contains(t, gw.last(t).services, gateway.Service{Leg: "vmbr1", Service: m.Service, Addr: dbAddr, Ports: []uint16{8080}},
		"this node forwards it through its own leg")

	r := route(st, "db.example.com")
	require.Equal(t, planner.StateActive, r.State, r.Reason)
	require.Equal(t, "http://"+m.Service.String()+":8080", r.Service)
	require.Equal(t, TunnelShared, r.Placement.Kind)
	requireNoProblem(t, st, "has no leg on node")

	t.Run("a replica without a leg is named", func(t *testing.T) {
		w, _ := legsWorld(t, func(w *world) {
			w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve1": "10.0.0.250", "pve2": "10.0.0.251"})
			w.cardOn(102, "vmbr1")
			w.res.goVia("db.example.com", "vmbr1")
			w.acts["pve2"].legs = map[string]string{"db.example.com": "vmbr1"}
			w.acts["pve3"].fail = map[string]string{"db.example.com": "no leg into vmbr1"}
		})

		st := w.runFor(2 * time.Minute)

		require.Contains(t, st.Problems, "network vmbr1 has no leg on node pve3; its routes are served from the nodes that have one")
		pinned2, _ := planner.PinnedTunnelName(testInstall, "pve2")
		r := route(st, "db.example.com")
		require.Equal(t, TunnelPinned, r.Placement.Kind, "a new route waits for its service address before it is placed")
		require.Equal(t, "pve2", r.Placement.Node)
		require.Equal(t, pinned2, w.recordTunnel(t, "db.example.com"))
	})
}

// Two isolated bridges of one name on two nodes may hold the same address,
// which are two guests: each node's target gets a service address of its
// own, kept by node, and its route is pinned to that node.
func TestTheSameAddressOnANetworkOfEachNodeIsTwoTargets(t *testing.T) {
	isolated := netip.MustParseAddr("10.60.0.5")
	w, _ := legsWorld(t, func(w *world) {
		w.attach(t, "vmbr9", 3, store.ScopeNode, "10.60.0.0/24", map[string]string{"pve2": "10.60.0.250", "pve3": "10.60.0.250"})
		w.cardOn(102, "vmbr9")
		w.cardOn(103, "vmbr9")
		w.res.addrs["app.example.com"] = isolated
		w.acts["pve2"].legs = map[string]string{"db.example.com": "vmbr9"}
		w.acts["pve2"].addrs = map[string]netip.Addr{"db.example.com": isolated}
		w.acts["pve3"].legs = map[string]string{"app.example.com": "vmbr9"}
		w.acts["pve3"].addrs = map[string]netip.Addr{"app.example.com": isolated}
	})

	st := w.runFor(2 * time.Minute)

	nodes, err := w.store.NodeServiceAddrs()
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	byNode := map[string]store.NodeServiceAddr{}
	for _, m := range nodes {
		require.Equal(t, "vmbr9", m.Network)
		require.Equal(t, isolated, m.Addr)
		byNode[m.Node] = m
	}
	require.NotEqual(t, byNode["pve2"].Service, byNode["pve3"].Service)
	cluster, err := w.store.ServiceAddrs()
	require.NoError(t, err)
	require.Empty(t, cluster, "no mapping of the network across the cluster")

	for host, node := range map[string]string{"db.example.com": "pve2", "app.example.com": "pve3"} {
		r := route(st, host)
		require.Equal(t, TunnelPinned, r.Placement.Kind, host)
		require.Equal(t, node, r.Placement.Node, host)
		require.Equal(t, "it is reached through network vmbr9, which is a segment of node "+node+" alone", r.Placement.Why)
		require.Equal(t, byNode[node].Service, w.targetOf(t, node, host).Service)
		require.Equal(t, "http://"+byNode[node].Service.String()+":8080", r.Service, host)
	}
}

// requireNoProblem fails when a problem line of the state holds part.
func requireNoProblem(t *testing.T, st State, part string) {
	t.Helper()
	for _, p := range st.Problems {
		require.NotContains(t, p, part)
	}
}

// cardOn puts the one card of the guest of vmid on bridge.
func (w *world) cardOn(vmid int, bridge string) {
	snap := w.inv.snapshot()
	for i := range snap.Guests {
		if snap.Guests[i].Ref.VMID == vmid {
			snap.Guests[i].NICs[0].Bridge = bridge
		}
	}
	w.inv.set(snap)
}

// What an agent reports of a target behind its leg decides nothing the
// controller does not know: a leg the node has not, an address outside the
// network, a guest of another node or a card the guest has not on the
// network's bridge is refused, and the target is not served through the leg.
func TestALegTheControllerDoesNotKnowIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		setup func(w *world)
		why   string
	}{
		{"no leg on the node", func(w *world) {
			w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve1": "10.0.0.250", "pve3": "10.0.0.252"})
			w.cardOn(102, "vmbr1")
		}, "node pve2 has no leg into it"},
		{"an address outside the network", func(w *world) {
			w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.9.0.0/24", map[string]string{"pve2": "10.9.0.251"})
			w.cardOn(102, "vmbr1")
		}, "10.0.0.22 is no guest's address on 10.9.0.0/24"},
		{"the leg's own address", func(w *world) {
			w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve2": "10.0.0.22"})
			w.cardOn(102, "vmbr1")
		}, "10.0.0.22 is no guest's address on 10.0.0.0/24"},
		{"a card on another bridge", func(w *world) {
			w.attach(t, "vmbr1", 1, store.ScopeCluster, "10.0.0.0/24", map[string]string{"pve2": "10.0.0.251"})
		}, "qemu/102 has no card bc:24:11:00:02:01 on vmbr1"},
		{"a network that is not attached", func(w *world) {}, "network vmbr1 is not attached"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := legsWorld(t, func(w *world) {
				tc.setup(w)
				w.acts["pve2"].legs = map[string]string{"db.example.com": "vmbr1"}
			})

			st := w.runFor(time.Minute)

			require.Contains(t, st.Problems, "node pve2 reported db.example.com behind network vmbr1, which is refused: "+tc.why+"; it is not served through the leg")
			all, err := w.store.ServiceAddrs()
			require.NoError(t, err)
			require.Empty(t, all)
			r := route(st, "db.example.com")
			require.NotEqual(t, planner.StateActive, r.State)
			require.Empty(t, r.Service, "its real address is never dialed")
			require.False(t, w.targetOf(t, "pve3", "db.example.com").Service.IsValid())
		})
	}
}
