package gateway

import (
	"sync"
	"time"
)

// The limits of the global constraints (spec-ui 9.8): a diagnosis probes a
// guest and the doctor the host and Cloudflare; streams hold memory.
const (
	diagnosesPerMinute = 6
	doctorsPerMinute   = 2
	streamsPerSession  = 3
	maxStreams         = 200
	// streamRetryAfter is when a refused stream may try again.
	streamRetryAfter = 30
)

// userLimit allows a number of calls a window to each user, and with
// running set that many at once. A refused call is not counted. A user is
// forgotten once its window is over and nothing of it runs; there are no
// more users than sessions.
type userLimit struct {
	what    string
	per     int
	running int // 0: no bound
	window  time.Duration

	mu    sync.Mutex
	users map[string]*calls
}

// calls are the starts of a user's calls within the window, oldest first,
// and those of the calls that run.
type calls struct {
	times   []time.Time
	running []time.Time
}

func newUserLimit(what string, per, running int, window time.Duration) *userLimit {
	return &userLimit{what: what, per: per, running: running, window: window, users: map[string]*calls{}}
}

// start says whether user may make a call at now, and when not, how long it
// has to wait: until the oldest call of the window is a window old, or the
// call that runs reaches its timeout. done ends a call that was allowed.
func (l *userLimit) start(user string, now time.Time, timeout time.Duration) (done func(), wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for u, c := range l.users {
		c.times = recent(c.times, now, l.window)
		if len(c.times) == 0 && len(c.running) == 0 {
			delete(l.users, u)
		}
	}
	c := l.users[user]
	if c == nil {
		c = &calls{}
		l.users[user] = c
	}
	switch {
	case l.running > 0 && len(c.running) >= l.running:
		return nil, max(time.Second, c.running[0].Add(timeout).Sub(now)), false
	case len(c.times) >= l.per:
		return nil, c.times[0].Add(l.window).Sub(now), false
	}
	c.times = append(c.times, now)
	c.running = append(c.running, now)
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if i := indexOf(c.running, now); i >= 0 {
				c.running = append(c.running[:i], c.running[i+1:]...)
			}
		})
	}, 0, true
}

// recent are the times of s less than a window before now.
func recent(s []time.Time, now time.Time, window time.Duration) []time.Time {
	i := 0
	for i < len(s) && now.Sub(s[i]) >= window {
		i++
	}
	return append(s[:0], s[i:]...)
}

func indexOf(s []time.Time, t time.Time) int {
	for i, v := range s {
		if v.Equal(t) {
			return i
		}
	}
	return -1
}

// streamSlots counts the browser streams: three per session, 200 in all.
type streamSlots struct {
	mu        sync.Mutex
	bySession map[string]int
	total     int
}

func newStreamSlots() *streamSlots { return &streamSlots{bySession: map[string]int{}} }

// take holds a place for a stream of session, when there is one.
func (s *streamSlots) take(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= maxStreams || s.bySession[session] >= streamsPerSession {
		return false
	}
	s.bySession[session]++
	s.total++
	return true
}

// give gives a place taken for session back.
func (s *streamSlots) give(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bySession[session] == 0 {
		return
	}
	s.total--
	if s.bySession[session]--; s.bySession[session] == 0 {
		delete(s.bySession, session)
	}
}

func (s *streamSlots) open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}
