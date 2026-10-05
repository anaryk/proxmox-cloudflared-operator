package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// shortFirstCheck makes the time of the first checks a millisecond, and
// counts how often the engine waited for them.
func shortFirstCheck(e *env) *int {
	waits := new(int)
	e.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d == firstCheckTimeout {
			*waits++
			d = time.Millisecond
		}
		return context.WithTimeout(ctx, d)
	}
	return waits
}

// The time of the first checks is one for all: a credential after one that
// used it up is not tried, which the log says, and not said to have run out of
// time itself.
func TestACredentialThatTheFirstChecksHadNoTimeForIsSaidNotToBeTried(t *testing.T) {
	e := newEnv(t)
	second := cffake.New()
	second.AddAccount("acc2", "Other")
	second.AddZone("zone2", "example.org", "acc2")
	e.addSecondCredential("second-token", second)
	e.useAPI(testToken, stalling{API: e.cf, stalled: new(atomic.Bool)})
	lines := logged(e, zerolog.WarnLevel)
	shortFirstCheck(e)
	triedBefore := -1
	e.inv.hook(func() {
		if triedBefore < 0 {
			triedBefore = verifies(second)
		}
	})

	_, stop := e.startRun()
	stop()

	require.Zero(t, triedBefore, "no check of the second token before the first cycle")
	var late, untried []string
	for _, l := range lines() {
		switch l["message"] {
		case "the first check of the token did not finish in time; its zones are planned without it":
			late = append(late, l["credential"].(string))
		case "the first check of the token was not tried in time; its zones are planned without it":
			untried = append(untried, l["credential"].(string))
		}
	}
	require.Equal(t, []string{testCred}, late)
	require.Equal(t, []string{"cred2"}, untried)
}

// A credential whose first check did not end in time is left to the recheck
// beside the cycles: the next iteration of Run does not wait for it again.
func TestAFirstCheckThatRanOutOfTimeIsNotWaitedForAgain(t *testing.T) {
	e := newEnv(t)
	e.useAPI(testToken, stalling{API: e.cf, stalled: new(atomic.Bool)})
	waits := shortFirstCheck(e)

	e.eng.checkNew(t.Context())
	require.Equal(t, 1, *waits)

	e.eng.checkNew(t.Context())
	e.eng.checkNew(t.Context())
	require.Equal(t, 1, *waits, "not waited for again")

	e.eng.recheck(t.Context())
	require.Equal(t, 1, verifies(e.cf), "the recheck finds it due all the same")
}

// With every stored credential checked or tried, nothing is read from the
// store, and the cycle lock is not asked for. A cycle that reads a credential
// nothing is known of opens the look again.
func TestNoLookAtTheStoreForNewCredentialsWhenNoneCanBeNew(t *testing.T) {
	e := newEnv(t)
	e.eng.checkNew(t.Context())
	require.Equal(t, 1, verifies(e.cf), "checked at the start")
	e.eng.checkNew(t.Context())

	second := cffake.New()
	second.AddAccount("acc2", "Other")
	second.AddZone("zone2", "example.org", "acc2")
	e.addSecondCredential("second-token", second)
	require.NoError(t, e.eng.acquire(t.Context()))
	done := make(chan struct{})
	go func() {
		e.eng.checkNew(t.Context())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		e.eng.release()
		t.Fatal("the look at the store waited for the cycle lock")
	}
	e.eng.release()
	require.Zero(t, verifies(second), "the stored credential is not looked for")

	e.cycle()
	e.eng.checkNew(t.Context())
	require.Equal(t, 1, verifies(second), "a cycle that read it opens the look")
}
