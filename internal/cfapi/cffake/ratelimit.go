package cffake

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// WithRateLimit makes the handler count requests as Cloudflare counts its rate
// limit: quota requests in a window, which begins with the first request
// after the last one ended, on the clock of the fake. Every answer says what
// is left, in the ratelimit and ratelimit-policy headers, and a request over
// the quota is answered with a 429 whose Retry-After is the rest of the
// window. Without it the handler sends neither header and limits nothing.
func WithRateLimit(quota int, window time.Duration) Option {
	return func(h *handler) { h.limit = &rateWindow{quota: quota, window: window} }
}

// rateWindow is the count of the requests of one window.
type rateWindow struct {
	quota  int
	window time.Duration

	mu    sync.Mutex
	start time.Time
	used  int
}

// take counts a request made at now and reports whether it is within the
// quota, with what is left and the time until the window ends.
func (r *rateWindow) take(now time.Time) (left int, reset time.Duration, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.start.IsZero() || !now.Before(r.start.Add(r.window)) {
		r.start, r.used = now, 0
	}
	reset = r.start.Add(r.window).Sub(now)
	if r.used >= r.quota {
		return 0, reset, false
	}
	r.used++
	return r.quota - r.used, reset, true
}

// admit counts the request and writes the headers of the rate limit; when the
// quota is spent it answers the request itself and returns false.
func (r *rateWindow) admit(w http.ResponseWriter, now time.Time) bool {
	left, reset, ok := r.take(now)
	seconds := int(math.Ceil(reset.Seconds()))
	w.Header().Set("Ratelimit", fmt.Sprintf(`"default";r=%d;t=%d`, left, seconds))
	w.Header().Set("Ratelimit-Policy", `"default";q=`+strconv.Itoa(r.quota)+";w="+strconv.Itoa(int(r.window.Seconds())))
	if !ok {
		writeError(w, &cfapi.Error{Status: http.StatusTooManyRequests, Codes: []int{codeRateLimited}, Message: "Rate limited", RetryAfter: reset})
	}
	return ok
}

// clock is the time of the fake.
func (f *Fake) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now()
}
