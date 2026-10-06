package upgrade

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	tunnelA = "00000000-0000-4000-8000-00000000000a"
	tunnelB = "00000000-0000-4000-8000-00000000000b"
)

func ready(id string, connections int) connector.Status {
	return connector.Status{Active: true, Ready: true, Connections: connections, ConnectorID: id}
}

type restartRig struct {
	clock *fakeClock
	conns *fakeConnectors
	sd    *fakeSystemd
	said  []string
	r     ConnectorRestart
}

func newRestartRig(statuses map[string][]connector.Status, ids ...string) *restartRig {
	rr := &restartRig{
		clock: &fakeClock{now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)},
		conns: &fakeConnectors{ids: ids, statuses: statuses},
		sd:    &fakeSystemd{},
	}
	rr.r = ConnectorRestart{
		Connectors: rr.conns, Systemd: rr.sd, Now: rr.clock.Now, Sleep: rr.clock.Sleep,
		Say: func(format string, args ...any) { rr.said = append(rr.said, fmt.Sprintf(format, args...)) },
	}
	return rr
}

// The old cloudflared may still answer /ready right after the restart was
// queued; only another connector id is the new one.
func TestARestartWaitsForTheNewCloudflaredToBeReady(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {
		ready("old", 4),
		ready("old", 4),
		{Active: true},
		ready("new", 2),
	}}, tunnelA)

	require.NoError(t, rr.r.Restart(t.Context()))

	require.Equal(t, []string{"pco-cloudflared@" + tunnelA + ".service"}, rr.sd.restarted)
	require.Equal(t, []string{
		"restarting the connector of tunnel " + tunnelA + ": the tunnel has no connection from this appliance until it is ready again, at most 1m0s",
		"the connector of tunnel " + tunnelA + " is ready again after 3s: active, ready, 2 connections",
	}, rr.said)
}

// A connector that was not ready before has no id to tell the new one by.
func TestAConnectorThatWasNotReadyIsReadyWhenItIs(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {{Active: true}, ready("new", 1)}}, tunnelA)

	require.NoError(t, rr.r.Restart(t.Context()))

	require.Contains(t, rr.said, "the connector of tunnel "+tunnelA+" is ready again after 1s: active, ready, 1 connection")
}

func TestAConnectorThatIsNotReadyWithinAMinuteIsAnError(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {ready("old", 4), {Active: true}}}, tunnelA)

	err := rr.r.Restart(t.Context())

	require.EqualError(t, err, "the connector of tunnel "+tunnelA+" is not ready 1m0s after its restart: active, not ready; pco status shows how it fares")
	require.Equal(t, time.Date(2026, 10, 6, 9, 1, 0, 0, time.UTC), rr.clock.now, "it waited a minute and no longer")
}

func TestAConnectorWhoseStatusFailsIsGivenItsMinute(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {ready("old", 4)}}, tunnelA)
	calls := 0
	rr.r.Connectors = statusFunc{ids: []string{tunnelA}, status: func(string) (connector.Status, error) {
		calls++
		if calls == 1 {
			return ready("old", 4), nil
		}
		return connector.Status{}, errors.New("reading env file: permission denied")
	}}

	err := rr.r.Restart(t.Context())

	require.EqualError(t, err, "the connector of tunnel "+tunnelA+" is not ready 1m0s after its restart: reading env file: permission denied")
	require.Equal(t, 61, calls)
}

func TestAConnectorThatDoesNotRunIsLeftAlone(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {{}}}, tunnelA)

	require.NoError(t, rr.r.Restart(t.Context()))

	require.Empty(t, rr.sd.restarted)
	require.Equal(t, []string{"the connector of tunnel " + tunnelA + " does not run; it runs the new cloudflared once it is started"}, rr.said)
}

func TestConnectorsRestartOneAfterTheOther(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{
		tunnelA: {ready("a1", 4), ready("a2", 4)},
		tunnelB: {ready("b1", 4), ready("b2", 4)},
	}, tunnelA, tunnelB)

	require.NoError(t, rr.r.Restart(t.Context()))

	require.Equal(t, []string{"pco-cloudflared@" + tunnelA + ".service", "pco-cloudflared@" + tunnelB + ".service"}, rr.sd.restarted)
	require.Len(t, rr.said, 4)
}

func TestARestartThatFailsStopsTheRestarts(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {ready("a1", 4)}, tunnelB: {ready("b1", 4)}}, tunnelA, tunnelB)
	rr.sd.err = errors.New("systemctl: exit status 1")

	err := rr.r.Restart(t.Context())

	require.EqualError(t, err, "restarting pco-cloudflared@"+tunnelA+".service: systemctl: exit status 1")
	require.Len(t, rr.sd.restarted, 1)
}

func TestARestartEndsWithItsContext(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {ready("old", 4), {Active: true}}}, tunnelA)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.ErrorIs(t, rr.r.Restart(ctx), context.Canceled)
}

type statusFunc struct {
	ids    []string
	status func(id string) (connector.Status, error)
}

func (s statusFunc) List(context.Context) ([]string, error) { return s.ids, nil }

func (s statusFunc) Status(_ context.Context, id string) (connector.Status, error) {
	return s.status(id)
}
