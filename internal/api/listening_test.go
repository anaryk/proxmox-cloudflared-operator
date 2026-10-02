package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
)

func TestOnListeningRunsOnceTheSocketAnswers(t *testing.T) {
	socket := socketPath(t)
	s := newServer(&fakeEngine{state: testState()})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var calls int
	var version string
	var versionErr error
	s.OnListening(func() {
		calls++
		version, versionErr = apiclient.New(socket).Version(ctx)
		cancel()
	})

	require.NoError(t, s.Serve(ctx, socket, os.Getgid()))
	require.Equal(t, 1, calls)
	require.NoError(t, versionErr)
	require.Equal(t, "1.2.3", version)
}

func TestSetShutdownTimeoutIsHowLongServeWaitsForARequest(t *testing.T) {
	started := make(chan struct{})
	f := &fakeEngine{hook: func(ctx context.Context) error {
		close(started)
		<-ctx.Done() // ends only when the connection is closed
		return ctx.Err()
	}}
	s := newServer(f)
	s.SetShutdownTimeout(20 * time.Millisecond)
	socket := socketPath(t)
	stop := serve(t, s, socket)

	applied := make(chan error, 1)
	go func() { applied <- apiclient.New(socket).Apply(t.Context(), false) }()
	<-started

	require.ErrorIs(t, stop(), context.DeadlineExceeded)
	require.Error(t, <-applied)
}
