package reconcile

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// spent is the refusal of a limiter whose budget is spent for longer than a
// request may wait.
var spent = &cfapi.Error{Status: http.StatusTooManyRequests, Message: "not sent: Cloudflare's rate limit leaves no request for now", RetryAfter: 4 * time.Minute}

func manyNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("h%03d.example.com", i)
	}
	return out
}

// New names cost a listing of each zone and a create each: whether another
// record holds a name is read from the listing.
func TestDNSDecidesNewNamesFromTheListing(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "taken.example.com", Content: "192.0.2.1"})
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "TXT", Name: "h001.example.com", Content: "verification"})
	names := manyNames(50)

	res := newDNS(f, &memStore{}, t0).Run(context.Background(), dnsIn(append(names, "taken.example.com")...), Enforce)

	require.Empty(t, res.Problems)
	want := []string{"Records zone1", "Records zone2"}
	for _, name := range names {
		want = append(want, "CreateRecord zone1 "+name)
	}
	require.Equal(t, want, f.Calls())
	require.Equal(t, []Conflict{{Zone: "example.com", Name: "taken.example.com", Type: "A", Content: "192.0.2.1"}}, res.Conflicts)
}

// A cycle whose budget is spent stops writing: what is left waits for a later
// one, which takes it up from its own listing.
func TestDNSStopsWritingWhenTheBudgetIsSpent(t *testing.T) {
	f := newDNSFake()
	creates := 0
	s := &dnsSpy{API: f, failCreate: func(cfapi.Record) error {
		if creates++; creates > 3 {
			return spent
		}
		return nil
	}}
	names := manyNames(7)

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn(names...), Enforce)

	require.Equal(t, []string{"4 changes wait for Cloudflare's rate limit"}, res.Problems)
	require.Len(t, dnsWrites(f), 3)
	require.Equal(t, 4, creates, "nothing is sent after the refusal")
	var held []Action
	for _, a := range withoutDetail(res.Actions) {
		if !a.Applied {
			held = append(held, a)
		}
	}
	require.Equal(t, []Action{
		dnsAction(CreateRecord, "h003.example.com", HeldBudget, false),
		dnsAction(CreateRecord, "h004.example.com", HeldBudget, false),
		dnsAction(CreateRecord, "h005.example.com", HeldBudget, false),
		dnsAction(CreateRecord, "h006.example.com", HeldBudget, false),
	}, held)

	s.failCreate = nil
	res = newDNS(s, &memStore{}, t0.Add(10*time.Second)).Run(context.Background(), dnsIn(names...), Enforce)

	require.Empty(t, res.Problems)
	require.Len(t, f.RecordsIn(zone1.ID), 7)
}

func TestDNSOneChangeWaitsForTheBudget(t *testing.T) {
	f := newDNSFake()
	s := &dnsSpy{API: f, failCreate: func(cfapi.Record) error { return spent }}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

	require.Equal(t, []string{"1 change waits for Cloudflare's rate limit"}, res.Problems)
}

// A delete that is due waits for the budget without reading the record again;
// one the guard holds keeps saying so.
func TestDNSDeletesWaitForTheBudgetBehindTheGuard(t *testing.T) {
	t.Run("due", func(t *testing.T) {
		f := newDNSFake()
		f.SeedRecord(zone1.ID, ourCNAME("old.example.com", testTunnelID))
		s := &dnsSpy{API: f, failCreate: func(cfapi.Record) error { return spent }}
		store := &memStore{m: map[string]Tombstone{stoneKey(zone1.ID, "old.example.com"): overdue}}

		res := newDNS(s, store, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

		require.Equal(t, []string{"2 changes wait for Cloudflare's rate limit"}, res.Problems)
		require.Equal(t, []string{"Records zone1", "Records zone2"}, f.Calls(), "no read before a delete that waits")
		require.Contains(t, withoutDetail(res.Actions), dnsAction(DeleteRecord, "old.example.com", HeldBudget, true))
		require.Contains(t, store.m, stoneKey(zone1.ID, "old.example.com"), "its grace is kept")
	})

	t.Run("behind the guard", func(t *testing.T) {
		f := newDNSFake()
		stones := map[string]Tombstone{}
		for _, name := range manyNames(6) {
			f.SeedRecord(zone1.ID, ourCNAME(name, testTunnelID))
			stones[stoneKey(zone1.ID, name)] = overdue
		}
		s := &dnsSpy{API: f, failCreate: func(cfapi.Record) error { return spent }}

		res := newDNS(s, &memStore{m: stones}, t0).Run(context.Background(), dnsIn("app.example.com"), Enforce)

		require.NotNil(t, res.Guard)
		require.Contains(t, res.Problems, "1 change waits for Cloudflare's rate limit")
		for _, a := range res.Actions {
			if a.Kind == DeleteRecord {
				require.Contains(t, a.Held, HeldByGuard, a.Target)
			}
		}
	})
}

// An adoption that replaces a record takes three requests: the delete, the
// create, and the put-back should the create fail. It is not begun without
// room for them, as a name left empty stays so until a later cycle.
func TestDNSAdoptionWaitsForRoomForItsRequests(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"})
	var asked []int
	s := &dnsSpy{API: f, room: func(n int) bool { asked = append(asked, n); return false }}
	in := dnsIn("app.example.com")
	in.Adopt = map[string]bool{"app.example.com": true}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Equal(t, []int{3}, asked)
	require.Empty(t, dnsWrites(f))
	require.Len(t, f.RecordsIn(zone1.ID), 1)
	require.Equal(t, []Action{
		dnsAction(DeleteRecord, "app.example.com", HeldBudget, true),
		dnsAction(CreateRecord, "app.example.com", HeldBudget, true),
	}, withoutDetail(res.Actions))
}

// A delete of an adoption that the rate limit refuses holds its create too.
func TestDNSAdoptionWhoseDeleteIsRefusedHoldsItsCreate(t *testing.T) {
	f := newDNSFake()
	f.SeedRecord(zone1.ID, cfapi.Record{Type: "A", Name: "app.example.com", Content: "192.0.2.10"})
	s := &dnsSpy{API: f, deleteErr: spent}
	in := dnsIn("app.example.com")
	in.Adopt = map[string]bool{"app.example.com": true}

	res := newDNS(s, &memStore{}, t0).Run(context.Background(), in, Enforce)

	require.Empty(t, dnsWrites(f))
	require.Equal(t, []Action{
		dnsAction(DeleteRecord, "app.example.com", HeldBudget, true),
		dnsAction(CreateRecord, "app.example.com", HeldBudget, true),
	}, withoutDetail(res.Actions))
}
