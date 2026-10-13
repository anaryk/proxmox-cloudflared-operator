package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// fakeObserver stands in for the observer of the forwarding tables: it
// hands the test what the agent gave it and runs until the test ends it or
// its context ends.
type fakeObserver struct {
	mu       sync.Mutex
	expected func() map[string]egress.Expected
	report   func(egress.Observation)
	runs     int
	end      chan error
}

func (f *fakeObserver) observe(ctx context.Context, expected func() map[string]egress.Expected, report func(egress.Observation)) error {
	f.mu.Lock()
	f.expected, f.report, f.runs = expected, report, f.runs+1
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil
	case err := <-f.end:
		return err
	}
}

// started waits until the observer runs for the nth time.
func (f *fakeObserver) started(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.runs >= n
	}, 5*time.Second, time.Millisecond)
}

// see reports obs as the forwarding table would.
func (f *fakeObserver) see(obs egress.Observation) {
	f.mu.Lock()
	report := f.report
	f.mu.Unlock()
	report(obs)
}

func (f *fakeObserver) asked() map[string]egress.Expected {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.expected()
}

// observing starts the observer on the agent and waits for its first run.
func (e *env) observing(t *testing.T) *fakeObserver {
	t.Helper()
	f := &fakeObserver{end: make(chan error)}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer close(done)
		_ = e.agent.Observe(ctx, f.observe)
	}()
	t.Cleanup(func() { cancel(); <-done; e.agent.Wait() })
	f.started(t, 1)
	return f
}

// said waits until the agent said msg.
func (e *env) said(t *testing.T, msg string) {
	t.Helper()
	require.Eventually(t, func() bool { return slices.Contains(e.events.messages(), msg) }, 5*time.Second, time.Millisecond)
}

// status waits until the agent's status satisfies want.
func (e *env) status(t *testing.T, want func(agentproto.Status) bool) agentproto.Status {
	t.Helper()
	var st agentproto.Status
	require.Eventually(t, func() bool {
		st = e.agent.Status(t.Context())
		return want(st)
	}, 5*time.Second, time.Millisecond)
	return st
}

func learned(mac, bridge, port string, at time.Time) egress.Observation {
	return egress.Observation{Kind: egress.Learned, MAC: mac, Bridge: bridge, Port: port, At: at}
}

func TestTheObserverIsGivenEveryCardOfThePublishedGuests(t *testing.T) {
	const second = "bc:24:11:00:00:09"
	e := newEnv(t)
	f := e.observing(t)
	require.Empty(t, f.asked())
	s := e.snapshot("www.example.com -> :8080", "api.example.com -> :8081")
	s.Targets[0].Guest.NICs = append(s.Targets[0].Guest.NICs, model.NIC{Index: 1, MAC: "BC:24:11:00:00:09", Bridge: "vmbr1"})
	s.Targets = append(s.Targets, remoteTarget())
	s.Targets[2].Key = "remote.example.com"

	e.apply(s)

	qemu101 := model.GuestRef{Kind: model.KindQEMU, VMID: 101}
	require.Equal(t, map[string]egress.Expected{
		testMAC:   {MAC: testMAC, Guest: qemu101, NIC: 0},
		second:    {MAC: second, Guest: qemu101, NIC: 1},
		remoteMAC: {MAC: remoteMAC, Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 202}, NIC: 0},
	}, f.asked(), "the cards of the guest of this node and of the guest of another node alike")

	t.Run("and no more once they are not published", func(t *testing.T) {
		e.apply(e.snapshot())
		require.Empty(t, f.asked())
	})
}

