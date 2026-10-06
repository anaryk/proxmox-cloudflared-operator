package agent

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// booted is an agent that served www.example.com through the shared tunnel
// and the pinned one, and whose node then booted: the agent is made anew and
// Boot runs before any controller answers.
func booted(t *testing.T) (*env, agentproto.Snapshot) {
	t.Helper()
	e := newEnv(t)
	s := pinned(verified(e.snapshot("www.example.com -> :8080"), "10.0.0.11:8080", "10.0.0.12:443"))
	e.apply(s)
	e.agent = e.newAgent()
	e.conn.reset()
	return e, s
}

func TestAtBootPinnedConnectorsStartAtOnceAndTheSharedOneWaits(t *testing.T) {
	e, _ := booted(t)
	e.res.stop("www.example.com", "guest is not running")
	sets := e.egr.setCount()

	require.NoError(t, e.agent.Boot(t.Context()))

	require.Equal(t, []string{"Ensure " + pinnedID + " " + pinnedToken}, e.conn.changes())
	require.Equal(t, sets+1, e.egr.setCount())
	got, _ := e.egr.last()
	require.Empty(t, got, "the filter loads empty at boot; a target enters as it verifies")
	st := e.agent.Status(t.Context())
	require.Contains(t, st.Problems, "the shared connector waits for the controller to mark this node active; "+
		"if none answers within 2m0s of boot, the first shared target that verifies starts it")

	e.res.start()
	e.clock.advance(10 * time.Second)
	_, err := e.agent.Reverify(t.Context())
	require.NoError(t, err)
	got, _ = e.egr.last()
	require.Equal(t, egressTargets("10.0.0.11:8080"), got, "what verifies enters; what the verified configuration names alone does not")
	require.NotContains(t, e.conn.changes(), "Ensure "+sharedID+" "+sharedToken, "the gate holds for 120 s")
}

func TestAControllerThatMarksTheNodeActiveStartsTheSharedConnectorAtOnce(t *testing.T) {
	e, s := booted(t)
	require.NoError(t, e.agent.Boot(t.Context()))
	e.conn.reset()

	s.Seq++
	e.apply(s)

	require.Contains(t, e.conn.changes(), "Ensure "+sharedID+" "+sharedToken)
}

// A controller that answered is no silence: the node waits until it is
// marked active, however long that takes.
func TestANodeTheControllerHasNotMarkedActiveKeepsWaiting(t *testing.T) {
	e, s := booted(t)
	require.NoError(t, e.agent.Boot(t.Context()))

	s.Seq++
	s.State = agentproto.Joining
	e.apply(s)
	e.clock.advance(5 * time.Minute)
	_, err := e.agent.Reverify(t.Context())
	require.NoError(t, err)

	require.NotContains(t, e.conn.changes(), "Ensure "+sharedID+" "+sharedToken)
	require.Contains(t, e.agent.Status(t.Context()).Problems, "the shared connector waits until the controller marks this node active")
}

// The state the node cached before it booted says nothing about now.
func TestTheCachedStateDoesNotOpenTheGate(t *testing.T) {
	e, _ := booted(t)
	require.NoError(t, e.agent.Boot(t.Context()))
	e.agent.Heartbeat(agentproto.Heartbeat{Epoch: epoch1})

	_, err := e.agent.Reverify(t.Context())
	require.NoError(t, err)

	require.NotContains(t, e.conn.changes(), "Ensure "+sharedID+" "+sharedToken)
}

func TestWithTheControllerSilentFor120sTheSharedConnectorStartsAfterOneVerifiedTarget(t *testing.T) {
	e, _ := booted(t)
	e.res.stop("www.example.com", "guest is not running")
	require.NoError(t, e.agent.Boot(t.Context()))

	e.clock.advance(119 * time.Second)
	_, err := e.agent.Reverify(t.Context())
	require.NoError(t, err)
	e.clock.advance(2 * time.Second)
	_, err = e.agent.Reverify(t.Context())
	require.NoError(t, err)
	require.NotContains(t, e.conn.changes(), "Ensure "+sharedID+" "+sharedToken, "a cold cluster waits for a target")

	e.res.start()
	_, err = e.agent.Reverify(t.Context())
	require.NoError(t, err)

	require.Equal(t, 1, countOf(e.conn.changes(), "Ensure "+sharedID+" "+sharedToken))
	require.Contains(t, e.events.messages(), "no controller answered within 2m0s of boot and a shared target verified; the shared connector starts")
}

func TestWithNoSharedTargetTheSharedConnectorStartsAfter120s(t *testing.T) {
	e := newEnv(t)
	s := e.snapshot()
	e.apply(s)
	e.agent = e.newAgent()
	e.conn.reset()
	require.NoError(t, e.agent.Boot(t.Context()))
	require.Empty(t, e.conn.changes())

	e.clock.advance(120 * time.Second)
	_, err := e.agent.Reverify(t.Context())
	require.NoError(t, err)

	require.Equal(t, []string{"Ensure " + sharedID + " " + sharedToken}, e.conn.changes())
}

func TestTheGateStopsNothingThatRuns(t *testing.T) {
	e, _ := booted(t)
	e.conn.put(sharedID, testInstall, sharedToken)

	require.NoError(t, e.agent.Boot(t.Context()))

	require.True(t, e.conn.isRunning(sharedID), "a unit enabled at boot on a node alone runs on")
	require.Empty(t, slices.DeleteFunc(e.conn.changes(), func(c string) bool { return c != "Stop "+sharedID }))
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}
