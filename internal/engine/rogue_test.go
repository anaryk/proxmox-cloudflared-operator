package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var (
	ourConnector   = cfapi.Connector{ID: "c1-this-node", ConfigVersion: 1, Connections: 4, Version: "2026.9.1", OriginIP: "203.0.113.10"}
	theirConnector = cfapi.Connector{ID: "attacker-elsewhere", ConfigVersion: 1, Connections: 4, Version: "2026.8.0", OriginIP: "198.51.100.7"}
)

const theirProblem = "tunnel pco-abc123 in account acc1 is served by connector attacker-elsewhere from 198.51.100.7 (cloudflared 2026.8.0), " +
	"which pco does not run on this node: it takes a share of the requests to every hostname of the tunnel; " +
	"unless you run it, rotate the tunnel secret with pco tunnel rotate --account acc1"

// rogueEnv is an engine whose tunnel exists and whose connector on the node
// calls itself as ourConnector does.
func rogueEnv(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	e.conn.setConnectorID(id, ourConnector.ID)
	return e, id
}

func connectorEvents(e *env) []Event {
	var out []Event
	for _, ev := range unnumbered(e.eng.Events(time.Time{})) {
		if ev.Kind == kindConnector {
			out = append(out, ev)
		}
	}
	return out
}

// A stolen token with Cloudflare Tunnel Edit reads the run token and starts a
// connector elsewhere; Cloudflare then sends it a share of the requests to
// every hostname of the tunnel. The listing of the tunnel's connectors shows
// it, and nothing on the node calls itself so.
func TestAConnectorThatIsNotOursIsReported(t *testing.T) {
	e, id := rogueEnv(t)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(rolloutAskEvery)

	st := e.cycle()

	require.Equal(t, []RogueConnector{{
		Tunnel: tunnelName, TunnelID: id, Account: testAccount,
		ID: "attacker-elsewhere", OriginIP: "198.51.100.7", Version: "2026.8.0", Since: t0.Add(rolloutAskEvery),
	}}, st.RogueConnectors)
	require.Contains(t, st.Problems, theirProblem)
	require.Equal(t, []Event{{
		At: t0.Add(rolloutAskEvery), Level: "error", Kind: "connector", Subject: "attacker-elsewhere", Tunnel: tunnelName, Account: testAccount,
		Message: "connector attacker-elsewhere from 198.51.100.7 (cloudflared 2026.8.0) serves tunnel pco-abc123 in account acc1 " +
			"and is not one pco runs on this node",
	}}, connectorEvents(e))
}

func TestAConnectorThatIsNotOursIsAnErrorOnceAndAProblemWhileItIsThere(t *testing.T) {
	e, id := rogueEnv(t)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(rolloutAskEvery)
	e.cycle()

	for range 3 {
		e.clock.advance(rolloutAskEvery)
		st := e.cycle()
		require.Contains(t, st.Problems, theirProblem)
		require.Len(t, st.RogueConnectors, 1)
		require.Equal(t, t0.Add(rolloutAskEvery), st.RogueConnectors[0].Since)
	}
	require.Len(t, connectorEvents(e), 1)

	// A cycle that holds before the connectors still shows what is known.
	e.inv.set(incomplete("node pve2 did not answer"))
	e.clock.advance(rolloutAskEvery)
	st := e.cycle()
	require.Contains(t, st.Problems, theirProblem)
	require.Len(t, st.RogueConnectors, 1)
}

func TestAConnectorThatLeftIsNoLongerReported(t *testing.T) {
	e, id := rogueEnv(t)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(rolloutAskEvery)
	e.cycle()

	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector})
	e.clock.advance(rolloutAskEvery)
	st := e.cycle()

	require.Empty(t, st.RogueConnectors)
	require.NotContains(t, st.Problems, theirProblem)
	events := connectorEvents(e)
	require.Len(t, events, 2)
	require.Equal(t, Event{
		At: t0.Add(2 * rolloutAskEvery), Level: "info", Kind: "connector", Subject: "attacker-elsewhere", Tunnel: tunnelName, Account: testAccount,
		Message: "connector attacker-elsewhere no longer serves tunnel pco-abc123 in account acc1",
	}, events[1])
}

