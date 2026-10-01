package cfapi

import (
	"context"
	"sync"
	"time"
)

// Limiter is a token bucket: it lets `limit` requests through per `window`,
// with room for `burst` of them at once. It is safe for concurrent use.
//
// Tokens are counted in time rather than in units: a request costs
// window/limit of credit, the credit refills at one nanosecond per nanosecond
// and holds at most burst requests' worth. That keeps every amount an exact
// time.Duration.
type Limiter struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu       sync.Mutex
	cost     time.Duration // credit one request takes
	capacity time.Duration // burst * cost
	credit   time.Duration
	last     time.Time // when credit was last refilled
	until    time.Time // no request before this, set by Pause
}

// NewLimiter returns a full bucket. A nil now means time.Now; a limit, window
// or burst below one is raised to the smallest usable value.
func NewLimiter(limit int, window time.Duration, burst int, now func() time.Time) *Limiter {
	return newLimiter(limit, window, burst, now, sleepContext)
}

// newLimiter is NewLimiter with the sleep supplied, so that tests can drive
// time without waiting for it.
func newLimiter(limit int, window time.Duration, burst int, now func() time.Time, sleep func(context.Context, time.Duration) error) *Limiter {
	if now == nil {
		now = time.Now
	}
	limit = max(limit, 1)
	burst = max(burst, 1)
	if window < time.Nanosecond {
		window = time.Second
	}
	cost := max(window/time.Duration(limit), 1)
	capacity := cost * time.Duration(burst)
	return &Limiter{
		now:      now,
		sleep:    sleep,
		cost:     cost,
		capacity: capacity,
		credit:   capacity,
		last:     now(),
	}
}

// Wait blocks until a request may be sent and takes its token. It returns the
// error of ctx if that ends first, without having taken one.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := l.reserve()
		if d == 0 {
			return nil
		}
		if err := l.sleep(ctx, d); err != nil {
			return err
		}
	}
}

// Pause holds back every Wait until d from now, whatever tokens are left, and
// empties the bucket: nothing is saved up during the pause, so no burst goes
// out the moment it ends. It never shortens a pause that is already longer.
func (l *Limiter) Pause(d time.Duration) {
	if d <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	until := l.now().Add(d)
	if !until.After(l.until) {
		return
	}
	l.until = until
	l.credit = 0
	l.last = until // refilling starts when the pause ends
}

// reserve takes a token and returns 0, or returns how long to wait before
// asking again.
func (l *Limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if now.Before(l.until) {
		return l.until.Sub(now)
	}
	// A clock that steps back earns no credit.
	if elapsed := now.Sub(l.last); elapsed >= l.capacity-l.credit {
		l.credit = l.capacity
	} else if elapsed > 0 {
		l.credit += elapsed
	}
	l.last = now

	if l.credit >= l.cost {
		l.credit -= l.cost
		return 0
	}
	return l.cost - l.credit
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
