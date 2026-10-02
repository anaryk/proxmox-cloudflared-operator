package engine

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

var unavailable = &cfapi.Error{Status: http.StatusServiceUnavailable, Message: "Service Unavailable"}

// inTheWay is a world with a record of someone else at www.example.com and
// one of this install at api.example.com that lost its marker.
func inTheWay(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080", "api.example.com -> :8081")))
	e.cf.SeedRecord(testZone, cfapi.Record{Type: "A", Name: "www.example.com", Content: "192.0.2.1"})
	e.enforce()
	e.cycle()
	for _, rec := range e.records() {
		if rec.Name == "api.example.com" {
			rec.Comment = ""
			_, err := e.cf.UpdateRecord(t.Context(), testZone, rec)
			require.NoError(t, err)
		}
	}
	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.Len(t, st.Conflicts, 2)
	require.Equal(t, []string{"api.example.com"}, st.Lost)
	return e
}

func conflictEvents(e *env, after uint64) []Event {
	var out []Event
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Seq > after && ev.Kind == kindConflict {
			out = append(out, ev)
		}
	}
	return out
}

func lastSeq(e *env) uint64 {
	events := e.eng.Events(time.Time{})
	if len(events) == 0 {
		return 0
	}
	return events[len(events)-1].Seq
}

func TestAFailedRecordListingKeepsWhatItHid(t *testing.T) {
	e := inTheWay(t)
	seq := lastSeq(e)

	e.cf.FailNext("dns.read", 1, unavailable)
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.True(t, hasProblem(st, "listing the records of this install"), "%v", st.Problems)
	require.Len(t, st.Conflicts, 2, "what could not be listed is not gone")
	require.Equal(t, []string{"api.example.com"}, st.Lost)
	require.Empty(t, conflictEvents(e, seq), "nothing cleared")
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"), "the conflict can still be adopted")
	require.NoError(t, e.eng.Adopt(t.Context(), "api.example.com"))
}

func TestAConflictGoesOnlyWhenAListingShowsItGone(t *testing.T) {
	e := inTheWay(t)
	e.cf.FailNext("dns.read", 1, unavailable)
	e.clock.advance(10 * time.Second)
	e.cycle()
	seq := lastSeq(e)

	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.Len(t, st.Conflicts, 2, "the records are still there")
	require.Empty(t, conflictEvents(e, seq))

	for _, rec := range e.records() {
		if rec.Name == "www.example.com" {
			require.NoError(t, e.cf.DeleteRecord(t.Context(), testZone, rec.ID))
		}
	}
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.Len(t, st.Conflicts, 1)
	require.Equal(t, "api.example.com", st.Conflicts[0].Name)
	events := conflictEvents(e, seq)
	require.Len(t, events, 1)
	require.Equal(t, "www.example.com", events[0].Subject)
	require.Contains(t, events[0].Message, "no longer conflicts")
}
