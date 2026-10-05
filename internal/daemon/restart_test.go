package daemon

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// The settings the API says are read at start are those the daemon reports
// when they change.
func TestTheDaemonSaysWhichSettingsItReadsAtStart(t *testing.T) {
	d := newWorld(t).start()

	v, err := d.client.Settings(t.Context())

	require.NoError(t, err)
	require.Equal(t, slices.Sorted(slices.Values(wiredFields())), v.ReadAtStart)
}

// A restart that is asked for stops the daemon as a stop does: systemd starts
// it again.
func TestARestartAskedForStopsTheDaemon(t *testing.T) {
	d := newWorld(t).start()
	d.await(func(st engine.State) bool { return !st.At.IsZero() })

	require.NoError(t, d.client.Restart(t.Context()))

	select {
	case err := <-d.done:
		d.once.Do(func() { d.result = err }) // stopped: the cleanup has nothing to wait for
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not stop")
	}
}
