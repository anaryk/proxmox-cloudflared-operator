package engine

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// netCluster is the node of newNetEnv as the controller of a cluster of
// three: pve2 and pve3 have the network of this node as Proxmox has it
// configured, and agents that report.
type netCluster struct {
	*world
	gw   *probeGateway
	nets *fakeNetworks
}

func newNetCluster(t *testing.T) *netCluster {
	t.Helper()
	n := newNetEnv(t)
	e := n.env
	zero, one, two := 0, 1, 2
	for _, entry := range []store.NodeEntry{
		{Name: testNode, Since: t0, State: store.NodeActive, StateSince: t0, Index: &zero},
		{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one},
		{Name: "pve3", Since: t0, State: store.NodeActive, StateSince: t0, Index: &two},
	} {
		require.NoError(t, e.store.SaveNode(entry))
	}
	r := &fakeRemote{}
	listed := &[]members.Member{{Name: testNode, ID: 1, Online: true}, {Name: "pve2", ID: 2, Online: true}, {Name: "pve3", ID: 3, Online: true}}
	e.remote = r
	e.members = func() members.Members { return members.Members{Nodes: *listed} }
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	n.nets.nodes = map[string][]pve.NodeIface{"pve2": slices.Clone(n.nets.config), "pve3": slices.Clone(n.nets.config)}
	n.nets.zones = []pve.SDNZone{{Name: "pco", Type: "simple"}, {Name: "z1", Type: "vxlan"}}
	e.eng = e.newEngine()
	e.enforce()
	w := &world{env: e, r: r, lease: l, listed: listed, acts: map[string]*agentAct{
		"pve2": {kind: pki.KindHost, started: t0.Add(-time.Hour)},
		"pve3": {kind: pki.KindHost, started: t0.Add(-time.Hour)},
	}}
	w.watchHere(t)
	w.settle()
	return &netCluster{world: w, gw: n.gw, nets: n.nets}
}

// agentIfaces are the interfaces of the node of an agent that answers a
// probe: vmbr0 with the node's own address, vmbr1 and vmbr2 with its bond,
// pconet and the vnet lab.
func agentIfaces(node string) []agentproto.ProbeIface {
	own := map[string]string{"pve2": "192.168.4.208/24", "pve3": "192.168.4.209/24"}[node]
	return []agentproto.ProbeIface{
		{Name: "vmbr0", Bridge: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix(own)}},
		{Name: "eno1", Master: "vmbr0"},
		{Name: "vmbr1", Bridge: "vmbr1"},
		{Name: "vmbr2", Bridge: "vmbr2"},
		{Name: "bond0", Master: "vmbr2"},
		{Name: "pconet", Bridge: "pconet"},
		{Name: "lab", Bridge: "lab"},
		{Name: "vxlan_lab", Master: "lab", Bridge: "lab"},
	}
}

// probeAnswer answers a probe with the interfaces of the node and nobody
// at the address.
func probeAnswer(node string, _ agentproto.Probe) agentproto.ActionResult {
	return answered(agentproto.ProbeAnswer{Interfaces: agentIfaces(node), AnsweredBy: []string{}})
}

func answered(a agentproto.ProbeAnswer) agentproto.ActionResult {
	data, err := json.Marshal(a)
	if err != nil {
		panic(err)
	}
	return agentproto.ActionResult{OK: true, Data: data}
}

// probes runs cycles while call runs, and has the agent of every node that
// is handed a probe answer it with answer, once; a nil answer leaves it
// unanswered. It returns what each was asked.
func (c *netCluster) probes(t *testing.T, answer func(node string, p agentproto.Probe) agentproto.ActionResult, call func()) map[string]agentproto.Probe {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		call()
	}()
	asked := map[string]agentproto.Probe{}
	for {
		select {
		case <-done:
			return asked
		case <-time.After(time.Millisecond):
		}
		c.tick(time.Second)
		for _, node := range []string{"pve2", "pve3"} {
			s, ok := c.r.lastOf(node)
			if !ok {
				continue
			}
			for _, act := range s.Actions {
				if act.Kind != agentproto.ActionProbe {
					continue
				}
				if _, seen := asked[node]; seen {
					continue
				}
				var p agentproto.Probe
				require.NoError(t, json.Unmarshal(act.Args, &p))
				asked[node] = p
				if answer == nil {
					continue
				}
				res := answer(node, p)
				res.ID = act.ID
				c.eng.TakeAgentEvents(node, agentproto.Status{Node: node, AgentStartedAt: t0.Add(-time.Hour), EpochSeen: s.Epoch,
					Actions: []agentproto.ActionResult{res}}, s.Epoch)
			}
		}
	}
}

