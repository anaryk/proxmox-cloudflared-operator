package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

var rotatedSecret = []byte("rotated-at-cloudflare-32-bytes!!")

const refusedProblem = "tunnel pco-abc123 in account acc1: Cloudflare refuses the token its connector runs with; " +
	"pco reads the token again when that starts and every five minutes, and pco tunnel rotate gives the tunnel a new secret"

func (e *env) tokenReads() int {
	n := 0
	for _, c := range e.cf.Calls() {
		if strings.HasPrefix(c, "TunnelToken ") {
			n++
		}
	}
	return n
}

// lastToken is the token the last Ensure gave the connector of a tunnel.
func (e *env) lastToken(id string) string {
	e.t.Helper()
	var last string
	for _, c := range e.conn.ensures() {
		if c.id == id {
			last = c.token
		}
	}
	require.NotEmpty(e.t, last, "no ensure of %s", id)
	return last
}

// The secret of the tunnel was rotated at Cloudflare, as the remedy of a
// stolen token is: the connector on disk has a token Cloudflare no longer
// takes.
func TestTheRunTokenIsReadAgainWithTheAccounts(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	require.Equal(t, cffake.RunToken(testAccount, id), e.lastToken(id))
	require.Equal(t, 1, e.tokenReads())

	require.NoError(t, e.cf.RotateTunnelSecret(t.Context(), testAccount, id, rotatedSecret))
	for range 9 {
		e.clock.advance(rolloutAskEvery)
		e.cycle()
	}
	require.Equal(t, 1, e.tokenReads(), "not before the accounts are listed again")
	require.Equal(t, cffake.RunToken(testAccount, id), e.lastToken(id))

	e.clock.advance(rolloutAskEvery)
	e.cycle()
	require.Equal(t, 2, e.tokenReads())
	require.Equal(t, cffake.RunTokenWith(testAccount, id, rotatedSecret), e.lastToken(id), "Ensure writes it and restarts the connector")
	require.Equal(t, []Event{{
		At: t0.Add(zoneRefreshEvery), Level: "info", Kind: "connector", Subject: tunnelName, Tunnel: tunnelName, Account: testAccount,
		Message: "the run token of tunnel pco-abc123 in account acc1 changed at Cloudflare; its connector restarts with the new one",
	}}, connectorEvents(e))

	e.clock.advance(zoneRefreshEvery)
	e.cycle()
	require.Equal(t, 3, e.tokenReads(), "one read per tunnel and refresh")
	require.Len(t, connectorEvents(e), 1, "the same token again changes nothing")
}

func TestARefusedTokenIsReadAgainWhenTheRefusalStarts(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	require.NoError(t, e.cf.RotateTunnelSecret(t.Context(), testAccount, id, rotatedSecret))
	e.conn.setRefused(id, true)

	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Equal(t, 2, e.tokenReads())
	require.Equal(t, cffake.RunTokenWith(testAccount, id, rotatedSecret), e.lastToken(id))
	require.Contains(t, st.Problems, refusedProblem)

	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, 2, e.tokenReads(), "once when the refusal starts, not in every cycle")

	e.conn.setRefused(id, false)
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.NotContains(t, st.Problems, refusedProblem)
	e.conn.setRefused(id, true)
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.Equal(t, 3, e.tokenReads(), "a refusal that starts again")
}

// In observe mode nothing is done to the connectors, but the refusal shows.
func TestARefusedTokenShowsInObserveMode(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	e.settings(func(s *store.Settings) { s.ObserveOnly = true })
	e.conn.setRefused(id, true)
	ensures := len(e.conn.ensures())

	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems, refusedProblem)
	require.Equal(t, 1, e.tokenReads())
	require.Len(t, e.conn.ensures(), ensures)
}

// tokenFailing fails TunnelToken once it is armed.
type tokenFailing struct {
	cfapi.API
	armed atomic.Bool
}

func (f *tokenFailing) TunnelToken(ctx context.Context, account, id string) (string, error) {
	if f.armed.Load() {
		return "", errors.New("cloudflare api: HTTP 503: unavailable")
	}
	return f.API.TunnelToken(ctx, account, id)
}

func TestATokenThatCannotBeReadAgainLeavesTheConnectorAsItIs(t *testing.T) {
	e := newEnv(t)
	api := &tokenFailing{API: e.cf}
	e.useAPI(testToken, api)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	api.armed.Store(true)
	ensures := len(e.conn.ensures())

	e.clock.advance(zoneRefreshEvery)
	st := e.cycle()

	require.Contains(t, st.Problems, "tunnel pco-abc123 in account acc1: reading its token again: cloudflare api: HTTP 503: unavailable; "+
		"its connector keeps the one it has")
	require.Len(t, e.conn.ensures(), ensures+1, "the connector is still kept running")
	require.Equal(t, cffake.RunToken(testAccount, id), e.lastToken(id))
}

