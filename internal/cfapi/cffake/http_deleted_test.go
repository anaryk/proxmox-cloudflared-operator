package cffake_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
)

// What the handler says of tunnels that were deleted.

const listing = "/client/v4/accounts/" + acct + "/cfd_tunnel"

// deletedFixture is an account with a tunnel that is live and one that was
// deleted a day after it was made.
func deletedFixture(t *testing.T, opts ...cffake.Option) (h http.Handler, live, dead string) {
	t.Helper()
	f := cffake.New()
	f.SetNow(func() time.Time { return t0 })
	std(f)
	live = f.SeedTunnel(acct, "pco-live", nil).ID
	dead = f.SeedTunnel(acct, "pco-dead", nil).ID
	f.SetNow(func() time.Time { return t0.Add(24 * time.Hour) })
	require.NoError(t, f.DeleteTunnel(ctx, acct, dead))
	return cffake.Handler(f, opts...), live, dead
}

func TestDeletedTunnelsAreListedUnlessAskedNot(t *testing.T) {
	h, live, dead := deletedFixture(t)
	tunnel := func(id, name, deleted string) string {
		return `{"id":"` + id + `","account_tag":"acct1","name":"` + name + `","status":"inactive",` +
			`"created_at":"2026-03-04T05:06:07.123456789Z","deleted_at":` + deleted + `,"tun_type":"cfd_tunnel","remote_config":true}`
	}
	liveJSON := tunnel(live, "pco-live", "null")
	deadJSON := tunnel(dead, "pco-dead", `"2026-03-05T05:06:07.123456789Z"`)

	for _, tc := range []struct{ query, want string }{
		{"", `[` + liveJSON + `,` + deadJSON + `]`},
		{"?is_deleted=false", `[` + liveJSON + `]`},
		{"?is_deleted=true", `[` + deadJSON + `]`},
		{"?name=pco-dead", `[` + deadJSON + `]`},
		{"?name=pco-dead&is_deleted=false", `[]`},
		{"?name=pco-dead&is_deleted=true", `[` + deadJSON + `]`},
		{"?include_prefix=pco-", `[` + liveJSON + `,` + deadJSON + `]`},
		{"?include_prefix=pco-&is_deleted=false", `[` + liveJSON + `]`},
		{"?name=pco-live&is_deleted=true", `[]`},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := do(t, h, http.MethodGet, listing+tc.query, "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			a := decode(t, rec)
			require.JSONEq(t, tc.want, string(a.Result))
			require.Contains(t, string(a.ResultInfo), `"total_count":2`, "the total is what the account has, deleted ones too")
		})
	}
}

func TestTheTotalOfAListingMayCountWhatItLeftOut(t *testing.T) {
	h, _, _ := deletedFixture(t, cffake.WithFilteredTunnelTotals())

	for query, total := range map[string]string{"": "2", "?is_deleted=false": "1", "?is_deleted=true": "1", "?name=nothing": "0"} {
		rec := do(t, h, http.MethodGet, listing+query, "")
		require.Contains(t, rec.Body.String(), `"total_count":`+total, query)
	}
}

func TestAServerCanIgnoreIsDeleted(t *testing.T) {
	h, _, _ := deletedFixture(t, cffake.WithIgnoredIsDeleted())

	for _, query := range []string{"", "?is_deleted=false", "?is_deleted=true", "?include_prefix=pco-&is_deleted=false"} {
		a := decode(t, do(t, h, http.MethodGet, listing+query, ""))
		require.Contains(t, string(a.Result), `"name":"pco-live"`, query)
		require.Contains(t, string(a.Result), `"name":"pco-dead"`, query)
		require.Contains(t, string(a.Result), `"deleted_at":"2026-03-05T05:06:07.123456789Z"`, query)
	}
	requireRefused(t, do(t, h, http.MethodGet, listing+"?is_deleted=maybe", ""), http.StatusBadRequest, codeBadRequest, "is_deleted must be")
}

func TestADeletedTunnelIsNoTunnelForTheOtherEndpoints(t *testing.T) {
	h, _, dead := deletedFixture(t)
	tun := listing + "/" + dead
	for _, req := range []struct{ method, path, body string }{
		{"DELETE", tun, ""},
		{"GET", tun + "/token", ""},
		{"GET", tun + "/configurations", ""},
		{"PUT", tun + "/configurations", `{"config":{"ingress":[{"service":"http_status:404"}]}}`},
		{"GET", tun + "/connections", ""},
	} {
		requireRefused(t, do(t, h, req.method, req.path, req.body), http.StatusNotFound, 7003, "not found")
	}

	rec := do(t, h, "POST", listing, `{"name":"pco-dead"}`)
	require.Equal(t, http.StatusOK, rec.Code, "the name is free again: %s", rec.Body.String())
	rec = do(t, h, "GET", listing+"?name=pco-dead", "")
	require.Contains(t, rec.Body.String(), `"total_count":3`)
}

func TestTunnelListingsAskForLiveTunnelsOnly(t *testing.T) {
	fx := newWireFixture()
	var mu sync.Mutex
	var queries []url.Values
	spy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/cfd_tunnel") {
			mu.Lock()
			queries = append(queries, r.URL.Query())
			mu.Unlock()
		}
		fx.h.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(spy)
	t.Cleanup(srv.Close)
	c, err := cfapi.New(cfapi.Options{BaseURL: srv.URL + "/client/v4", Token: token, Limiter: cfapi.NewLimiter(1000, time.Minute, 100, nil)})
	require.NoError(t, err)

	_, _, err = c.FindTunnel(ctx, acct, "pco-abc")
	require.NoError(t, err)
	_, err = c.Tunnels(ctx, acct, "pco-")
	require.NoError(t, err)

	require.Len(t, queries, 2)
	require.Equal(t, "pco-abc", queries[0].Get("name"))
	require.Equal(t, "pco-", queries[1].Get("include_prefix"))
	for _, q := range queries {
		require.Equal(t, "false", q.Get("is_deleted"), "a listing of tunnels asks for the ones that are not deleted: %v", q)
	}
}