func (c *netCluster) attach(t *testing.T, r AttachRequest, answer func(string, agentproto.Probe) agentproto.ActionResult) (NetworkView, map[string]agentproto.Probe, error) {
	t.Helper()
	var (
		v   NetworkView
		err error
	)
	asked := c.probes(t, answer, func() { v, err = c.eng.Attach(t.Context(), r) })
	return v, asked, err
}

var (
	leg250 = netip.MustParseAddr("10.20.0.250")
	leg249 = netip.MustParseAddr("10.20.0.249")
	leg248 = netip.MustParseAddr("10.20.0.248")
	vlan20 = netip.MustParsePrefix("10.20.0.0/24")
)

// A network of scope cluster gets a leg on every node the attach names,
// each checked by the agent of its node before it is saved, with the MAC
// of the node's index; the next snapshot of the node gives its gateway the
// leg.
func TestAClusterNetworkGetsALegOnEveryNodeItNames(t *testing.T) {
	c := newNetCluster(t)

	v, asked, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true,
		Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": leg249, "pve3": leg248}}, probeAnswer)

	require.NoError(t, err)
	require.Equal(t, map[string]agentproto.Probe{
		"pve2": {Bridge: "vmbr2", VLAN: 20, Addr: leg249},
		"pve3": {Bridge: "vmbr2", VLAN: 20, Addr: leg248},
	}, asked)
	require.Equal(t, []string{"vmbr2 20 10.20.0.250"}, c.gw.probed(), "this node probes its own")
	stored, err := c.store.Networks()
	require.NoError(t, err)
	n := stored["vmbr2.20"]
	require.Equal(t, map[string]netip.Addr{testNode: leg250, "pve2": leg249, "pve3": leg248}, n.Legs)
	require.Equal(t, store.ScopeCluster, n.Scope)
	require.Equal(t, map[string]string{
		testNode: store.NodeLegMAC(0, n.Mark), "pve2": store.NodeLegMAC(1, n.Mark), "pve3": store.NodeLegMAC(2, n.Mark),
	}, n.LegMACs)
	require.Len(t, v.Legs, 3, "the view has the leg of every node")

	c.tick(time.Second)
	s, ok := c.r.lastOf("pve2")
	require.True(t, ok)
	var cache gateway.Cache
	require.NoError(t, json.Unmarshal(s.Networks, &cache))
	require.Len(t, cache.Legs, 1)
	require.Equal(t, "10.20.0.249/24", cache.Legs[0].Prefix.String())
	require.Equal(t, store.NodeLegMAC(1, n.Mark), cache.Legs[0].MAC.String())
}

