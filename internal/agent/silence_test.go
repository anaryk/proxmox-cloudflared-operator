package agent

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// wireProber is the host as the resolver sees it from this node: vmbr0 with
// an address of the guests' network, the guest of the other node answering
// ARP for remoteAddr, and its MAC learned on the uplink eno1. A test changes
// what the wire shows.
type wireProber struct {
	mu  sync.Mutex
	arp []string // the MACs that answer for remoteAddr
	fdb []string // the ports of vmbr0 remoteMAC is learned on
}

func newWireProber() *wireProber {
	return &wireProber{arp: []string{remoteMAC}, fdb: []string{"eno1"}}
}

func (p *wireProber) set(arp, fdb []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.arp, p.fdb = arp, fdb
}

func (p *wireProber) Interfaces(context.Context) ([]resolve.HostIface, error) {
	return []resolve.HostIface{{Name: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("10.0.0.2/24")}}}, nil
}

func (p *wireProber) Route(context.Context, string, netip.Addr) (string, bool, error) {
	return "vmbr0", true, nil
}

func (p *wireProber) ARP(_ context.Context, _ string, addr netip.Addr) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if addr != remoteAddr {
		return nil, nil
	}
	return slices.Clone(p.arp), nil
}

func (p *wireProber) FDBPorts(_ context.Context, _ string, _ int, mac string) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if mac != remoteMAC {
		return nil, nil
	}
	return slices.Clone(p.fdb), nil
}

func (p *wireProber) PortKinds(context.Context, string) (map[string]resolve.PortKind, error) {
	return map[string]resolve.PortKind{
		"eno1": resolve.PortUplink, "eno2": resolve.PortUplink, "tap101i0": resolve.PortGuest,
	}, nil
}

func (p *wireProber) Dial(context.Context, string, netip.AddrPort) error { return nil }

// silenceEnv is an agent with the resolver of a host node over a wireProber,
// and a snapshot in which this node verifies the address the other node
// chose for its guest, under that node's attestation.
func silenceEnv(t *testing.T) (*env, *wireProber, agentproto.Snapshot) {
	t.Helper()
	e := newEnv(t)
	p := newWireProber()
	e.resolver = resolve.NewResolver(p, resolve.Settings{LocalNode: testNode, Attested: true}, e.clock.now)
	e.agent = e.newAgent()
	s := e.snapshot("www.example.com -> :8080")
	s.Targets[0] = remoteTarget()
	s.Targets[0].Guest.NICs[0].Static = []netip.Addr{remoteAddr}
	s.Nodes = append(s.Nodes, agentproto.Node{Name: otherNode, Addr: netip.MustParseAddr("10.0.0.3"), Online: true})
	return e, p, s
}

// served reports whether the egress set the filter was last given holds the
// guest's address.
func (e *env) served() bool {
	set, _ := e.egr.last()
	return slices.Equal(set, egressTargets("10.0.0.30:8080"))
}

// serve applies s with a heartbeat that says the other node reported 4 s
// ago, heard again once the agent follows the epoch of s, as the next status
// would be answered.
func (e *env) serve(s agentproto.Snapshot) {
	e.t.Helper()
	e.heard(4 * time.Second)
	e.apply(s)
	e.heard(4 * time.Second)
}

func (e *env) reverify() agentproto.Status {
	e.t.Helper()
	st, err := e.agent.Reverify(e.t.Context())
	require.NoError(e.t, err)
	return st
}

// A controller that goes silent takes nothing from what it granted: a
// replica keeps serving a guest of another node on the attestation its last
// heartbeat stood behind for as long as a takeover takes, its own checks
// passing all along.
func TestSilenceOfTheControllerWithdrawsNothingAttested(t *testing.T) {
	e, _, s := silenceEnv(t)
	e.heard(4 * time.Second)
	st := e.apply(s)
	r := report(st, "www.example.com")
	require.Equal(t, "port", r.Level)
	require.True(t, r.Attested)
	require.True(t, e.served())

	// The controller's node loses power; another takes over about two
	// minutes later and holds its first cycle for up to a minute more.
	for range 19 {
		e.clock.advance(10 * time.Second)
		st = e.reverify()
		r = report(st, "www.example.com")
		require.Equal(t, "port", r.Level, "at %s of silence: %s", e.clock.now().Sub(t0), r.Reason)
		require.True(t, r.Attested)
		require.True(t, r.OK)
		require.True(t, e.served(), "at %s of silence", e.clock.now().Sub(t0))
	}
}

