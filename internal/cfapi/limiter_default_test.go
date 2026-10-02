package cfapi

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The default budget is 300 requests in 5 minutes, one a second, with room for
// 20 at once.
func TestTheDefaultLimiter(t *testing.T) {
	clock := newFakeClock()
	l := NewDefaultLimiter(clock.now)

	for i := range 20 {
		wait, paused := l.reserve()
		require.Zero(t, wait, "request %d is within the burst", i+1)
		require.False(t, paused)
	}
	wait, paused := l.reserve()
	require.False(t, paused)
	require.Equal(t, time.Second, wait, "the 21st waits for a token, which comes every second")
}

func TestAClientWithoutALimiterHasTheDefaultOne(t *testing.T) {
	c, err := New(Options{Token: "tok-0123456789"})
	require.NoError(t, err)

	for i := range 20 {
		wait, _ := c.limiter.reserve()
		require.Zero(t, wait, "request %d is within the burst", i+1)
	}
	wait, _ := c.limiter.reserve()
	require.Positive(t, wait)
	require.LessOrEqual(t, wait, time.Second)
}

func TestAClientKeepsTheLimiterItIsGiven(t *testing.T) {
	l := NewDefaultLimiter(time.Now)

	c, err := New(Options{Token: "tok-0123456789", Limiter: l})

	require.NoError(t, err)
	require.Same(t, l, c.limiter)
}
