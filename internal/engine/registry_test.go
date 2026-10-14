package engine

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeRemote is the agents of the other nodes as a test sets their
// reports, which keeps what the engine served them.
type fakeRemote struct {
	mu      sync.Mutex
	reports map[string]NodeReport
	served  []map[string]agentproto.Snapshot
}

func (f *fakeRemote) Serve(snaps map[string]agentproto.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, maps.Clone(snaps))
}

func (f *fakeRemote) Reports(context.Context, time.Duration, map[string]uint64) map[string]NodeReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.reports)
}

// report has node's agent report now.
func (f *fakeRemote) report(node string, at time.Time, started time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reports == nil {
		f.reports = map[string]NodeReport{}
	}
	f.reports[node] = NodeReport{ReceivedAt: at, Fresh: true, Status: agentproto.Status{
		Node: node, Kind: pki.KindHost, Protocol: agentproto.Range{Min: 1, Max: 1}, AgentStartedAt: started, Clock: at,
	}}
}

func (f *fakeRemote) last() map[string]agentproto.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.served) == 0 {
		return nil
	}
	return f.served[len(f.served)-1]
}

// clusterOf is an engine whose registry names this node, active with index
// 0, and others as given, with their agents remote and the members file as
// the test sets it.
func clusterOf(t *testing.T, others ...store.NodeEntry) (*env, *fakeRemote, *[]members.Member) {
	t.Helper()
	e := newEnv(t)
	zero := 0
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: testNode, Since: t0, State: store.NodeActive, StateSince: t0, Index: &zero}))
	for _, n := range others {
		require.NoError(t, e.store.SaveNode(n))
	}
	r := &fakeRemote{}
	listed := &[]members.Member{{Name: testNode, ID: 1, Online: true}}
	e.remote = r
	e.members = func() members.Members { return members.Members{Nodes: *listed} }
	e.eng = e.newEngine()
	return e, r, listed
}

func entryOf(t *testing.T, e *env, name string) store.NodeEntry {
	t.Helper()
	nodes, err := e.store.Nodes()
	require.NoError(t, err)
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no entry of %s", name)
	return store.NodeEntry{}
}

func nodeNames(t *testing.T, e *env) []string {
	t.Helper()
	nodes, err := e.store.Nodes()
	require.NoError(t, err)
	var out []string
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func nodeEventsOf(e *env) []string {
	var out []string
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindNode {
			out = append(out, ev.Message)
		}
	}
	return out
}

// A node that joins and reports is admitted in the next cycle, and gets its
// index then, once: the lowest one no other node holds.
func TestAJoiningNodeThatReportsIsAdmittedWithAnIndex(t *testing.T) {
	e, r, listed := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeJoining, StateSince: t0})
	*listed = append(*listed, members.Member{Name: "pve2", ID: 2, Online: true})
	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0)

	e.cycle()

	n := entryOf(t, e, "pve2")
	require.Equal(t, store.NodeActive, n.State)
	require.NotNil(t, n.Index)
	require.Equal(t, 1, *n.Index)
	require.Equal(t, e.clock.now(), n.StateSince)
	require.Equal(t, []string{"node pve2 is active, was joining", "node pve2 has index 1"}, nodeEventsOf(e))

	e.clock.advance(5 * time.Second)
	r.report("pve2", e.clock.now(), t0)
	e.cycle()
	again := entryOf(t, e, "pve2")
	require.Equal(t, n, again, "nothing written once it is active")
	require.Len(t, nodeEventsOf(e), 2)
}

// A node that joined but never reported stays joining, without an index.
func TestAJoiningNodeThatDoesNotReportStaysJoining(t *testing.T) {
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeJoining, StateSince: t0})
	e.clock.advance(10 * time.Second)
	r.report("pve3", e.clock.now(), t0) // a node the registry does not hold

	e.cycle()

	_, err := e.store.Nodes()
	require.NoError(t, err)
	require.NotContains(t, nodeNames(t, e), "pve3")
	n := entryOf(t, e, "pve2")
	require.Equal(t, store.NodeJoining, n.State)
	require.Nil(t, n.Index)
}