// The new controller answers before it heard the attesting node: its
// heartbeat does not name that node, which is no word against it.
func TestAHeartbeatThatDoesNotNameTheAttestingNodeYetWithdrawsNothing(t *testing.T) {
	e, _, s := silenceEnv(t)
	e.serve(s)
	e.clock.advance(2 * time.Minute)
	e.agent.Heartbeat(agentproto.Heartbeat{
		Epoch: agentproto.Epoch{InstallID: testInstall, Generation: 2, Nonce: "n2"},
		Nodes: map[string]agentproto.NodeFreshness{testNode: {WatchRunning: true}},
	})

	st := e.reverify()

	r := report(st, "www.example.com")
	require.Equal(t, "port", r.Level, r.Reason)
	require.True(t, e.served())
}

// Silence holds what was served, nothing more: what the wire shows against
// the guest still takes its address out at once.
func TestWhatTheWireShowsDuringSilenceStillWithdraws(t *testing.T) {
	for _, tc := range []struct {
		name     string
		arp, fdb []string
	}{
		{"a second MAC answers for the address", []string{remoteMAC, "bc:24:11:00:00:99"}, []string{"eno1"}},
		{"the MAC is on the port of a guest of this node", []string{remoteMAC}, []string{"tap101i0"}},
		{"the MAC came in through another uplink", []string{remoteMAC}, []string{"eno2"}},
		{"the guest stopped answering", nil, []string{"eno1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, p, s := silenceEnv(t)
			e.serve(s)
			e.clock.advance(time.Minute)
			e.reverify()
			require.True(t, e.served(), "held through the silence so far")

			p.set(tc.arp, tc.fdb)
			e.clock.advance(10 * time.Second)
			st := e.reverify()

			r := report(st, "www.example.com")
			require.False(t, r.OK)
			require.False(t, r.Attested)
			require.False(t, e.served())
		})
	}
}

// Silence admits nothing: a target whose attestation did not stand before
// the controller went silent is not served on it during the silence, also
// once the attestation would have stood by then.
func TestSilenceRaisesNoLevel(t *testing.T) {
	t.Run("an attestation not yet stable", func(t *testing.T) {
		e, _, s := silenceEnv(t)
		s.Targets[0].Attestation.Since = t0.Add(-time.Minute)
		e.heard(4 * time.Second)
		st := e.apply(s)
		require.Equal(t, "observed", report(st, "www.example.com").Level)
		require.False(t, e.served())

		for range 30 {
			e.clock.advance(10 * time.Second)
			st = e.reverify()
			r := report(st, "www.example.com")
			require.Equal(t, "observed", r.Level)
			require.False(t, r.Attested)
			require.False(t, e.served())
		}
	})

	t.Run("a target first verified during the silence", func(t *testing.T) {
		e, _, s := silenceEnv(t)
		e.heard(4 * time.Second)
		e.clock.advance(30 * time.Second)

		st := e.apply(s)

		r := report(st, "www.example.com")
		require.Equal(t, "observed", r.Level)
		require.False(t, r.Attested)
		require.False(t, e.served())
	})

	t.Run("an attesting node whose status was stale when last heard", func(t *testing.T) {
		e, _, s := silenceEnv(t)
		e.serve(s)
		e.clock.advance(5 * time.Second)
		e.heard(31 * time.Second)
		e.clock.advance(20 * time.Second)

		st := e.reverify()

		require.Equal(t, "observed", report(st, "www.example.com").Level)
		require.False(t, e.served())
	})

	t.Run("an agent that started again during the silence", func(t *testing.T) {
		e, _, s := silenceEnv(t)
		e.serve(s)
		e.clock.advance(30 * time.Second)
		e.agent = e.newAgent()

		st := e.reverify()

		require.Equal(t, "observed", report(st, "www.example.com").Level)
		require.False(t, e.served())
	})

	t.Run("an attestation of a node no heartbeat named", func(t *testing.T) {
		e, _, s := silenceEnv(t)
		e.serve(s)
		e.clock.advance(30 * time.Second)
		s.Targets[0].Guest.Node, s.Targets[0].HostingNode, s.Targets[0].Attestation.Node = "pve3", "pve3", "pve3"
		s.Nodes = append(s.Nodes, agentproto.Node{Name: "pve3", Addr: netip.MustParseAddr("10.0.0.4"), Online: true})
		s.Seq = e.seq + 1

		st := e.apply(s)

		require.Equal(t, "observed", report(st, "www.example.com").Level)
		require.False(t, e.served())
	})
}

