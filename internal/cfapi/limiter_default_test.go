package cfapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The default budget is 1000 requests in 5 minutes, all of them at once if
// need be, which leaves 200 of Cloudflare's 1200 to the dashboard and other
// tools.
func TestTheDefaultLimiter(t *testing.T) {
	clock := newFakeClock()
	l := NewDefaultLimiter(clock.now)

	for i := range 1000 {
		wait, paused := l.reserve()
		require.Zero(t, wait, "request %d is within the budget", i+1)
		require.False(t, paused)
	}
	wait, paused := l.reserve()
	require.False(t, paused)
	require.Equal(t, 300*time.Millisecond, wait, "the 1001st waits for a token, which comes every 0.3 s")
	require.Equal(t, 20*time.Second, l.maxWait)
}

func TestAClientWithoutALimiterHasTheDefaultOne(t *testing.T) {
	c, err := New(Options{Token: "tok-0123456789"})
	require.NoError(t, err)

	for i := range 1000 {
		wait, _ := c.limiter.reserve()
		require.Zero(t, wait, "request %d is within the budget", i+1)
	}
	wait, _ := c.limiter.reserve()
	require.Positive(t, wait)
	require.LessOrEqual(t, wait, 300*time.Millisecond)
}

func TestAClientKeepsTheLimiterItIsGiven(t *testing.T) {
	l := NewDefaultLimiter(time.Now)

	c, err := New(Options{Token: "tok-0123456789", Limiter: l})

	require.NoError(t, err)
	require.Same(t, l, c.limiter)
}
