package cfapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock is a clock that only moves when a test says so, or when a limiter
// "sleeps" on it.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// sleep records the request and moves the clock by exactly that much.
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return nil
}

// takeSleeps returns the sleeps requested since the last call.
func (c *fakeClock) takeSleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sleeps
	c.sleeps = nil
	return s
}

// testLimiter is the bucket most tests use: 6 requests a minute, so one token
// every 10 s, with room for 3.
func testLimiter(clock *fakeClock) *Limiter {
	return newLimiter(6, time.Minute, 3, clock.now, clock.sleep)
}

func TestLimiterSchedule(t *testing.T) {
	type step struct {
		advance time.Duration   // move the clock first
		pause   time.Duration   // then call Pause
		waits   int             // then call Wait this many times
		want    []time.Duration // the sleeps those waits asked for
	}
	const s = time.Second
	tests := []struct {
		name  string
		steps []step
	}{
		{"burst is free, then one token per interval", []step{
			{waits: 3},
			{waits: 1, want: []time.Duration{10 * s}},
			{waits: 1, want: []time.Duration{10 * s}},
		}},
		{"refills at limit over window", []step{
			{waits: 3},
			{advance: 20 * s, waits: 3, want: []time.Duration{10 * s}},
		}},
		{"partial refill is kept", []step{
			{waits: 3},
			{advance: 4 * s, waits: 1, want: []time.Duration{6 * s}},
		}},
		{"refill stops at the burst size", []step{
			{waits: 3},
			{advance: time.Hour, waits: 4, want: []time.Duration{10 * s}},
		}},
		{"idle bucket starts full", []step{
			{advance: time.Hour, waits: 3},
			{waits: 1, want: []time.Duration{10 * s}},
		}},
		{"pause blocks although tokens are left", []step{
			{pause: 7 * s, waits: 1, want: []time.Duration{7 * s}},
			{waits: 1},
		}},
		{"pause is counted from now", []step{
			{advance: time.Hour, pause: 7 * s, waits: 1, want: []time.Duration{7 * s}},
		}},
		{"tokens refill while paused", []step{
			{waits: 3},
			{pause: 7 * s, waits: 1, want: []time.Duration{7 * s, 3 * s}},
		}},
		{"a shorter pause does not cut a longer one", []step{
			{pause: 30 * s},
			{pause: 7 * s, waits: 1, want: []time.Duration{30 * s}},
		}},
		{"a later pause extends an earlier one", []step{
			{pause: 7 * s},
			{advance: 3 * s, pause: 7 * s, waits: 1, want: []time.Duration{7 * s}},
		}},
		{"a pause that is over changes nothing", []step{
			{pause: 7 * s},
			{advance: 8 * s, waits: 3},
		}},
		{"non-positive pause is ignored", []step{
			{pause: 0, waits: 1},
			{pause: -5 * s, waits: 1},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			l := testLimiter(clock)
			for i, st := range tt.steps {
				clock.advance(st.advance)
				l.Pause(st.pause)
				for range st.waits {
					require.NoError(t, l.Wait(context.Background()), "step %d", i)
				}
				require.Equal(t, st.want, clock.takeSleeps(), "step %d", i)
			}
		})
	}
}

func TestLimiterDefaultBudget(t *testing.T) {
	clock := newFakeClock()
	l := newLimiter(300, 5*time.Minute, 20, clock.now, clock.sleep)
	for range 20 {
		require.NoError(t, l.Wait(context.Background()))
	}
	require.Empty(t, clock.takeSleeps())

	require.NoError(t, l.Wait(context.Background()))
	require.Equal(t, []time.Duration{time.Second}, clock.takeSleeps())
}

func TestLimiterIgnoresClockGoingBackwards(t *testing.T) {
	clock := newFakeClock()
	l := testLimiter(clock)
	for range 3 {
		require.NoError(t, l.Wait(context.Background()))
	}

	clock.advance(-time.Hour)
	require.NoError(t, l.Wait(context.Background()))
	require.Equal(t, []time.Duration{10 * time.Second}, clock.takeSleeps())
}

func TestLimiterCancelledContext(t *testing.T) {
	clock := newFakeClock()
	l := testLimiter(clock)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, l.Wait(ctx), context.Canceled)
	require.Empty(t, clock.takeSleeps())

	// The refused call took no token.
	for range 3 {
		require.NoError(t, l.Wait(context.Background()))
	}
	require.Empty(t, clock.takeSleeps())
}

func TestLimiterContextEndsWhileWaiting(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	l := newLimiter(6, time.Minute, 1, clock.now, func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	})
	require.NoError(t, l.Wait(ctx))
	require.ErrorIs(t, l.Wait(ctx), context.Canceled)
}

func TestLimiterConcurrentWaiters(t *testing.T) {
	const waiters = 50
	clock := newFakeClock()
	start := clock.now()
	// One token a second, room for 5.
	l := newLimiter(1, time.Second, 5, clock.now, clock.sleep)

	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.Wait(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// 50 tokens cost 5 of burst plus 45 refills; time only moves when someone
	// sleeps, so it cannot have been handed out faster than that.
	require.GreaterOrEqual(t, clock.now().Sub(start), 45*time.Second)
}

func TestNewLimiterToleratesNonsense(t *testing.T) {
	l := NewLimiter(0, 0, 0, nil)
	require.NoError(t, l.Wait(context.Background()))
	require.NotNil(t, l.now)
	require.NotNil(t, l.sleep)
}

func TestSleepContext(t *testing.T) {
	require.NoError(t, sleepContext(context.Background(), 0))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, sleepContext(ctx, time.Hour), context.Canceled)
}