func TestAPublishedCardLearnedOnItsOwnPortIsAttested(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Targets = append(s.Targets, remoteTarget())
	s.Targets[1].Key = "remote.example.com"
	e.apply(s)
	at := t0.Add(5 * time.Second)

	f.see(learned(testMAC, "vmbr0", "tap101i0", at))
	f.see(learned(remoteMAC, "vmbr0", "tap202i0", at.Add(time.Second)))

	st := e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 2 })
	require.Equal(t, []agentproto.Attestation{
		{Node: testNode, Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 101}, NIC: 0, MAC: testMAC, Bridge: "vmbr0", Port: "tap101i0", Since: at},
		{Node: testNode, Guest: model.GuestRef{Kind: model.KindQEMU, VMID: 202}, NIC: 0, MAC: remoteMAC, Bridge: "vmbr0", Port: "tap202i0", Since: at.Add(time.Second)},
	}, st.Attestations, "the guest the snapshot says runs elsewhere is attested where it is found")
	require.Empty(t, st.Observations)

	t.Run("seen again on the same port it keeps its time", func(t *testing.T) {
		f.see(learned(testMAC, "vmbr0", "tap101i0", at.Add(time.Minute)))
		e.clock.advance(time.Minute)
		st := e.status(t, func(st agentproto.Status) bool { return st.Clock.Equal(t0.Add(time.Minute)) })
		require.Equal(t, at, st.Attestations[0].Since)
	})

	t.Run("on another of its own ports it starts anew", func(t *testing.T) {
		f.see(learned(testMAC, "vmbr0", "fwpr101p0", at.Add(2*time.Minute)))
		st := e.status(t, func(st agentproto.Status) bool { return st.Attestations[0].Port == "fwpr101p0" })
		require.Equal(t, at.Add(2*time.Minute), st.Attestations[0].Since)
	})

	t.Run("gone, it is attested no more", func(t *testing.T) {
		f.see(egress.Observation{Kind: egress.Gone, MAC: testMAC, Bridge: "vmbr0", Port: "fwpr101p0", At: at.Add(3 * time.Minute)})
		st := e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })
		require.Equal(t, remoteMAC, st.Attestations[0].MAC)
	})

	t.Run("nor once its guest is not published", func(t *testing.T) {
		e.apply(e.snapshot())
		require.Empty(t, e.agent.Status(t.Context()).Attestations)
	})
}

// A guest behind its firewall is learned on two bridges: on the firewall
// bridge by its tap, on the card's bridge by the firewall port. Only the
// card's bridge is attested, with the port the other nodes find the MAC
// behind. A card with a VLAN tag on a bridge that does not filter VLANs is
// on the bridge Proxmox makes for the tag.
func TestOnlyTheCardsOwnBridgeIsAttested(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	s := e.snapshot("www.example.com -> :8080", "api.example.com -> :8081")
	s.Targets[1].Guest.Ref, s.Targets[1].Route.Guest = model.GuestRef{Kind: model.KindQEMU, VMID: 103}, &model.GuestRef{Kind: model.KindQEMU, VMID: 103}
	s.Targets[1].Guest.NICs = []model.NIC{{Index: 0, MAC: "bc:24:11:00:00:03", Bridge: "vmbr0", VLAN: 10}}
	e.apply(s)

	f.see(learned(testMAC, "fwbr101i0", "tap101i0", t0))
	f.see(learned(testMAC, "vmbr0", "fwpr101p0", t0))
	f.see(learned("bc:24:11:00:00:03", "vmbr0v10", "tap103i0", t0))
	f.see(learned("bc:24:11:00:00:03", "vmbr2", "tap103i0", t0))

	st := e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 2 })
	require.Equal(t, "vmbr0", st.Attestations[0].Bridge)
	require.Equal(t, "fwpr101p0", st.Attestations[0].Port)
	require.Equal(t, "vmbr0v10", st.Attestations[1].Bridge)
	require.Equal(t, "tap103i0", st.Attestations[1].Port)
}

const conflictSaid = "guest qemu/101: its MAC bc:24:11:00:00:01 is on port fwpr102p0 of bridge vmbr0, a port of guest lxc/102; " +
	"the address is not reached until the controller lifts the block"

func TestAConflictIsReportedWhileItHoldsAndBlocksTheAddressAtOnce(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	s := e.snapshot("www.example.com -> :8080")
	s.Guests = []model.Guest{{Ref: model.GuestRef{Kind: model.KindLXC, VMID: 102}, Node: testNode, Running: true, NICs: []model.NIC{{Index: 0, MAC: testMAC, Bridge: "vmbr0"}}}}
	e.apply(verified(s, "10.0.0.11:8080"))
	log := &callLog{}
	e.egr.log = log
	owner := &model.GuestRef{VMID: 102}

	f.see(egress.Observation{Kind: egress.Conflict, MAC: testMAC, Bridge: "vmbr0", Port: "fwpr102p0", From: "tap101i0", Owner: owner, At: t0})

	st := e.status(t, func(st agentproto.Status) bool { return len(st.Observations) == 1 })
	require.Equal(t, agentproto.Observation{
		Kind: agentproto.ObservationConflict, MAC: testMAC, Bridge: "vmbr0", Port: "fwpr102p0", From: "tap101i0",
		Owner: &model.GuestRef{Kind: model.KindLXC, VMID: 102}, At: t0,
	}, st.Observations[0], "the kind of the owner is the snapshot's")
	e.agent.Wait()
	require.Equal(t, "egress remove 10.0.0.11", log.all()[0], "the address this node serves on the MAC is out at once")
	e.said(t, conflictSaid)

	t.Run("seen again, it is the same conflict", func(t *testing.T) {
		f.see(egress.Observation{Kind: egress.Conflict, MAC: testMAC, Bridge: "vmbr0", Port: "fwpr102p0", Owner: owner, At: t0.Add(time.Minute)})
		e.clock.advance(time.Minute)
		st := e.status(t, func(st agentproto.Status) bool { return st.Clock.Equal(t0.Add(time.Minute)) })
		require.Len(t, st.Observations, 1)
		require.Equal(t, t0, st.Observations[0].At)
		require.Equal(t, 1, countOf(e.events.messages(), conflictSaid))
	})

	t.Run("and over once the entry goes", func(t *testing.T) {
		f.see(egress.Observation{Kind: egress.Gone, MAC: testMAC, Bridge: "vmbr0", Port: "fwpr102p0", At: t0.Add(2 * time.Minute)})
		e.status(t, func(st agentproto.Status) bool { return len(st.Observations) == 0 })
	})
}

