package agent

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

const (
	otherNode = "pve2"
	remoteMAC = "bc:24:11:00:00:22"
)

var remoteAddr = netip.MustParseAddr("10.0.0.30")

// remoteTarget is the target of the route "www.example.com -> :8080" of guest
// qemu/202 on another node, whose address that node chose and this node
// verifies, with the hosting node's attestation and coverage.
func remoteTarget() agentproto.Target {
	t := guestTarget("www.example.com -> :8080")
	ref := model.GuestRef{Kind: model.KindQEMU, VMID: 202}
	t.Route.Guest, t.Guest.Ref, t.Guest.Node, t.Guest.Name = &ref, ref, otherNode, "web-2"
	t.Guest.NICs = []model.NIC{{Index: 0, MAC: remoteMAC, Bridge: "vmbr0"}}
	t.Resolve, t.Addr, t.HostingNode, t.MACs, t.Covered = false, remoteAddr, otherNode, []string{remoteMAC}, true
	t.Attestation = &agentproto.Attestation{
		Node: otherNode, Guest: ref, NIC: 0, MAC: remoteMAC, Bridge: "vmbr0", Port: "tap202i0", Since: t0.Add(-time.Hour),
	}
	return t
}

// heard hands the agent a heartbeat that says the other node reported age
// ago, with its watch running.
func (e *env) heard(age time.Duration) {
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, Nodes: map[string]agentproto.NodeFreshness{
		testNode:  {StatusAge: time.Second, WatchRunning: true},
		otherNode: {StatusAge: age, WatchRunning: true},
	}})
}

func TestATargetAnotherNodeChoseIsVerifiedWithWhatTheSnapshotAndTheHeartbeatSay(t *testing.T) {
	e := newEnv(t)
	e.heard(4 * time.Second)
	e.clock.advance(2 * time.Second)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()

	st := e.apply(s)

	reqs := e.res.verifications()
	require.Len(t, reqs, 1)
	req := reqs[0]
	require.Equal(t, "www.example.com", req.Route.Hostname)
	require.Equal(t, *s.Targets[0].Guest, req.Guest)
	require.Equal(t, remoteAddr, req.Addr)
	require.EqualValues(t, 8080, req.Port)
	require.Equal(t, []string{remoteMAC}, req.MACs)
	require.Equal(t, resolve.LevelPort, req.Required)
	require.True(t, req.Covered)
	require.Nil(t, req.Prev)
	require.Equal(t, &resolve.Attestation{
		Node: otherNode, MAC: remoteMAC, Bridge: "vmbr0", Port: "tap202i0",
		StatusAge: 4 * time.Second, HeardAt: t0, StableSince: t0.Add(-time.Hour),
	}, req.Attestation)
	require.Equal(t, inventoryOf(s), req.Snap, "the inventory of the snapshot")
	require.Empty(t, e.res.calls(), "this node resolves nothing of it")

	r := report(st, "www.example.com")
	require.Equal(t, remoteAddr, r.Addr)
	require.Equal(t, "port", r.Level)
	require.True(t, r.Attested)
	require.Equal(t, otherNode, r.AttestedBy, "the node whose attestation it stands on")
	require.True(t, r.OK)
	require.True(t, r.InEgressSet)
	set, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.30:8080"), set)

	t.Run("the next pass verifies with the binding it kept", func(t *testing.T) {
		e.apply(s)
		reqs := e.res.verifications()
		require.Len(t, reqs, 2)
		require.NotNil(t, reqs[1].Prev)
		require.Equal(t, remoteAddr, reqs[1].Prev.Addr)
		require.Equal(t, "www.example.com", reqs[1].Prev.Hostname)
	})
}

func TestEachVerificationIsBoundedAsAResolutionIs(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	start := time.Now()

	e.apply(s)

	require.Len(t, e.res.deadlines, 1)
	require.WithinDuration(t, start.Add(resolveTimeout), e.res.deadlines[0], 5*time.Second)
}