// The members file listing a node offline makes it down at once.
func TestANodeTheClusterListsOfflineIsDownAtOnce(t *testing.T) {
	one := 1
	e, r, listed := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	*listed = append(*listed, members.Member{Name: "pve2", ID: 2, Online: false})
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	e.cycle()

	require.Equal(t, store.NodeDown, entryOf(t, e, "pve2").State)
	require.Contains(t, nodeEventsOf(e), "node pve2 is down, was active")
}

// An agent silent for 30 seconds makes its node unknown; once it reports
// again the node joins again and is admitted in the cycle after.
func TestASilentNodeIsUnknownAndComesBackThroughJoining(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	require.Equal(t, store.NodeActive, entryOf(t, e, "pve2").State)

	e.clock.advance(31 * time.Second)
	e.cycle()
	require.Equal(t, store.NodeUnknown, entryOf(t, e, "pve2").State)

	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	require.Equal(t, store.NodeJoining, entryOf(t, e, "pve2").State)

	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	n := entryOf(t, e, "pve2")
	require.Equal(t, store.NodeActive, n.State)
	require.Equal(t, 1, *n.Index, "the index it had")
}

// An agent that started after its node became active returned: a reboot,
// which passes through joining.
func TestAnAgentThatStartedAgainMakesItsNodeJoinAgain(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	e.clock.advance(time.Minute)
	r.report("pve2", e.clock.now(), t0.Add(30*time.Second))

	e.cycle()

	require.Equal(t, store.NodeJoining, entryOf(t, e, "pve2").State)
}

// The controller's own node is heard in process: its daemon starting again
// is no return.
func TestTheControllersOwnNodeDoesNotReturn(t *testing.T) {
	e, _, _ := clusterOf(t)
	e.clock.advance(time.Minute)

	e.cycle()

	n := entryOf(t, e, testNode)
	require.Equal(t, store.NodeActive, n.State)
	require.Equal(t, t0, n.StateSince)
}

// A single node writes nothing of its registry entry, and its state shows
// no node views.
func TestASingleNodeLeavesItsEntryAsItIs(t *testing.T) {
	e := newEnv(t)
	before := entryOf(t, e, testNode)

	st := e.cycle()

	require.Equal(t, before, entryOf(t, e, testNode))
	require.Empty(t, st.Nodes)
	require.Empty(t, nodeEventsOf(e))
}

// Proxmox's maintenance mode drains a node and ends the drain when it ends;
// a read that cannot tell keeps what was known.
func TestMaintenanceDrainsTheNodeAndAnUnknownModeKeepsIt(t *testing.T) {
	one := 1
	mode := pve.MaintenanceOn
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	e.maintenance = func(context.Context) (map[string]pve.Maintenance, error) {
		return map[string]pve.Maintenance{"pve2": mode}, nil
	}
	e.eng = e.newEngine()
	step := func() store.NodeEntry {
		e.clock.advance(time.Second)
		r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
		e.cycle()
		return entryOf(t, e, "pve2")
	}

	n := step()
	require.Equal(t, store.NodeDraining, n.State)
	require.Equal(t, store.DrainMaintenance, n.Drained.Reason)

	mode = pve.MaintenanceUnknown
	require.Equal(t, store.NodeDraining, step().State, "unknown keeps what was known")

	mode = pve.MaintenanceOff
	n = step()
	require.Equal(t, store.NodeActive, n.State)
	require.Nil(t, n.Drained)
}

// An admin's drain is not ended by maintenance ending.
func TestTheEndOfMaintenanceLeavesAnAdminsDrain(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeDraining, StateSince: t0, Index: &one,
		Drained: &store.NodeDrain{Reason: store.DrainAdmin, Since: t0}})
	e.maintenance = func(context.Context) (map[string]pve.Maintenance, error) {
		return map[string]pve.Maintenance{"pve2": pve.MaintenanceOff}, nil
	}
	e.eng = e.newEngine()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	e.cycle()

	n := entryOf(t, e, "pve2")
	require.Equal(t, store.NodeDraining, n.State)
	require.Equal(t, store.DrainAdmin, n.Drained.Reason)
}