func TestAMoveIsReportedForAWhileAndBlocksTheAddressAtOnce(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	e.apply(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080"))
	f.see(learned(testMAC, "vmbr0", "tap101i0", t0))
	e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })
	log := &callLog{}
	e.egr.log = log

	f.see(egress.Observation{Kind: egress.Moved, MAC: testMAC, Bridge: "vmbr0", From: "tap101i0", Port: "eno1", At: t0.Add(time.Second)})

	st := e.status(t, func(st agentproto.Status) bool { return len(st.Observations) == 1 })
	require.Empty(t, st.Attestations, "what moved is attested no more")
	require.Equal(t, agentproto.Observation{Kind: agentproto.ObservationMoved, MAC: testMAC, Bridge: "vmbr0", From: "tap101i0", Port: "eno1", At: t0.Add(time.Second)}, st.Observations[0])
	e.agent.Wait()
	require.Equal(t, "egress remove 10.0.0.11", log.all()[0])
	e.said(t, "guest qemu/101: its MAC bc:24:11:00:00:01 left its own port tap101i0 for eno1 on bridge vmbr0; the address is verified again before it is reached")

	t.Run("learned on its own port again, it is attested from then", func(t *testing.T) {
		f.see(learned(testMAC, "vmbr0", "tap101i0", t0.Add(2*time.Second)))
		st := e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })
		require.Equal(t, t0.Add(2*time.Second), st.Attestations[0].Since)
	})

	t.Run("and is forgotten after a while", func(t *testing.T) {
		e.clock.advance(movesKeptFor + 2*time.Second)
		require.Empty(t, e.agent.Status(t.Context()).Observations)
	})
}

func TestAConflictOnTheGuestsOwnEntryEndsItsAttestation(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	e.apply(e.snapshot("www.example.com -> :8080"))
	f.see(learned(testMAC, "vmbr0", "tap101i0", t0))
	e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })

	f.see(egress.Observation{Kind: egress.Conflict, MAC: testMAC, Bridge: "vmbr0", Port: "tap102i0", From: "tap101i0", Owner: &model.GuestRef{Kind: model.KindQEMU, VMID: 102}, At: t0.Add(time.Second)})

	st := e.status(t, func(st agentproto.Status) bool { return len(st.Observations) == 1 })
	require.Empty(t, st.Attestations)

	t.Run("learned on its own port again, it is attested from then", func(t *testing.T) {
		f.see(learned(testMAC, "vmbr0", "tap101i0", t0.Add(time.Minute)))
		st := e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })
		require.Equal(t, t0.Add(time.Minute), st.Attestations[0].Since)
		require.Empty(t, st.Observations, "the conflict is over")
	})
}

// A port of a guest the snapshot says is stopped may still be in the table
// for a moment, or the agent may hold a word of it the bridge has not taken
// back yet: nothing is attested of it until the snapshot says it runs.
func TestAGuestTheSnapshotSaysIsStoppedIsNotAttested(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	s := e.snapshot("www.example.com -> :8080")
	e.apply(s)
	f.see(learned(testMAC, "vmbr0", "tap101i0", t0))
	e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })

	s.Targets[0].Guest.Running = false
	st := e.apply(s)

	require.Empty(t, st.Attestations)

	t.Run("and attested again once it runs", func(t *testing.T) {
		s.Targets[0].Guest.Running = true
		st := e.apply(s)
		require.Len(t, st.Attestations, 1)
	})
}

