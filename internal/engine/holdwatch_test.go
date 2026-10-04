package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// forgeSentinel writes the configuration of a tunnel as it is, but with the
// sentinel of a newer generation of this install, as a stolen token can.
func forgeSentinel(t *testing.T, e *env, id string, generation int) {
	t.Helper()
	cfg, err := e.cf.TunnelConfig(t.Context(), testAccount, id)
	require.NoError(t, err)
	forged := planner.SentinelHostname(planner.Writer{InstallID: testInstall, Generation: generation, Nonce: "forged"})
	rules := cfg.Ingress
	for i, r := range rules {
		if _, ok := planner.ParseSentinel(r.Hostname); ok {
			rules[i].Hostname = forged
		}
	}
	_, err = e.cf.PutTunnelConfig(t.Context(), testAccount, id, rules)
	require.NoError(t, err)
}

// The H1 attacker holds a token that can write the tunnel's configuration: a
// sentinel of a newer generation makes the writer foreign, and the cycle holds
// before the connectors. The connectors are still held against the node's.
func TestAForgedSentinelHidesNoConnectorThatIsNotOurs(t *testing.T) {
	e, id := rogueEnv(t)
	forgeSentinel(t, e, id, 2)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(zoneRefreshEvery)

	st := e.cycle()

	require.Equal(t, VerdictForeign, st.WriterVerdict)
	require.NotEmpty(t, st.Hold)
	require.Len(t, st.RogueConnectors, 1)
	require.Contains(t, st.Problems, theirProblem)
	require.Len(t, connectorEvents(e), 1)

	// The secret can be rotated all the same: the tunnel's id and credential
	// are known.
	res, err := e.eng.RotateTunnel(t.Context(), "")
	require.NoError(t, err)
	require.Equal(t, id, res.TunnelID)
	conns, err := e.cf.Connectors(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.Empty(t, conns)
}

// A hold before the tunnel run, here an inventory that stays incomplete, does
// not stop the watch: it goes on with the tunnels the last state showed.
func TestALongHoldDoesNotStopTheWatch(t *testing.T) {
	e, id := rogueEnv(t)
	e.inv.set(incomplete("node pve2 did not answer", guest(101, "web-1", "www.example.com -> :8080")))
	for range 3 {
		e.clock.advance(rolloutAskEvery)
		st := e.cycle()
		require.Equal(t, problemIncomplete, st.Hold)
	}

	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()

	require.Equal(t, problemIncomplete, st.Hold)
	require.Len(t, st.RogueConnectors, 1)
	require.Contains(t, st.Problems, theirProblem)
	require.Len(t, st.Connectors, 1, "the status of the connector is read")

	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector})
	e.clock.advance(rolloutAskEvery)
	st = e.cycle()
	require.Empty(t, st.RogueConnectors, "listed again every rolloutAskEvery while one is shown")
}

// The order docs/security.md gives after a stolen token was used, end to
// end: replace the token, rotate the secret, recover the writer, apply, and
// one cycle writes the configuration again.
func TestTheIncidentResponseThatTheDocsGiveWorks(t *testing.T) {
	e, id := rogueEnv(t)
	forgeSentinel(t, e, id, 7)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()
	require.Equal(t, VerdictForeign, st.WriterVerdict)
	require.Len(t, st.RogueConnectors, 1)

	// 1. A new token in a credential of its own, the old one revoked and
	// removed.
	const newToken = "cf-api-token-new-0123456789-do-not-leak"
	e.useAPI(newToken, e.cf)
	require.NoError(t, e.store.SaveCredential(store.Credential{ID: "cred2", Label: "main-2", Kind: "scoped", Token: store.NewSecret(newToken), AddedAt: t0}))
	require.NoError(t, e.store.DeleteCredential(testCred))
	e.clock.advance(10 * time.Second)
	e.cycle()

	// 2. The secret: the other connector loses its session for good.
	_, err := e.eng.RotateTunnel(t.Context(), "")
	require.NoError(t, err)
	require.NotEqual(t, cffake.RunToken(testAccount, id), e.lastToken(id), "the connector of the node got the new token")

	// 3. pco setup --recover: a writer of a generation above every one in the
	// tunnels, and observe-only mode.
	require.NoError(t, e.store.SaveWriter(planner.Writer{InstallID: testInstall, Generation: 8, Nonce: "recovered"}))
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })

	// 4. pco apply, and the next cycle.
	e.apply(false)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector})
	e.clock.advance(time.Minute)
	st = e.cycle()

	require.Equal(t, VerdictOK, st.WriterVerdict)
	require.Empty(t, st.Hold)
	require.Empty(t, st.RogueConnectors)
	cfg, err := e.cf.TunnelConfig(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.Contains(t, cfg.Ingress, planner.IngressRule{
		Hostname: planner.SentinelHostname(planner.Writer{InstallID: testInstall, Generation: 8, Nonce: "recovered"}), Service: "http_status:404",
	})
}
