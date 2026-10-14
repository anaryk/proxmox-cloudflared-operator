package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// fakeLease is a lease as a test sets it, which takes the reports of the
// agents as the keeper does: one leader.json does not explain makes it
// behind before the first write of the tenure, and stops its writes after.
type fakeLease struct {
	mu         sync.Mutex
	st         lease.State
	gateErr    error
	gates      int
	failAfter  int // the gates that pass before gateErr counts; 0 for none
	firstWrite bool
	alarm      string
	reported   []agentproto.Epoch
	promoted   []int
	promoteErr error
	// next is the writer a promotion draws, and saved what writes it to
	// leader.json.
	next  planner.Writer
	saved func(w planner.Writer) error
}

// holding is a lease this node holds, valid, for w.
func holding(w planner.Writer) *fakeLease {
	return &fakeLease{st: lease.State{
		Holder: true, Writer: w, Valid: true, ValidFor: time.Minute,
		Record: store.LeaderRecord{Writer: w, Node: testNode, AcquiredAt: t0},
	}}
}

func (f *fakeLease) Gate(context.Context) (planner.Writer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates++
	switch {
	case !f.st.Holder:
		return planner.Writer{}, fmt.Errorf("%w: %s", lease.ErrNotHolder, f.st.Waiting)
	case f.st.Behind != "":
		return planner.Writer{}, fmt.Errorf("%w: %s", lease.ErrBehind, f.st.Behind)
	case f.alarm != "":
		return planner.Writer{}, fmt.Errorf("%w: %s", lease.ErrFenced, f.alarm)
	case f.gateErr != nil && f.gates > f.failAfter:
		return planner.Writer{}, f.gateErr
	}
	return f.st.Writer, nil
}

func (f *fakeLease) State() lease.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeLease) ReportedEpoch(e agentproto.Epoch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reported = append(f.reported, e)
	ours := f.st.Writer
	if e.Generation < ours.Generation || e.Generation == ours.Generation && e.Nonce == ours.Nonce {
		return
	}
	if f.firstWrite {
		f.alarm = "an agent follows " + e.String() + ": another controller writes"
		return
	}
	f.st.Behind = "an agent follows " + e.String() + ": the store went back"
}

func (f *fakeLease) FirstWrite() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firstWrite = true
}

func (f *fakeLease) Promote(_ context.Context, seed int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoted = append(f.promoted, seed)
	if f.promoteErr != nil {
		return f.promoteErr
	}
	if f.saved != nil {
		if err := f.saved(f.next); err != nil {
			return err
		}
	}
	f.st.Writer, f.st.Record.Writer = f.next, f.next
	f.st.Behind, f.alarm, f.firstWrite = "", "", false
	return nil
}

func (f *fakeLease) set(change func(st *lease.State)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(&f.st)
}

func (f *fakeLease) gateCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gates
}

// onHost is an engine of the host with a lease this node holds for the
// writer of the store, and a quorum read made now.
func onHost(t *testing.T) (*env, *fakeLease) {
	t.Helper()
	e := newEnv(t)
	l := holding(testWriter)
	e.lease = l
	e.quorum = func() (bool, time.Time) { return true, e.clock.now() }
	e.eng = e.newEngine()
	return e, l
}

func TestAHostHoldingTheLeaseWritesAsBefore(t *testing.T) {
	e, l := onHost(t)
	e.enforce()

	st := e.cycle()

	require.Empty(t, st.Problems)
	require.NotEmpty(t, e.writes())
	require.Positive(t, l.gateCalls(), "every write asks the gate")
	require.NotNil(t, st.Lease)
	require.Equal(t, testNode, st.Lease.Holder)
	require.True(t, st.Lease.Valid)
	require.Equal(t, 1, st.Lease.Generation)
	require.True(t, l.firstWrite)
}

// The agent of this node runs in the process and reports at once: the first
// cycle of a new tenure is not held for the report round, also with the clock
// standing still.
func TestTheFirstCycleOfANewTenureIsNotHeldForTheLocalAgent(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	next := planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	require.NoError(t, e.store.SaveWriter(next))
	l.set(func(st *lease.State) { st.Writer, st.Record.Writer = next, next })

	st := e.cycle()

	require.Empty(t, st.Problems)
	require.Empty(t, st.Hold, "no wait for the report of the agent of this node")
	require.Equal(t, VerdictOK, st.WriterVerdict)
}