// An agent's answer is the word of its node, and of it alone: a leg it does
// not find safe is not made, and neither is the network; an answer that
// does not come, or cannot be read, makes no leg either.
func TestALegIsRefusedOnWhatTheAgentOfItsNodeFinds(t *testing.T) {
	ifacesWith := func(extra ...agentproto.ProbeIface) func(string, agentproto.Probe) agentproto.ActionResult {
		return func(node string, _ agentproto.Probe) agentproto.ActionResult {
			return answered(agentproto.ProbeAnswer{Interfaces: append(agentIfaces(node), extra...), AnsweredBy: []string{}})
		}
	}
	for _, tc := range []struct {
		name   string
		answer func(string, agentproto.Probe) agentproto.ActionResult
		err    string
	}{
		{name: "someone answers for the address", answer: func(node string, p agentproto.Probe) agentproto.ActionResult {
			if node != "pve2" {
				return probeAnswer(node, p)
			}
			return answered(agentproto.ProbeAnswer{Interfaces: agentIfaces(node), AnsweredBy: []string{"bc:24:11:00:00:09"}})
		}, err: "10.20.0.249 is in use on vmbr2 VLAN 20 of node pve2: bc:24:11:00:00:09 answered"},
		{name: "the node has an address there", answer: ifacesWith(agentproto.ProbeIface{Name: "vmbr2.20", Bridge: "vmbr2", VLAN: 20,
			Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.2/24")}}), err: "node pve2 reaches vmbr2 VLAN 20 directly"},
		{name: "the address is the node's", answer: ifacesWith(agentproto.ProbeIface{Name: "vmbr2.40", Bridge: "vmbr2", VLAN: 40,
			Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.0.249/32")}}), err: "10.20.0.249 is an address of node pve2"},
		{name: "the node has no such bridge", answer: func(node string, p agentproto.Probe) agentproto.ActionResult {
			return answered(agentproto.ProbeAnswer{Interfaces: agentIfaces(node)[:3], AnsweredBy: []string{}})
		}, err: "vmbr2 is no bridge or vnet of node pve2"},
		{name: "the agent cannot probe", answer: func(string, agentproto.Probe) agentproto.ActionResult {
			return agentproto.ActionResult{Error: "the gateway namespace is not up"}
		}, err: "the agent of node pve2 could not probe: the gateway namespace is not up"},
		{name: "the answer cannot be read", answer: func(string, agentproto.Probe) agentproto.ActionResult {
			return agentproto.ActionResult{OK: true, Data: json.RawMessage(`{"answeredBy":["nobody"]}`)}
		}, err: "the probe answer of the agent of node pve2 is not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newNetCluster(t)

			_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true, OverlapOK: true,
				Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": leg249, "pve3": leg248}}, tc.answer)

			require.Error(t, err)
			require.ErrorContains(t, err, tc.err)
			stored, err := c.store.Networks()
			require.NoError(t, err)
			require.Empty(t, stored, "no leg and no network")
		})
	}
}

// An answer is taken only from the node whose agent was asked: another
// node cannot answer for it, and a node that does not answer in time gets
// no leg.
func TestAnAnswerOfAnotherNodeIsNotTaken(t *testing.T) {
	c := newNetCluster(t)
	c.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d == probeWait {
			d = 300 * time.Millisecond
		}
		return context.WithTimeout(ctx, d)
	}
	var err error
	c.probes(t, nil, func() {
		go func() {
			for range 50 {
				time.Sleep(5 * time.Millisecond)
				s, ok := c.r.lastOf("pve2")
				if !ok || len(s.Actions) == 0 {
					continue
				}
				res := probeAnswer("pve3", agentproto.Probe{})
				res.ID = s.Actions[0].ID
				c.eng.TakeAgentEvents("pve3", agentproto.Status{Node: "pve3", EpochSeen: s.Epoch, Actions: []agentproto.ActionResult{res}}, s.Epoch)
				return
			}
		}()
		_, err = c.eng.Attach(t.Context(), AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, ConfirmUplink: true,
			Scope: store.ScopeNode, Legs: map[string]netip.Addr{"pve2": leg249}})
	})

	require.ErrorContains(t, err, "the agent of node pve2 did not answer the probe within")
}

// What an agent says of its node never widens what the controller decides:
// a bridge Proxmox does not have configured on the node is no bridge to
// attach there, and a bridge of one node stays one whatever ports its agent
// lists.
func TestAnAgentDoesNotWidenWhatIsAttached(t *testing.T) {
	t.Run("a bridge Proxmox does not list", func(t *testing.T) {
		c := newNetCluster(t)
		_, _, err := c.attach(t, AttachRequest{Bridge: "pcot2", Prefix: netip.MustParsePrefix("10.60.0.0/24"),
			Scope: store.ScopeNode, Legs: map[string]netip.Addr{"pve2": netip.MustParseAddr("10.60.0.250")}},
			func(node string, _ agentproto.Probe) agentproto.ActionResult {
				return answered(agentproto.ProbeAnswer{Interfaces: append(agentIfaces(node), agentproto.ProbeIface{Name: "pcot2", Bridge: "pcot2"}), AnsweredBy: []string{}})
			})
		require.ErrorContains(t, err, "pcot2 is no bridge or vnet of node pve2")
	})
	t.Run("an uplink of a bridge of one node", func(t *testing.T) {
		c := newNetCluster(t)
		_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: netip.MustParseAddr("10.77.0.250"),
			ConfirmUplink: true, Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": netip.MustParseAddr("10.77.0.249")}},
			func(node string, _ agentproto.Probe) agentproto.ActionResult {
				return answered(agentproto.ProbeAnswer{Interfaces: append(agentIfaces(node), agentproto.ProbeIface{Name: "eno9", Master: "vmbr1"}), AnsweredBy: []string{}})
			})
		require.ErrorIs(t, err, ErrRefused)
		require.ErrorContains(t, err, "vmbr1 has no physical uplink on node pve1, so it is a bridge of that node alone; attach it with the scope node")
	})
}

// A vnet of a Simple zone is a bridge of each node of its own.
func TestAVnetOfASimpleZoneIsNoClusterNetwork(t *testing.T) {
	c := newNetCluster(t)
	c.nets.zones = []pve.SDNZone{{Name: "pco", Type: "simple"}, {Name: "z1", Type: "simple"}}

	_, _, err := c.attach(t, AttachRequest{Bridge: "lab", Prefix: netip.MustParsePrefix("10.88.0.0/24"), Address: netip.MustParseAddr("10.88.0.250"),
		ConfirmUplink: true, Scope: store.ScopeCluster}, probeAnswer)

	require.ErrorContains(t, err, "the vnet lab is in the zone z1 of type simple, a bridge of each node of its own")
}

// A network of scope node has its leg on the node the attach chose, which
// need not be this one; this node probes nothing.
func TestANodeNetworkGetsItsLegOnTheNodeChosen(t *testing.T) {
	c := newNetCluster(t)
	leg := netip.MustParseAddr("10.77.0.250")

	_, asked, err := c.attach(t, AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"), Address: leg,
		Scope: store.ScopeNode, Node: "pve2"}, probeAnswer)

	require.NoError(t, err)
	require.Equal(t, map[string]agentproto.Probe{"pve2": {Bridge: "vmbr1", Addr: leg}}, asked)
	require.Empty(t, c.gw.probed())
	stored, err := c.store.Networks()
	require.NoError(t, err)
	n := stored["vmbr1"]
	require.Equal(t, map[string]netip.Addr{"pve2": leg}, n.Legs)
	require.Equal(t, store.ScopeNode, n.Scope)
}

func TestAnAttachOnAClusterIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		r     AttachRequest
		field string
		err   string
	}{
		{name: "without a scope", r: AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true},
			field: "scope", err: "on a cluster, name the scope of the network"},
		{name: "with two legs of scope node", r: AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
			Address: netip.MustParseAddr("10.77.0.250"), Scope: store.ScopeNode, Legs: map[string]netip.Addr{"pve2": netip.MustParseAddr("10.77.0.249")}},
			field: "legs", err: "a network of scope node has one leg"},
		{name: "with two legs at one address", r: AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true,
			Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": leg250}}, field: "legs", err: "10.20.0.250 is the leg of two nodes"},
		{name: "with a leg of a node the install does not have", r: AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true,
			Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve9": leg249}}, field: "legs", err: "node pve9 is not a node of this install"},
		{name: "with a scope of another name", r: AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, Scope: "everywhere"},
			field: "scope", err: "scope everywhere: want cluster or node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newNetCluster(t)

			_, asked, err := c.attach(t, tc.r, probeAnswer)

			var fe *FieldError
			require.ErrorAs(t, err, &fe)
			require.Equal(t, tc.field, fe.Field)
			require.ErrorContains(t, err, tc.err)
			require.Empty(t, asked, "no agent was asked")
		})
	}
}