// What this node cannot tell of the attesting node's status is not made up:
// without a heartbeat that names it, or with the attestation missing, the
// resolver gets no times to go by, and times that are not of the last
// heartbeat are said to be recalled; the address stands at observed.
func TestWhatTheAttestationLacksIsNotMadeUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env, s *agentproto.Snapshot)
		want  func(t *testing.T, req resolve.VerifyRequest)
	}{
		{"no heartbeat yet", func(*env, *agentproto.Snapshot) {}, func(t *testing.T, req resolve.VerifyRequest) {
			require.True(t, req.Attestation.HeardAt.IsZero())
			require.Zero(t, req.Attestation.StatusAge)
		}},
		{"a heartbeat that does not name the node", func(e *env, _ *agentproto.Snapshot) {
			e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, Nodes: map[string]agentproto.NodeFreshness{testNode: {}}})
		}, func(t *testing.T, req resolve.VerifyRequest) {
			require.True(t, req.Attestation.HeardAt.IsZero())
		}},
		{"a heartbeat of a lower epoch, which is only recalled", func(e *env, s *agentproto.Snapshot) {
			s.Epoch.Generation = 2
			e.heard(time.Second)
		}, func(t *testing.T, req resolve.VerifyRequest) {
			require.True(t, req.Attestation.Recalled)
			require.Equal(t, t0, req.Attestation.HeardAt)
		}},
		{"no coverage", func(e *env, s *agentproto.Snapshot) {
			e.heard(time.Second)
			s.Targets[0].Covered = false
		}, func(t *testing.T, req resolve.VerifyRequest) {
			require.False(t, req.Covered)
			require.NotNil(t, req.Attestation)
		}},
		{"no attestation", func(e *env, s *agentproto.Snapshot) {
			e.heard(time.Second)
			s.Targets[0].Attestation = nil
		}, func(t *testing.T, req resolve.VerifyRequest) {
			require.Nil(t, req.Attestation)
			require.True(t, req.Covered)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			s := e.snapshot("www.example.com -> :8080")
			s.Targets[0] = remoteTarget()
			tc.setup(e, &s)

			st := e.apply(s)

			reqs := e.res.verifications()
			require.Len(t, reqs, 1)
			tc.want(t, reqs[0])
			r := report(st, "www.example.com")
			require.Equal(t, "observed", r.Level)
			require.False(t, r.Attested)
			require.Empty(t, r.AttestedBy)
			require.False(t, r.OK)
			require.False(t, r.InEgressSet)
			require.Equal(t, "identity level observed is below the required port", r.Reason)
			set, _ := e.egr.last()
			require.Empty(t, set, "an address proven below the minimum is not reached")
		})
	}
}

func TestAnApplianceAgentPassesNoAttestationAndNoCoverage(t *testing.T) {
	e := newEnv(t)
	e.kind = pki.KindAppliance
	e.agent = e.newAgent()
	e.heard(time.Second)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	s.Targets[0].Required = "observed"

	st := e.apply(s)

	reqs := e.res.verifications()
	require.Len(t, reqs, 1)
	require.Nil(t, reqs[0].Attestation)
	require.False(t, reqs[0].Covered)
	r := report(st, "www.example.com")
	require.Equal(t, "observed", r.Level)
	require.False(t, r.Attested)
	require.Empty(t, r.AttestedBy)
	require.True(t, r.OK, "observed meets the required level")
}

func TestATargetWithoutAChosenAddressIsNotVerified(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	s.Targets[0].Addr = netip.Addr{}

	st := e.apply(s)

	require.Empty(t, e.res.verifications())
	r := report(st, "www.example.com")
	require.Equal(t, reasonNotResolved, r.Reason)
	require.False(t, r.OK)
}

func TestAManualRouteIsVerifiedByEveryReplica(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	m := manualTarget("manual.example.com", netip.MustParseAddrPort("10.0.0.50:443"), false)
	m.Resolve, m.Addr = false, netip.MustParseAddr("10.0.0.50")
	s.Targets = []agentproto.Target{m}

	st := e.apply(s)

	reqs := e.res.verifications()
	require.Len(t, reqs, 1)
	require.Nil(t, reqs[0].Route.Guest)
	require.Equal(t, netip.MustParseAddr("10.0.0.50"), reqs[0].Addr)
	r := report(st, "manual.example.com")
	require.True(t, r.OK)
	require.Equal(t, "manual", r.Level)
	require.True(t, r.InEgressSet)
}

