package agent

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// begin starts a pass of steps of s and ends it with the test.
func (e *env) begin(s agentproto.Snapshot) *Steps {
	e.t.Helper()
	st, err := e.agent.Begin(e.t.Context(), s)
	require.NoError(e.t, err)
	e.t.Cleanup(st.End)
	return st
}

func TestStepsResolveOnceAndGiveTheFilterOneSet(t *testing.T) {
	e := newEnv(t)
	log := &callLog{}
	e.egr.log = log
	s := e.snapshot("www.example.com -> :8080", "api.example.com -> :9090")

	st := e.begin(s)
	res, err := st.Resolve()
	require.NoError(t, err)
	require.Len(t, res.Results, 2)
	require.Empty(t, log.all(), "nothing reaches the filter while the engine decides")

	require.Empty(t, st.Bind(res.Results, nil))
	set, err := st.Feed()
	require.NoError(t, err)
	st.End()

	require.Equal(t, egressTargets("10.0.0.11:8080", "10.0.0.11:9090"), set)
	require.Equal(t, []string{"egress set 10.0.0.11:8080 10.0.0.11:9090"}, log.all())
	require.Len(t, e.res.calls(), 2, "each target resolved once")
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Contains(t, bindings, "www.example.com")
	require.Contains(t, e.agent.Bound(), guestAddr)
	require.True(t, report(e.agent.Status(t.Context()), "www.example.com").InEgressSet)
}

// What the engine holds back after the resolution does not enter the set,
// though the agent verified it; its binding is kept all the same.
func TestATargetTheEngineHoldsBackStaysOutOfTheSet(t *testing.T) {
	e := newEnv(t)
	st := e.begin(e.snapshot("www.example.com -> :8080", "api.example.com -> :9090"))
	res, err := st.Resolve()
	require.NoError(t, err)

	served := res.Results
	held := served["api.example.com"]
	held.Target = planner.ResolvedTarget{Reason: "segment not acknowledged"}
	served["api.example.com"] = held
	require.Equal(t, egressTargets("10.0.0.11:8080"), st.EgressSet(served))
	st.Bind(served, nil)
	set, err := st.Feed()
	require.NoError(t, err)
	st.End()

	require.Equal(t, egressTargets("10.0.0.11:8080"), set)
	got, _ := e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got)
	bindings, err := e.store.Bindings()
	require.NoError(t, err)
	require.Contains(t, bindings, "api.example.com")
}

func TestStepsHandTheEngineWhatTheResolverFoundAsItFoundIt(t *testing.T) {
	e := newEnv(t)
	st := e.begin(e.snapshot("www.example.com -> :8080"))
	res, err := st.Resolve()
	require.NoError(t, err)

	res.Results["www.example.com"].Binding.Service = netip.MustParseAddr("198.18.0.1")

	again := st.EgressSet(map[string]resolve.Result{})
	require.Empty(t, again)
	require.False(t, st.p.results["www.example.com"].Binding.Service.IsValid(), "the engine's copy is its own")
}

