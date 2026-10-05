package cfapi

import (
	"context"
	"net/http"
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
//
// The answers of Cloudflare say what is left of its rate limit; a client
// tells its limiter, which then never lets more through than that, and never
// more than the limit Cloudflare names.
type Limiter struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
	// maxWait, when set, is the longest Wait sleeps for a token: a request
	// that would wait longer is refused.
	maxWait time.Duration

	mu       sync.Mutex
	cost     time.Duration // credit one request takes
	capacity time.Duration // burst * cost
	base     time.Duration // the cost the limiter was made with
	burst    time.Duration // the capacity it was made with
	credit   time.Duration
	last     time.Time // when credit was last refilled
	until    time.Time // no request before this, set by Pause
	held     time.Time // no request before this, as nothing is left until Cloudflare resets its count
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
		base:     cost,
		burst:    capacity,
		credit:   capacity,
		last:     now(),
	}
}

// Wait blocks until the bucket holds a token for a request and takes it. A
// pause is not waited out: while one is in force Wait returns at once, without
// a token, an *Error with status 429 whose RetryAfter is what is left of the
// pause, in whole seconds rounded up, so that a caller never sits on a lock for
// as long as Cloudflare asked to wait. A limiter with a longest wait refuses a
// request that would wait longer the same way. It returns the error of ctx if
// that ends first.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		d, paused := l.reserve()
		switch {
		case paused:
			return refused("not sent: holding back after an earlier 429", d)
		case d == 0:
			return nil
		case l.maxWait > 0 && d > l.maxWait:
			return refused("not sent: Cloudflare's rate limit leaves no request for now", d)
		}
		if err := l.sleep(ctx, d); err != nil {
			return err
		}
	}
}

// refused is the 429 of a request the limiter does not let through for d.
// RetryAfter is in whole seconds, so that the same wait reads the same in
// every report.
func refused(message string, d time.Duration) error {
	left := (d + time.Second - 1).Truncate(time.Second)
	return &Error{Status: http.StatusTooManyRequests, Message: message, RetryAfter: left}
}

// Pause refuses every Wait until d from now, whatever tokens are left, and
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
// asking again. paused says that the wait is what is left of a pause.
func (l *Limiter) reserve() (wait time.Duration, paused bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if now.Before(l.until) {
		return l.until.Sub(now), true
	}
	l.refill(now)
	if now.Before(l.held) {
		return l.held.Sub(now), false
	}
	if l.credit >= l.cost {
		l.credit -= l.cost
		return 0, false
	}
	return l.cost - l.credit, false
}

// Room reports whether n requests could go now, one after the other, without
// one of them being refused: no pause is in force, and neither what is left
// of a hold nor the credit they need takes longer than the longest wait.
func (l *Limiter) Room(n int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Before(l.until) {
		return false
	}
	l.refill(now)
	wait := max(l.held.Sub(now), time.Duration(n)*l.cost-l.credit)
	return l.maxWait <= 0 || wait <= l.maxWait
}

// refill adds the credit earned since the last refill. A clock that steps back
// earns none. The caller holds the lock.
func (l *Limiter) refill(now time.Time) {
	if elapsed := now.Sub(l.last); elapsed >= l.capacity-l.credit {
		l.credit = l.capacity
	} else if elapsed > 0 {
		l.credit += elapsed
	}
	l.last = now
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

// NewDefaultLimiter returns the budget of one credential: DefaultBudget
// requests in 5 minutes. It is what a client has that is not given a limiter;
// a caller that builds many clients for one credential makes one and gives it
// to all of them.
func NewDefaultLimiter(now func() time.Time) *Limiter {
	return NewCredentialLimiter(DefaultBudget, now)
}

// NewCredentialLimiter returns a budget of requests in every 5 minutes, all
// at once if need be, that refuses a request which would wait longer than 20
// s for its turn: a cycle that has spent the budget stops and goes on in a
// later one rather than hold its lock.
func NewCredentialLimiter(budget int, now func() time.Time) *Limiter {
	return NewBudgetLimiter(budget, budgetWindow, maxBudgetWait, now, nil)
}

// NewBudgetLimiter returns a budget of requests in every window, all at once
// if need be, that refuses a request which would wait longer than maxWait for
// its turn. sleep waits out a shorter wait; nil sleeps on the system clock.
func NewBudgetLimiter(requests int, window, maxWait time.Duration, now func() time.Time, sleep func(context.Context, time.Duration) error) *Limiter {
	if sleep == nil {
		sleep = sleepContext
	}
	l := newLimiter(requests, window, requests, now, sleep)
	l.maxWait = maxWait
	return l
}
