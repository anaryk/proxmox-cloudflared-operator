package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// listen subscribes as a client that has seen nothing yet.
func listen(t *testing.T, e *Engine) <-chan Notice {
	t.Helper()
	ch, _, err := e.Subscribe(t.Context(), "", 0)
	require.NoError(t, err)
	return ch
}

// until sends a state notice as a mark and returns what came before it on ch.
func until(t *testing.T, e *Engine, ch <-chan Notice) []Notice {
	t.Helper()
	e.notify.state(StateNotice{Digest: "mark"})
	var out []Notice
	for {
		select {
		case n, ok := <-ch:
			require.True(t, ok, "the stream ended")
			if n.Kind == NoticeState && n.State.Digest == "mark" {
				return out
			}
			out = append(out, n)
		case <-time.After(10 * time.Second):
			t.Fatalf("the mark did not come; got %d notices", len(out))
		}
	}
}

// receive returns the next notice on ch.
func receive(t *testing.T, ch <-chan Notice) Notice {
	t.Helper()
	select {
	case n, ok := <-ch:
		require.True(t, ok, "the stream ended")
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("no notice came")
	}
	return Notice{}
}

// shown says in a line what each notice is.
func shown(notices []Notice) []string {
	out := make([]string, len(notices))
	for i, n := range notices {
		switch n.Kind {
		case NoticeEvent:
			out[i] = fmt.Sprintf("event %s %d", n.Event.Kind, n.Event.Seq)
		case NoticeGap:
			out[i] = fmt.Sprintf("gap %d-%d %d %s", n.Gap.From, n.Gap.To, n.Gap.Count, n.Gap.Level)
		case NoticeState:
			out[i] = "state " + n.State.Digest
		case NoticeTraffic:
			out[i] = "traffic " + n.Traffic.At.Format(time.TimeOnly)
		case NoticeReset:
			out[i] = "reset " + n.Reason
		}
	}
	return out
}

// eventsOf makes n events of a kind, all at level info.
func eventsOf(kind string, n int) []Event {
	out := make([]Event, n)
	for i := range out {
		out[i] = Event{At: t0, Level: levelInfo, Kind: kind, Subject: fmt.Sprintf("g%d.example.com", i), Message: "something changed"}
	}
	return out
}

// requireCovered checks that the ids of the stream only grow and that every
// event from 1 to last came on its own or within a gap, only the high-volume
// kinds within one.
func requireCovered(t *testing.T, notices []Notice, last uint64, kinds map[uint64]string) {
	t.Helper()
	var id uint64
	seen := map[uint64]bool{}
	for _, n := range notices {
		switch n.Kind {
		case NoticeEvent:
			require.Greater(t, n.Event.Seq, id, "ids only grow")
			id = n.Event.Seq
			seen[id] = true
		case NoticeGap:
			require.Greater(t, n.Gap.To, id, "ids only grow")
			id = n.Gap.To
			count := 0
			for seq := n.Gap.From; seq <= n.Gap.To; seq++ {
				if highVolume(kinds[seq]) && !seen[seq] {
					seen[seq] = true
					count++
				}
			}
			require.Equal(t, n.Gap.Count, count, "the count of the gap")
		}
	}
	for seq := uint64(1); seq <= last; seq++ {
		require.True(t, seen[seq], "event %d was neither sent nor in a gap", seq)
	}
}

func kindsOf(events []Event) map[uint64]string {
	out := map[uint64]string{}
	for i, ev := range events {
		out[uint64(i+1)] = ev.Kind
	}
	return out
}

func TestTheDigestNamesWhatAStateHoldsAndNotWhenItWasMade(t *testing.T) {
	st := populatedState().normalized()
	d := digestOf(st)

	require.Regexp(t, `^[0-9a-f]{16}$`, d)
	require.Equal(t, d, digestOf(populatedState().normalized()), "equal states")

	later := st.clone()
	later.At, later.FinishedAt, later.Digest = st.At.Add(time.Hour), st.FinishedAt.Add(time.Hour), "0123456789abcdef"
	require.Equal(t, d, digestOf(later), "only the times of the cycle and the digest differ")

	changed := st.clone()
	changed.Routes[0].State = planner.StateUnreachable
	require.NotEqual(t, d, digestOf(changed), "a route changed")
}

func TestTheBootIsSixteenHexDigitsOfItsOwn(t *testing.T) {
	e := newEnv(t)
	again := e.newEngine()

	require.Regexp(t, `^[0-9a-f]{16}$`, e.eng.Boot())
	require.NotEqual(t, e.eng.Boot(), again.Boot())
	e.cycle()
	for _, ev := range e.eng.Events(time.Time{}) {
		require.Equal(t, e.eng.Boot(), ev.Boot)
	}
}