// A node whose agent does not report gets no leg.
func TestANodeWithoutAnAgentGetsNoLeg(t *testing.T) {
	c := newNetCluster(t)
	c.acts["pve3"].silent = true
	for range 8 {
		c.tick(10 * time.Second)
	}

	_, asked, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true,
		Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": leg249, "pve3": leg248}}, probeAnswer)

	require.ErrorContains(t, err, "node pve3 has no host agent that reports to this controller")
	require.NotContains(t, asked, "pve3")
}

// The probe of a network of scope cluster proposes a leg for every node,
// each at an address of its own, and says why a node gets none.
func TestAProbeOfAClusterNetworkProposesALegForEveryNode(t *testing.T) {
	c := newNetCluster(t)
	var (
		res ProbeResult
		err error
	)
	asked := c.probes(t, func(node string, p agentproto.Probe) agentproto.ActionResult {
		if node == "pve3" {
			return answered(agentproto.ProbeAnswer{Interfaces: agentIfaces(node)[:3], AnsweredBy: []string{}})
		}
		return probeAnswer(node, p)
	}, func() {
		res, err = c.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr2", VLAN: 20, Scope: store.ScopeCluster})
	})

	require.NoError(t, err)
	require.True(t, res.Clustered)
	require.Equal(t, store.ScopeCluster, res.Suggested)
	require.Equal(t, store.ScopeCluster, res.Scope)
	require.Equal(t, vlan20, res.Prefix)
	require.Equal(t, netip.MustParseAddr("10.20.0.254"), res.Address)
	require.Equal(t, map[string]agentproto.Probe{
		"pve2": {Bridge: "vmbr2", VLAN: 20, Addr: netip.MustParseAddr("10.20.0.253")},
		"pve3": {Bridge: "vmbr2", VLAN: 20, Addr: netip.MustParseAddr("10.20.0.252")},
	}, asked)
	require.Equal(t, []LegProbe{
		{Node: testNode, Address: netip.MustParseAddr("10.20.0.254"), Free: true, AnsweredBy: []string{}, Uplink: true},
		{Node: "pve2", Address: netip.MustParseAddr("10.20.0.253"), Free: true, AnsweredBy: []string{}, Uplink: true},
		{Node: "pve3", Address: netip.MustParseAddr("10.20.0.252"), AnsweredBy: []string{}, Problem: "vmbr2 is no bridge or vnet of node pve3", Missing: true},
	}, res.Legs)
}

