package clusterapi

import (
	"net/netip"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

const (
	// joinPerMinute is how many joins one source may try in a minute.
	joinPerMinute = 10
	// maxJoinBody and maxRenewBody bound the bodies of a join and of a
	// renewal: a token, a certificate request and a few facts.
	maxJoinBody  = 64 << 10
	maxRenewBody = 16 << 10
	// maxChecks bounds the connections whose source is being checked at
	// once; more are closed.
	maxChecks = 256
)

// rateLimiter lets each source try a thing at most limit times within a
// sliding window.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu    sync.Mutex
	tries map[netip.Addr][]time.Time
	swept time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, tries: map[netip.Addr][]time.Time{}}
}

// allow records a try of src at now, unless it tried limit times within the
// window before.
func (r *rateLimiter) allow(src netip.Addr, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.swept) >= r.window {
		for a, ts := range r.tries {
			if now.Sub(ts[len(ts)-1]) >= r.window {
				delete(r.tries, a)
			}
		}
		r.swept = now
	}
	ts := r.tries[src]
	for len(ts) > 0 && now.Sub(ts[0]) >= r.window {
		ts = ts[1:]
	}
	if len(ts) >= r.limit {
		r.tries[src] = ts
		return false
	}
	r.tries[src] = append(ts, now)
	return true
}

func (r *rateLimiter) size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tries)
}

// resendCounter counts, per node and epoch, the events an agent sent while
// it said it was sending the last 24 hours again. The agent's own count is a
// hint only: the controller holds what it receives to the bound itself.
type resendCounter struct {
	mu    sync.Mutex
	nodes map[string]resendCount
}

type resendCount struct {
	epoch agentproto.Epoch
	n     int
}

func newResendCounter() *resendCounter { return &resendCounter{nodes: map[string]resendCount{}} }

// over adds n events of node in epoch, when they are of a resend, and
// reports whether the resend of that node and epoch passed MaxResend.
func (r *resendCounter) over(node string, epoch agentproto.Epoch, n int, resend bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.nodes[node]
	if c.epoch != epoch {
		c = resendCount{epoch: epoch}
	}
	if resend {
		c.n += n
	}
	r.nodes[node] = c
	return resend && c.n > agentproto.MaxResend
}

func (r *resendCounter) count(node string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodes[node].n
}