// A node joining with the intent to drain it is drained in the step that
// admits it, so that no snapshot shows it active.
func TestADrainedNodeIsDrainedAsItIsAdmitted(t *testing.T) {
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeJoining, StateSince: t0,
		Drained: &store.NodeDrain{Reason: store.DrainAdmin, Since: t0}})
	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0)

	e.cycle()

	require.Equal(t, store.NodeDraining, entryOf(t, e, "pve2").State)
	require.Equal(t, agentproto.Draining, r.last()["pve2"].State)
}

// Every node that is joining, active or draining is served a snapshot of
// the controller's epoch, without a run token while placement gives it no
// route, and, while no node watches its tables, without the targets of
// pve1's guests, which are pinned to pve1; others none. A snapshot that does
// not change keeps its seq.
func TestTheOtherNodesAreServedSnapshotsOfTheirState(t *testing.T) {
	one, two := 1, 2
	e, r, _ := clusterOf(t,
		store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one},
		store.NodeEntry{Name: "pve3", Since: t0, State: store.NodeDown, StateSince: t0, Index: &two},
		store.NodeEntry{Name: "pve4", Since: t0, State: store.NodeRemoved, StateSince: t0},
	)
	e.enforce()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	e.cycle()

	snaps := r.last()
	require.Len(t, snaps, 1)
	s := snaps["pve2"]
	require.Equal(t, agentproto.Active, s.State)
	require.Equal(t, "pve2", s.Node)
	require.Equal(t, agentproto.Epoch{InstallID: testInstall, Generation: testWriter.Generation, Nonce: testWriter.Nonce}, s.Epoch)
	require.Empty(t, s.Targets)
	require.Equal(t, []string{"10.0.0.2:8644"}, s.Endpoints, "an agent that lost its controller's address finds it here")
	for _, tun := range s.Tunnels {
		require.False(t, tun.Run)
		require.Empty(t, tun.Token.Reveal())
	}
	require.NoError(t, s.Validate("pve2", agentproto.Epoch{InstallID: testInstall}, 0))

	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	require.Equal(t, s.Seq, r.last()["pve2"].Seq, "the same content keeps its seq")
}

// A controller that does not hold the lease serves nothing.
func TestANodeWithoutTheLeaseServesNoSnapshot(t *testing.T) {
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0})
	l := holding(testWriter)
	l.st.Holder = false
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()

	e.cycle()

	require.Empty(t, r.last())
}

// A controller that loses the lease serves nothing more: what it served is
// taken back with the first cycle that holds.
func TestAControllerThatLosesTheLeaseServesNothingMore(t *testing.T) {
	e, r, l := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0})
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	e.cycle()
	require.Contains(t, r.last(), "pve2")

	l.set(func(st *lease.State) { st.Holder = false })
	e.clock.advance(time.Second)
	e.cycle()

	require.Empty(t, r.last())
}

// The registry is written only while the lease holds: one that went invalid
// after the cycle was let through writes no entry.
func TestTheRegistryIsWrittenOnlyWhileTheLeaseHolds(t *testing.T) {
	e, r, l := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeJoining, StateSince: t0})
	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0)
	l.set(func(st *lease.State) { st.Valid = false })

	e.cycle()

	require.Equal(t, store.NodeJoining, entryOf(t, e, "pve2").State)
	require.Nil(t, entryOf(t, e, "pve2").Index)
}

// A new controller writes nothing for up to a minute while an agent of a
// node that is neither down nor removed has not reported to it, and then
// goes on with that node unknown.
func TestTheTakeoverHoldWaitsAMinuteForALateAgent(t *testing.T) {
	one := 1
	e, r, listed := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	*listed = append(*listed, members.Member{Name: "pve2", ID: 2, Online: true})
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()
	e.enforce()

	st := e.cycle()
	require.Contains(t, st.Hold, "waiting for the agents of pve2 to report to this controller; nothing is written at Cloudflare for up to 1m0s")
	require.Empty(t, e.writes())
	require.Empty(t, r.last())

	e.clock.advance(30 * time.Second)
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng.d.Quorum = e.quorum
	st = e.cycle()
	require.Contains(t, st.Hold, "for up to 30s")
	require.Empty(t, e.writes())

	e.clock.advance(31 * time.Second)
	st = e.cycle()
	require.NotContains(t, st.Hold, "waiting for the agents")
	require.NotEmpty(t, e.writes())
	require.Equal(t, store.NodeUnknown, entryOf(t, e, "pve2").State)
	require.True(t, hasProblem(st, ".members lists 1 nodes online that have sent no status (pve2)"), st.Problems)
}