func TestALostProofLeavesTheFilterWhenTheStepResolves(t *testing.T) {
	e := newEnv(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	log := &callLog{}
	e.egr.log = log
	e.res.stop("www.example.com", "identity check failed")

	st := e.begin(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	res, err := st.Resolve()
	require.NoError(t, err)

	require.Equal(t, []netip.Addr{guestAddr}, res.Lost)
	require.Equal(t, []string{"egress remove 10.0.0.11"}, log.all())
	st.Bind(res.Results, nil)
	set, err := st.Feed()
	require.NoError(t, err)
	require.Empty(t, set, "and out of the set of the pass, whose rule is still at Cloudflare")
}

func TestTakeOutRemovesOnlyWhatTheSetHolds(t *testing.T) {
	e := newEnv(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	st := e.begin(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	res, err := st.Resolve()
	require.NoError(t, err)

	require.NoError(t, st.TakeOut(netip.MustParseAddr("10.0.0.99")))
	require.NoError(t, st.TakeOut(guestAddr))
	require.NoError(t, st.TakeOut(guestAddr))

	require.Equal(t, []netip.Addr{guestAddr}, e.egr.removes(), "once, and only what the set holds")
	require.Empty(t, st.EgressSet(res.Results), "out of the set of the pass")
}

func TestAFailedSetIsReturned(t *testing.T) {
	e := newEnv(t)
	e.egr.failSet(errBoom)
	st := e.begin(e.snapshot("www.example.com -> :8080"))
	res, err := st.Resolve()
	require.NoError(t, err)
	st.Bind(res.Results, nil)

	_, err = st.Feed()
	require.ErrorIs(t, err, errBoom)

	e.egr.failSet(egress.ErrOff)
	st.End()
	st = e.begin(e.snapshot("www.example.com -> :8080"))
	res, err = st.Resolve()
	require.NoError(t, err)
	st.Bind(res.Results, nil)
	_, err = st.Feed()
	require.NoError(t, err, "a filter switched off is no failure")
}

func TestAResolutionCutShortChangesNothing(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	st, err := e.agent.Begin(ctx, e.snapshot("www.example.com -> :8080"))
	require.NoError(t, err)
	defer st.End()
	cancel()

	_, err = st.Resolve()
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, e.egr.setCount())
}

func TestEachResolutionTakesItsDeadlineFromTheEngine(t *testing.T) {
	e := newEnv(t)
	var asked []time.Duration
	d := e.deps()
	d.Timeout = func(ctx context.Context, wait time.Duration) (context.Context, context.CancelFunc) {
		asked = append(asked, wait)
		return context.WithTimeout(ctx, wait)
	}
	a, err := New(d)
	require.NoError(t, err)
	e.agent = a

	st := e.begin(e.snapshot("www.example.com -> :8080"))
	_, err = st.Resolve()
	require.NoError(t, err)

	require.Equal(t, []time.Duration{resolveTimeout}, asked)
}

func TestBindWatchesTargetsOnALegByTheInterfaceOfTheirNetwork(t *testing.T) {
	e := newEnv(t)
	e.res.onLeg("www.example.com", "vmbr1")
	st := e.begin(e.snapshot("www.example.com -> :8080"))
	res, err := st.Resolve()
	require.NoError(t, err)
	service := netip.MustParseAddr("198.18.0.1")
	served := res.Results
	r := served["www.example.com"]
	r.Target.Leg, r.Target.Via, r.Binding.Service = true, service, service
	served["www.example.com"] = r

	st.Bind(served, []gateway.Leg{{ID: "vmbr1", Mark: 7}})

	pin, ok := e.agent.Bound()[service]
	require.True(t, ok)
	require.Equal(t, gateway.LegIface(7), pin.Dev)
	require.Equal(t, guestAddr, pin.Real)
}

func TestFinishKeepsTheSnapshotAndTheConnectors(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot("www.example.com -> :8080")
	st := e.begin(s)
	res, err := st.Resolve()
	require.NoError(t, err)
	st.Bind(res.Results, nil)
	_, err = st.Feed()
	require.NoError(t, err)

	conns, problems, err := st.Finish(s, true)
	require.NoError(t, err)
	st.End()

	require.Empty(t, problems)
	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken}, e.conn.changes())
	require.Len(t, conns, 1)
	require.True(t, conns[0].Active)
	raw, found, err := e.store.AgentSnapshot()
	require.NoError(t, err)
	require.True(t, found)
	require.JSONEq(t, encoded(t, s), string(raw))
	require.Equal(t, s.Seq, e.agent.Status(t.Context()).SeqApplied)

	t.Run("the same snapshot again writes nothing", func(t *testing.T) {
		before := modTimes(t, e.paths.Local)
		st := e.begin(s)
		_, _, err := st.Finish(s, true)
		require.NoError(t, err)
		st.End()
		require.Equal(t, before, modTimes(t, e.paths.Local))
	})
}

func TestFinishWithoutKeepWritesNothing(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	s.Tunnels[0].Run = false
	st := e.begin(s)

	conns, problems, err := st.Finish(s, false)
	require.NoError(t, err)
	st.End()

	require.Empty(t, problems)
	require.Len(t, conns, 1, "its status is read all the same")
	require.False(t, conns[0].Active)
	require.Empty(t, e.conn.changes(), "a tunnel the snapshot does not run is left as it is")
	_, found, err := e.store.AgentSnapshot()
	require.NoError(t, err)
	require.False(t, found)
}

func TestFinishKeepsNoSnapshotOfAnEpochBelowTheOneFollowed(t *testing.T) {
	e := newEnv(t)
	e.apply(e.snapshot())
	s := e.snapshot()
	s.Epoch = agentproto.Epoch{InstallID: testInstall, Generation: 1, Nonce: "other"}

	st := e.begin(s)
	require.ErrorIs(t, st.Refused(), agentproto.ErrOtherNonce, "said as the pass begins")
	e.conn.reset()
	_, _, err := st.Finish(s, true)
	st.End()

	require.ErrorIs(t, err, agentproto.ErrOtherNonce)
	require.Empty(t, e.conn.changes(), "a refused snapshot changes no connector")
	require.Contains(t, e.agent.View().Problems[0], "refused a desired state")
}

func TestAPassOfALowerEpochIsRefusedAsItBegins(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	s.Epoch.Generation = 5
	e.apply(s)

	low := e.snapshot()
	st := e.begin(low)
	require.ErrorIs(t, st.Refused(), agentproto.ErrLowerEpoch)

	none := e.snapshot()
	none.Epoch = agentproto.Epoch{InstallID: testInstall}
	st.End()
	st = e.begin(none)
	require.NoError(t, st.Refused(), "a controller without a writer identity writes nothing anyway")
}

func TestASnapshotThatCannotBeKeptChangesNoConnector(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	require.NoError(t, os.MkdirAll(filepath.Join(e.paths.Local, "agent", "desired.json", "x"), 0o700))
	st := e.begin(s)

	_, _, err := st.Finish(s, true)
	st.End()

	require.Error(t, err)
	require.Empty(t, e.conn.changes())
}

// A verification of an address whose MAC moved waits for the pass of steps,
// as it waits for the engine's cycle.
func TestAVerificationWaitsForThePassOfSteps(t *testing.T) {
	e, _ := watching(t)
	e.agent.after = func(time.Duration) (<-chan time.Time, func() bool) {
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch, func() bool { return true }
	}
	st := e.begin(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	resolved := len(e.res.calls())

	e.agent.Moved(t.Context(), guestAddr)
	time.Sleep(20 * time.Millisecond)
	require.Len(t, e.res.calls(), resolved, "not while the pass runs")

	st.End()
	e.agent.Wait()
	require.Len(t, e.res.calls(), resolved+1)
}

func TestAHaltedNodeLetsNothingBackIntoTheFilter(t *testing.T) {
	e, _ := watching(t)
	e.agent.after = func(time.Duration) (<-chan time.Time, func() bool) {
		ch := make(chan time.Time, 1)
		ch <- time.Time{}
		return ch, func() bool { return true }
	}
	require.NoError(t, e.agent.Halt(t.Context()))
	got, _ := e.egr.last()
	require.Empty(t, got)
	require.Equal(t, egressTargets("10.0.0.11:8080"), e.agent.EgressSet(), "the set is kept for when it serves again")
	sets := e.egr.setCount()

	e.agent.Moved(t.Context(), guestAddr)
	e.agent.Wait()

	require.Equal(t, sets, e.egr.setCount(), "a verification that passes sets nothing")

	e.agent.Resume()
	e.agent.Moved(t.Context(), guestAddr)
	e.agent.Wait()
	require.Equal(t, sets+1, e.egr.setCount())
}

func TestHaltOfAFilterSwitchedOffIsNoFailure(t *testing.T) {
	e := newEnv(t)
	e.egr.failSet(egress.ErrOff)
	require.NoError(t, e.agent.Halt(t.Context()))
	e.egr.failSet(errBoom)
	require.True(t, errors.Is(e.agent.Halt(t.Context()), errBoom))
}

func TestTheNodeOperationsOfTheEngine(t *testing.T) {
	e := newEnv(t)
	e.conn.put(sharedID, testInstall, sharedToken)

	token, found, err := e.agent.Token(sharedID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, sharedToken, token)

	require.NoError(t, e.agent.Restart(t.Context(), sharedID, "rotated"))
	require.NoError(t, e.agent.StopConnectors(t.Context()))
	require.Equal(t, []string{"Ensure " + sharedID + " rotated", "StopAll"}, e.conn.changes())
}

// Begin checks the snapshot as Apply would: the seq the agent applied, the
// denylist and the levels, not the epoch alone.
func TestBeginRefusesWhatApplyWouldRefuse(t *testing.T) {
	e := newEnv(t)
	applied := e.snapshot()
	e.seq = 5
	applied.Seq = 5
	e.apply(applied)

	back := e.snapshot()
	back.Seq = 4
	st := e.begin(back)
	require.ErrorIs(t, st.Refused(), agentproto.ErrSeqBack)
	st.End()

	bad := e.snapshot()
	bad.Deny.Soft = map[string]string{"not an address": "why"}
	st = e.begin(bad)
	require.Error(t, st.Refused())
	st.End()

	level := e.snapshot("www.example.com -> :8080")
	level.Targets[0].Required = "none"
	st = e.begin(level)
	require.Error(t, st.Refused())
	st.End()

	st = e.begin(e.snapshot())
	require.NoError(t, st.Refused())
}

// Only a snapshot the agent takes counts as a controller heard.
func TestARefusedPassIsNoControllerHeard(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	s.Epoch.Generation = 5
	e.apply(s)
	e.clock.advance(90 * time.Second)

	st := e.begin(e.snapshot())
	require.Error(t, st.Refused())
	st.End()
	require.Equal(t, 90*time.Second, e.agent.View().SilentFor)

	taken := e.snapshot()
	taken.Epoch.Generation = 5
	st = e.begin(taken)
	require.NoError(t, st.Refused())
	st.End()
	require.Zero(t, e.agent.View().SilentFor)
}
