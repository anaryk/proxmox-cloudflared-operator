package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTheServicePrefixLoadedAgainIsAnEventAndAProblemOfTheNextCycle(t *testing.T) {
	e := published(t)

	e.eng.NoteNet(NetCheck{Reloaded: "the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing"})

	require.Equal(t, []string{"the service-prefix route or table was changed outside pco and was loaded again"},
		egressEvents(e, t0.Add(-time.Hour)))
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Equal(t, []string{"the service-prefix route or table was changed outside pco and was loaded again " +
		"(the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing)"}, st.Problems)
	require.Empty(t, st.Hold)

	e.clock.advance(20 * time.Second)
	require.Empty(t, e.cycle().Problems)
}

// A route or table that cannot be loaded again is a problem for as long as
// that stands, and holds no tunnel run: the egress table confines the
// connectors whatever happens to the route.
func TestTheServicePrefixNotLoadedAgainIsAProblemThatHoldsNothing(t *testing.T) {
	e := published(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :9090")))
	writes := len(e.writes())
	failed := NetCheck{NotKept: "adding the dummy device pco0: operation not permitted"}

	e.eng.NoteNet(failed)
	e.eng.NoteNet(failed)

	line := "the service-prefix route or table was changed outside pco and could not be loaded again: " +
		"adding the dummy device pco0: operation not permitted"
	require.Equal(t, []string{line}, egressEvents(e, t0.Add(-time.Hour)), "once")
	for range 2 {
		e.clock.advance(20 * time.Second)
		require.Equal(t, []string{line}, e.cycle().Problems)
	}
	require.Greater(t, len(e.writes()), writes, "the tunnel run went on")

	e.eng.NoteNet(NetCheck{})
	e.clock.advance(20 * time.Second)
	require.Empty(t, e.cycle().Problems)
}

func TestTheServicePrefixThatCannotBeCheckedIsAProblemOfTheNextCycle(t *testing.T) {
	e := published(t)

	e.eng.NoteNet(NetCheck{Failed: "listing the table inet pco_net: signal: killed"})

	e.clock.advance(20 * time.Second)
	require.Equal(t, []string{"checking the service-prefix route and table: listing the table inet pco_net: signal: killed"},
		e.cycle().Problems)
	require.Empty(t, egressEvents(e, t0.Add(-time.Hour)))
}