// The hold ends as soon as every agent reported.
func TestTheTakeoverHoldEndsOnceEveryAgentReported(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()
	e.enforce()
	st := e.cycle()
	require.Contains(t, st.Hold, "waiting for the agents of pve2")

	e.clock.advance(2 * time.Second)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	st = e.cycle()

	require.Empty(t, st.Hold)
	require.NotEmpty(t, e.writes())
	require.Equal(t, store.NodeActive, entryOf(t, e, "pve2").State)
}

// The node views show every node of a cluster with what its agent reported.
func TestTheStateShowsTheNodesOfACluster(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one})
	e.clock.advance(2 * time.Second)
	r.report("pve2", e.clock.now().Add(-time.Second), t0.Add(-time.Hour))

	st := e.cycle()

	require.Len(t, st.Nodes, 2)
	require.Equal(t, "pve1", st.Nodes[0].Name)
	require.True(t, st.Nodes[0].Controller)
	require.Equal(t, "pve2", st.Nodes[1].Name)
	require.Equal(t, store.NodeActive, st.Nodes[1].State)
	require.Equal(t, "1-1", st.Nodes[1].Protocol)
	require.Equal(t, time.Second, st.Nodes[1].StatusAge)
}

// The events of an agent land in the merged log tagged with its node, each
// once: a status sent again, and the resend of a new epoch, add nothing
// taken before.
func TestTheEventsOfAnAgentAreTakenOnceTaggedByNode(t *testing.T) {
	e := newEnv(t)
	epoch := agentproto.Epoch{InstallID: testInstall, Generation: 1, Nonce: "n1"}
	ev := func(cursor uint64, msg string) json.RawMessage {
		raw, err := json.Marshal(map[string]any{"cursor": cursor, "at": t0, "level": "warn", "kind": "egress", "subject": "10.0.0.5", "message": msg})
		require.NoError(t, err)
		return raw
	}
	st := agentproto.Status{Node: "pve2", AgentStartedAt: t0, Events: []json.RawMessage{ev(1, "one"), ev(2, "two")}, EventCursor: 2}

	require.Equal(t, uint64(2), e.eng.TakeAgentEvents("pve2", st, epoch))
	require.Equal(t, uint64(2), e.eng.TakeAgentEvents("pve2", st, epoch), "sent again")
	st.Events, st.EventCursor = []json.RawMessage{ev(3, "three")}, 3
	e.eng.TakeAgentEvents("pve2", st, epoch)

	var got []Event
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Node == "pve2" {
			got = append(got, ev)
		}
	}
	require.Len(t, got, 3)
	require.Equal(t, "one", got[0].Message)
	require.Equal(t, "egress", got[0].Kind)
	require.Equal(t, levelWarn, got[0].Level)
	require.Equal(t, "three", got[2].Message)

	// An agent started again numbers its events from one.
	st = agentproto.Status{Node: "pve2", AgentStartedAt: t0.Add(time.Hour), Events: []json.RawMessage{ev(1, "after a restart")}, EventCursor: 1}
	e.eng.TakeAgentEvents("pve2", st, epoch)
	last := e.eng.Events(time.Time{})
	require.Equal(t, "after a restart", last[len(last)-1].Message)
}