func TestEveryPublishSendsTheStateWithTheTimesOfItsCycle(t *testing.T) {
	e := newEnv(t)
	// The proofs stand, as the watch of the network lets them.
	e.res.now = func() time.Time { return t0 }
	e.cycle()
	ch := listen(t, e.eng)

	e.clock.advance(10 * time.Second)
	e.cycle()
	first := e.eng.State()
	e.clock.advance(10 * time.Second)
	e.cycle()
	second := e.eng.State()
	e.res.setUnreachable("www.example.com", "connection refused")
	e.clock.advance(10 * time.Second)
	e.cycle()
	third := e.eng.State()

	require.Equal(t, testNode, first.Node)
	require.Equal(t, digestOf(first), first.Digest)
	require.Equal(t, first.Digest, second.Digest, "an idle cycle over the same world")
	require.NotEqual(t, second.Digest, third.Digest, "a route changed")
	var states []StateNotice
	for _, n := range until(t, e.eng, ch) {
		if n.Kind == NoticeState {
			states = append(states, *n.State)
		}
	}
	require.Equal(t, []StateNotice{
		{At: first.At, FinishedAt: first.FinishedAt, Digest: first.Digest},
		{At: second.At, FinishedAt: second.FinishedAt, Digest: second.Digest},
		{At: third.At, FinishedAt: third.FinishedAt, Digest: third.Digest},
	}, states, "one for every cycle, whether its digest changed or not")
}

func TestTheStateOfACycleComesBeforeItsEvents(t *testing.T) {
	e := newEnv(t)
	ch := listen(t, e.eng)

	e.cycle()

	got := until(t, e.eng, ch)
	require.NotEmpty(t, got)
	require.Equal(t, NoticeState, got[0].Kind)
	require.Equal(t, e.eng.State().Digest, got[0].State.Digest)
	events := e.eng.Events(time.Time{})
	require.Len(t, got, 1+len(events))
	for i, ev := range events {
		require.Equal(t, ev, *got[i+1].Event)
	}
}

func TestAnEgressCheckThatChangesTheStateSendsIt(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	before := e.eng.State().Digest
	ch := listen(t, e.eng)

	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOff}})
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOff}})

	now := e.eng.State()
	require.NotEqual(t, before, now.Digest)
	require.Equal(t, digestOf(now), now.Digest)
	var states []string
	for _, n := range until(t, e.eng, ch) {
		if n.Kind == NoticeState {
			states = append(states, n.State.Digest)
		}
	}
	require.Equal(t, []string{now.Digest}, states, "once: the second check changed nothing")
}

func TestAConfirmationThatWithdrawsTheOfferSendsTheState(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(many(10)...))
	e.cycle()
	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(20 * time.Second)
	require.NotEmpty(t, e.cycle().Offer)
	before := e.eng.State().Digest
	ch := listen(t, e.eng)

	e.apply(true)

	now := e.eng.State()
	require.Empty(t, now.Offer)
	require.NotEqual(t, before, now.Digest)
	require.Equal(t, digestOf(now), now.Digest)
	got := until(t, e.eng, ch)
	require.Contains(t, shown(got), "state "+now.Digest)
}

func TestABatchOfFortyRouteEventsIsThirtyTwoAndAGap(t *testing.T) {
	e := newEnv(t)
	ch := listen(t, e.eng)
	batch := eventsOf(kindRoute, 40)
	batch[35].Level = levelError
	batch[38].Level = levelWarn

	e.eng.events.add(batch...)

	got := until(t, e.eng, ch)
	require.Len(t, got, 33)
	for i := range 32 {
		require.Equal(t, fmt.Sprintf("event route %d", i+1), shown(got)[i])
	}
	require.Equal(t, GapNotice{Boot: e.eng.Boot(), From: 33, To: 40, Count: 8, Level: levelError}, *got[32].Gap)
}

func TestBatchesOfAThousandActionsOrClaimsCoalesceAlike(t *testing.T) {
	for _, kind := range []string{kindAction, kindClaim, kindRoute} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			ch := listen(t, e.eng)
			batch := eventsOf(kind, 1000)
			batch[500].Level = levelWarn

			e.eng.events.add(batch...)

			got := until(t, e.eng, ch)
			require.Len(t, got, 33)
			require.Equal(t, fmt.Sprintf("event %s 32", kind), shown(got)[31])
			require.Equal(t, "gap 33-1000 968 warn", shown(got)[32])
			requireCovered(t, got, 1000, kindsOf(batch))
		})
	}
}

