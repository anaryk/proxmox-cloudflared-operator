package cfapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// observe takes what an answer says of Cloudflare's rate limit:
//
//	ratelimit: "default";r=<requests left>;t=<seconds until the count resets>
//	ratelimit-policy: "default";q=<requests>;w=<seconds>
//
// A policy that allows fewer requests than the limiter was made with lowers
// it to the policy. What is left counts less the requests of the policy the
// limiter leaves to others; when that is fewer than the limiter thinks it has,
// it keeps to that, and when nothing is left it lets no request through until
// the count resets. What is left never adds to what the limiter has, and a
// header that does not parse says nothing.
func (l *Limiter) observe(h http.Header) {
	policies := parseLimits(h.Get("Ratelimit-Policy"))
	left := parseLimits(h.Get("Ratelimit"))
	if len(policies) == 0 && len(left) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range policies {
		l.follow(p)
	}
	now := l.now()
	if !now.Before(l.until) {
		l.refill(now)
	}
	for name, rl := range left {
		r, ok := rl["r"]
		if !ok {
			continue
		}
		usable := r - l.leftToOthers(policies[name])
		if have := int64(l.credit / l.cost); usable < have {
			l.credit = time.Duration(max(usable, 0)) * l.cost
		}
		if t := rl["t"]; usable <= 0 && t > 0 {
			if held := now.Add(time.Duration(t) * time.Second); held.After(l.held) {
				l.held = held
			}
		}
	}
}

// follow lowers the limiter to a policy of q requests in w seconds when that
// allows fewer than the limiter was made with. The caller holds the lock.
func (l *Limiter) follow(p map[string]int64) {
	q, w := p["q"], p["w"]
	if q <= 0 || w <= 0 {
		return
	}
	l.cost = max(l.base, time.Duration(w)*time.Second/time.Duration(q))
	// The bucket holds one request at least, or none would ever go.
	l.capacity = max(min(l.burst, time.Duration(q)*l.cost), l.cost)
	l.credit = min(l.credit, l.capacity)
}

// leftToOthers is how many requests of policy p the limiter leaves to others:
// what it does not spend of them itself. The caller holds the lock.
func (l *Limiter) leftToOthers(p map[string]int64) int64 {
	q, w := p["q"], p["w"]
	if q <= 0 || w <= 0 {
		return 0
	}
	return max(q-int64(time.Duration(w)*time.Second/l.cost), 0)
}

// maxLimitValue is the largest count or number of seconds of a rate limit
// field that is taken as an answer. Cloudflare counts in thousands and minutes;
// a larger one would overflow what the limiter computes from it.
const maxLimitValue = 1_000_000

// parseLimits reads a header of the rate limit fields, a list of names each
// with integer parameters: `"default";r=50;t=30, "other";r=5`. A member with
// a parameter that is not a number, or is larger than maxLimitValue, keeps the
// others.
func parseLimits(v string) map[string]map[string]int64 {
	out := map[string]map[string]int64{}
	for member := range strings.SplitSeq(v, ",") {
		parts := strings.Split(member, ";")
		name := strings.Trim(strings.TrimSpace(parts[0]), `"`)
		if name == "" {
			continue
		}
		params := map[string]int64{}
		for _, p := range parts[1:] {
			key, val, ok := strings.Cut(strings.TrimSpace(p), "=")
			n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			if ok && err == nil && n >= 0 && n <= maxLimitValue {
				params[strings.ToLower(strings.TrimSpace(key))] = n
			}
		}
		out[name] = params
	}
	return out
}
