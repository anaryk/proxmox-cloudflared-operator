package daemon

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// fakeController is the controller a remote fleet answers for.
type fakeController struct {
	mu       sync.Mutex
	valid    bool
	epochs   []agentproto.Epoch
	taken    []agentproto.Status
	triggers int
}

func (f *fakeController) LeaseValid(time.Time) (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.valid, time.Minute
}

func (f *fakeController) TakeAgentEvents(_ string, st agentproto.Status, _ agentproto.Epoch) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taken = append(f.taken, st)
	return st.EventCursor
}

func (f *fakeController) NoteAgentEpoch(_ string, _ pki.Kind, ep agentproto.Epoch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.epochs = append(f.epochs, ep)
}

func (f *fakeController) Trigger() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers++
}

var fleetEpoch = agentproto.Epoch{InstallID: clusterInstall, Generation: 3, Nonce: "n3"}

func newTestFleet(now *time.Time) (*RemoteFleet, *fakeController) {
	f := newRemoteFleet(func() time.Time { return *now }, func() agentproto.Epoch { return fleetEpoch })
	ctl := &fakeController{valid: true}
	f.attach(ctl, func(context.Context) agentproto.Status {
		return agentproto.Status{Node: testNode, Connectors: []connector.Status{{TunnelID: "t1", Ready: true}}}
	})
	return f, ctl
}

// A long-poll answers at once with a snapshot of another epoch than the
// agent follows, waits for a seq above the one it applied, and ends with its
// context when none comes.
func TestTheRemoteFleetAnswersALongPollWithANewSnapshot(t *testing.T) {
	now := t0
	f, _ := newTestFleet(&now)
	f.Serve(map[string]agentproto.Snapshot{otherNode: {Node: otherNode, Epoch: fleetEpoch, Seq: 4}})

	s, err := f.Snapshot(t.Context(), otherNode, agentproto.Epoch{}, 0)
	require.NoError(t, err)
	require.Equal(t, uint64(4), s.Seq, "another epoch: at once")

	got := make(chan agentproto.Snapshot, 1)
	go func() {
		s, err := f.Snapshot(t.Context(), otherNode, fleetEpoch, 4)
		if err == nil {
			got <- s
		}
	}()
	select {
	case <-got:
		t.Fatal("answered before a new seq")
	case <-time.After(50 * time.Millisecond):
	}
	f.Serve(map[string]agentproto.Snapshot{otherNode: {Node: otherNode, Epoch: fleetEpoch, Seq: 5}})
	select {
	case s := <-got:
		require.Equal(t, uint64(5), s.Seq)
	case <-time.After(5 * time.Second):
		t.Fatal("no answer to the new seq")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = f.Snapshot(ctx, "pve3", agentproto.Epoch{}, 0)
	require.ErrorIs(t, err, context.DeadlineExceeded, "a node served no snapshot waits")
}

// A status is answered with the heartbeat: the controller's epoch and
// lease, the node's seq, the freshness of every node, the ready replicas,
// and the cursor of the events taken. Its epoch reaches the lease as it
// arrives, and a node heard for the first time asks for a cycle.
func TestTheRemoteFleetAnswersAStatusWithTheHeartbeat(t *testing.T) {
	now := t0
	f, ctl := newTestFleet(&now)
	f.Serve(map[string]agentproto.Snapshot{otherNode: {Node: otherNode, Epoch: fleetEpoch, Seq: 7}})
	st := agentproto.Status{
		Node: otherNode, EpochSeen: fleetEpoch, SeqApplied: 7, AgentStartedAt: t0,
		Events: []json.RawMessage{json.RawMessage(`{"cursor":1}`)}, EventCursor: 1,
		Connectors: []connector.Status{{TunnelID: "t1", Ready: true}},
	}

	hb, err := f.Report(t.Context(), otherNode, st)
	require.NoError(t, err)

	require.Equal(t, fleetEpoch, hb.Epoch)
	require.True(t, hb.LeaseValid)
	require.Equal(t, uint64(7), hb.Seq)
	require.Equal(t, uint64(1), hb.EventAck)
	require.Equal(t, 2, hb.ReadyReplicas["t1"], "this node's and the other's")
	require.Contains(t, hb.Nodes, otherNode)
	require.Contains(t, hb.Nodes, testNode)
	require.Equal(t, []agentproto.Epoch{fleetEpoch}, ctl.epochs)
	require.Equal(t, 1, ctl.triggers)

	now = now.Add(10 * time.Second)
	_, err = f.Report(t.Context(), otherNode, st)
	require.NoError(t, err)
	require.Equal(t, 1, ctl.triggers, "nothing new")

	reports := f.Reports(t.Context(), 0, nil)
	require.True(t, reports[otherNode].Fresh)
	require.Empty(t, reports[otherNode].Status.Events, "the events go to the log, not into the report kept")
	now = now.Add(31 * time.Second)
	require.False(t, f.Reports(t.Context(), 0, nil)[otherNode].Fresh)
}

// The proof wait returns once every node named reported the seq asked for,
// or when the wait is over.
func TestTheRemoteFleetWaitsForTheSeqOfACycle(t *testing.T) {
	now := t0
	f, _ := newTestFleet(&now)
	_, err := f.Report(t.Context(), otherNode, agentproto.Status{Node: otherNode, SeqApplied: 1})
	require.NoError(t, err)

	start := time.Now()
	reports := f.Reports(t.Context(), 50*time.Millisecond, map[string]uint64{otherNode: 2})
	require.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond)
	require.Equal(t, uint64(1), reports[otherNode].Status.SeqApplied)

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = f.Report(context.Background(), otherNode, agentproto.Status{Node: otherNode, SeqApplied: 2})
	}()
	reports = f.Reports(t.Context(), 5*time.Second, map[string]uint64{otherNode: 2})
	require.Equal(t, uint64(2), reports[otherNode].Status.SeqApplied)
}