func TestRotateGivesTheTunnelANewSecretAndRestartsItsConnector(t *testing.T) {
	e, id := rogueEnv(t)
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector, theirConnector})
	e.clock.advance(rolloutAskEvery)
	e.cycle()
	n := len(e.cf.Calls())

	res, err := e.eng.RotateTunnel(t.Context(), "")

	require.NoError(t, err)
	require.Equal(t, TunnelRotation{Tunnel: tunnelName, TunnelID: id, Account: testAccount}, res)
	require.Equal(t, []string{"RotateTunnelSecret acc1 " + id, "CleanUpConnections acc1 " + id, "TunnelToken acc1 " + id}, e.callsSince(n))
	token, err := e.cf.TunnelToken(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.NotEqual(t, cffake.RunToken(testAccount, id), token, "a new secret")
	require.Equal(t, token, e.lastToken(id))
	conns, err := e.cf.Connectors(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.Empty(t, conns, "every connector lost its session")
	var admin []string
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Kind == kindAdmin {
			admin = append(admin, ev.Message)
		}
	}
	require.Equal(t, []string{"the secret of tunnel pco-abc123 in account acc1 was rotated: every connector of the tunnel " +
		"was disconnected, and the one on this node restarts with the new token"}, admin)
	require.Len(t, e.eng.trigger, 1, "a cycle is asked for")

	// The connector of the node connects again; the other one cannot.
	e.cf.SetConnectors(testAccount, id, []cfapi.Connector{ourConnector})
	st := e.cycle()
	require.Empty(t, st.RogueConnectors, "the connectors are listed in the next cycle")
	require.NotContains(t, st.Problems, theirProblem)
}

func TestRotateNamesTheAccountWhenThereAreSeveral(t *testing.T) {
	e := newEnv(t)
	e.cf.AddAccount("acc2", "Second")
	e.cf.AddZone("zone2", "example.net", "acc2")
	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080"), guest(102, "web-2", "www.example.net -> :8080")))
	e.enforce()
	e.cycle()

	_, err := e.eng.RotateTunnel(t.Context(), "")
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, "the install has tunnels in accounts acc1 and acc2; name one with --account")

	res, err := e.eng.RotateTunnel(t.Context(), "acc2")
	require.NoError(t, err)
	require.Equal(t, "acc2", res.Account)
	require.Equal(t, e.cf.TunnelsIn("acc2")[0].ID, res.TunnelID)
}

func TestRotateRefusesWhatItCannotDo(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prepare func(e *env)
		account string
		is      error
		says    string
	}{
		{"before the first cycle", func(e *env) { e.enforce() }, "", ErrNotFound, "no tunnel of this install is known"},
		{"another account", func(e *env) { e.enforce(); e.cycle() }, "acc9", ErrNotFound, "no tunnel of this install is known in account acc9"},
		{"observe-only mode", func(e *env) { e.enforce(); e.cycle(); e.settings(func(s *store.Settings) { s.ObserveOnly = true }) }, "",
			ErrRefused, "pco is in observe-only mode and changes nothing at Cloudflare; pco apply ends it"},
		{"a tunnel left as it is", func(e *env) {
			e.enforce()
			e.cycle()
			e.settings(func(s *store.Settings) { s.ZonePins = map[string]string{"example.com": "cred9"} })
			e.clock.advance(10 * time.Second)
			e.cycle()
		}, "", ErrRefused, "tunnel pco-abc123 in account acc1 is left as it is: account frozen"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.prepare(e)
			n := len(e.cf.Calls())

			_, err := e.eng.RotateTunnel(t.Context(), tt.account)

			require.ErrorIs(t, err, tt.is)
			require.ErrorContains(t, err, tt.says)
			require.Empty(t, e.callsSince(n), "nothing is asked of Cloudflare")
		})
	}
}

func TestRotateThatCloudflareRefusesChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	ensures := len(e.conn.ensures())
	e.cf.Deny("tunnel.write")

	_, err := e.eng.RotateTunnel(t.Context(), "")

	require.True(t, cfapi.IsAuth(err), "%v", err)
	require.ErrorContains(t, err, "rotating the secret of tunnel pco-abc123 in account acc1")
	require.Len(t, e.conn.ensures(), ensures)
	token, err := e.cf.TunnelToken(t.Context(), testAccount, id)
	require.NoError(t, err)
	require.Equal(t, cffake.RunToken(testAccount, id), token)
}

// cleanUpFailing fails every clean-up of connections.
type cleanUpFailing struct{ cfapi.API }

func (cleanUpFailing) CleanUpConnections(context.Context, string, string) error {
	return errors.New("cloudflare api: HTTP 500: internal")
}

// The secret is rotated already: the connector of the node gets the new token
// all the same, and the admin is told what is left to do.
func TestRotateWhoseCleanUpFailsStillRestartsTheConnector(t *testing.T) {
	e := newEnv(t)
	e.useAPI(testToken, cleanUpFailing{e.cf})
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID

	_, err := e.eng.RotateTunnel(t.Context(), "")

	require.ErrorContains(t, err, "the secret of tunnel pco-abc123 in account acc1 was rotated and its connector on this node "+
		"restarts with the new token, but the connections of the tunnel could not be ended: cloudflare api: HTTP 500: internal; "+
		"a connector elsewhere keeps its session until it reconnects: run pco tunnel rotate again")
	token, terr := e.cf.TunnelToken(t.Context(), testAccount, id)
	require.NoError(t, terr)
	require.Equal(t, token, e.lastToken(id))
}