// The probe of a node alone says nothing of scopes, as before the cluster.
func TestAProbeWithoutAScopeSuggestsOne(t *testing.T) {
	c := newNetCluster(t)

	res, err := c.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr1"})

	require.NoError(t, err)
	require.True(t, res.Clustered)
	require.Equal(t, store.ScopeNode, res.Suggested, "vmbr1 has no uplink")
	require.Empty(t, res.Legs)
}

// A node that has no leg of a network attached gets one by an attach that
// names it: the network keeps its mark and its other legs.
func TestALegIsAddedToANetworkAttached(t *testing.T) {
	c := newNetCluster(t)
	_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true,
		Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": leg249}}, probeAnswer)
	require.NoError(t, err)
	before, err := c.store.Networks()
	require.NoError(t, err)

	_, asked, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, ConfirmUplink: true,
		Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve3": leg248}}, probeAnswer)

	require.NoError(t, err)
	require.Contains(t, asked, "pve3")
	after, err := c.store.Networks()
	require.NoError(t, err)
	n := after["vmbr2.20"]
	require.Equal(t, map[string]netip.Addr{testNode: leg250, "pve2": leg249, "pve3": leg248}, n.Legs)
	require.Equal(t, before["vmbr2.20"].Mark, n.Mark)
	require.Equal(t, store.NodeLegMAC(2, n.Mark), n.LegMACs["pve3"])

	t.Run("not a leg a node has", func(t *testing.T) {
		_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, ConfirmUplink: true,
			Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": netip.MustParseAddr("10.20.0.240")}}, probeAnswer)
		require.ErrorContains(t, err, "network vmbr2.20 is attached already with a leg on node pve2")
	})
	t.Run("not in another prefix", func(t *testing.T) {
		one := n
		one.Legs, one.LegMACs = map[string]netip.Addr{testNode: leg250}, map[string]string{testNode: n.LegMACs[testNode]}
		require.NoError(t, c.store.SaveNetwork(one, n.Rev))
		_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: netip.MustParsePrefix("10.20.0.128/25"), ConfirmUplink: true,
			Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve3": leg248}}, probeAnswer)
		require.ErrorContains(t, err, "network vmbr2.20 is attached already in 10.20.0.0/24")
	})
}

