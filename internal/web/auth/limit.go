package auth

import (
	"container/list"
	"net"
	"net/http"
	"net/netip"
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
//
// It keeps at most max keys, in the order of their last attempt. A key is
// dropped a window after its last attempt; a new key that finds the limiter
// full pushes out the one tried longest ago, which only loosens that key's
// limit. A call takes constant time but for dropping the keys that are over,
// and a key is dropped once.
type limiter struct {
	per    int
	window time.Duration
	max    int

	mu    sync.Mutex
	byKey map[string]*list.Element // of *attempts
	order *list.List               // tried longest ago first
}

// attempts are the times a key was allowed within the window, oldest first.
type attempts struct {
	key   string
	times []time.Time
}

// newLimiter allows ten sign-ins a minute to each of maxKeys keys.
func newLimiter(maxKeys int) *limiter {
	return &limiter{per: signInsPerMinute, window: limitWindow, max: maxKeys, byKey: map[string]*list.Element{}, order: list.New()}
}

// allow says whether key may try at now, and else how long it has to wait.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for e := l.order.Front(); e != nil && l.over(e, now); e = l.order.Front() {
		l.drop(e)
	}
	e, ok := l.byKey[key]
	if !ok {
		if l.order.Len() >= l.max {
			l.drop(l.order.Front())
		}
		e = l.order.PushBack(&attempts{key: key})
		l.byKey[key] = e
	}
	a := e.Value.(*attempts)
	i := 0
	for i < len(a.times) && now.Sub(a.times[i]) >= l.window {
		i++
	}
	a.times = append(a.times[:0], a.times[i:]...)
	if len(a.times) >= l.per {
		return false, a.times[0].Add(l.window).Sub(now)
	}
	a.times = append(a.times, now)
	l.order.MoveToBack(e)
	return true, 0
}

// over says whether the last attempt of e is a window old.
func (l *limiter) over(e *list.Element, now time.Time) bool {
	times := e.Value.(*attempts).times
	return now.Sub(times[len(times)-1]) >= l.window
}

func (l *limiter) drop(e *list.Element) {
	delete(l.byKey, e.Value.(*attempts).key)
	l.order.Remove(e)
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

// bucket is what the sign-ins of a client address are limited by: an IPv4
// address, or the /64 of an IPv6 address, since a host is commonly given a
// whole /64 and could try from every address of it.
func bucket(addr string) string {
	ip, err := netip.ParseAddr(addr)
	switch {
	case err != nil:
		return addr
	case ip.Is4() || ip.Is4In6():
		return ip.Unmap().String()
	}
	network, err := ip.Prefix(64)
	if err != nil {
		return addr
	}
	return network.String()
}
