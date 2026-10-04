//go:build scale

package scale

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

const (
	// The budget of one credential, as cfapi.NewDefaultLimiter has it, and
	// the rate limit of Cloudflare it is spent from.
	budgetRequests  = cfapi.DefaultBudget
	budgetWindow    = 5 * time.Minute
	budgetWait      = 20 * time.Second
	cloudflareQuota = 1200
)

// budgetFloor is the least time n requests take on the default budget: the
// budget of a window goes out at once, and what is over it waits for the
// next window.
func budgetFloor(n int) time.Duration {
	if n == 0 {
		return 0
	}
	return time.Duration((n-1)/budgetRequests) * budgetWindow
}

// defaultBudget is the limiter of a credential on the clock of the bench,
// whose waits move that clock.
func defaultBudget(c *clock) *cfapi.Limiter {
	return cfapi.NewBudgetLimiter(budgetRequests, budgetWindow, budgetWait, c.now, c.sleep)
}

// TestDefaultLimiter starts the engine on 1000 routes against the default
// budget of a credential, with a fake Cloudflare that counts its rate limit
// and says what is left in every answer, as Cloudflare does, and counts the
// cycles until every record exists. The time it took is what the cycles were
// apart and what the limiter waited, on the clock of the bench, and the time
// the cycles worked.
func TestDefaultLimiter(t *testing.T) {
	const guests, routes = 2000, 1000

	b := newBench(t, guests, routes, defaultBudget, cffake.WithRateLimit(cloudflareQuota, budgetWindow))
	began := b.clock.now()
	cycles, calls := 0, 0
	said := "nothing"
	var worked time.Duration
	var last result
	for b.records() < routes && cycles < 60 {
		last = b.cycle()
		if i := slices.IndexFunc(last.state.Problems, func(p string) bool { return strings.Contains(p, "wait") }); cycles == 0 && i >= 0 {
			said = last.state.Problems[i]
		}
		worked += last.wall
		calls += last.calls.total()
		cycles++
	}
	require.Equal(t, routes, b.records())
	require.Empty(t, last.state.Problems)
	took := b.clock.now().Sub(began) - pollEvery + worked

	tab := newTable()
	tab.line("routes\tcycles until every record exists\tCF calls\tthe first cycle says\ttook on the default budget\tleast on the default budget")
	tab.line("%d\t%d\t%d\t%s\t%s\t%s", routes, cycles, calls, said, took.Round(time.Second), budgetFloor(calls))
	tab.flush()
}