// The view of a network has the leg of every node, each as the gateway of
// its node last found it, and the service addresses of each node.
func TestTheViewOfANetworkHasTheLegOfEveryNode(t *testing.T) {
	c := newNetCluster(t)
	_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true,
		Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": leg249, "pve3": leg248}}, probeAnswer)
	require.NoError(t, err)
	c.acts["pve2"].edit = func(st *agentproto.Status) {
		st.Legs = []agentproto.LegReport{{ID: "vmbr2.20", Up: true}, {ID: "vmbr9", Up: true}}
	}
	c.acts["pve3"].edit = func(st *agentproto.Status) {
		st.Legs = []agentproto.LegReport{{ID: "vmbr2.20", Problem: "10.20.0.248 is answered by bc:24:11:00:00:09"}}
	}
	c.settle()
	_, err = c.store.AllocateNodeServiceAddr("vmbr2.20", "pve3", netip.MustParseAddr("10.20.0.5"), store.ServiceOwner{Owner: "lxc/107"}, t0)
	require.NoError(t, err)

	d, err := c.eng.Network("vmbr2.20")

	require.NoError(t, err)
	var legs []string
	for _, l := range d.Legs {
		legs = append(legs, l.Node+" "+l.Addr.String()+" "+l.MAC+" "+map[bool]string{true: "up", false: "down"}[l.Up]+" "+l.Problem)
	}
	n := d.Network
	require.Equal(t, []string{
		testNode + " 10.20.0.250 " + store.NodeLegMAC(0, n.Mark) + " up ",
		"pve2 10.20.0.249 " + store.NodeLegMAC(1, n.Mark) + " up ",
		"pve3 10.20.0.248 " + store.NodeLegMAC(2, n.Mark) + " down 10.20.0.248 is answered by bc:24:11:00:00:09",
	}, legs)
	require.Len(t, d.NodeServices, 1)
	require.Equal(t, "pve3", d.NodeServices[0].Node)
}

// The managed network gets its leg on every node whose agent finds pconet
// there and its first address free; a node without it gets none, and a
// later enable gives it one.
func TestTheManagedNetworkGetsALegOnEveryNodeWithAnAgent(t *testing.T) {
	c := newNetCluster(t)
	facts := &managedFacts{vnet: ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true}}
	c.managed = facts.deps
	c.eng = c.newEngine()
	c.watchHere(t)
	c.settle()
	withoutPconet := func(node string, p agentproto.Probe) agentproto.ActionResult {
		if node == "pve3" {
			return answered(agentproto.ProbeAnswer{Interfaces: agentIfaces(node)[:4], AnsweredBy: []string{}})
		}
		return probeAnswer(node, p)
	}
	var err error

	asked := c.probes(t, withoutPconet, func() { err = c.eng.EnableManaged(t.Context()) })

	require.NoError(t, err)
	first := netip.MustParseAddr("10.203.118.1")
	require.Equal(t, map[string]agentproto.Probe{"pve2": {Bridge: "pconet", Addr: first}, "pve3": {Bridge: "pconet", Addr: first}}, asked)
	stored, err := c.store.Networks()
	require.NoError(t, err)
	require.Equal(t, map[string]netip.Addr{testNode: first, "pve2": first}, stored[gateway.ManagedBridge].Legs)
	require.Contains(t, c.eventMessages(), "the managed network has no leg on node pve3: pconet is no bridge or vnet of node pve3")

	c.probes(t, probeAnswer, func() { err = c.eng.EnableManaged(t.Context()) })

	require.NoError(t, err)
	stored, err = c.store.Networks()
	require.NoError(t, err)
	require.Equal(t, map[string]netip.Addr{testNode: first, "pve2": first, "pve3": first}, stored[gateway.ManagedBridge].Legs)
	require.Contains(t, c.eventMessages(), "the managed network has a leg on node pve3 now: 10.203.118.1")
}

// An answer under another epoch than the one the probe was handed out
// under is not taken: the node gets no leg, and nothing is saved.
func TestAnAnswerOfAnotherEpochIsNotTaken(t *testing.T) {
	c := newNetCluster(t)
	c.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d == probeWait {
			d = 300 * time.Millisecond
		}
		return context.WithTimeout(ctx, d)
	}
	var err error
	c.probes(t, nil, func() {
		go func() {
			for range 50 {
				time.Sleep(5 * time.Millisecond)
				s, ok := c.r.lastOf("pve2")
				if !ok || len(s.Actions) == 0 {
					continue
				}
				res := probeAnswer("pve2", agentproto.Probe{})
				res.ID = s.Actions[0].ID
				other := s.Epoch
				other.Generation++
				c.eng.TakeAgentEvents("pve2", agentproto.Status{Node: "pve2", EpochSeen: other, Actions: []agentproto.ActionResult{res}}, s.Epoch)
				return
			}
		}()
		_, err = c.eng.Attach(t.Context(), AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
			Address: netip.MustParseAddr("10.77.0.250"), Scope: store.ScopeNode, Node: "pve2"})
	})

	require.ErrorContains(t, err, "the agent of node pve2 did not answer the probe within")
	stored, serr := c.store.Networks()
	require.NoError(t, serr)
	require.Empty(t, stored)
}

