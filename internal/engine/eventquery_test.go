package engine

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func seqsOf(events []Event) []uint64 {
	out := []uint64{}
	for _, ev := range events {
		out = append(out, ev.Seq)
	}
	return out
}

func span(from, to uint64) []uint64 {
	out := []uint64{}
	for seq := from; seq <= to; seq++ {
		out = append(out, seq)
	}
	return out
}

func query(t *testing.T, e *Engine, q EventQuery) []Event {
	t.Helper()
	events, err := e.QueryEvents(q)
	require.NoError(t, err)
	return events
}

// writeLog writes events as the event log has them, one JSON line each, and
// the lines given as they are.
func writeLog(t *testing.T, path string, parts ...any) {
	t.Helper()
	var buf bytes.Buffer
	for _, p := range parts {
		switch p := p.(type) {
		case Event:
			b, err := json.Marshal(p)
			require.NoError(t, err)
			buf.Write(append(b, '\n'))
		case string:
			buf.WriteString(p)
		}
	}
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
}

func TestEventsAfterASeqOfThisBoot(t *testing.T) {
	e := newEnv(t)
	e.eng.events.add(eventsOf(kindRoute, 10)...)

	require.Equal(t, span(8, 10), seqsOf(query(t, e.eng, EventQuery{After: 7})), "a seq of this boot when none is named")
	require.Equal(t, span(8, 10), seqsOf(query(t, e.eng, EventQuery{After: 7, Boot: e.eng.Boot()})))
	require.Equal(t, span(1, 10), seqsOf(query(t, e.eng, EventQuery{After: 7, Boot: "0123456789abcdef"})), "of another boot: all of them")
	require.Empty(t, query(t, e.eng, EventQuery{After: 10}))
	require.NotNil(t, query(t, e.eng, EventQuery{After: 10}), "an empty list")
}

func TestEventFiltersCombine(t *testing.T) {
	e := newEnv(t)
	at := t0.Add(time.Minute)
	e.eng.events.add(
		Event{At: t0, Level: levelInfo, Kind: kindRoute, Route: "a.example.com", Guest: "qemu/101", Account: "acc1"},
		Event{At: at, Level: levelWarn, Kind: kindRoute, Route: "b.example.com", Guest: "qemu/102", Account: "acc1"},
		Event{At: at, Level: levelInfo, Kind: kindAction, Route: "a.example.com", Account: "acc2"},
		Event{At: at, Level: levelInfo, Kind: kindAction, Tunnel: "pco-abc123", Account: "acc2"},
		Event{At: at, Level: levelError, Kind: kindWriter},
		Event{At: at, Level: levelInfo, Kind: kindClaim, Route: "a.example.com", Guest: "qemu/101"},
	)

	for _, tt := range []struct {
		name string
		q    EventQuery
		want []uint64
	}{
		{"nothing asked", EventQuery{}, span(1, 6)},
		{"one route", EventQuery{Route: []string{"a.example.com"}}, []uint64{1, 3, 6}},
		{"either route", EventQuery{Route: []string{"b.example.com", "a.example.com"}}, []uint64{1, 2, 3, 6}},
		{"a route and a kind", EventQuery{Route: []string{"a.example.com"}, Kind: []string{kindAction, kindClaim}}, []uint64{3, 6}},
		{"a guest", EventQuery{Guest: []string{"qemu/101"}}, []uint64{1, 6}},
		{"a tunnel", EventQuery{Tunnel: []string{"pco-abc123"}}, []uint64{4}},
		{"an account", EventQuery{Account: []string{"acc2"}}, []uint64{3, 4}},
		{"an account and a level", EventQuery{Account: []string{"acc1", "acc2"}, Level: []string{levelWarn}}, []uint64{2}},
		{"a level", EventQuery{Level: []string{levelError, levelWarn}}, []uint64{2, 5}},
		{"since", EventQuery{Since: t0, Kind: []string{kindRoute}}, []uint64{2}},
		{"until, the end included", EventQuery{Until: t0, Kind: []string{kindRoute}}, []uint64{1}},
		{"since and until", EventQuery{Since: t0.Add(-time.Second), Until: at.Add(-time.Second)}, []uint64{1}},
		{"until before every event", EventQuery{Until: t0.Add(-time.Second)}, []uint64{}},
		{"an empty value matches nothing", EventQuery{Account: []string{""}}, []uint64{}},
		{"the case of a value", EventQuery{Account: []string{"ACC1"}}, []uint64{}},
		{"a filter and after", EventQuery{Route: []string{"a.example.com"}, After: 3}, []uint64{6}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, seqsOf(query(t, e.eng, tt.q)))
		})
	}
}

