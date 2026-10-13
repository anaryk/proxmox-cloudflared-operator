package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// inspected is a cycle of e that read everything and resolved, as far as the
// snapshot of the node is made of it; the pass of the agent is ended.
func (e *env) inspected() *cycleRun {
	e.t.Helper()
	e.eng.refreshAccess(e.t.Context())
	c := e.eng.newCycle(e.t.Context())
	require.NoError(e.t, e.eng.acquire(e.t.Context()))
	defer e.eng.release()
	defer c.endSteps()
	require.True(e.t, c.prepare())
	require.True(e.t, c.inspect(), "%v", c.st.Problems)
	return c
}

func TestTheSnapshotOfACycleCarriesEveryWinnerAsATargetItResolves(t *testing.T) {
	e := newEnv(t)
	web := guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")
	web.NICs = []model.NIC{{Index: 0, MAC: testMAC, Bridge: "vmbr0"}}
	clone := guest(102, "clone", "")
	clone.NICs = []model.NIC{{Index: 0, MAC: "BC:24:11:00:00:01", Bridge: "vmbr0"}}
	stopped := guest(103, "stopped", "")
	stopped.NICs, stopped.Running = clone.NICs, false
	other := guest(104, "other", "")
	other.NICs = []model.NIC{{Index: 0, MAC: "bc:24:11:00:00:99", Bridge: "vmbr0"}}
	e.inv.set(snapshot(web, clone, stopped, other))
	require.NoError(t, e.store.SaveManualRoute(model.Route{
		Hostname: "pve.example.com", ManualID: "pve", Source: model.SourceManual,
		Target:  model.Target{Scheme: model.SchemeHTTPS, Addr: nodeAddr, Port: 8006},
		Options: model.RouteOptions{AllowNode: true, NoTLSVerify: true},
	}))
	e.settings(func(s *store.Settings) { s.IdentityMinimum = string(resolve.LevelObserved) })

	s := e.inspected().snapshot()

	require.Equal(t, agentproto.Epoch{InstallID: testInstall, Generation: testWriter.Generation, Nonce: testWriter.Nonce}, s.Epoch)
	require.Equal(t, testNode, s.Node)
	require.Equal(t, "observed", s.Settings.IdentityMinimum)
	require.Equal(t, 10*time.Second, s.Settings.PollInterval)
	keys := map[string]agentproto.Target{}
	for _, tg := range s.Targets {
		require.True(t, tg.Resolve, tg.Key)
		require.Equal(t, "observed", tg.Required, tg.Key)
		keys[tg.Key] = tg
	}
	require.Len(t, keys, 3)
	require.Equal(t, uint16(8080), keys["www.example.com"].Port)
	require.Equal(t, "web-1", keys["www.example.com"].Guest.Name)
	require.Equal(t, testNode, keys["www.example.com"].HostingNode)
	require.True(t, keys["pve.example.com"].AllowNode)
	require.Nil(t, keys["pve.example.com"].Guest)
	require.Len(t, s.Guests, 1, "the running guest with a MAC of a target's guest, and no other")
	require.Equal(t, "clone", s.Guests[0].Name)
	require.Equal(t, []netip.Addr{nodeAddr}, s.Deny.Denied)
	require.Len(t, s.Nodes, 1)
	require.True(t, s.Nodes[0].Local)
}

func TestTheSnapshotDeniesTheSDNGatewaysAndSoftDeniesTheNodesGateways(t *testing.T) {
	e := newEnv(t)
	e.acc.subnets = []pve.Subnet{{Vnet: "labx", Prefix: netip.MustParsePrefix("10.9.0.0/24"), Gateway: netip.MustParseAddr("10.9.0.1")}}
	snap := snapshot(guest(101, "web-1", "www.example.com -> :8080"))
	snap.Nodes[0].Ifaces = []pve.NodeIface{{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/24")}, Gateway: netip.MustParseAddr("10.0.0.1")}}
	e.inv.set(snap)

	s := e.inspected().snapshot()

	require.Equal(t, map[string]string{"10.9.0.1": "gateway of SDN subnet 10.9.0.0/24 of vnet labx"}, s.Deny.Reasons)
	require.Equal(t, "gateway of node "+testNode, s.Deny.Soft["10.0.0.1"])
	require.Equal(t, []netip.Addr{nodeAddr}, s.Nodes[0].Addrs)
}

// The tunnels of the snapshot carry what the configuration last verified at
// Cloudflare sends a connector to, which the egress set keeps until a
// configuration without it is verified.
func TestTheSnapshotCarriesTheVerifiedConfigurations(t *testing.T) {
	e := published(t)

	s := e.inspected().snapshot()

	require.Len(t, s.Tunnels, 1)
	tun := s.Tunnels[0]
	require.Equal(t, testAccount, tun.AccountID)
	require.Equal(t, e.tunnels()[0].ID, tun.TunnelID)
	require.Equal(t, []netip.AddrPort{netip.MustParseAddrPort("10.0.0.11:8080")}, tun.Verified)
	require.False(t, tun.Run, "kept as it is: the cycle decides on the connectors after its tunnel run")
}

// Without a writer identity the cycle still resolves and feeds the egress
// set; the snapshot has no epoch an agent would follow.
func TestTheSnapshotWithoutAWriterHasNoEpoch(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.store.SaveWriter(testWriter))
	c := e.inspected()
	c.writerRead = false

	s := c.snapshot()

	require.Equal(t, agentproto.Epoch{InstallID: testInstall}, s.Epoch)
	require.Error(t, s.Validate(testNode, agentproto.Epoch{InstallID: testInstall}, 0))
}

// The cycle's results are the resolver's for the table of cases the cycle
// tests run, through the agent: one resolution each, with the minimum.
func TestTheCycleResolvesEachWinnerOnceThroughTheAgent(t *testing.T) {
	e := webAndAPI(t)
	e.res.setLevel("api.example.com", resolve.LevelObserved)

	st := e.cycle()

	require.ElementsMatch(t, []string{"www.example.com", "api.example.com"}, e.res.hosts)
	require.Equal(t, "active", string(route(st, "www.example.com").State))
	require.Equal(t, "unreachable", string(route(st, "api.example.com").State))
	report := e.eng.Agent().Status(t.Context())
	require.Len(t, report.Targets, 2)
	for _, r := range report.Targets {
		require.Equal(t, r.Key == "www.example.com", r.InEgressSet, r.Key)
	}
}
