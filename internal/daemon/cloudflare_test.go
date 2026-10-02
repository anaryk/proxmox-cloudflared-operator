package daemon

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestCloudflareClientsSendTheTokenOfTheCredential(t *testing.T) {
	const token = "tok-0123456789"
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"messages":[],"result":{"id":"t1","status":"active"}}`))
	}))
	defer srv.Close()
	f := newCloudflareClients(srv.URL, time.Now)

	api, err := f.New(store.Credential{ID: "cred1", Label: "main", Token: store.NewSecret(token)})
	require.NoError(t, err)
	status, err := api.VerifyToken(t.Context())

	require.NoError(t, err)
	require.Equal(t, "active", status.Status)
	require.Equal(t, "Bearer "+token, auth)
}

func TestACloudflareClientForAnEmptyTokenIsRefused(t *testing.T) {
	_, err := newCloudflareClients("", time.Now).New(store.Credential{ID: "cred1", Label: "main"})
	require.Error(t, err)
}

// rateLimited is a Cloudflare that answers the first request of every test
// with a 429 and counts what reaches it. A client that is told to hold back
// sends nothing until the pause ends, so the count says who shares a budget.
type rateLimited struct {
	srv  *httptest.Server
	hits atomic.Int32
}

func newRateLimited(t *testing.T) *rateLimited {
	t.Helper()
	r := &rateLimited{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":971,"message":"rate limited"}]}`))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// A 429 pauses the limiter of the client that got it. What the pause reaches
// shows what shares a limiter: every other client of the credential is held
// back without sending a thing, and the clients of other credentials are not.
//
// This goes through the factory of the daemon as Run gets it, not through a
// map of it: clients that do not share a limiter, or factories that do not
// share their map, make each of these requests reach the server.
func TestClientsOfOneCredentialShareOneBudget(t *testing.T) {
	cf := newRateLimited(t)
	build := Deps{CloudflareURL: cf.srv.URL}.withDefaults().NewClient
	credential := func(id string) store.Credential {
		return store.Credential{ID: id, Label: id, Token: store.NewSecret("tok-" + id + "-0123456789")}
	}
	asked := func(api cfapi.API) error {
		_, err := api.VerifyToken(t.Context())
		return err
	}
	requireRateLimit := func(err error) {
		t.Helper()
		var apiErr *cfapi.Error
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, http.StatusTooManyRequests, apiErr.Status)
	}

	first, err := build(credential("cred1"))
	require.NoError(t, err)
	requireRateLimit(asked(first))
	require.EqualValues(t, 1, cf.hits.Load(), "the first request was sent, and answered with a 429")

	// Built later, as a cycle that found a new token or a check does.
	second, err := build(credential("cred1"))
	require.NoError(t, err)
	requireRateLimit(asked(second))
	require.EqualValues(t, 1, cf.hits.Load(), "a second client of the credential is held back: nothing was sent")

	other, err := build(credential("cred2"))
	require.NoError(t, err)
	requireRateLimit(asked(other))
	require.EqualValues(t, 2, cf.hits.Load(), "another credential has a budget of its own")

	// A token that is not stored has no id: it is being checked, and its
	// client paces itself, so two of them do not hold each other back.
	unsaved1, err := build(credential(""))
	require.NoError(t, err)
	unsaved2, err := build(credential(""))
	require.NoError(t, err)
	requireRateLimit(asked(unsaved1))
	requireRateLimit(asked(unsaved2))
	require.EqualValues(t, 4, cf.hits.Load())
}

// The daemon reaches Cloudflare through its own clients when it is not given
// others: here the whole way, with the HTTP fake of Cloudflare.
func TestTheDaemonTalksToCloudflareThroughItsOwnClients(t *testing.T) {
	w := newWorld(t)
	fake := httptest.NewServer(cffake.Handler(w.cf, cffake.WithToken(cfToken)))
	t.Cleanup(fake.Close)
	w.deps.NewClient = nil
	w.deps.CloudflareURL = fake.URL + "/client/v4"
	d := w.start()

	require.NoError(t, d.client.Apply(t.Context(), false))
	d.await(func(st engine.State) bool {
		return st.Mode == "enforce" && len(st.Tunnels) == 1 && st.Tunnels[0].Verified
	})

	tunnel, found, err := w.cf.FindTunnel(t.Context(), "acc1", planner.TunnelName(testInstall))
	require.NoError(t, err)
	require.True(t, found, "the tunnel was made at the fake, through the real client")
	require.Equal(t, tunnelID, tunnel.ID)
	require.NotContains(t, w.logs.String(), cfToken)
}

func TestARefusedTokenIsNotTheOneOfTheStore(t *testing.T) {
	// The fake accepts only the token of the store: a client with another
	// token is refused, so the check above is not passing by accident.
	w := newWorld(t)
	fake := httptest.NewServer(cffake.Handler(w.cf, cffake.WithToken("another-token-0123456789")))
	t.Cleanup(fake.Close)
	build := Deps{CloudflareURL: fake.URL + "/client/v4"}.withDefaults().NewClient

	api, err := build(store.Credential{ID: testCred, Label: "main", Token: store.NewSecret(cfToken)})
	require.NoError(t, err)
	_, err = api.Zones(t.Context())

	require.True(t, cfapi.IsAuth(err), "the fake refuses a token it does not know: %v", err)
}