func TestLowVolumeEventsInAThousandRouteEventsComeOneByOne(t *testing.T) {
	e := newEnv(t)
	ch := listen(t, e.eng)
	batch := eventsOf(kindRoute, 1000)
	batch[500] = Event{At: t0, Level: levelWarn, Kind: kindProblem, Message: "a problem"}
	batch[999] = Event{At: t0, Level: levelError, Kind: kindWriter, Subject: "leader.json", Message: "writer verdict is stale"}

	e.eng.events.add(batch...)

	got := shown(until(t, e.eng, ch))
	require.Len(t, got, 35)
	require.Equal(t, "event route 32", got[31])
	require.Equal(t, []string{"event problem 501", "gap 33-999 966 info", "event writer 1000"}, got[32:])
}

func TestEveryLowVolumeKindComesOneByOne(t *testing.T) {
	for _, kind := range []string{kindProblem, kindWriter, kindHold, kindCredential, kindAdmin, kindEgress, kindRollout, kindConflict} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			ch := listen(t, e.eng)

			e.eng.events.add(eventsOf(kind, 40)...)

			got := until(t, e.eng, ch)
			require.Len(t, got, 40)
			for _, n := range got {
				require.Equal(t, NoticeEvent, n.Kind)
			}
		})
	}
}

func TestAnOverflowingSubscriberKeepsItsProblems(t *testing.T) {
	e := newEnv(t)
	ch := listen(t, e.eng)
	var all []Event
	for i := range 300 {
		ev := eventsOf(kindRoute, 1)[0]
		if i%30 == 29 {
			ev = Event{At: t0, Level: levelWarn, Kind: kindProblem, Message: fmt.Sprint("problem ", i)}
		}
		all = append(all, ev)
		e.eng.events.add(ev)
	}

	got := until(t, e.eng, ch)

	problems, gaps := 0, 0
	for _, n := range got {
		switch {
		case n.Kind == NoticeEvent && n.Event.Kind == kindProblem:
			problems++
		case n.Kind == NoticeGap:
			gaps++
		}
	}
	require.Equal(t, 10, problems, "every problem came on its own")
	require.Equal(t, 1, gaps)
	require.Less(t, len(got), 300)
	requireCovered(t, got, 300, kindsOf(all))
}

func TestASubscriberThreeHundredNoticesBehindGetsOneGapAndTheNewestState(t *testing.T) {
	e := newEnv(t)
	ch := listen(t, e.eng)
	for i := 1; i <= 150; i++ {
		e.eng.notify.state(StateNotice{Digest: fmt.Sprint(i)})
		e.eng.events.add(eventsOf(kindRoute, 1)...)
	}

	got := shown(until(t, e.eng, ch))

	want := []string{"state 1", "state 129", "gap 1-128 128 info", "event route 129"}
	for i := 130; i <= 150; i++ {
		want = append(want, fmt.Sprintf("state %d", i), fmt.Sprintf("event route %d", i))
	}
	require.Equal(t, want, got, "the first was on its way already; of the rest the events and the states collapsed")

	e.eng.events.add(eventsOf(kindRoute, 1)...)
	require.Equal(t, []string{"event route 151"}, shown([]Notice{receive(t, ch)}), "the stream goes on")
}

func TestAMegabyteBehindIsFull(t *testing.T) {
	e := newEnv(t)
	ch := listen(t, e.eng)
	for range 4 {
		ev := eventsOf(kindRoute, 1)[0]
		ev.Message = strings.Repeat("x", 300<<10)
		e.eng.events.add(ev)
	}

	require.Equal(t, []string{"event route 1", "gap 2-4 3 info"}, shown(until(t, e.eng, ch)))
}

func TestOfTheTrafficOnlyTheNewestIsKept(t *testing.T) {
	e := newEnv(t)
	ch := listen(t, e.eng)
	at := func(i int) time.Time { return t0.Add(time.Duration(i) * time.Second) }
	for i := 1; i <= 300; i++ {
		e.eng.notify.send(Notice{Kind: NoticeTraffic, Traffic: &TrafficNotice{At: at(i)}})
	}

	got := shown(until(t, e.eng, ch))

	want := []string{"traffic " + at(1).Format(time.TimeOnly)}
	for i := 257; i <= 300; i++ {
		want = append(want, "traffic "+at(i).Format(time.TimeOnly))
	}
	require.Equal(t, want, got)
}

