package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// grid is the whole table: a row per state, a column per event, the state the
// node is in afterwards, or a dash where the event is not a transition of it.
const grid = `
          joined   admitted heard    silent   offline  online   drain    undrain  forget   contest   returned
none      joining  -        -        -        -        -        -        -        -        contested -
joining   joining  active   joining  unknown  down     joining  -        -        removed  contested joining
active    -        active   active   unknown  down     active   draining active   removed  contested joining
draining  -        -        draining unknown  down     draining draining active   removed  contested joining
unknown   -        -        joining  unknown  down     unknown  -        -        removed  contested joining
down      -        -        down     down     down     unknown  -        -        removed  contested down
contested -        -        -        contested contested contested -       -        removed  contested -
removed   joining  -        -        removed  removed  removed  -        -        removed  contested -
`

func readGrid(t *testing.T) (events []Event, rows map[agentproto.NodeState]map[Event]string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(grid), "\n")
	for _, h := range strings.Fields(lines[0]) {
		events = append(events, Event(h))
	}
	rows = map[agentproto.NodeState]map[Event]string{}
	for _, line := range lines[1:] {
		cols := strings.Fields(line)
		state := agentproto.NodeState(cols[0])
		if cols[0] == "none" {
			state = NoEntry
		}
		require.Len(t, cols[1:], len(events), "row %s", cols[0])
		rows[state] = map[Event]string{}
		for i, ev := range events {
			rows[state][ev] = cols[1+i]
		}
	}
	return events, rows
}

func TestEveryStateTakesEveryEventAsTheTableSays(t *testing.T) {
	events, rows := readGrid(t)
	require.Equal(t, []Event{
		EventJoined, EventAdmitted, EventHeard, EventSilent, EventOffline, EventOnline,
		EventDrain, EventUndrain, EventForget, EventContest, EventReturned,
	}, events)
	require.Equal(t, []agentproto.NodeState{
		NoEntry, agentproto.Joining, agentproto.Active, agentproto.Draining, agentproto.Unknown,
		agentproto.Down, agentproto.Contested, agentproto.Removed,
	}, statesInOrder(rows))

	for state, row := range rows {
		for _, ev := range events {
			t.Run(string(state)+"/"+string(ev), func(t *testing.T) {
				got, err := Next(state, ev)
				if row[ev] == "-" {
					require.ErrorIs(t, err, ErrNoTransition)
					require.Empty(t, got)
					return
				}
				require.NoError(t, err)
				require.Equal(t, agentproto.NodeState(row[ev]), got)
			})
		}
	}
}

func statesInOrder(rows map[agentproto.NodeState]map[Event]string) []agentproto.NodeState {
	var out []agentproto.NodeState
	for _, s := range []agentproto.NodeState{
		NoEntry, agentproto.Joining, agentproto.Active, agentproto.Draining, agentproto.Unknown,
		agentproto.Down, agentproto.Contested, agentproto.Removed,
	} {
		if _, ok := rows[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

func TestTheCodeKnowsNoStateOrEventTheTableLeavesOut(t *testing.T) {
	events, rows := readGrid(t)
	for state, evs := range transitions {
		require.Contains(t, rows, state)
		for ev := range evs {
			require.Contains(t, events, ev)
		}
	}
}

func TestAStateOrEventNobodyDefinedIsNotATransition(t *testing.T) {
	_, err := Next("standby", EventHeard)
	require.ErrorIs(t, err, ErrNoTransition)
	require.ErrorContains(t, err, `"standby"`)

	_, err = Next(agentproto.Active, "wake")
	require.ErrorIs(t, err, ErrNoTransition)
	require.ErrorContains(t, err, `"wake"`)
}

func TestTheRefusalNamesTheStateAndTheEvent(t *testing.T) {
	_, err := Next(agentproto.Removed, EventHeard)
	require.EqualError(t, err, `a removed node cannot take "heard": not a transition of the node registry`)

	_, err = Next(NoEntry, EventDrain)
	require.EqualError(t, err, `a node without an entry cannot take "drain": not a transition of the node registry`)
}

func TestWhatThePathsOfTheDocumentationDo(t *testing.T) {
	walk := func(t *testing.T, state agentproto.NodeState, evs ...Event) agentproto.NodeState {
		t.Helper()
		for _, ev := range evs {
			var err error
			state, err = Next(state, ev)
			require.NoError(t, err, "after %v", ev)
		}
		return state
	}

	t.Run("a node is enrolled, admitted and serves", func(t *testing.T) {
		require.Equal(t, agentproto.Active, walk(t, NoEntry, EventJoined, EventHeard, EventAdmitted))
	})
	t.Run("a rebooted node is admitted again", func(t *testing.T) {
		require.Equal(t, agentproto.Joining, walk(t, agentproto.Active, EventReturned))
		require.Equal(t, agentproto.Active, walk(t, agentproto.Active, EventReturned, EventAdmitted))
	})
	t.Run("a node that went silent comes back through joining", func(t *testing.T) {
		require.Equal(t, agentproto.Unknown, walk(t, agentproto.Active, EventSilent))
		require.Equal(t, agentproto.Joining, walk(t, agentproto.Active, EventSilent, EventHeard))
	})
	t.Run("a node that went down comes back through unknown and joining", func(t *testing.T) {
		require.Equal(t, agentproto.Down, walk(t, agentproto.Active, EventOffline))
		require.Equal(t, agentproto.Down, walk(t, agentproto.Active, EventOffline, EventHeard))
		require.Equal(t, agentproto.Unknown, walk(t, agentproto.Active, EventOffline, EventOnline))
		require.Equal(t, agentproto.Joining, walk(t, agentproto.Active, EventOffline, EventOnline, EventHeard))
	})
	t.Run("a drain is undone, and a node that is not draining stays as it is", func(t *testing.T) {
		require.Equal(t, agentproto.Active, walk(t, agentproto.Active, EventDrain, EventUndrain))
		require.Equal(t, agentproto.Active, walk(t, agentproto.Active, EventUndrain))
	})
	t.Run("a contested node leaves only by being forgotten, and is then enrolled again", func(t *testing.T) {
		require.Equal(t, agentproto.Contested, walk(t, agentproto.Active, EventContest, EventSilent, EventOffline, EventOnline))
		require.Equal(t, agentproto.Removed, walk(t, agentproto.Contested, EventForget))
		require.Equal(t, agentproto.Joining, walk(t, agentproto.Contested, EventForget, EventJoined))
	})
	t.Run("forgetting is safe to repeat", func(t *testing.T) {
		require.Equal(t, agentproto.Removed, walk(t, agentproto.Active, EventForget, EventForget))
	})
	t.Run("a token used in a way that does not match contests a node that has no entry yet", func(t *testing.T) {
		require.Equal(t, agentproto.Contested, walk(t, NoEntry, EventContest))
	})
	t.Run("a forgotten node's new token misused contests it", func(t *testing.T) {
		require.Equal(t, agentproto.Contested, walk(t, agentproto.Active, EventForget, EventContest))
		require.Equal(t, agentproto.Joining, walk(t, agentproto.Active, EventForget, EventContest, EventForget, EventJoined))
	})
	t.Run("what the agent of a contested or removed node says is no transition", func(t *testing.T) {
		for _, state := range []agentproto.NodeState{agentproto.Contested, agentproto.Removed} {
			for _, ev := range []Event{EventHeard, EventReturned} {
				_, err := Next(state, ev)
				require.ErrorIs(t, err, ErrNoTransition, "%s %s", state, ev)
			}
		}
	})
}