// A status that does not hold together is refused for its node alone, and
// is not kept: the reports of the others go on, and a heartbeat that waits
// on the agent of this node holds no other node's report.
func TestABadStatusIsRefusedForItsNodeAlone(t *testing.T) {
	now := t0
	f, ctl := newTestFleet(&now)
	_, err := f.Report(t.Context(), "pve3", agentproto.Status{Node: "pve3", SeqApplied: 1})
	require.NoError(t, err)

	for name, st := range map[string]agentproto.Status{
		"of another node":             {Node: "pve3"},
		"of an epoch no draw makes":   {Node: otherNode, EpochSeen: agentproto.Epoch{InstallID: clusterInstall, Generation: agentproto.MaxGeneration + 1, Nonce: "x"}},
		"of an epoch without a nonce": {Node: otherNode, EpochSeen: agentproto.Epoch{InstallID: clusterInstall, Generation: 3}},
		"of more service addresses than a gateway maps": {Node: otherNode, ServiceAddrs: serviceAddrs(agentproto.MaxServiceAddrs + 1)},
		"larger than a status body": {Node: otherNode, Problems: []string{strings.Repeat("x", agentproto.MaxStatusBytes)}},
	} {
		_, err := f.Report(t.Context(), otherNode, st)
		require.Error(t, err, name)
	}
	reports := f.Reports(t.Context(), 0, nil)
	require.NotContains(t, reports, otherNode)
	require.Equal(t, uint64(1), reports["pve3"].Status.SeqApplied, "the other node's report stands")
	require.Equal(t, []agentproto.Epoch{{}}, ctl.epochs, "only the status taken reached the lease")

	held := make(chan struct{})
	release := make(chan struct{})
	f.attach(ctl, func(context.Context) agentproto.Status {
		close(held)
		<-release
		return agentproto.Status{}
	})
	go func() { _, _ = f.Report(context.Background(), otherNode, agentproto.Status{Node: otherNode}) }()
	<-held
	done := make(chan map[string]engine.NodeReport, 1)
	go func() { done <- f.Reports(context.Background(), 0, nil) }()
	select {
	case got := <-done:
		require.Contains(t, got, "pve3")
	case <-time.After(5 * time.Second):
		t.Fatal("a heartbeat that waits held the reports of the others")
	}
	close(release)
}

// serviceAddrs are n service addresses from the start of the pool.
func serviceAddrs(n int) []netip.Addr {
	out := make([]netip.Addr, 0, n)
	for a := netip.MustParseAddr("198.18.1.0"); len(out) < n; a = a.Next() {
		out = append(out, a)
	}
	return out
}

// A host node joins with pco setup and issues its own certificates: the
// endpoint takes neither over the channel.
func TestTheRemoteFleetTakesNoJoinOrRenewal(t *testing.T) {
	now := t0
	f, _ := newTestFleet(&now)
	_, err := f.Join(t.Context(), pki.JoinToken{}, nil, netip.Addr{}, clusterapi.JoinFacts{})
	require.ErrorIs(t, err, errHostJoin)
	_, err = f.Renew(t.Context(), otherNode, nil)
	require.Error(t, err)
}
