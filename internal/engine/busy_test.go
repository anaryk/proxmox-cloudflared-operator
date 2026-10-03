package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// An admin request gives up on a cycle that holds the lock for too long, in
// time for the command line to tell the admin why.
func TestAnAdminRequestBehindALongCycleSaysSo(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	waited := &timeouts{}
	e.eng.timeout = func(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		waited.withTimeout(ctx, d)
		if d == lockWait {
			d = time.Millisecond
		}
		return context.WithTimeout(ctx, d)
	}
	require.NoError(t, e.eng.acquire(t.Context()))
	defer e.eng.release()

	for name, call := range map[string]func(ctx context.Context) error{
		"apply": func(ctx context.Context) error {
			_, err := e.eng.Apply(ctx, false, "")
			return err
		},
		"remove a credential": func(ctx context.Context) error { return e.eng.RemoveCredential(ctx, testCred) },
		"check a credential": func(ctx context.Context) error {
			_, err := e.eng.CheckCredential(ctx, testCred, false)
			return err
		},
	} {
		err := call(t.Context())

		require.ErrorIs(t, err, ErrBusy, name)
		require.EqualError(t, err, "a cycle is running and took longer than 35s; try again", name)
	}
	require.Equal(t, 3, waited.count(lockWait))
	require.Less(t, lockWait, clientWait)
}

// clientWait is how long the command line waits for the answer to a request
// that changes something: apiclient.longTimeout.
const clientWait = 60 * time.Second

// The move of a claim looks at the inventory and then waits for the cycle that
// runs. If the two could outlast the command line, it would give up while the
// move still went through.
func TestAClaimMoveEndsWithinWhatTheCommandLineWaits(t *testing.T) {
	e := contested(t)
	asked := &timeouts{}
	e.eng.timeout = asked.withTimeout

	require.NoError(t, e.eng.ResolveClaim(t.Context(), "www.example.com", "qemu/102"))

	require.Equal(t, 1, asked.count(lockWait))
	require.Equal(t, 1, asked.count(lookTimeout))
	var worst time.Duration
	for _, d := range asked.seen {
		worst += d
	}
	require.Less(t, worst, clientWait, "the look and the wait for the cycle, one after the other")
}

func TestTheCredentialsAreAnsweredFromTheStore(t *testing.T) {
	e := newEnv(t)
	e.cycle()
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.AddZone("zone2", "example.org", "acc2")
	e.useAPI("other-token-0123456789", other)

	added, err := e.eng.AddCredential(t.Context(), "second", "other-token-0123456789")
	require.NoError(t, err)

	views, err := e.eng.Credentials()
	require.NoError(t, err)
	require.Len(t, views, 2, "added, without a cycle")
	got := views[0]
	if got.ID != added.ID {
		got = views[1]
	}
	require.Equal(t, "second", got.Label)
	require.True(t, got.Checked)
	require.True(t, got.Report.Usable)

	require.NoError(t, e.eng.RemoveCredential(t.Context(), added.ID))
	views, err = e.eng.Credentials()
	require.NoError(t, err)
	require.Len(t, views, 1, "removed, without a cycle")
	require.Equal(t, testCred, views[0].ID)
}
