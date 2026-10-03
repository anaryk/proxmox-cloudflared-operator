//go:build scale

package scale

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

const (
	// The budget of one credential, as cfapi.NewDefaultLimiter has it.
	budgetRequests = 300
	budgetWindow   = 5 * time.Minute
	budgetBurst    = 20

	// speedup makes the wait of the limiter that many times shorter: the same
	// budget in a window of five seconds. The limiter sleeps in real time, and
	// a first start at the default budget takes over half an hour.
	speedup = 60
)

// budgetFloor is the least time a cycle of n requests takes on the default
// budget when it starts with a full bucket: the burst goes out at once and
// every other request costs window/limit, a second.
func budgetFloor(n int) time.Duration {
	return time.Duration(max(n-budgetBurst, 0)) * (budgetWindow / budgetRequests)
}

// TestDefaultLimiter starts the engine on 1000 routes against the default
// budget of a credential and counts the cycles until every record exists. Time
// the limiter makes a cycle wait is counted speedup times; the time the cycle
// works is counted as it was, and is taken from the same cycle without a limit.
// The other cycles of the table are not run this way, as the limiter would
// refill during their work speedup times faster than it does: their time on
// the budget is the floor of the column, which this cycle checks.
func TestDefaultLimiter(t *testing.T) {
	const guests, routes = 2000, 1000

	work := newBench(t, guests, routes, unlimited()).cycle().wall

	limiter := cfapi.NewLimiter(budgetRequests, budgetWindow/speedup, budgetBurst, nil)
	b := newBench(t, guests, routes, limiter)
	cycles, took := 0, time.Duration(0)
	var calls int
	for b.records() < routes && cycles < 5 {
		res := b.cycle()
		require.Empty(t, res.state.Problems)
		took += work + max(res.wall-work, 0)*speedup
		calls += res.calls.total()
		cycles++
	}
	require.Equal(t, routes, b.records())

	tab := newTable()
	tab.line("routes\tcycles until every record exists\tCF calls\ttook on the default budget\tleast on the default budget")
	tab.line("%d\t%d\t%d\t%s\t%s", routes, cycles, calls, took.Round(time.Second), budgetFloor(calls))
	tab.flush()
}