// A node with an agent and no leg of the managed network says so every
// cycle, until an enable gives it one.
func TestANodeWithoutAManagedLegIsAProblem(t *testing.T) {
	c := newNetCluster(t)
	facts := &managedFacts{
		registered: []string{"cf-tunnel", managednet.Tag},
		vnet:       ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true},
		cluster:    pve.ClusterFirewallOptions{Enabled: true},
		service:    "running",
		guestFW:    map[string]bool{},
		guestFWErr: map[string]error{},
	}
	c.managed = facts.deps
	host := managednet.NewHostConfigurer(&hostRunner{answers: map[string]string{}}, testNode)
	c.guests = host
	saveManagedNetwork(t, c.store, 1)
	c.eng = c.newEngine()
	c.watchHere(t)

	st := c.settle()

	require.Contains(t, st.Problems, "the managed network has no leg on node pve2; pco network managed enable gives it one once its agent finds pconet there")
	require.Contains(t, st.Problems, "the managed network has no leg on node pve3; pco network managed enable gives it one once its agent finds pconet there")
}

// A node whose agent was not asked, as one whose report became fresh while
// the others were probed, gets no leg of the managed network: never one
// without the checks of its agent's answer.
func TestANodeNotAskedGetsNoManagedLeg(t *testing.T) {
	c := newNetCluster(t)
	f, err := c.eng.networkFacts(t.Context())
	require.NoError(t, err)
	ln, err := c.eng.legNodes()
	require.NoError(t, err)
	first := netip.MustParseAddr("10.203.118.1")
	n := store.Network{ID: gateway.ManagedBridge, Kind: store.NetworkManaged, Bridge: gateway.ManagedBridge,
		Prefix: netip.MustParsePrefix(managedSubnet), Legs: map[string]netip.Addr{testNode: first}}
	answers := map[string]nodeAnswer{"pve3": {answer: agentproto.ProbeAnswer{Interfaces: agentIfaces("pve3"), AnsweredBy: []string{}}}}

	said := c.eng.managedLegs(f, ln, configsOf(t, c, "pve2", "pve3"), &n, answers)

	require.Equal(t, map[string]netip.Addr{testNode: first, "pve3": first}, n.Legs)
	require.Contains(t, said, "the managed network has no leg on node pve2: its agent was not asked")
}

func configsOf(t *testing.T, c *netCluster, nodes ...string) map[string][]pve.NodeIface {
	t.Helper()
	out := map[string][]pve.NodeIface{}
	for _, n := range nodes {
		cfg, err := c.nets.NodeNetwork(t.Context(), n)
		require.NoError(t, err)
		out[n] = cfg
	}
	return out
}

// The answers of the agents are at most a few seconds old when they are
// judged: an attach that cannot take the cycle lock soon after the probes
// is refused, and asks again when tried again.
func TestAnAttachRefusesAnswersThatWaitedTooLong(t *testing.T) {
	c := newNetCluster(t)
	c.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d == freshWait {
			d = 50 * time.Millisecond
		}
		return context.WithTimeout(ctx, d)
	}
	hog := func(node string, p agentproto.Probe) agentproto.ActionResult {
		c.eng.sem <- struct{}{}
		go func() {
			time.Sleep(500 * time.Millisecond)
			<-c.eng.sem
		}()
		return probeAnswer(node, p)
	}

	_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
		Address: netip.MustParseAddr("10.77.0.250"), Scope: store.ScopeNode, Node: "pve2"}, hog)

	require.ErrorIs(t, err, ErrBusy)
	require.ErrorContains(t, err, "the answers of the agents would be older than")
	stored, serr := c.store.Networks()
	require.NoError(t, serr)
	require.Empty(t, stored)
}

// A node that is not the controller asks no agent.
func TestAnAttachOffTheControllerAsksNoAgent(t *testing.T) {
	c := newNetCluster(t)
	c.lease.mu.Lock()
	c.lease.st.Holder = false
	c.lease.mu.Unlock()

	_, asked, err := c.attach(t, AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
		Address: netip.MustParseAddr("10.77.0.250"), Scope: store.ScopeNode, Node: "pve2"}, probeAnswer)

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "not the controller")
	require.Empty(t, asked)
}

