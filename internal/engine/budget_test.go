package engine

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// spentAfter is Cloudflare through a credential whose budget lets only so
// many records be created; the rest are refused as the limiter refuses a
// request that would wait too long.
type spentAfter struct {
	cfapi.API
	mu      sync.Mutex
	allowed int
}

func (s *spentAfter) CreateRecord(ctx context.Context, zoneID string, r cfapi.Record) (cfapi.Record, error) {
	s.mu.Lock()
	allowed := s.allowed > 0
	s.allowed--
	s.mu.Unlock()
	if !allowed {
		return cfapi.Record{}, &cfapi.Error{Status: http.StatusTooManyRequests, Message: "not sent: Cloudflare's rate limit leaves no request for now", RetryAfter: 4 * time.Minute}
	}
	return s.API.CreateRecord(ctx, zoneID, r)
}

func (s *spentAfter) allow(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowed = n
}

// A cycle whose budget is spent ends, saying what waits; a later one makes
// the rest from its own plan.
func TestAChangeThatWaitsForTheBudgetIsMadeByALaterCycle(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(many(5)...))
	api := &spentAfter{API: e.cf, allowed: 2}
	e.useAPI(testToken, api)

	st := e.cycle()

	require.Contains(t, st.Problems, "3 changes wait for Cloudflare's rate limit")
	require.Len(t, e.recordNames(), 2)

	api.allow(10)
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	require.Empty(t, st.Problems)
	require.Len(t, e.recordNames(), 5)
}
