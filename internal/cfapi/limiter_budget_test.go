package cfapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// budgetLimiter is the budget of a credential on a fake clock: 1000 requests
// in 5 minutes, all at once if need be, and no wait longer than 20 s.
func budgetLimiter(clock *fakeClock) *Limiter {
	return NewBudgetLimiter(1000, 5*time.Minute, 20*time.Second, clock.now, clock.sleep)
}

// rateHeaders are the headers of an answer that says what is left of the
// rate limit, and what the limit is.
func rateHeaders(limit, policy string) http.Header {
	h := http.Header{}
	if limit != "" {
		h.Set("Ratelimit", limit)
	}
	if policy != "" {
		h.Set("Ratelimit-Policy", policy)
	}
	return h
}

const cloudflarePolicy = `"default";q=1200;w=300`

func waits(t *testing.T, l *Limiter, n int) {
	t.Helper()
	for i := range n {
		require.NoError(t, l.Wait(context.Background()), "request %d", i+1)
	}
}

func TestTheBudgetGoesOutAtOnceAndThenRefills(t *testing.T) {
	clock := newFakeClock()
	l := budgetLimiter(clock)

	waits(t, l, 1000)
	require.Empty(t, clock.takeSleeps())

	waits(t, l, 1)
	require.Equal(t, []time.Duration{300 * time.Millisecond}, clock.takeSleeps(), "one request every 0.3 s")
}

func TestTheLimiterFollowsWhatCloudflareSaysIsLeft(t *testing.T) {
	const s = time.Second
	tests := []struct {
		name          string
		before        int // requests made first
		limit, policy string
		then          int             // requests made after the answer, without a wait
		sleeps        []time.Duration // what the one request after those waits
		refused       time.Duration   // or, when set, how long it is refused for
	}{
		{"what it thinks is left", 0, `"default";r=1200;t=300`, cloudflarePolicy, 1000, []time.Duration{300 * time.Millisecond}, 0},
		{"fewer left than it thinks, less what the budget leaves to others", 0, `"default";r=500;t=300`, cloudflarePolicy, 300, []time.Duration{300 * time.Millisecond}, 0},
		{"more left than it thinks is not taken", 900, `"default";r=1199;t=100`, cloudflarePolicy, 100, []time.Duration{300 * time.Millisecond}, 0},
		{"nothing left, waited for when it is short", 0, `"default";r=150;t=15`, cloudflarePolicy, 0, []time.Duration{15 * s}, 0},
		{"nothing left for longer than 20 s", 0, `"default";r=200;t=240`, cloudflarePolicy, 0, nil, 240 * s},
		{"what is left without a policy", 0, `"default";r=10;t=30`, "", 10, []time.Duration{300 * time.Millisecond}, 0},
		{"a policy below the budget", 0, "", `"default";q=500;w=300`, 500, []time.Duration{600 * time.Millisecond}, 0},
		{"a policy below the budget leaves nothing to others", 0, `"default";r=50;t=300`, `"default";q=500;w=300`, 50, []time.Duration{600 * time.Millisecond}, 0},
		{"tokens rather than strings", 0, `default;r=500;t=300`, `default;q=1200;w=300`, 300, []time.Duration{300 * time.Millisecond}, 0},
		{"what does not parse is no answer", 0, `"default";r=lots`, `"default";q=;w=300`, 1000, []time.Duration{300 * time.Millisecond}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := newFakeClock()
			l := budgetLimiter(clock)
			waits(t, l, tt.before)

			l.observe(rateHeaders(tt.limit, tt.policy))

			waits(t, l, tt.then)
			require.Empty(t, clock.takeSleeps(), "%d requests go at once", tt.then)
			if tt.refused > 0 {
				err := l.Wait(context.Background())
				requirePaused(t, err, tt.refused)
				require.Contains(t, err.Error(), "rate limit leaves no request")
				require.Empty(t, clock.takeSleeps(), "a refusal does not wait")
				return
			}
			if tt.sleeps != nil {
				require.NoError(t, l.Wait(context.Background()))
				require.Equal(t, tt.sleeps, clock.takeSleeps())
			}
		})
	}
}

// After the reset Cloudflare reported, the limiter goes on with what it
// saved up meanwhile.
func TestTheLimiterGoesOnAfterTheReset(t *testing.T) {
	clock := newFakeClock()
	l := budgetLimiter(clock)
	waits(t, l, 1000)
	l.observe(rateHeaders(`"default";r=200;t=240`, cloudflarePolicy))
	requirePaused(t, l.Wait(context.Background()), 240*time.Second)

	clock.advance(225 * time.Second)
	require.NoError(t, l.Wait(context.Background()))
	require.Equal(t, []time.Duration{15 * time.Second}, clock.takeSleeps(), "the rest of the wait")
	waits(t, l, 799)
	require.Empty(t, clock.takeSleeps(), "what refilled meanwhile goes at once")
}

func TestAPauseIsNotChangedByWhatIsLeft(t *testing.T) {
	clock := newFakeClock()
	l := budgetLimiter(clock)
	l.Pause(time.Minute)

	l.observe(rateHeaders(`"default";r=1200;t=300`, cloudflarePolicy))

	requirePaused(t, l.Wait(context.Background()), time.Minute)
	require.Contains(t, l.Wait(context.Background()).Error(), "holding back after an earlier 429")
}

func TestEveryAnswerTellsTheLimiterWhatIsLeft(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Ratelimit", `"default";r=200;t=240`)
		w.Header().Set("Ratelimit-Policy", cloudflarePolicy)
		reply(http.StatusOK, okBody(`[]`))(w, r)
	}, func(o *Options) { o.Limiter = budgetLimiter(newFakeClock()) })

	_, err := env.c.Records(context.Background(), "z1", RecordFilter{})
	require.NoError(t, err)

	_, err = env.c.Records(context.Background(), "z1", RecordFilter{})
	require.True(t, IsRateLimited(err), "%v", err)
	require.Len(t, env.requests(), 1, "the second request is not sent")
}

func TestTheBudgetOfACredential(t *testing.T) {
	clock := newFakeClock()
	l := NewCredentialLimiter(150, clock.now)
	for range 150 {
		wait, paused := l.reserve()
		require.Zero(t, wait)
		require.False(t, paused)
	}
	wait, _ := l.reserve()
	require.Equal(t, 2*time.Second, wait, "150 in 5 minutes")
	require.Equal(t, 20*time.Second, l.maxWait)
}
