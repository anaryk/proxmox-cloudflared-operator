package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// zoneless gives the token of e n accounts more, none of them with a zone.
func zoneless(e *env, n int) {
	for i := range n {
		e.cf.AddAccount(fmt.Sprintf("empty%02d", i), fmt.Sprintf("Empty %d", i))
	}
}

// idleCalls runs a cycle after the first ones and returns the calls it made,
// sorted.
func idleCalls(e *env) []string {
	e.clock.advance(10 * time.Second)
	n := len(e.cf.Calls())
	e.cycle()
	calls := slices.Clone(e.callsSince(n))
	slices.Sort(calls)
	return calls
}

func findTunnels(calls []string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "FindTunnel ") {
			n++
		}
	}
	return n
}

func TestAccountsWithoutZonesCostNothingInAnIdleCycle(t *testing.T) {
	plain := newEnv(t)
	plain.enforce()
	plain.cycle()
	crowded := newEnv(t)
	zoneless(crowded, 20)
	crowded.enforce()
	first := crowded.cycle()
	require.False(t, hasProblem(first, "connectors are not pruned"), "%v", first.Problems)

	require.Equal(t, idleCalls(plain), idleCalls(crowded))
	_, pruned := crowded.conn.lastPrune()
	require.True(t, pruned)
}

func TestAccountsWithoutZonesAreLookedUpAgainWithTheAccounts(t *testing.T) {
	e := newEnv(t)
	zoneless(e, 20)
	e.enforce()
	e.cycle()
	tunnel := e.tunnels()[0]
	// A tunnel of this install turns up in an account without a zone.
	e.cf.SeedTunnel("empty07", tunnelName, nil)

	st := e.cycle()
	require.False(t, hasProblem(st, "empty07"), "not looked at before the accounts are listed again")

	e.clock.advance(zoneRefreshEvery)
	n := len(e.cf.Calls())
	st = e.cycle()

	require.Equal(t, 21, findTunnels(e.callsSince(n)), "the tunnel of acc1 and of the twenty others")
	require.True(t, hasProblem(st, "in account empty07 serves no zone pco sees"), "%v", st.Problems)
	require.Equal(t, tunnel.ID, e.tunnels()[0].ID)
}

// failingLookup fails the lookups of the tunnel of one account while fail is
// set.
type failingLookup struct {
	cfapi.API
	account string
	fail    *atomic.Bool
}

func (f failingLookup) FindTunnel(ctx context.Context, account, name string) (cfapi.Tunnel, bool, error) {
	if f.fail.Load() && account == f.account {
		return cfapi.Tunnel{}, false, errors.New("503 Service Unavailable")
	}
	return f.API.FindTunnel(ctx, account, name)
}

func TestAFailedLookupOfAnAccountWithoutZonesIsTriedAgainInTheNextCycle(t *testing.T) {
	e := newEnv(t)
	zoneless(e, 1)
	var fail atomic.Bool
	e.useAPI(testToken, failingLookup{API: e.cf, account: "empty00", fail: &fail})
	e.enforce()
	e.cycle()
	// The lookup that fails would have found a tunnel of this install.
	e.cf.SeedTunnel("empty00", tunnelName, nil)
	e.clock.advance(zoneRefreshEvery)
	fail.Store(true)
	st := e.cycle()
	require.True(t, hasProblem(st, "connectors are not pruned in this cycle: looking up the tunnel of account empty00 failed"), "%v", st.Problems)

	fail.Store(false)
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	require.False(t, hasProblem(st, "connectors are not pruned"), "%v", st.Problems)
	require.True(t, hasProblem(st, "in account empty00 serves no zone pco sees"), "a failure is no answer: %v", st.Problems)
}
