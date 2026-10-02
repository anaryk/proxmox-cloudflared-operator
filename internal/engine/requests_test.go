package engine

import (
	"errors"
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
	e := guarded(t, nil)
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
	e := guarded(t, nil)
	path := filepath.Join(e.paths.Cluster, "meta", "tombstones.json")
	good, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))

	e.apply(true)
	for range 3 {
		e.clock.advance(10 * time.Second)
		e.cycle()
	}

	require.NotNil(t, e.eng.confirm)
	require.Equal(t, 1, eventsContaining(e, "confirmation waits"))

	require.NoError(t, os.WriteFile(path, good, 0o600))
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Nil(t, e.eng.confirm)
	require.Empty(t, e.records())
}

func TestAConfirmationExpires(t *testing.T) {
	e := guarded(t, nil)
	path := filepath.Join(e.paths.Cluster, "meta", "tombstones.json")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	e.apply(true)

	e.clock.advance(4 * time.Minute)
	e.cycle()
	require.NotNil(t, e.eng.confirm)

	e.clock.advance(time.Minute)
	e.cycle()

	require.Nil(t, e.eng.confirm)
	require.Contains(t, adminEvents(e, e.clock.now().Add(-time.Minute)), ": confirmation expired before it could be applied")

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

// Reproduced: the adopted log cannot be written, so the reconciler holds the
// adoption; the request waits and is carried out once the log works.
func TestAnAdoptionWaitsWhileItsRecordCannotBeStored(t *testing.T) {
	e, _ := conflicted(t)
	// A directory where the log should be makes the write fail.
	log := filepath.Join(e.paths.Cluster, "adopted.jsonl")
	require.NoError(t, os.Mkdir(log, 0o700))
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))

	for range 2 {
		e.clock.advance(20 * time.Second)
		st := e.cycle()

		require.Equal(t, "A", e.records()[0].Type, "nothing is taken over without its copy")
		require.True(t, hasProblem(st, "storing the record before the adoption"))
	}
	require.Contains(t, e.eng.adopt, "www.example.com")
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com waits: snapshot not stored"))

	require.NoError(t, os.Remove(log))
	e.clock.advance(20 * time.Second)
	e.cycle()
	require.Equal(t, "CNAME", e.records()[0].Type)
	require.Empty(t, e.eng.adopt)
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com applied"))
}

func TestAnAdoptionWaitsOutAListingThatFailed(t *testing.T) {
	e, _ := conflicted(t)
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))
	e.cf.FailNext("dns.read", 1, errors.New("connection reset"))

	e.clock.advance(20 * time.Second)
	e.cycle()

	require.Equal(t, "A", e.records()[0].Type)
	require.Contains(t, e.eng.adopt, "www.example.com", "a listing that failed takes nothing over and uses nothing up")

	e.clock.advance(20 * time.Second)
	e.cycle()
	require.Equal(t, "CNAME", e.records()[0].Type)
	require.Empty(t, e.eng.adopt)
}

// C2: the engine's own reason, not the reconciler's: a verified tunnel is
// asked for before an adoption is passed on at all.
func TestAnAdoptionWaitsForAVerifiedTunnel(t *testing.T) {
	e, _ := conflicted(t)
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))
	// A change the rate limit holds leaves the tunnel unverified.
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com api.example.com -> :8080")))

	e.clock.advance(2 * time.Second)
	st := e.cycle()

	require.False(t, st.Tunnels[0].Verified)
	require.Equal(t, "A", e.records()[0].Type)
	require.Equal(t, 1, eventsContaining(e, "adoption of www.example.com waits: its tunnel is not verified or its connector is not ready"))
}

// B2: a confirmation that could not be saved, as the tombstones could not
// be, or that the guard still needs, stays pending.
func TestAConfirmationTheRunCouldNotKeepStaysPending(t *testing.T) {
	for name, held := range map[string]string{
		"tombstones not saved": "tombstones not saved",
		"guard":                "mass delete guard: 6 of 6 records are being removed; confirm to proceed",
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			c := e.eng.newCycle(t.Context())
			e.eng.confirm = &request{at: t0}
			in := reconcile.DNSInput{ConfirmDeletes: true}
			res := reconcile.DNSResult{Decided: true, Actions: []reconcile.Action{
				{Kind: reconcile.DeleteRecord, Target: "a.example.com", Destructive: true, Held: held},
			}}

			c.settleRequests(in, res, reconcile.Enforce)
			c.notePending(nil)

			require.NotNil(t, e.eng.confirm)
			require.Len(t, c.events, 1)
			require.Contains(t, c.events[0].Message, "confirmation waits: the run could not keep the confirmation of the removals it holds ("+held+")")
		})
	}
	t.Run("kept", func(t *testing.T) {
		e := newEnv(t)
		c := e.eng.newCycle(t.Context())
		e.eng.confirm = &request{at: t0}
		res := reconcile.DNSResult{Decided: true, Confirmed: 6, Actions: []reconcile.Action{
			{Kind: reconcile.DeleteRecord, Target: "a.example.com", Destructive: true, Held: "grace period: 30s left"},
		}}

		c.settleRequests(reconcile.DNSInput{ConfirmDeletes: true}, res, reconcile.Enforce)

		require.Nil(t, e.eng.confirm)
	})
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