// The network of the other nodes and the SDN zones are read through the API
// before the cycle lock is taken, not under it.
func TestTheOtherNodesAreReadOutsideTheCycleLock(t *testing.T) {
	c := newNetCluster(t)
	c.nets.mu.Lock()
	c.nets.locked = func() bool { return len(c.eng.sem) == 1 }
	c.nets.mu.Unlock()

	_, _, err := c.attach(t, AttachRequest{Bridge: "vmbr2", VLAN: 20, Prefix: vlan20, Address: leg250, ConfirmUplink: true,
		Scope: store.ScopeCluster, Legs: map[string]netip.Addr{"pve2": leg249, "pve3": leg248}}, probeAnswer)
	require.NoError(t, err)
	var probeErr error
	c.probes(t, probeAnswer, func() {
		_, probeErr = c.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr1", Scope: store.ScopeCluster})
	})
	require.NoError(t, probeErr)

	c.nets.mu.Lock()
	defer c.nets.mu.Unlock()
	require.Empty(t, c.nets.lockedReads)
}

// A bridge the agent would refuse is refused before any agent is asked.
func TestAProbeNamesABridgeAsTheAgentTakesIt(t *testing.T) {
	c := newNetCluster(t)

	_, err := c.eng.ProbeNetwork(t.Context(), ProbeRequest{Bridge: "vmbr0123456789ab", Scope: store.ScopeCluster})

	var fe *FieldError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, "bridge", fe.Field)
	require.ErrorContains(t, err, "want up to 15 letters")
}

// The managed network is saved with the subnet its legs were probed in:
// a subnet changed meanwhile refuses the enable.
func TestEnableManagedRefusesASubnetChangedWhileItProbed(t *testing.T) {
	c := newNetCluster(t)
	facts := &managedFacts{vnet: ManagedVnet{Exists: true, ZoneType: "simple", IsolatePorts: true}}
	c.managed = facts.deps
	c.eng = c.newEngine()
	c.watchHere(t)
	c.settle()
	changed := false
	change := func(node string, p agentproto.Probe) agentproto.ActionResult {
		if !changed {
			changed = true
			st, err := c.store.Settings()
			require.NoError(t, err)
			st.Managed.Subnet = netip.MustParsePrefix("10.204.0.0/24")
			require.NoError(t, c.store.SaveSettings(st))
		}
		return probeAnswer(node, p)
	}
	var err error

	c.probes(t, change, func() { err = c.eng.EnableManaged(t.Context()) })

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "managed.subnet changed while the nodes were probed")
	stored, serr := c.store.Networks()
	require.NoError(t, serr)
	require.NotContains(t, stored, gateway.ManagedBridge)
}

// An answer whose epoch is the one the probe was listed under, taken after
// the controller moved on to another epoch, is not taken: the controller
// that asked is not the one that would act on it.
func TestAnAnswerAfterTheEpochChangedIsNotTaken(t *testing.T) {
	c := newNetCluster(t)
	c.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d == probeWait {
			d = 300 * time.Millisecond
		}
		return context.WithTimeout(ctx, d)
	}
	var err error
	c.probes(t, nil, func() {
		go func() {
			for range 50 {
				time.Sleep(5 * time.Millisecond)
				s, ok := c.r.lastOf("pve2")
				if !ok || len(s.Actions) == 0 {
					continue
				}
				res := probeAnswer("pve2", agentproto.Probe{})
				res.ID = s.Actions[0].ID
				now := s.Epoch
				now.Generation++
				c.eng.TakeAgentEvents("pve2", agentproto.Status{Node: "pve2", EpochSeen: s.Epoch, Actions: []agentproto.ActionResult{res}}, now)
				return
			}
		}()
		_, err = c.eng.Attach(t.Context(), AttachRequest{Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
			Address: netip.MustParseAddr("10.77.0.250"), Scope: store.ScopeNode, Node: "pve2"})
	})

	require.ErrorContains(t, err, "the agent of node pve2 did not answer the probe within")

	t.Run("nor handed out again under the new epoch", func(t *testing.T) {
		d := &doctorAsks{acts: map[string]*doctorAsk{"probe-1": {node: "pve2", action: agentproto.Action{ID: "probe-1", Kind: agentproto.ActionProbe}}}}
		a, b := agentproto.Epoch{InstallID: "i", Generation: 1, Nonce: "a"}, agentproto.Epoch{InstallID: "i", Generation: 2, Nonce: "b"}
		require.Len(t, d.actionsFor("pve2", a), 1)
		require.Empty(t, d.actionsFor("pve2", b))
	})
}
