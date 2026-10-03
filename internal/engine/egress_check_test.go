package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// egressEvents returns the messages of the egress events after t.
func egressEvents(e *env, after time.Time) []string {
	var out []string
	for _, ev := range e.eng.Events(after) {
		if ev.Kind == kindEgress {
			out = append(out, ev.Message)
		}
	}
	return out
}

func TestTheStateShowsTheEgressFilterAsTheLastCheckFoundIt(t *testing.T) {
	e := published(t)
	off := EgressView{State: EgressOff, Since: t0.Add(-time.Hour)}

	e.eng.NoteEgress(EgressCheck{View: off})

	require.Equal(t, off, e.eng.State().Egress, "at once, without a cycle")
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Equal(t, off, st.Egress)
	require.Empty(t, st.Problems, "the warning is the state's own")

	e.eng.NoteEgress(EgressCheck{View: off})
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})

	require.Equal(t, EgressView{State: EgressOn}, e.eng.State().Egress)
	require.Equal(t, []string{
		"the egress filter was switched off (since 2026-10-01T11:00:00Z); the connectors are not confined until pco egress on",
		"the egress filter was switched on",
	}, egressEvents(e, t0.Add(-time.Hour)))
}

func TestTheFirstCheckOfAFilterThatIsOnIsNoEvent(t *testing.T) {
	e := newEnv(t)

	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})

	require.Empty(t, egressEvents(e, t0.Add(-time.Hour)))
}

func TestATableLoadedAgainIsAnEventAndAProblemOfTheNextCycle(t *testing.T) {
	e := published(t)

	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}, Reloaded: "the egress table is not loaded"})

	require.Equal(t, []string{"the egress table was changed or removed outside pco and was loaded again"}, egressEvents(e, t0.Add(-time.Hour)))
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Equal(t, []string{"the egress table was changed or removed outside pco and was loaded again (the egress table is not loaded)"}, st.Problems)
	require.Empty(t, st.Hold, "the tunnel run went on")

	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.Empty(t, st.Problems, "the table is in place again")
}

func TestATableThatCouldNotBeLoadedAgainHoldsTheTunnelRuns(t *testing.T) {
	e := published(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	failed := EgressCheck{View: EgressView{State: EgressNotLoaded}, Failed: "nft -f -: exit status 1: Error: Could not process rule", Holds: true}
	writes := len(e.writes())

	e.eng.NoteEgress(failed)
	e.eng.NoteEgress(failed)

	require.Equal(t, EgressView{State: EgressNotLoaded}, e.eng.State().Egress)
	line := "the egress table was changed or removed outside pco and could not be loaded again: " +
		"nft -f -: exit status 1: Error: Could not process rule; no tunnel configuration is written until it is"
	require.Equal(t, []string{line}, egressEvents(e, t0.Add(-time.Hour)), "once")
	for range 2 {
		e.clock.advance(20 * time.Second)
		st := e.cycle()

		require.Equal(t, []string{line}, st.Problems, "for as long as it stands")
		require.Contains(t, actionKinds(st), "put-config pco-abc123 held: the egress filter could not be set")
		require.Len(t, e.writes(), writes)
	}

	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Empty(t, st.Problems)
	require.Greater(t, len(e.writes()), writes)
}

// A table found changed again while pco waits to load it: the state says
// so, and a hold that stands stays until a check finds the table in place.
func TestATableFoundChangedAgainKeepsTheHold(t *testing.T) {
	e := published(t)
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressNotLoaded}, Failed: "nft -f -: exit status 1", Holds: true})

	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressChanged}})

	require.Equal(t, EgressView{State: EgressChanged}, e.eng.State().Egress)
	require.NotEmpty(t, e.eng.egressFault())
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})
	require.Empty(t, e.eng.egressFault())

	t.Run("and it holds nothing by itself", func(t *testing.T) {
		e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressNotLoaded}})
		require.Empty(t, e.eng.egressFault())
	})
}

// A check that comes while a cycle runs is what the state shows after it.
func TestACheckDuringACycleOutlastsIt(t *testing.T) {
	e := published(t)
	off := EgressView{State: EgressOff, Since: t0}
	var once sync.Once
	e.res.hook(func() { once.Do(func() { e.eng.NoteEgress(EgressCheck{View: off}) }) })

	e.clock.advance(20 * time.Second)
	e.cycle()

	require.Equal(t, off, e.eng.State().Egress)
}

// A table that cannot be listed says nothing about the filter: a problem line
// of the next cycle, and nothing held.
func TestATableThatCannotBeListedIsAProblemOfTheNextCycle(t *testing.T) {
	e := published(t)
	e.eng.NoteEgress(EgressCheck{View: EgressView{State: EgressOn}})

	e.eng.NoteEgress(EgressCheck{Failed: "listing the egress table: nft: no answer"})

	require.Equal(t, EgressView{State: EgressOn}, e.eng.State().Egress)
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Equal(t, []string{"checking the egress table: listing the egress table: nft: no answer"}, st.Problems)
	require.Empty(t, egressEvents(e, t0.Add(-time.Hour)))
}