func TestTheMovesTheStatusCarriesAreBounded(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	e.apply(e.snapshot("www.example.com -> :8080"))

	for i := range movesKept + 10 {
		f.see(egress.Observation{Kind: egress.Moved, MAC: testMAC, Bridge: "vmbr0", From: "tap101i0", Port: "eno1", At: t0.Add(time.Duration(i) * time.Millisecond)})
	}

	st := e.status(t, func(st agentproto.Status) bool {
		return len(st.Observations) == movesKept && st.Observations[0].At.Equal(t0.Add(10*time.Millisecond))
	})
	require.Equal(t, t0.Add(time.Duration(movesKept+9)*time.Millisecond), st.Observations[movesKept-1].At, "the oldest are left out")
}

func TestWhatTheTablesShowIsForgottenWhenTheObserverFails(t *testing.T) {
	e := newEnv(t)
	tm := &timers{}
	tm.install(e.agent)
	f := e.observing(t)
	e.apply(e.snapshot("www.example.com -> :8080"))
	f.see(learned(testMAC, "vmbr0", "tap101i0", t0))
	e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })
	e.agent.Watching(true)
	require.Equal(t, t0, e.agent.Status(t.Context()).WatchRunningSince)
	f.mu.Lock()
	ended := f.report
	f.mu.Unlock()
	e.clock.advance(time.Second)

	f.end <- errors.New("the subscription ended")

	st := e.status(t, func(st agentproto.Status) bool { return st.WatchRunningSince.IsZero() })
	require.Empty(t, st.Attestations)
	require.Equal(t, []time.Duration{observeAgainAfter}, tm.await(t))
	tm.fire(t)
	f.started(t, 2)
	e.status(t, func(st agentproto.Status) bool { return st.WatchRunningSince.Equal(t0.Add(time.Second)) })

	t.Run("an observation of the run that ended says nothing", func(t *testing.T) {
		ended(learned(testMAC, "vmbr0", "tap101i0", t0))
		f.see(learned(testMAC, "vmbr0", "tap101i0", t0.Add(2*time.Second)))
		st := e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })
		require.Equal(t, t0.Add(2*time.Second), st.Attestations[0].Since, "what the run that ended saw is not the first sighting")
	})
}

// The node is covered only while both watches of its tables run: the one
// that observes the forwarding tables and the one that pins the addresses
// it serves.
func TestTheWatchRunsSinceBothWatchesRun(t *testing.T) {
	e := newEnv(t)
	require.True(t, e.agent.Status(t.Context()).WatchRunningSince.IsZero())
	e.agent.Watching(true)
	require.True(t, e.agent.Status(t.Context()).WatchRunningSince.IsZero(), "the forwarding tables are not observed yet")
	e.clock.advance(time.Second)

	e.observing(t)

	require.Equal(t, t0.Add(time.Second), e.agent.Status(t.Context()).WatchRunningSince)
	e.agent.Watching(false)
	require.True(t, e.agent.Status(t.Context()).WatchRunningSince.IsZero())
	e.agent.Watching(true)
	require.Equal(t, t0.Add(time.Second), e.agent.Status(t.Context()).WatchRunningSince, "the later start of the two")
}

func TestAnApplianceAgentObservesNothingAndAttestsNothing(t *testing.T) {
	e := newEnv(t)
	e.kind = pki.KindAppliance
	e.agent = e.newAgent()
	e.apply(e.snapshot("www.example.com -> :8080"))

	err := e.agent.Observe(t.Context(), (&fakeObserver{}).observe)

	require.EqualError(t, err, "an appliance agent reads no forwarding table")
	require.Nil(t, e.agent.Expected())
	e.agent.Watching(true)
	st := e.agent.Status(t.Context())
	require.True(t, st.WatchRunningSince.IsZero())
	require.Empty(t, st.Attestations)
	require.Empty(t, st.Observations)
}

func TestANodeThatStoodDownAttestsNothing(t *testing.T) {
	e := newEnv(t)
	f := e.observing(t)
	e.apply(e.snapshot("www.example.com -> :8080"))
	f.see(learned(testMAC, "vmbr0", "tap101i0", t0))
	e.status(t, func(st agentproto.Status) bool { return len(st.Attestations) == 1 })

	require.NoError(t, e.agent.StandDown(t.Context(), agentproto.Refusal{Code: agentproto.RefusalRemoved}))

	st := e.agent.Status(t.Context())
	require.Empty(t, st.Attestations)
	require.True(t, st.WatchRunningSince.IsZero())
}