func TestALimitKeepsTheNewest(t *testing.T) {
	e := newEnv(t)
	for range 60 {
		e.eng.events.add(eventsOf(kindRoute, 100)...)
	}

	require.Equal(t, span(5001, 6000), seqsOf(query(t, e.eng, EventQuery{})), "the ring, 1000 by default")
	require.Equal(t, span(5991, 6000), seqsOf(query(t, e.eng, EventQuery{Limit: 10})))
	require.Equal(t, span(4001, 6000), seqsOf(query(t, e.eng, EventQuery{Limit: 2000, History: true})))
	require.Equal(t, span(1001, 6000), seqsOf(query(t, e.eng, EventQuery{Limit: 6000, History: true})), "at most 5000")
	require.Equal(t, span(5991, 6000), seqsOf(query(t, e.eng, EventQuery{Limit: 10, History: true})))
}

// A range in the past gets the newest events of the range, not the newest
// events of all that happen to be in it.
func TestALimitCountsBackFromUntil(t *testing.T) {
	e := newEnv(t)
	for i := range 3000 {
		e.eng.events.add(Event{At: t0.Add(time.Duration(i) * time.Second), Level: levelInfo, Kind: kindRoute, Message: "something changed"})
	}

	got := query(t, e.eng, EventQuery{Until: t0.Add(999 * time.Second), Limit: 10, History: true})
	require.Equal(t, span(991, 1000), seqsOf(got))
}

func TestHistoryReadsBothFilesAndTheRing(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.paths.Local, eventsFile)
	const old = "aaaaaaaaaaaaaaaa"
	event := func(seq uint64, msg string) Event {
		return Event{Seq: seq, Boot: old, At: t0, Level: levelInfo, Kind: kindAdmin, Message: msg}
	}
	writeLog(t, path+".1", event(1, "first"), "not an event\n", event(2, "second"), "{\"seq\":3,\"kind\"\n")
	writeLog(t, path, event(3, "third"), "\n", event(4, "fourth"))
	e.eng.events.add(Event{At: t0, Level: levelInfo, Kind: kindAdmin, Message: "now"})
	e.eng.events.add(Event{At: t0, Level: levelInfo, Kind: kindAdmin, Message: "then"})

	messages := func(events []Event) []string {
		out := []string{}
		for _, ev := range events {
			out = append(out, ev.Message)
		}
		return out
	}
	require.Equal(t, []string{"now", "then"}, messages(query(t, e.eng, EventQuery{})), "the ring alone")
	got := query(t, e.eng, EventQuery{History: true})
	require.Equal(t, []string{"first", "second", "third", "fourth", "now", "then"}, messages(got), "the broken lines skipped, the ring's own not twice")
	require.Equal(t, e.eng.Boot(), got[4].Boot)
	require.Equal(t, []string{"fourth", "now", "then"}, messages(query(t, e.eng, EventQuery{History: true, Limit: 3})))
	require.Equal(t, []string{"then"}, messages(query(t, e.eng, EventQuery{History: true, After: 1})),
		"after a seq of this boot: only its events")

	again := e.newEngine()
	require.Equal(t, []string{"first", "second", "third", "fourth", "now", "then"}, messages(query(t, again, EventQuery{History: true})),
		"the events of the process before")
}

func TestHistoryReadsTheNewestTenMegabytes(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.paths.Local, eventsFile)
	junk := string(bytes.Repeat([]byte("x"), 1023)) + "\n"
	lines := func(mib int) string { return string(bytes.Repeat([]byte(junk), mib<<10)) }
	event := func(seq uint64, msg string) Event {
		return Event{Seq: seq, Boot: "aaaaaaaaaaaaaaaa", At: t0, Level: levelInfo, Kind: kindAdmin, Message: msg}
	}
	writeLog(t, path+".1", event(1, "too old"), lines(6), event(2, "old"))
	writeLog(t, path, lines(5), event(3, "new"))

	got := query(t, e.eng, EventQuery{History: true})

	require.Equal(t, []uint64{2, 3}, seqsOf(got))
}
