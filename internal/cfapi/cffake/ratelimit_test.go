package cffake_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

func TestTheRateLimitIsCountedAndSaid(t *testing.T) {
	f := cffake.New()
	now := t0
	f.SetNow(func() time.Time { return now })
	std(f)
	h := cffake.Handler(f, cffake.WithRateLimit(2, 5*time.Minute))
	accounts := "/client/v4/accounts"

	first := do(t, h, http.MethodGet, accounts, "")
	require.Equal(t, http.StatusOK, first.Code)
	require.Equal(t, `"default";r=1;t=300`, first.Header().Get("Ratelimit"))
	require.Equal(t, `"default";q=2;w=300`, first.Header().Get("Ratelimit-Policy"))

	now = now.Add(time.Minute)
	second := do(t, h, http.MethodGet, accounts, "")
	require.Equal(t, http.StatusOK, second.Code)
	require.Equal(t, `"default";r=0;t=240`, second.Header().Get("Ratelimit"))

	over := do(t, h, http.MethodGet, accounts, "")
	require.Equal(t, http.StatusTooManyRequests, over.Code)
	require.Equal(t, "240", over.Header().Get("Retry-After"))
	require.Equal(t, `"default";r=0;t=240`, over.Header().Get("Ratelimit"))
	require.Len(t, f.Calls(), 2, "the refused request reached nothing")

	now = now.Add(4 * time.Minute)
	again := do(t, h, http.MethodGet, accounts, "")
	require.Equal(t, http.StatusOK, again.Code)
	require.Equal(t, `"default";r=1;t=300`, again.Header().Get("Ratelimit"), "a new window")
}

func TestWithoutARateLimitNothingIsSaid(t *testing.T) {
	fx := newWireFixture()

	res := do(t, fx.h, http.MethodGet, "/client/v4/accounts", "")

	require.Equal(t, http.StatusOK, res.Code)
	require.Empty(t, res.Header().Get("Ratelimit"))
	require.Empty(t, res.Header().Get("Ratelimit-Policy"))
}
