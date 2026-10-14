package upgrade

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
)

const (
	tunnelA = "00000000-0000-4000-8000-00000000000a"
	tunnelB = "00000000-0000-4000-8000-00000000000b"
	tunnelC = "00000000-0000-4000-8000-00000000000c"
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

// heard is the view of an agent whose controller's heartbeat, age old,
// counted ready replicas of the tunnels.
func heard(age time.Duration, ready map[string]int) agentproto.AgentView {
	return agentproto.AgentView{Node: "pve2", Remote: true, Heard: true, HeartbeatAge: age, ReadyReplicas: ready}
}

func views(vs ...agentproto.AgentView) func(context.Context) (agentproto.AgentView, error) {
	return func(context.Context) (agentproto.AgentView, error) {
		v := vs[0]
		if len(vs) > 1 {
			vs = vs[1:]
		}
		return v, nil
	}
}

func TestAnAgentRestartsItsConnectorBesideAnotherReadyReplica(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {ready("old", 4), ready("new", 4)}}, tunnelA)
	rr.r.Replicas = views(heard(4*time.Second, map[string]int{tunnelA: 2}))

	require.NoError(t, rr.r.Restart(t.Context()))

	require.Equal(t, []string{"pco-cloudflared@" + tunnelA + ".service"}, rr.sd.restarted)
	require.Equal(t, []string{
		"restarting the connector of tunnel " + tunnelA + ": 1 other replica of the tunnel is ready and serves it meanwhile",
		"the connector of tunnel " + tunnelA + " is ready again after 1s: active, ready, 4 connections",
	}, rr.said)
}

func TestAnAgentWaitsForAnotherReplicaToBeReady(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {ready("old", 4), ready("new", 4)}}, tunnelA)
	rr.r.Replicas = views(
		heard(3*time.Second, map[string]int{tunnelA: 1}),
		heard(8*time.Second, map[string]int{tunnelA: 1}),
		heard(2*time.Second, map[string]int{tunnelA: 2}),
	)

	require.NoError(t, rr.r.Restart(t.Context()))

	require.Len(t, rr.sd.restarted, 1)
	require.Equal(t, "waiting up to 5m0s for another replica of tunnel "+tunnelA+" to be ready before its connector restarts: "+
		"the controller counts no other ready replica", rr.said[0])
	require.Len(t, rr.said, 3, "the wait is said once")
}

func TestAnAgentWithoutAnotherReadyReplicaRefusesAfterFiveMinutes(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{
		tunnelA: {ready("a1", 4)},
		tunnelB: {ready("b1", 4), ready("b2", 4)},
	}, tunnelA, tunnelB)
	rr.r.Replicas = func(context.Context) (agentproto.AgentView, error) {
		return heard(5*time.Second, map[string]int{tunnelA: 1, tunnelB: 3}), nil
	}
	start := rr.clock.now

	err := rr.r.Restart(t.Context())

	require.ErrorIs(t, err, ErrNoOtherReplica)
	require.EqualError(t, err, "the connector of tunnel "+tunnelA+" is not restarted: no other replica of the tunnel is ready "+
		"5m0s later (the controller counts no other ready replica); it runs the cloudflared before until "+
		"systemctl restart pco-cloudflared@"+tunnelA+".service, once pco agent status counts another ready replica")
	require.Equal(t, []string{"pco-cloudflared@" + tunnelB + ".service"}, rr.sd.restarted, "the other tunnel restarts all the same")
	require.Equal(t, 5*time.Minute, rr.clock.now.Sub(start)-time.Second, "five minutes, and the second tunnel's wait for ready")
}

func TestAnAgentTakesNoStaleOrMissingHeartbeatForAReadyReplica(t *testing.T) {
	for _, tt := range []struct {
		name string
		view func(context.Context) (agentproto.AgentView, error)
		why  string
	}{
		{"no heartbeat", views(agentproto.AgentView{Node: "pve2", Remote: true}), "the agent of this node has heard no heartbeat of its controller"},
		{"stale", views(heard(45*time.Second, map[string]int{tunnelA: 3})), "the last heartbeat of the controller is 45s old"},
		{"from the future", views(heard(-5*time.Second, map[string]int{tunnelA: 3})),
			"the last heartbeat of the controller is -5s old: the clock of this node stepped back"},
		{"no daemon", func(context.Context) (agentproto.AgentView, error) {
			return agentproto.AgentView{}, errors.New("dial unix /run/pco/pco.sock: connect: no such file or directory")
		}, "the daemon does not answer: dial unix /run/pco/pco.sock: connect: no such file or directory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := newRestartRig(map[string][]connector.Status{tunnelA: {ready("old", 4)}}, tunnelA)
			rr.r.Replicas = tt.view

			err := rr.r.Restart(t.Context())

			require.ErrorIs(t, err, ErrNoOtherReplica)
			require.ErrorContains(t, err, "("+tt.why+")")
			require.Empty(t, rr.sd.restarted)

			_, err = CheckReplicas(t.Context(), tt.view, rr.conns)
			require.ErrorIs(t, err, ErrCannotTell)
			require.EqualError(t, err, "pco cannot tell whether another replica of each tunnel is ready: "+
				tt.why+"; an agent restarts its connectors only beside a ready replica, and --force restarts them without")
		})
	}
}

// The tunnels of this node that the heartbeat counts one ready replica of
// at most, this node's own, are named before anything is installed: their
// connectors will wait, and may be left on the cloudflared before.
func TestTheCheckNamesTheTunnelsWithoutAnotherReadyReplica(t *testing.T) {
	conns := &fakeConnectors{ids: []string{tunnelA, tunnelB, tunnelC}}

	lone, err := CheckReplicas(t.Context(), views(heard(time.Second, map[string]int{tunnelA: 1, tunnelB: 2})), conns)

	require.NoError(t, err)
	require.Equal(t, []string{tunnelA, tunnelC}, lone)
}

// A connector that is not ready serves nothing a restart could take away.
func TestAnAgentRestartsAConnectorThatIsNotReadyWithoutWaiting(t *testing.T) {
	rr := newRestartRig(map[string][]connector.Status{tunnelA: {{Active: true}, ready("new", 1)}}, tunnelA)
	asked := 0
	rr.r.Replicas = func(context.Context) (agentproto.AgentView, error) {
		asked++
		return agentproto.AgentView{}, nil
	}

	require.NoError(t, rr.r.Restart(t.Context()))

	require.Len(t, rr.sd.restarted, 1)
	require.Zero(t, asked)
}