// While a connector that is not ours is shown, its tunnel is looked at every
// rolloutAskEvery, so that a rotation shows its effect soon; otherwise with
// the accounts, every zoneRefreshEvery.
func TestTheConnectorsAreListedWithEveryAccountRefresh(t *testing.T) {
	e, id := rogueEnv(t)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector})
	e.clock.advance(rolloutAskEvery)
	e.cycle()
	listings := func() int {
		n := 0
		for _, c := range e.cf.Calls() {
			if strings.HasPrefix(c, "Connectors ") {
				n++
			}
		}
		return n
	}
	before := listings()

	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	for range 8 {
		e.clock.advance(rolloutAskEvery)
		st := e.cycle()
		require.Empty(t, st.RogueConnectors, "the rollout is confirmed; nothing is listed before the accounts are")
	}
	require.Equal(t, before, listings())

	e.clock.advance(zoneRefreshEvery - 8*rolloutAskEvery)
	st := e.cycle()
	require.Len(t, st.RogueConnectors, 1)
	require.Equal(t, before+1, listings())

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, before+1, listings(), "not before rolloutAskEvery")
	e.clock.advance(rolloutAskEvery)
	e.cycle()
	require.Equal(t, before+2, listings())
}

// Without a ready connector of its own on the node pco cannot tell which of
// the connectors Cloudflare lists is its own: one that just came up is listed
// before its /ready says so.
func TestNothingIsComparedWhileOurConnectorIsNotReady(t *testing.T) {
	e, id := rogueEnv(t)
	e.conn.setReady(id, false)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(rolloutAskEvery)

	st := e.cycle()

	require.Empty(t, st.RogueConnectors)
	require.Empty(t, connectorEvents(e))

	e.conn.setReady(id, true)
	e.clock.advance(rolloutAskEvery)
	st = e.cycle()
	require.Empty(t, st.RogueConnectors, "the rollout was confirmed, and the accounts are not due")

	e.clock.advance(zoneRefreshEvery)
	st = e.cycle()
	require.Len(t, st.RogueConnectors, 1)
}

// A connector that restarted calls itself anew; Cloudflare may still list
// the session of the process before for a moment.
func TestTheIDOurConnectorHadInTheCycleBeforeIsOurs(t *testing.T) {
	e, id := rogueEnv(t)
	e.clock.advance(10 * time.Second)
	e.cycle()

	e.conn.setConnectorID(id, "c2-after-a-restart")
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, {ID: "c2-after-a-restart", ConfigVersion: 1, Connections: 1}})
	e.clock.advance(rolloutAskEvery)
	st := e.cycle()

	require.Empty(t, st.RogueConnectors)
	require.Empty(t, connectorEvents(e))
}

func TestAConnectorThatIsNotOursIsReportedInObserveMode(t *testing.T) {
	e, id := rogueEnv(t)
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(zoneRefreshEvery)

	st := e.cycle()

	require.Len(t, st.RogueConnectors, 1)
	require.Contains(t, st.Problems, theirProblem)
}

func TestWhatCloudflareDoesNotSayIsSaidToBeUnknown(t *testing.T) {
	e, id := rogueEnv(t)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, {ID: "bare", ConfigVersion: 1}})
	e.clock.advance(rolloutAskEvery)

	st := e.cycle()

	require.Contains(t, st.Problems, "tunnel pco-abc123 in account acc1 is served by connector bare from an unknown address "+
		"(cloudflared of an unknown version), which pco does not run on this node: it takes a share of the requests to every hostname "+
		"of the tunnel; unless you run it, rotate the tunnel secret with pco tunnel rotate --account acc1")
}

func TestTheConnectorsOfATunnelThatIsGoneAreForgotten(t *testing.T) {
	e, id := rogueEnv(t)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(rolloutAskEvery)
	e.cycle()

	// The cycle makes the tunnel anew, under another id.
	e.cf.SetConnectors(testAccount, id, nil)
	require.NoError(t, e.cf.DeleteTunnel(t.Context(), testAccount, id))
	e.clock.advance(rolloutAskEvery)
	st := e.cycle()

	require.NotEqual(t, id, e.tunnels()[0].ID)
	require.Empty(t, st.RogueConnectors)
	require.NotContains(t, st.Problems, theirProblem)
}

// A connector that dies without saying goodbye stays listed under its old id
// for a while after its successor is up: an id the node's connector had within
// accountsFreshFor is still its own.
func TestAnIDOurConnectorHadWithinTenMinutesIsOurs(t *testing.T) {
	e, id := rogueEnv(t)
	e.clock.advance(10 * time.Second)
	e.cycle()
	e.conn.setConnectorID(id, "c2-after-a-crash")
	// The configuration is not seen running, so the connectors are listed
	// every rolloutAskEvery.
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{
		{ID: ourConnector.ID, Connections: 4}, {ID: "c2-after-a-crash", Connections: 4},
	})

	for range 3 {
		e.clock.advance(rolloutAskEvery)
		st := e.cycle()
		require.Empty(t, st.RogueConnectors)
	}
	require.Empty(t, connectorEvents(e))

	e.clock.advance(accountsFreshFor)
	st := e.cycle()
	require.Len(t, st.RogueConnectors, 1, "an id not seen for longer is not ours")
	require.Equal(t, ourConnector.ID, st.RogueConnectors[0].ID)
}