func TestTheWriterCallbackFailsWhileTheGateDoesAndSaysWhy(t *testing.T) {
	e, l := onHost(t)
	e.eng.us = testWriter
	l.gateErr = fmt.Errorf("%w: a pause of 4s without a tick", lease.ErrFenced)

	_, _, err := e.eng.writer()

	require.ErrorIs(t, err, lease.ErrFenced)
	require.EqualError(t, err, "the lease lets no write through: the lease is fenced: a pause of 4s without a tick")

	l.gateErr = nil
	l.set(func(st *lease.State) { st.Writer.Nonce = "other" })
	_, _, err = e.eng.writer()
	require.EqualError(t, err, "the lease is of epoch 1.other, not of this cycle's writer 1.n1")
}

// A gate that fails between the start of a cycle and its first write at
// Cloudflare stops the write.
func TestATunnelRunWritesNothingOnceTheGateFails(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	l.gateErr = fmt.Errorf("%w: the last refresh failed", lease.ErrFenced)
	l.failAfter = 1 // the cycle's own look at the lease passes

	st := e.cycle()

	require.Empty(t, e.writes())
	require.True(t, containsLine(st.Problems, "the lease lets no write through: the lease is fenced: the last refresh failed"),
		"%q", st.Problems)
}

func TestThePreTunnelGateAsksTheLeaseToo(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	c := e.eng.newCycle(t.Context())
	c.writerRead = true
	e.eng.us = testWriter
	require.Empty(t, c.gateClosed(), "open while the lease lets writes through")

	l.gateErr = fmt.Errorf("%w: the lease was not refreshed for 1m0s", lease.ErrFenced)

	require.Contains(t, c.gateClosed(), "the lease lets no write through: the lease is fenced: the lease was not refreshed for 1m0s")
}

// Without quorum nothing is written at all, also by a cycle with nothing to
// save: the cycle holds before it reads more than the settings, and says the
// line of the quorum.
func TestAHostWithoutQuorumWritesNothingAndSaysSo(t *testing.T) {
	for name, quorum := range map[string]func(*env) (bool, time.Time){
		"not quorate": func(e *env) (bool, time.Time) { return false, e.clock.now() },
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := onHost(t)
			e.enforce()
			e.cycle()
			before, calls := e.files(), len(e.cf.Calls())
			e.quorum = func() (bool, time.Time) { return quorum(e) }
			e.eng.d.Quorum = e.quorum

			st := e.cycle()

			require.Equal(t, problemNotQuorate, st.Hold)
			require.Contains(t, st.Problems, problemNotQuorate)
			require.Equal(t, before, e.files(), "nothing is saved")
			require.Len(t, e.cf.Calls(), calls, "Cloudflare is not asked")
		})
	}
}

func TestAQuorumReadThatIsOldOrMissingHoldsTheCycle(t *testing.T) {
	for name, tc := range map[string]struct {
		at   func(now time.Time) time.Time
		line string
	}{
		"never read": {func(time.Time) time.Time { return time.Time{} },
			"the quorum of the cluster is not known: the members file was not read yet; nothing is written"},
		"read 6 s ago": {func(now time.Time) time.Time { return now.Add(-6 * time.Second) },
			"the quorum of the cluster is not known: the members file was last read 6s ago; nothing is written"},
	} {
		t.Run(name, func(t *testing.T) {
			e, _ := onHost(t)
			e.enforce()
			e.eng.d.Quorum = func() (bool, time.Time) { return true, tc.at(e.clock.now()) }

			st := e.cycle()

			require.Equal(t, tc.line, st.Hold)
			require.Empty(t, e.writes())
		})
	}
}

func TestANodeThatDoesNotHoldTheLeaseWritesNothing(t *testing.T) {
	t.Run("another node is the controller", func(t *testing.T) {
		e, l := onHost(t)
		e.enforce()
		rec, _, _, err := e.store.Leader()
		require.NoError(t, err)
		rec.Node = "pve2"
		require.NoError(t, e.store.SaveLeader(rec))
		l.set(func(st *lease.State) { *st = lease.State{Waiting: "the lock is held; leader.json names node \"pve2\""} })

		st := e.cycle()

		require.Equal(t, "this node does not hold the lease; node pve2 is the controller", st.Hold)
		require.Empty(t, e.writes())
	})
	t.Run("nobody yet", func(t *testing.T) {
		e, l := onHost(t)
		e.enforce()
		l.set(func(st *lease.State) { *st = lease.State{Waiting: "the cluster is not quorate"} })

		st := e.cycle()

		require.Equal(t, "this node does not hold the lease yet: the cluster is not quorate; nothing is written", st.Hold)
		require.Empty(t, e.writes())
	})
}