// An admin action that writes the store is the controller's: a node that
// does not hold the lease refuses it, naming the node that does, and writes
// nothing.
func TestAnAdminWriteIsRefusedOffTheController(t *testing.T) {
	e, l := onHost(t)
	rec, rev, _, err := e.store.Leader()
	require.NoError(t, err)
	rec.Node = "pve2"
	_, err = e.store.SaveLeaderIf(rec, rev)
	require.NoError(t, err)
	l.set(func(st *lease.State) { st.Holder = false })

	err = e.eng.AcknowledgeSegment(t.Context(), "vmbr0", 0)

	require.ErrorIs(t, err, ErrRefused)
	require.ErrorContains(t, err, "this node is not the controller; node pve2 holds the lease")
	segs, err := e.store.Segments()
	require.NoError(t, err)
	require.Empty(t, segs)
}

// reportEpoch has the agent of pve2 report now that it follows ep.
func (f *fakeRemote) reportEpoch(at time.Time, ep agentproto.Epoch) {
	const node = "pve2"
	f.report(node, at, t0.Add(-time.Hour))
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.reports[node]
	r.Status.EpochSeen = ep
	f.reports[node] = r
}

// forget drops what the agent of pve2 reported.
func (f *fakeRemote) forget() {
	const node = "pve2"
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.reports, node)
}

// leasedCluster is clusterOf with a lease this node holds for the writer of
// the store.
func leasedCluster(t *testing.T, others ...store.NodeEntry) (*env, *fakeRemote, *fakeLease) {
	t.Helper()
	e, r, _ := clusterOf(t, others...)
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()
	e.enforce()
	return e, r, l
}

var aboveTheStore = agentproto.Epoch{InstallID: testInstall, Generation: 7, Nonce: "later"}

// An agent that follows an epoch above the store's makes the controller
// behind, whatever the state of its node: one that only joins counts as
// much as an active one. Behind stays when its reports stop, and after the
// controller starts again, until pco promote.
func TestAReportAboveTheStoreKeepsTheControllerBehind(t *testing.T) {
	e, r, l := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeJoining, StateSince: t0})
	r.reportEpoch(e.clock.now(), aboveTheStore)

	st := e.cycle()
	require.Equal(t, problemBehindHost, st.Hold)
	require.Equal(t, ModeObserve, st.Mode)
	require.Empty(t, e.writes())

	r.forget()
	e.clock.advance(2 * time.Minute)
	st = e.cycle()
	require.Equal(t, problemBehindHost, st.Hold, "a report that stopped does not end it")
	require.Empty(t, e.writes())

	e.eng = e.newEngine()
	st = e.cycle()
	require.Equal(t, problemBehindHost, st.Hold, "nor a start of the engine")
	require.Empty(t, e.writes())
	require.NotEmpty(t, l.st.Behind)
}

// A report above the store that arrives after the first write stops the
// writes as it arrives, before the next cycle.
func TestALateReportAboveTheStoreStopsTheWritesAtOnce(t *testing.T) {
	e, r, l := leasedCluster(t)
	st := e.cycle()
	require.Empty(t, st.Hold)
	require.True(t, l.firstWrite)

	e.eng.NoteAgentEpoch("pve2", pki.KindHost, aboveTheStore)

	_, err := l.Gate(t.Context())
	require.Error(t, err)
	_, _, err = e.eng.writer()
	require.Error(t, err)
	writes := len(e.writes())
	e.clock.advance(time.Second)
	r.reportEpoch(e.clock.now(), aboveTheStore)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :8081")))
	st = e.cycle()
	require.Contains(t, st.Hold, "another controller writes")
	require.Len(t, e.writes(), writes)
}

// The epoch file of this node's agent that cannot be read may hold an epoch
// above the store's: nothing is written.
func TestAnUnreadableEpochOfTheLocalAgentHolds(t *testing.T) {
	e, _, _ := leasedCluster(t)
	e.cycle()
	writes := len(e.writes())
	require.NoError(t, os.WriteFile(filepath.Join(e.paths.Local, "agent", "epoch.json"), []byte("{not json"), 0o600))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :8081")))

	st := e.cycle()

	require.Contains(t, st.Hold, "the epoch the agent of this node follows cannot be read")
	require.Len(t, e.writes(), writes)
}

