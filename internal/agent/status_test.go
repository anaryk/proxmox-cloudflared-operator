package agent

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

func TestTheStatusSaysWhatTheAgentIsAt(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets = append(s.Targets, agentproto.Target{Key: "remote.example.com", Tunnels: []string{sharedID}, Port: 443, Required: "port", Addr: netip.MustParseAddr("10.0.0.40")})
	e.apply(s)
	e.clock.advance(3 * time.Second)

	st := e.agent.Status(t.Context())

	require.Equal(t, agentproto.Range{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax}, st.Protocol)
	require.Equal(t, version.Version, st.Build)
	require.Equal(t, testNode, st.Node)
	require.Equal(t, pki.KindHost, st.Kind)
	require.Equal(t, t0, st.AgentStartedAt)
	require.Equal(t, epoch1, st.EpochSeen)
	require.EqualValues(t, 1, st.SeqApplied)
	require.Equal(t, t0.Add(3*time.Second), st.Clock)
	require.Equal(t, "on", st.Egress)
	require.Len(t, st.Targets, 2)
	remote := report(st, "remote.example.com")
	require.False(t, remote.OK)
	require.Equal(t, "this node does not resolve this target, and it verifies no address another node chose", remote.Reason)
	require.Equal(t, netip.MustParseAddr("10.0.0.40"), remote.Addr)
}

func TestTheViewSaysHowTheAgentFares(t *testing.T) {
	e := newEnv(t)
	e.apply(e.snapshot("www.example.com -> :8080"))
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, ReadyReplicas: map[string]int{sharedID: 2}})
	e.clock.advance(40 * time.Second)

	v := e.agent.View()

	require.Equal(t, agentproto.AgentView{
		Node: testNode, Kind: pki.KindHost, SilentFor: 40 * time.Second, Epoch: epoch1, Seq: 1, SeqAge: 40 * time.Second,
		Connectors: 1, EgressTargets: 1, ReadyReplicas: map[string]int{sharedID: 2}, HeartbeatAge: 40 * time.Second, Problems: []string{},
	}, v)

	e.clock.advance(30 * time.Second)
	require.Contains(t, e.agent.View().Problems, "no controller answered for 1m10s; this node serves what it was last told")
}

// A heartbeat of an epoch older than the one the agent follows is a stale
// controller's: it counts for nothing.
func TestAHeartbeatOfALowerEpochIsIgnored(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	s.Epoch.Generation = 3
	e.apply(s)
	e.clock.advance(time.Minute)

	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, ReadyReplicas: map[string]int{sharedID: 9}})

	v := e.agent.View()
	require.Equal(t, time.Minute, v.SilentFor)
	require.Empty(t, v.ReadyReplicas)
}

func TestAResultSurvivesItsWireForm(t *testing.T) {
	failing := t0.Add(-time.Minute)
	for _, res := range []resolve.Result{
		{
			Target: planner.ResolvedTarget{Addr: guestAddr, Reachable: true, Owner: "qemu/101", Leg: true, Via: netip.MustParseAddr("10.203.118.5")},
			Binding: &resolve.Binding{
				Owner: "qemu/101", Hostname: "www.example.com", Guest: "qemu/101", Addr: guestAddr, MAC: testMAC,
				VerifiedAt: t0, FailingSince: &failing, Level: resolve.LevelPort, Since: t0.Add(-time.Hour),
				Bridge: "vmbr0", Port: "tap101i0", Ports: map[string]string{testMAC: "tap101i0"},
				Segment: resolve.Segment{Bridge: "vmbr0", VLAN: 20}, Via: "lan", Service: netip.MustParseAddr("10.203.118.5"),
			},
			Candidates: []resolve.CandidateResult{{Addr: guestAddr, Source: resolve.FromStatic, OK: true, Level: "port"}},
			Level:      resolve.LevelPort,
			SoftDenied: "gateway of node pve1",
		},
		{Target: planner.ResolvedTarget{Addr: nodeAddr, Reachable: true}, Level: resolve.LevelManual},
		{Target: planner.ResolvedTarget{Rejected: true, Reason: "address of a cluster node"}},
	} {
		raw, err := EncodeResult(res)
		require.NoError(t, err)
		got, err := DecodeResult(raw)
		require.NoError(t, err)
		require.Equal(t, res, got)
	}
	_, err := DecodeResult(json.RawMessage(`{"level":"guessed"}`))
	require.ErrorContains(t, err, "unknown identity level")
}

func TestLocalAppliesWhatTheWireCarries(t *testing.T) {
	e := newEnv(t)
	l := Local{A: e.agent}
	s := e.snapshot("www.example.com -> :8080")

	st, err := l.Publish(t.Context(), s)

	require.NoError(t, err)
	require.EqualValues(t, 1, st.SeqApplied)
	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken}, e.conn.changes(), "the token crossed")
	s.Targets[0].Route.Hostname = "changed.example.com"
	require.Equal(t, "www.example.com", e.agent.desired.Targets[0].Route.Hostname, "the agent holds its own copy")

	stale := s
	stale.Epoch.Generation = 0
	_, err = l.Publish(t.Context(), stale)
	require.ErrorIs(t, err, agentproto.ErrBadEpoch, "the same checks as over the network")

	l.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, ReadyReplicas: map[string]int{sharedID: 1}})
	require.Equal(t, map[string]int{sharedID: 1}, e.agent.View().ReadyReplicas)
	require.EqualValues(t, 1, l.Report(t.Context()).SeqApplied)
}

func TestTheTrafficCountsAreTheFiltersOwn(t *testing.T) {
	e := newEnv(t)

	gen, counts, err := e.agent.FlowCounts(t.Context())

	require.NoError(t, err)
	require.Equal(t, "g1", gen)
	require.Equal(t, map[netip.AddrPort]uint64{netip.MustParseAddrPort("10.0.0.11:8080"): 7}, counts)
}

func TestNewNeedsWhatTheAgentWorksWith(t *testing.T) {
	e := newEnv(t)
	for name, change := range map[string]func(*Deps){
		"node":       func(d *Deps) { d.Node = "" },
		"kind":       func(d *Deps) { d.Kind = "toaster" },
		"install":    func(d *Deps) { d.InstallID = "" },
		"store":      func(d *Deps) { d.Store = nil },
		"resolver":   func(d *Deps) { d.Resolver = nil },
		"connectors": func(d *Deps) { d.Connectors = nil },
		"egress":     func(d *Deps) { d.Egress = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := e.deps()
			change(&d)
			_, err := New(d)
			require.Error(t, err)
		})
	}
}