// A node that does not hold the lease keeps no Cloudflare credential in
// memory: the clients go when it loses it, and none is built until it holds
// it again.
func TestTheCredentialsAreDroppedWithTheLease(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	require.NotEmpty(t, e.eng.clients)
	made := len(e.made)

	l.set(func(st *lease.State) { *st = lease.State{Waiting: "the tenure ended"} })
	e.cycle()

	require.Empty(t, e.eng.clients)
	require.Empty(t, e.eng.tokens)
	e.eng.recheck(t.Context())
	e.eng.checkNew(t.Context())
	require.Len(t, e.made, made, "no client is built without the lease")
}

func TestAFencedHolderDropsItsClients(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	l.set(func(st *lease.State) { st.Valid, st.Fenced = false, "the lease was not refreshed for 1m0s" })
	l.gateErr = fmt.Errorf("%w: the lease was not refreshed for 1m0s", lease.ErrFenced)

	st := e.cycle()

	require.Equal(t, "the lease is fenced: the lease is fenced: the lease was not refreshed for 1m0s; nothing is written", st.Hold)
	require.Empty(t, e.eng.clients)
}

// An agent that follows an epoch above the one leader.json names, before the
// first write of the tenure: the store went back, and the controller only
// observes until pco promote.
func TestAReportAboveTheStoreBeforeTheFirstWriteIsBehind(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	require.NoError(t, e.store.SaveAgentEpoch(store.Epoch{InstallID: testInstall, Generation: 4, Nonce: "later"}))
	before := e.files()

	st := e.cycle()

	require.Equal(t, problemBehindHost, st.Hold)
	require.Equal(t, ModeObserve, st.Mode, "observe-only shown")
	require.Contains(t, st.Lease.Behind, "4.later")
	require.Empty(t, e.cf.Calls(), "nothing is written, Cloudflare not even asked")
	require.Equal(t, before, e.files(), "nothing is saved")
	require.False(t, l.firstWrite)
	require.True(t, hasLeaseEvent(e.eng, "behind at generation 1"))
}

func TestAReportAboveTheStoreAfterTheFirstWriteStopsTheWrites(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	require.True(t, l.firstWrite)
	writes := len(e.writes())

	require.NoError(t, e.store.SaveAgentEpoch(store.Epoch{InstallID: testInstall, Generation: 4, Nonce: "other"}))
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :8081")))
	st := e.cycle()

	require.Empty(t, l.st.Behind, "not behind: an alarm")
	require.Contains(t, st.Hold, "another controller writes")
	require.Len(t, e.writes(), writes)
}

// The first write of a tenure waits until every node that is neither down
// nor removed has reported since the tenure began: a node whose agent
// remembers a later epoch than the store must be heard first.
func TestTheFirstWriteWaitsForEveryAgentToReport(t *testing.T) {
	since := t0
	nodes := []store.NodeEntry{
		{Name: "pve1"}, {Name: "pve2", State: store.NodeActive}, {Name: "pve3", State: store.NodeDown},
		{Name: "pve4", State: store.NodeRemoved}, {Name: "pve5", State: store.NodeUnknown},
	}
	reports := map[string]NodeReport{
		"pve1": {ReceivedAt: since},
		"pve2": {ReceivedAt: since.Add(-time.Second)},
	}

	require.Equal(t, []string{"pve2", "pve5"}, unreported(nodes, reports, since))

	reports["pve2"] = NodeReport{ReceivedAt: since.Add(time.Second)}
	reports["pve5"] = NodeReport{ReceivedAt: since.Add(2 * time.Second)}
	require.Empty(t, unreported(nodes, reports, since))
}

