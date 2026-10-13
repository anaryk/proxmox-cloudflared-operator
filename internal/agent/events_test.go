package agent

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

var (
	clientEpoch1 = agentproto.Epoch{InstallID: testInstall, Generation: 1, Nonce: "n1"}
	clientEpoch2 = agentproto.Epoch{InstallID: testInstall, Generation: 2, Nonce: "n2"}
)

func cursors(t *testing.T, events []json.RawMessage) []uint64 {
	t.Helper()
	out := make([]uint64, len(events))
	for i, raw := range events {
		var ev eventWire
		require.NoError(t, json.Unmarshal(raw, &ev))
		out[i] = ev.Cursor
	}
	return out
}

func addEvents(l *EventLog, c *clock, n int) {
	for i := range n {
		l.Add(Event{At: c.now(), Level: levelInfo, Kind: kindNode, Subject: testNode, Message: "event " + strconv.Itoa(i)})
	}
}

func TestTheEventCursorMovesOnlyOnTheAcknowledgement(t *testing.T) {
	c := &clock{t: t0}
	l := NewEventLog(c.now)
	l.Ack(clientEpoch1, 0) // the first controller heard, before any event
	addEvents(l, c, 3)

	events, cursor, resend := l.Batch()
	require.Equal(t, []uint64{1, 2, 3}, cursors(t, events))
	require.Equal(t, uint64(3), cursor)
	require.Zero(t, resend)

	again, _, _ := l.Batch()
	require.Equal(t, events, again, "a status lost on its way is sent again")

	l.Ack(clientEpoch1, 2)
	events, cursor, _ = l.Batch()
	require.Equal(t, []uint64{3}, cursors(t, events))
	require.Equal(t, uint64(3), cursor)

	t.Run("an acknowledgement of what was never sent moves nothing", func(t *testing.T) {
		l.Ack(clientEpoch1, 99)
		events, _, _ := l.Batch()
		require.Equal(t, []uint64{3}, cursors(t, events))
	})
	t.Run("nor one that goes back", func(t *testing.T) {
		l.Ack(clientEpoch1, 3)
		l.Ack(clientEpoch1, 1)
		events, cursor, _ := l.Batch()
		require.Empty(t, events)
		require.Equal(t, uint64(3), cursor, "the cursor of nothing new is the acknowledged one")
	})
}

func TestABatchHoldsAtMostFiveHundredEvents(t *testing.T) {
	c := &clock{t: t0}
	l := NewEventLog(c.now)
	l.Ack(clientEpoch1, 0)
	addEvents(l, c, 1200)
	events, cursor, _ := l.Batch()
	require.Len(t, events, agentproto.MaxEventBatch)
	require.Equal(t, uint64(500), cursor)
	l.Ack(clientEpoch1, cursor)
	events, cursor, _ = l.Batch()
	require.Len(t, events, agentproto.MaxEventBatch)
	require.Equal(t, uint64(1000), cursor)
}

func TestANewEpochGetsTheLastDayAgain(t *testing.T) {
	c := &clock{t: t0}
	l := NewEventLog(c.now)
	l.Ack(clientEpoch1, 0)
	addEvents(l, c, 2) // older than a day by the time the epoch changes
	c.advance(eventsResent + time.Minute)
	addEvents(l, c, 3)
	_, cursor, _ := l.Batch()
	l.Ack(clientEpoch1, cursor)
	events, _, _ := l.Batch()
	require.Empty(t, events)

	l.Ack(clientEpoch2, 0)
	events, cursor, resend := l.Batch()
	require.Equal(t, []uint64{3, 4, 5}, cursors(t, events), "the last 24 hours, and nothing older")
	require.Equal(t, 3, resend)
	l.Ack(clientEpoch2, cursor)
	_, _, resend = l.Batch()
	require.Zero(t, resend, "the resend ends once acknowledged")

	t.Run("the same epoch again resends nothing", func(t *testing.T) {
		l.Ack(clientEpoch2, cursor)
		events, _, _ := l.Batch()
		require.Empty(t, events)
	})
}

func TestAResendHoldsTheNewestTenThousandAndEndsWhenRefused(t *testing.T) {
	c := &clock{t: t0}
	l := NewEventLog(c.now)
	l.Ack(clientEpoch1, 0)
	addEvents(l, c, 12000)
	_, cursor, _ := l.Batch()
	l.Ack(clientEpoch1, cursor)

	l.Ack(clientEpoch2, 0)
	events, _, resend := l.Batch()
	require.Equal(t, agentproto.MaxResend, resend)
	require.Equal(t, uint64(2001), cursors(t, events)[0], "the newest ten thousand")

	addEvents(l, c, 1) // cursor 12001, after the resend
	l.ResendRefused()
	events, _, resend = l.Batch()
	require.Zero(t, resend)
	require.Equal(t, []uint64{12001}, cursors(t, events), "what the controller refused is given up, what came after goes")
}

func TestTheLogKeepsADayAndABound(t *testing.T) {
	c := &clock{t: t0}
	l := NewEventLog(c.now)
	l.Ack(clientEpoch1, 0)
	addEvents(l, c, eventsKept+10)
	l.Ack(clientEpoch2, 0)
	_, _, resend := l.Batch()
	require.Equal(t, agentproto.MaxResend, resend)
	l.mu.Lock()
	require.Len(t, l.events, eventsKept)
	l.mu.Unlock()

	c.advance(eventsResent + time.Second)
	addEvents(l, c, 1)
	l.mu.Lock()
	defer l.mu.Unlock()
	require.Len(t, l.events, 1)
}

func TestTheFirstControllerHeardGetsWhatIsPendingOnce(t *testing.T) {
	c := &clock{t: t0}
	l := NewEventLog(c.now)
	addEvents(l, c, 2)
	_, cursor, _ := l.Batch()
	l.Ack(clientEpoch1, cursor)
	events, _, resend := l.Batch()
	require.Empty(t, events, "what it took is not sent again")
	require.Zero(t, resend)
}

func TestSkipGivesUpABatch(t *testing.T) {
	c := &clock{t: t0}
	l := NewEventLog(c.now)
	l.Ack(clientEpoch1, 0)
	addEvents(l, c, 3)
	_, cursor, _ := l.Batch()
	l.Skip(cursor)
	events, _, _ := l.Batch()
	require.Empty(t, events)
}
