package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// retired publishes x and y on guest 101, then takes them from it, so that
// both records are due for deletion once the claims and then the records
// have waited out their grace.
func retired(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(guest(101, "web-1", "x.example.com y.example.com -> :8080")))
	e.cycle()
	require.Equal(t, []string{"x.example.com", "y.example.com"}, e.recordNames())

	e.inv.set(snapshot(untagged(guest(101, "web-1"))))
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	st := e.cycle()
	require.Contains(t, actionKinds(st), "delete-record x.example.com held: grace period: 1m0s left")
	e.clock.advance(61 * time.Second)
	return e
}

func TestStillUnwantedKeepsANameAnotherGuestPublishes(t *testing.T) {
	e := retired(t)
	// The cycle sees no route for x; the look right before the deletes finds
	// another guest that publishes it now.
	idle := untagged(guest(101, "web-1"))
	e.inv.enqueue(snapshot(idle), snapshot(idle, guest(102, "web-2", "x.example.com -> :8080")))
	refreshes := e.inv.refreshes()

	e.cycle()

	require.Equal(t, []string{"x.example.com"}, e.recordNames(), "x is published again, y is gone")
	require.Equal(t, refreshes+2, e.inv.refreshes(), "the second look is taken once per cycle, not once per delete")
}

func TestStillUnwantedCountsAHostnameAGuestStillNames(t *testing.T) {
	e := retired(t)
	// Another guest names x in its Notes, outside a route: a held name.
	named := guest(102, "web-2")
	named.Description = "Takes over from x.example.com next week."
	idle := untagged(guest(101, "web-1"))
	e.inv.enqueue(snapshot(idle), snapshot(idle, named))

	e.cycle()

	require.Equal(t, []string{"x.example.com"}, e.recordNames(), "a held name is still wanted")
}

func TestStillUnwantedHoldsTheDeletesWhenTheSecondLookListsNoGuest(t *testing.T) {
	e := retired(t)
	e.inv.enqueue(snapshot(untagged(guest(101, "web-1"))), snapshot())

	st := e.cycle()

	require.Equal(t, []string{"x.example.com", "y.example.com"}, e.recordNames())
	require.True(t, hasProblem(st, "asking the inventory before deleting: the inventory lists no guest any more"))
}

func TestStillUnwantedHoldsTheDeletesWhenTheSecondLookIsIncomplete(t *testing.T) {
	e := retired(t)
	idle := untagged(guest(101, "web-1"))
	e.inv.enqueue(snapshot(idle), incomplete("node pve1: network not refreshed: timeout", idle))

	st := e.cycle()

	require.Equal(t, []string{"x.example.com", "y.example.com"}, e.recordNames())
	require.Contains(t, strings.Join(st.Problems, "\n"), "asking the inventory before deleting: the inventory is incomplete")

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Empty(t, e.records(), "the grace was kept, so the deletes go ahead once the inventory answers")
}

func TestReplacedRecordsReachTheAdoptedLog(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	foreign := e.cf.SeedRecord(testZone, cfapi.Record{Type: "A", Name: "www.example.com", Content: "192.0.2.10", TTL: 300, Comment: "by hand"})

	st := e.cycle()

	require.Equal(t, []reconcile.Conflict{{Zone: "example.com", Name: "www.example.com", Type: "A", Content: "192.0.2.10"}}, st.Conflicts)
	require.Contains(t, e.eng.Events(time.Time{}), Event{
		At: t0, Level: "warn", Kind: "conflict", Subject: "www.example.com",
		Message: "A 192.0.2.10 in zone example.com is not ours; the hostname is not published",
	})

	require.NoError(t, e.eng.Adopt(t.Context(), "WWW.example.com."))
	e.clock.advance(20 * time.Second)
	st = e.cycle()

	require.Empty(t, st.Conflicts)
	recs := e.records()
	require.Len(t, recs, 1)
	require.Equal(t, "CNAME", recs[0].Type)
	require.Empty(t, e.eng.adopt, "the adoption is used up")

	b, err := os.ReadFile(filepath.Join(e.paths.Cluster, "adopted.jsonl"))
	require.NoError(t, err)
	var line struct {
		At     time.Time `json:"at"`
		Zone   string    `json:"zone"`
		Record struct {
			ID, Type, Name, Content string
		} `json:"record"`
	}
	require.NoError(t, json.Unmarshal(b, &line))
	require.Equal(t, t0.Add(20*time.Second), line.At)
	require.Equal(t, "example.com", line.Zone)
	require.Equal(t, foreign.ID, line.Record.ID)
	require.Equal(t, "A", line.Record.Type)
	require.Equal(t, "192.0.2.10", line.Record.Content)

	events := e.eng.Events(t0)
	require.Contains(t, events, Event{
		At: t0.Add(20 * time.Second), Level: "info", Kind: "conflict", Subject: "www.example.com",
		Message: "A 192.0.2.10 in zone example.com no longer conflicts",
	})
}

func TestAdoptWaitsForAnEnforcingRun(t *testing.T) {
	e := newEnv(t)
	e.cf.SeedRecord(testZone, cfapi.Record{Type: "A", Name: "www.example.com", Content: "192.0.2.10", Comment: "by hand"})
	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))

	e.cycle()

	require.True(t, e.eng.adopt["www.example.com"], "an observing run does not use the adoption up")
	require.Equal(t, "A", e.records()[0].Type)

	e.apply(false)
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, "CNAME", e.records()[0].Type)
}

func TestConfirmedDeletesLiftTheGuardOnce(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	names := []string{"a", "b", "c", "d", "e", "f"}
	hosts := make([]string, len(names))
	for i, n := range names {
		hosts[i] = n + ".example.com"
	}
	e.inv.set(snapshot(guest(101, "web-1", strings.Join(hosts, " ")+" -> :8080")))
	e.cycle()
	require.Len(t, e.records(), 6)

	e.inv.set(snapshot(untagged(guest(101, "web-1"))))
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	e.cycle()
	e.clock.advance(61 * time.Second)
	st := e.cycle()

	require.Len(t, e.records(), 6, "six of six records at once trip the guard")
	require.Contains(t, actionKinds(st), "delete-record a.example.com held: mass delete guard: 6 of 6 records are being removed; confirm to proceed")

	e.apply(true)
	e.clock.advance(10 * time.Second)
	e.cycle()

	require.Empty(t, e.records())
	require.False(t, e.eng.confirmDeletes)
}

func TestAReplacedRecordThatCannotBeLoggedIsAProblemWithTheRecord(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cf.SeedRecord(testZone, cfapi.Record{Type: "A", Name: "www.example.com", Content: "192.0.2.10", TTL: 300, Comment: "by hand"})
	e.cycle()
	// A directory where the log should be makes the write fail.
	require.NoError(t, os.Mkdir(filepath.Join(e.paths.Cluster, "adopted.jsonl"), 0o700))

	require.NoError(t, e.eng.Adopt(t.Context(), "www.example.com"))
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Len(t, st.Problems, 1)
	require.Contains(t, st.Problems[0], "recording the replaced record A www.example.com 192.0.2.10 (ttl 300) of zone example.com")
}
