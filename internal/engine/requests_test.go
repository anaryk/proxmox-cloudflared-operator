package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func adminEvents(e *env, since time.Time) []string {
	var out []string
	for _, ev := range e.eng.Events(since) {
		if ev.Kind == "admin" {
			out = append(out, ev.Subject+": "+ev.Message)
		}
	}
	return out
}

func eventsContaining(e *env, part string) int {
	n := 0
	for _, ev := range e.eng.Events(time.Time{}) {
		if strings.Contains(ev.Message, part) {
			n++
		}
	}
	return n
}

// Reproduced: the DNS run stops at its start, as the writer was replaced
// after the tunnel run. The confirmation is not used up.
func TestAConfirmationWaitsForADNSRunThatDecided(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	var once sync.Once
	e.conn.onEnsure = func() { once.Do(func() { require.NoError(t, e.store.SaveWriter(takeover)) }) }

	e.apply(true)
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Equal(t, "stale", st.WriterVerdict)
	require.NotNil(t, e.eng.confirm, "a DNS run that stopped at its start decided nothing")
	e.clock.advance(10 * time.Second)
	e.conn.onEnsure = nil
	e.cycle()
	require.Nil(t, e.eng.confirm, "used by the run that decided")
	require.Equal(t, 1, eventsContaining(e, "confirmation waits"), "said once when it started to wait")
	require.Equal(t, 1, eventsContaining(e, "deletes confirmed:"))
}

func TestAConfirmationWaitsWhileTheTombstonesCannotBeRead(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	path := filepath.Join(e.paths.Cluster, "meta", "tombstones.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))

	e.apply(true)
	for range 3 {
		e.clock.advance(10 * time.Second)
		e.cycle()
	}

	require.NotNil(t, e.eng.confirm)
	require.Equal(t, 1, eventsContaining(e, "confirmation waits"))

	require.NoError(t, os.Remove(path))
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Nil(t, e.eng.confirm)
}

func TestAConfirmationExpires(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	path := filepath.Join(e.paths.Cluster, "meta", "tombstones.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	e.apply(true)

	e.clock.advance(4 * time.Minute)
	e.cycle()
	require.NotNil(t, e.eng.confirm)

	e.clock.advance(time.Minute)
	e.cycle()

	require.Nil(t, e.eng.confirm)
	require.Contains(t, adminEvents(e, t0.Add(4*time.Minute)), ": confirmation expired before it could be applied")

	require.NoError(t, os.Remove(path))
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Zero(t, eventsContaining(e, "deletes confirmed:"), "an expired confirmation lands on nothing")
}

// conflicted is an engine in enforce mode with a record of someone else at
// www.example.com.
func conflicted(t *testing.T) (*env, cfapi.Record) {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	foreign := e.cf.SeedRecord(testZone, cfapi.Record{Type: "A", Name: "www.example.com", Content: "192.0.2.10", TTL: 300, Comment: "by hand"})
	st := e.cycle()
	require.Len(t, st.Conflicts, 1)
	return e, foreign
}

func TestAdoptNeedsAConflictOrALostName(t *testing.T) {
	e, _ := conflicted(t)

	require.ErrorIs(t, e.eng.Adopt(t.Context(), "other.example.com"), ErrNotFound)
	require.NoError(t, e.eng.Adopt(t.Context(), "WWW.example.com."))
	require.Contains(t, e.eng.adopt, "www.example.com")
}

func TestAnAdoptionWaitsForAReadyConnector(t *testing.T) {
	e, _ := conflicted(t)
	id := e.tunnels()[0].ID
	e.conn.setReady(id, false)
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))

	for range 3 {
		e.clock.advance(10 * time.Second)
		e.cycle()
	}

	require.Equal(t, "A", e.records()[0].Type, "not taken over while the connector cannot serve it")
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com waits"))

	e.conn.setReady(id, true)
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, "CNAME", e.records()[0].Type)
	require.Empty(t, e.eng.adopt)
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com applied"))
}

func TestAnAdoptionExpires(t *testing.T) {
	e, _ := conflicted(t)
	e.conn.setReady(e.tunnels()[0].ID, false)
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))

	e.clock.advance(5 * time.Minute)
	e.cycle()

	require.Empty(t, e.eng.adopt)
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com expired before it could be applied"))
}

func TestAnAdoptionStoresTheRecordFirst(t *testing.T) {
	e, foreign := conflicted(t)
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Empty(t, st.Conflicts)
	recs := e.records()
	require.Len(t, recs, 1)
	require.Equal(t, "CNAME", recs[0].Type)
	b, err := os.ReadFile(filepath.Join(e.paths.Cluster, "adopted.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(b), `"id":"`+foreign.ID+`"`)
	require.Contains(t, string(b), `"zone":"example.com"`)
}

func TestAnAdoptionIsHeldWhenItsRecordCannotBeStored(t *testing.T) {
	e, _ := conflicted(t)
	// A directory where the log should be makes the write fail.
	require.NoError(t, os.Mkdir(filepath.Join(e.paths.Cluster, "adopted.jsonl"), 0o700))
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Equal(t, "A", e.records()[0].Type, "nothing is taken over without its copy")
	require.True(t, hasProblem(st, "storing the record before the adoption"))
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com was tried but replaced nothing"))
}

// E1: a DNS run that did not look leaves the conflicts as they were.
func TestConflictsStayWhileDNSDoesNotLook(t *testing.T) {
	e, _ := conflicted(t)
	path := filepath.Join(e.paths.Cluster, "meta", "tombstones.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))

	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Len(t, st.Conflicts, 1)
	require.Zero(t, eventsContaining(e, "no longer conflicts"))
}

func TestTheConflictsOfAFrozenAccountStay(t *testing.T) {
	e, _ := conflicted(t)
	e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred9"} })

	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Equal(t, []reconcile.Conflict{{Zone: "example.com", Name: "www.example.com", Type: "A", Content: "192.0.2.10"}}, st.Conflicts)
	require.Zero(t, eventsContaining(e, "no longer conflicts"))
}