// Past the age a proof may reach, the attestation the silence held is shown
// as stale, at observed, and the address is still served: only a failing
// check withdraws while no controller can refresh it.
func TestALongSilenceShowsTheAttestationStaleAndWithdrawsNothing(t *testing.T) {
	e, _, s := silenceEnv(t)
	e.serve(s)

	for range 60 {
		e.clock.advance(10 * time.Second)
		e.reverify()
	}
	st := e.reverify()

	r := report(st, "www.example.com")
	require.Equal(t, "observed", r.Level)
	require.Contains(t, r.Reason, "attestation stale")
	require.True(t, r.OK)
	require.True(t, e.served())

	t.Run("a controller that answers again judges it afresh", func(t *testing.T) {
		e.heard(3 * time.Second)
		st := e.reverify()
		r := report(st, "www.example.com")
		require.Equal(t, "port", r.Level)
		require.True(t, r.Attested)
		require.Empty(t, r.Reason)
	})
}

// A controller that answers and leaves the attesting node out of its
// heartbeat says something: that node is gone from what it hears. That is
// no silence, and nothing is held over.
func TestALiveControllerThatLeavesTheNodeOutIsNoSilence(t *testing.T) {
	e, _, s := silenceEnv(t)
	e.serve(s)
	e.clock.advance(5 * time.Second)
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, Nodes: map[string]agentproto.NodeFreshness{testNode: {WatchRunning: true}}})

	st := e.reverify()

	require.Equal(t, "observed", report(st, "www.example.com").Level)
	require.False(t, e.served())

	t.Run("and the node it left out is not recalled once it falls silent", func(t *testing.T) {
		e.clock.advance(time.Minute)
		e.reverify()
		require.False(t, e.served())
	})
}

// A takeover moves the agent to a newer epoch before the new controller
// heard the attesting node: what the old one said of it is held over.
func TestASnapshotOfTheControllerThatTookOverKeepsWhatWasHeldOver(t *testing.T) {
	e, _, s := silenceEnv(t)
	e.serve(s)
	e.clock.advance(2 * time.Minute)
	s.Epoch = agentproto.Epoch{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	s.Seq = 1

	st := e.apply(s)

	require.Equal(t, "port", report(st, "www.example.com").Level)
	require.True(t, e.served())
}

// A hold-over has a ceiling: past it, without a word of a controller about
// the attesting node, the address is withdrawn.
func TestAHoldOverEndsAtItsCeiling(t *testing.T) {
	e, _, s := silenceEnv(t)
	e.serve(s)

	for e.clock.now().Sub(t0) < resolve.MaxHoldOver-time.Minute {
		e.clock.advance(time.Minute)
		e.reverify()
		require.True(t, e.served(), "at %s of silence", e.clock.now().Sub(t0))
	}
	e.clock.advance(2 * time.Minute)
	st := e.reverify()

	require.Equal(t, "observed", report(st, "www.example.com").Level)
	require.False(t, e.served())
}

// What the agent keeps of heartbeats is of the epoch it follows and of the
// nodes its desired state lists, and no older than a hold-over may be.
func TestTheHeartbeatsKeptAreOfTheEpochFollowedAndTheNodesListed(t *testing.T) {
	epoch2 := agentproto.Epoch{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	e, _, s := silenceEnv(t)
	e.serve(s)
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, Nodes: map[string]agentproto.NodeFreshness{
		testNode: {}, otherNode: {StatusAge: 4 * time.Second}, "pve9": {},
	}})
	e.clock.advance(10 * time.Second)
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch2, Nodes: map[string]agentproto.NodeFreshness{otherNode: {StatusAge: time.Second}}})
	e.agent.Heartbeat(agentproto.Heartbeat{
		Epoch: agentproto.Epoch{InstallID: "another", Generation: 3, Nonce: "n3"},
		Nodes: map[string]agentproto.NodeFreshness{otherNode: {StatusAge: time.Second}},
	})
	e.clock.advance(10 * time.Second)
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch2})

	age, at, recalled := e.agent.heardOf(otherNode)
	require.True(t, recalled)
	require.Equal(t, t0, at, "the heartbeat of the epoch followed, not the newer one")
	require.Equal(t, 4*time.Second, age)
	e.agent.heardMu.Lock()
	_, kept := e.agent.named["pve9"]
	e.agent.heardMu.Unlock()
	require.False(t, kept, "a node the desired state does not list")

	e.clock.advance(resolve.MaxHoldOver)
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch2})
	_, at, _ = e.agent.heardOf(otherNode)
	require.True(t, at.IsZero(), "older than a hold-over may be")
}