func TestAVerificationThatFailsKeepsTheAddressOut(t *testing.T) {
	e := newEnv(t)
	e.heard(time.Second)
	e.res.reject("www.example.com")
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()

	st := e.apply(s)

	r := report(st, "www.example.com")
	require.False(t, r.OK)
	require.False(t, r.InEgressSet)
	require.Equal(t, "address of a cluster node", r.Reason)
	set, _ := e.egr.last()
	require.Empty(t, set)
}

// The MAC of an address this node verifies may move on this node's bridge
// too: a guest of this node answering in the other's name. The address is
// verified again as the other node's choice, never resolved here.
func TestAVerifiedAddressWhoseMACMovesIsVerifiedAgain(t *testing.T) {
	e := newEnv(t)
	e.heard(time.Second)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	e.apply(verified(s, "10.0.0.30:8080"))
	log := &callLog{}
	e.egr.log = log

	e.agent.Moved(t.Context(), remoteAddr)
	e.agent.Wait()

	require.Equal(t, []string{"egress remove 10.0.0.30", "egress set 10.0.0.30:8080"}, log.all())
	require.Len(t, e.res.verifications(), 2)
	require.Empty(t, e.res.calls())

	t.Run("and stays out when it fails", func(t *testing.T) {
		e.res.observe("www.example.com")
		e.agent.Moved(t.Context(), remoteAddr)
		e.agent.Wait()

		require.Empty(t, e.agent.EgressSet())
		require.Contains(t, e.events.messages()[len(e.events.messages())-1], "did not pass its verification after its MAC moved")
	})
}

// What a pass found of an address another node chose is proven again by
// every pass with the heartbeat of the moment: once the heartbeat that
// carried the attestation is too old, the address stands at observed and
// leaves the set, whatever the pass before found.
func TestAVerifiedAddressIsOnlyAsFreshAsTheLastHeartbeat(t *testing.T) {
	e := newEnv(t)
	e.heard(time.Second)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	st := e.apply(s)
	require.True(t, report(st, "www.example.com").InEgressSet)

	e.clock.advance(20 * time.Second)
	st, err := e.agent.Reverify(t.Context())

	require.NoError(t, err)
	r := report(st, "www.example.com")
	require.Equal(t, "observed", r.Level)
	require.False(t, r.Attested)
	require.Empty(t, r.AttestedBy)
	require.False(t, r.InEgressSet)
	set, _ := e.egr.last()
	require.Empty(t, set)

	t.Run("and stands again with a new one", func(t *testing.T) {
		e.heard(time.Second)
		st, err := e.agent.Reverify(t.Context())
		require.NoError(t, err)
		require.True(t, report(st, "www.example.com").InEgressSet)
	})
}

func TestNothingOfAVerifiedTargetOutlivesItsPlaceInTheSnapshot(t *testing.T) {
	e := newEnv(t)
	e.heard(time.Second)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	e.apply(s)

	st := e.apply(e.snapshot())

	require.Empty(t, st.Targets)
	require.Empty(t, e.agent.EgressSet())
	require.Empty(t, e.agent.Bound())
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Empty(t, bindings)
}

// The status says whether an address is in the set as the set is now, not
// as the pass left it: an address taken out because its MAC moved is not
// reported in the set while its verification waits.
func TestInEgressSetFollowsTheSetAsItIsNow(t *testing.T) {
	e := newEnv(t)
	e.heard(time.Second)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	e.apply(verified(s, "10.0.0.30:8080"))
	require.NoError(t, e.agent.acquire(t.Context())) // the verification waits for the lock

	e.agent.Moved(t.Context(), remoteAddr)

	require.False(t, report(e.agent.Status(t.Context()), "www.example.com").InEgressSet)
	e.agent.release()
	e.agent.Wait()
	require.True(t, report(e.agent.Status(t.Context()), "www.example.com").InEgressSet, "back once verified")
}
