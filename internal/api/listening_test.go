package api

import (
	"context"
	"os"
	"testing"

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
