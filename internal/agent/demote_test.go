package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// serving is an agent that serves its targets through the shared tunnel,
// whose connector is ready, beside another ready replica, as the last
// heartbeat says.
func serving(t *testing.T, routes ...string) *env {
	t.Helper()
	e := newEnv(t)
	e.conn.setReady(sharedID, true)
	e.apply(e.snapshot(routes...))
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, ReadyReplicas: map[string]int{sharedID: 2}})
	e.conn.reset()
	return e
}

func (e *env) reverifyAfter(d time.Duration) {
	e.t.Helper()
	e.clock.advance(d)
	_, err := e.agent.Reverify(e.t.Context())
	require.NoError(e.t, err)
}

func TestASilentControllerAndNoSharedTargetVerifyingFor60sStopsTheSharedConnector(t *testing.T) {
	e := serving(t, "www.example.com -> :8080")
	e.res.stop("www.example.com", "guest is not running")

	e.reverifyAfter(30 * time.Second)
	require.NotContains(t, e.conn.changes(), "Stop "+sharedID)
	e.reverifyAfter(29 * time.Second)
	require.NotContains(t, e.conn.changes(), "Stop "+sharedID, "59 s")

	e.reverifyAfter(time.Second)
	require.Contains(t, e.conn.changes(), "Stop "+sharedID)
	require.False(t, e.conn.isRunning(sharedID))
	require.Contains(t, e.events.messages(), "no controller answered for 1m0s and no target of tunnel pco-abc123 verified for 1m0s while another replica is ready; its connector is stopped")

	e.conn.reset()
	e.reverifyAfter(10 * time.Second)
	require.Empty(t, e.conn.changes(), "it stays stopped")

	e.res.start()
	e.reverifyAfter(10 * time.Second)
	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken}, e.conn.changes(), "and starts again as soon as one target verifies")
	require.Contains(t, e.events.messages(), "a target of tunnel pco-abc123 verifies again; its connector starts")
}

func TestPartialLossNeverStopsIt(t *testing.T) {
	e := serving(t, "www.example.com -> :8080", "api.example.com -> :9090")
	e.res.stop("api.example.com", "guest is not running")

	e.reverifyAfter(5 * time.Minute)

	require.NotContains(t, e.conn.changes(), "Stop "+sharedID)
}

func TestWithoutAnotherReadyReplicaItIsNotStopped(t *testing.T) {
	e := serving(t, "www.example.com -> :8080")
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, ReadyReplicas: map[string]int{sharedID: 1}})
	e.res.stop("www.example.com", "guest is not running")

	e.reverifyAfter(5 * time.Minute)

	require.NotContains(t, e.conn.changes(), "Stop "+sharedID, "the only ready replica serves on")

	t.Run("nor without a heartbeat", func(t *testing.T) {
		e := newEnv(t)
		e.apply(e.snapshot("www.example.com -> :8080"))
		e.res.stop("www.example.com", "guest is not running")

		e.reverifyAfter(5 * time.Minute)

		require.NotContains(t, e.conn.changes(), "Stop "+sharedID)
	})
}

func TestAControllerThatAnswersKeepsItRunning(t *testing.T) {
	e := serving(t, "www.example.com -> :8080")
	e.res.stop("www.example.com", "guest is not running")

	for range 10 {
		e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, ReadyReplicas: map[string]int{sharedID: 2}})
		e.reverifyAfter(30 * time.Second)
	}

	require.NotContains(t, e.conn.changes(), "Stop "+sharedID)
}

// A target the agent never verified does not count: it could not have
// served it.
func TestATunnelWhoseTargetsNeverVerifiedIsNotStopped(t *testing.T) {
	e := newEnv(t)
	e.conn.setReady(sharedID, true)
	e.res.stop("www.example.com", "guest is not running")
	e.apply(e.snapshot("www.example.com -> :8080"))
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1, ReadyReplicas: map[string]int{sharedID: 2}})

	e.reverifyAfter(5 * time.Minute)

	require.NotContains(t, e.conn.changes(), "Stop "+sharedID)
}
