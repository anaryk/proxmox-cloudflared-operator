package auth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	signInsPerMinute = 10
	limitWindow      = time.Minute
	maxLimitedPeers  = 10000
)

// limiter allows a number of attempts per window and key: the last attempts
// it allowed are kept, so the next is allowed when the oldest of them is a
// window old. Attempts it refuses are not counted.
type limiter struct {
	per    int
	window time.Duration
	max    int // keys at most

	mu   sync.Mutex
	seen map[string][]time.Time
}

func newLimiter(per int, window time.Duration, maxKeys int) *limiter {
	return &limiter{per: per, window: window, max: maxKeys, seen: map[string][]time.Time{}}
}

// allow says whether key may try at now, and else how long it has to wait.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	times := l.recent(key, now)
	if len(times) >= l.per {
		return false, times[0].Add(l.window).Sub(now)
	}
	if len(times) == 0 && len(l.seen) >= l.max {
		for k := range l.seen {
			l.recent(k, now)
		}
		// Still full: so many addresses at once are refused rather than
		// let in unbounded.
		if len(l.seen) >= l.max {
			return false, l.window
		}
	}
	l.seen[key] = append(times, now)
	return true, 0
}

// recent are the attempts of key within the window; a key without any is
// dropped.
func (l *limiter) recent(key string, now time.Time) []time.Time {
	times := l.seen[key]
	i := 0
	for i < len(times) && now.Sub(times[i]) >= l.window {
		i++
	}
	times = times[i:]
	if len(times) == 0 {
		delete(l.seen, key)
		return nil
	}
	l.seen[key] = times
	return times
}

// client is the address of the TCP peer, without its port. Nothing stands in
// front of pco web, so X-Forwarded-For and its like are what the client
// wrote, and are never read.
func client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