func TestSubscribeReplaysTheRingAfterASeq(t *testing.T) {
	e := newEnv(t)
	for range 50 {
		e.eng.events.add(eventsOf(kindRoute, 1)...)
	}

	t.Run("a few", func(t *testing.T) {
		ch, hello, err := e.eng.Subscribe(t.Context(), e.eng.Boot(), 45)
		require.NoError(t, err)
		require.Equal(t, Hello{Boot: e.eng.Boot(), Seq: 50, Digest: e.eng.State().Digest, PollInterval: "10s"}, hello)
		got := shown(until(t, e.eng, ch))
		require.Equal(t, []string{"event route 46", "event route 47", "event route 48", "event route 49", "event route 50"}, got)
	})
	t.Run("more than 32", func(t *testing.T) {
		ch, _, err := e.eng.Subscribe(t.Context(), e.eng.Boot(), 5)
		require.NoError(t, err)
		got := shown(until(t, e.eng, ch))
		require.Len(t, got, 33)
		require.Equal(t, "event route 6", got[0])
		require.Equal(t, "gap 38-50 13 info", got[32])
	})
	t.Run("nothing after the last", func(t *testing.T) {
		ch, _, err := e.eng.Subscribe(t.Context(), e.eng.Boot(), 50)
		require.NoError(t, err)
		require.Empty(t, until(t, e.eng, ch))
	})
	t.Run("without a seq", func(t *testing.T) {
		ch, _, err := e.eng.Subscribe(t.Context(), e.eng.Boot(), 0)
		require.NoError(t, err)
		require.Empty(t, until(t, e.eng, ch), "a client that has seen nothing reads the events itself")
	})
}

func TestAReplayBeyondTheRingStartsWithAGap(t *testing.T) {
	e := newEnv(t)
	for range 1005 {
		e.eng.events.add(eventsOf(kindRoute, 1)...)
	}

	ch, _, err := e.eng.Subscribe(t.Context(), e.eng.Boot(), 2)
	require.NoError(t, err)

	got := shown(until(t, e.eng, ch))
	require.Equal(t, "gap 3-5 3 warn", got[0], "what is no longer kept, of a level not known")
	require.Equal(t, "event route 6", got[1])
	require.Equal(t, "gap 38-1005 968 info", got[len(got)-1])
	require.Len(t, got, 34)
}

func TestAnotherBootGetsAResetFirstAndTheStreamGoesOn(t *testing.T) {
	e := newEnv(t)
	e.eng.events.add(eventsOf(kindRoute, 3)...)

	ch, hello, err := e.eng.Subscribe(t.Context(), "0123456789abcdef", 2)
	require.NoError(t, err)
	require.Equal(t, uint64(3), hello.Seq)

	require.Equal(t, Notice{Kind: NoticeReset, Reason: "boot changed"}, receive(t, ch))
	e.eng.events.add(eventsOf(kindAdmin, 1)...)
	require.Equal(t, []string{"event admin 4"}, shown(until(t, e.eng, ch)))
}

func TestTheSeventeenthSubscriberIsRefused(t *testing.T) {
	e := newEnv(t)
	first, stop := context.WithCancel(t.Context())
	ch, _, err := e.eng.Subscribe(first, "", 0)
	require.NoError(t, err)
	for range 15 {
		listen(t, e.eng)
	}

	_, _, err = e.eng.Subscribe(t.Context(), "", 0)
	require.ErrorIs(t, err, ErrBusy)
	require.EqualError(t, err, "16 streams are open already; try again later")

	stop()
	for range ch {
	}
	listen(t, e.eng)
}

func TestTheFirstCycleOverAThousandRoutesIsThirtyTwoEventsAndAGap(t *testing.T) {
	e := newEnv(t)
	e.inv.set(snapshot(many(1000)...))
	ch := listen(t, e.eng)

	e.cycle()

	got := until(t, e.eng, ch)
	events, err := e.eng.QueryEvents(EventQuery{History: true, Limit: MaxEventLimit})
	require.NoError(t, err)
	kinds, high, routes := map[uint64]string{}, 0, 0
	for _, ev := range events {
		kinds[ev.Seq] = ev.Kind
		if highVolume(ev.Kind) {
			high++
		}
		if ev.Kind == kindRoute {
			routes++
		}
	}
	require.Equal(t, 1000, routes, "one route event for every route")
	singles, gaps := 0, 0
	for _, n := range got {
		switch {
		case n.Kind == NoticeEvent && highVolume(n.Event.Kind):
			singles++
		case n.Kind == NoticeGap:
			gaps++
			require.Equal(t, high-32, n.Gap.Count)
		}
	}
	require.Equal(t, 32, singles)
	require.Equal(t, 1, gaps)
	requireCovered(t, got, uint64(len(events)), kinds)
}

func TestAdminEventsSayWhoAsked(t *testing.T) {
	e := newEnv(t)

	_, err := e.eng.Apply(WithActor(t.Context(), "alice@pve (ticket)"), false, "")
	require.NoError(t, err)

	events := e.eng.Events(time.Time{})
	require.Len(t, events, 1)
	require.Equal(t, kindAdmin, events[0].Kind)
	require.Equal(t, "alice@pve (ticket)", events[0].Actor)
	require.Empty(t, ActorOf(t.Context()))
}
