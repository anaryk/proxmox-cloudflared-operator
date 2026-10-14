package cfapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A write the rate limit held for more than a second asks for its
// permission again: the gate that let it through is older than the wait.
func TestAWriteHeldByTheRateLimitAsksTheGateAgain(t *testing.T) {
	asked := 0
	var refuse error
	env := setup(t, reply(http.StatusOK, `{"success":true,"result":{}}`), func(o *Options) {
		// One request every 10 s, room for one.
		o.Limiter = newLimiter(6, time.Minute, 1, nil, nil)
		o.Regate = func(context.Context) error {
			asked++
			return refuse
		}
	})
	env.c.limiter.now, env.c.limiter.sleep = env.clock.now, env.clock.sleep
	ctx := t.Context()

	require.NoError(t, env.c.do(ctx, http.MethodPut, "/x", nil, struct{}{}, nil))
	require.Zero(t, asked, "no wait, no question")

	require.NoError(t, env.c.do(ctx, http.MethodPut, "/x", nil, struct{}{}, nil))
	require.Equal(t, 1, asked, "after a wait of 10 s")

	require.NoError(t, env.c.do(ctx, http.MethodGet, "/x", nil, nil, nil))
	require.Equal(t, 1, asked, "a read is not asked about")

	refuse = errors.New("the lease is fenced")
	err := env.c.do(ctx, http.MethodDelete, "/x", nil, nil, nil)
	require.EqualError(t, err, "the request waited 10s for the rate limit, and may not be sent now: the lease is fenced")
	require.Len(t, env.requests(), 3, "the refused write was not sent")
}