func TestTheStoreGoingBackHoldsUntilPromote(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	// A claim this tenure wrote is gone: the store was put back from a copy
	// made before it.
	removeClaims(t, e)
	e.cycle()
	writes := len(e.writes())

	st := e.cycle()

	require.Equal(t, problemStoreWentBack, st.Hold)
	require.Equal(t, ModeObserve, st.Mode)
	require.Len(t, e.writes(), writes)

	l.next = planner.Writer{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	l.saved = e.store.SaveWriter
	// A promotion that fails once it began a new store tenure leaves the
	// controller holding.
	l.promoteErr = errors.New("the cluster filesystem did not answer")
	_, err := e.eng.Promote(t.Context(), PromoteRequest{Confirm: true})
	require.Error(t, err)
	st = e.cycle()
	require.Equal(t, problemStoreWentBack, st.Hold, "still")
	require.Len(t, e.writes(), writes)

	l.promoteErr = nil
	res, err := e.eng.Promote(t.Context(), PromoteRequest{Confirm: true})
	require.NoError(t, err)
	require.True(t, res.Drawn)
	require.NoError(t, e.store.WentBack(), "a new tenure")
}

func TestAStoreOfAnotherClusterHoldsEveryWrite(t *testing.T) {
	e, _ := onHost(t)
	e.enforce()
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	inst.Cluster = &store.ClusterIdentity{CASHA256: caOld, Name: "old-lab"}
	require.NoError(t, e.store.SaveInstall(inst))
	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, nil }

	st := e.cycle()

	require.Equal(t, "this store was made in cluster old-lab ("+caOld+"); pco promote here makes this cluster its writer", st.Hold)
	require.Empty(t, e.cf.Calls())
}

func TestTheIdentityOfTheClusterIsRecordedOnceTheLeaseLetsTheCycleWrite(t *testing.T) {
	e, _ := onHost(t)
	e.enforce()
	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: caNew}, nil }

	e.cycle()
	inst, _, err := e.store.Install()
	require.NoError(t, err)
	require.Equal(t, &store.ClusterIdentity{CASHA256: caNew}, inst.Cluster)

	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, nil }
	e.cycle()
	inst, _, err = e.store.Install()
	require.NoError(t, err)
	require.Equal(t, &store.ClusterIdentity{CASHA256: caNew, Name: "lab"}, inst.Cluster, "the name once it has one")

	e.eng.d.Cluster = func() (store.ClusterIdentity, error) { return store.ClusterIdentity{}, errors.New("no such file") }
	st := e.cycle()
	require.Equal(t, "the identity of this cluster cannot be read (no such file); nothing is written", st.Hold)
}

func TestTheLeaseEventsTellWhatChanged(t *testing.T) {
	e, l := onHost(t)
	e.enforce()
	e.cycle()
	require.True(t, hasLeaseEvent(e.eng, "acquired at generation 1"))

	l.set(func(st *lease.State) { st.Valid, st.Fenced = false, "a pause of 4s without a tick" })
	l.gateErr = errors.New("fenced")
	e.cycle()
	require.True(t, hasLeaseEvent(e.eng, "fenced at generation 1: a pause of 4s without a tick"))

	l.set(func(st *lease.State) { st.Valid, st.Fenced = true, "" })
	l.gateErr = nil
	e.cycle()
	require.True(t, hasLeaseEvent(e.eng, "resumed at generation 1"))

	l.set(func(st *lease.State) { *st = lease.State{Waiting: "the lock is gone: it expired and was removed"} })
	e.cycle()
	require.True(t, hasLeaseEvent(e.eng, "this node no longer holds the lease: the lock is gone: it expired and was removed"))
}

// The appliance has no lease: nothing of this changes it.
func TestAnEngineWithoutALeaseShowsNone(t *testing.T) {
	e := newEnv(t)
	e.enforce()

	st := e.cycle()

	require.Nil(t, st.Lease)
	require.NotEmpty(t, e.writes())
}

var (
	caOld = strings.Repeat("aa", 32)
	caNew = strings.Repeat("bb", 32)
)

func containsLine(lines []string, part string) bool {
	for _, l := range lines {
		if strings.Contains(l, part) {
			return true
		}
	}
	return false
}

// hasLeaseEvent says whether an event of the lease begins with message.
func hasLeaseEvent(eng *Engine, message string) bool {
	for _, ev := range eng.Events(time.Time{}) {
		if ev.Kind == kindLease && strings.HasPrefix(ev.Message, message) {
			return true
		}
	}
	return false
}

// removeClaims takes away the files of the claims, as a copy of the store
// made before they were written has none.
func removeClaims(t *testing.T, e *env) {
	t.Helper()
	dir := filepath.Join(e.paths.Cluster, "claims")
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.NotEmpty(t, files)
	for _, f := range files {
		require.NoError(t, os.Remove(filepath.Join(dir, f.Name())))
	}
}