// The hold names the node whose agent follows an epoch above this
// controller's, and pco promote, which draws above it, ends both.
func TestTheNodeThatReportedAnEpochAboveIsNamedUntilPromote(t *testing.T) {
	e, r, l := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0})
	r.reportEpoch(e.clock.now(), aboveTheStore)

	st := e.cycle()
	require.Equal(t, problemBehindHost, st.Hold)
	require.True(t, hasProblem(st, "the agents of pve2 (epoch 7.later) follow an epoch above this controller's"), st.Problems)

	l.next = planner.Writer{InstallID: testInstall, Generation: 8, Nonce: "n8"}
	l.saved = e.store.SaveWriter
	_, err := confirmed(t, e, PromoteRequest{Confirm: true})
	require.NoError(t, err)
	r.reportEpoch(e.clock.now(), agentproto.Epoch{InstallID: testInstall, Generation: 8, Nonce: "n8"})
	e.clock.advance(time.Second)
	st = e.cycle()
	require.False(t, hasProblem(st, "follow an epoch above"), st.Problems)
	require.Empty(t, l.st.Behind)
}

// Only a host agent's epoch of this install, of a generation a draw makes
// and with a nonce, from a node the registry holds neither removed nor
// contested, may make the controller behind: a report that is none of these
// leaves it writing.
func TestAReportThatIsNoEpochOfThisInstallLeavesTheControllerWriting(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state string
		kind  pki.Kind
		ep    agentproto.Epoch
	}{
		{"of another install", store.NodeActive, pki.KindHost, agentproto.Epoch{InstallID: "fff000fff000", Generation: 9, Nonce: "x"}},
		{"of an appliance agent", store.NodeActive, pki.KindAppliance, aboveTheStore},
		{"past the bound", store.NodeActive, pki.KindHost, agentproto.Epoch{InstallID: testInstall, Generation: agentproto.MaxGeneration + 1, Nonce: "x"}},
		{"without a nonce", store.NodeActive, pki.KindHost, agentproto.Epoch{InstallID: testInstall, Generation: 9}},
		{"with a nonce no draw makes", store.NodeActive, pki.KindHost, agentproto.Epoch{InstallID: testInstall, Generation: 9, Nonce: "Not.One"}},
		{"of a removed node", store.NodeRemoved, pki.KindHost, aboveTheStore},
		{"of a contested node", store.NodeContested, pki.KindHost, aboveTheStore},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, r, l := leasedCluster(t, store.NodeEntry{Name: "pve2", Since: t0, State: tt.state, StateSince: t0})
			r.reportEpoch(e.clock.now(), tt.ep)
			r.mu.Lock()
			rep := r.reports["pve2"]
			rep.Status.Kind = tt.kind
			r.reports["pve2"] = rep
			r.mu.Unlock()
			if tt.state == store.NodeActive {
				// As the status arrives; the endpoint refuses a removed
				// or contested node before that, and the cycle has only
				// a report kept from before.
				e.eng.NoteAgentEpoch("pve2", tt.kind, tt.ep)
			}

			st := e.cycle()

			require.Empty(t, l.st.Behind)
			require.Empty(t, st.Hold)
			require.NotEmpty(t, e.writes())
		})
	}
}

// A registered node whose Proxmox API the client does not fall back on is
// named, as the members file lists no address for it.
func TestANodeWithoutAFallbackIsNamed(t *testing.T) {
	one := 1
	e, r, _ := clusterOf(t, store.NodeEntry{Name: "pve2", Since: t0, State: store.NodeActive, StateSince: t0, Index: &one},
		store.NodeEntry{Name: "pve3", Since: t0, State: store.NodeRemoved, StateSince: t0})
	have := []string{}
	e.fallbacks = func() []string { return have }
	e.eng = e.newEngine()
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))

	st := e.cycle()
	require.True(t, hasProblem(st, "the Proxmox API of pve2 is not fallen back on"), st.Problems)
	require.False(t, hasProblem(st, "pve3 is not fallen back on"), "a removed node is none of the cluster's")

	have = []string{"pve2"}
	e.clock.advance(time.Second)
	r.report("pve2", e.clock.now(), t0.Add(-time.Hour))
	st = e.cycle()
	require.False(t, hasProblem(st, "is not fallen back on"), st.Problems)
}
