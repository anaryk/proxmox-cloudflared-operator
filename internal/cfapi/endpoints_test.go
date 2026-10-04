package cfapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// only returns the one request the test server saw.
func only(t *testing.T, env *testEnv) seenRequest {
	t.Helper()
	reqs := env.requests()
	require.Len(t, reqs, 1)
	return reqs[0]
}

// path and query split the request URI of a seen request.
func (r seenRequest) path(t *testing.T) string {
	t.Helper()
	u, err := url.ParseRequestURI(r.uri)
	require.NoError(t, err)
	return u.Path
}

func (r seenRequest) query(t *testing.T) url.Values {
	t.Helper()
	u, err := url.ParseRequestURI(r.uri)
	require.NoError(t, err)
	return u.Query()
}

func ts(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return v
}

func TestVerifyToken(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(
		`{"id":"tok1","status":"active","not_before":"2026-01-01T00:00:00Z","expires_on":"2027-03-04T05:06:07Z"}`)))

	got, err := env.c.VerifyToken(context.Background())

	require.NoError(t, err)
	expires := ts(t, "2027-03-04T05:06:07Z")
	require.Equal(t, TokenStatus{ID: "tok1", Status: "active", ExpiresOn: &expires}, got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/user/tokens/verify", req.uri)
	require.Empty(t, req.body)
}

func TestVerifyTokenWithoutExpiry(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{"id":"tok1","status":"disabled"}`)))

	got, err := env.c.VerifyToken(context.Background())

	require.NoError(t, err)
	require.Equal(t, TokenStatus{ID: "tok1", Status: "disabled"}, got)
	require.Nil(t, got.ExpiresOn)
}

func TestVerifyTokenFailures(t *testing.T) {
	tests := []struct {
		name string
		h    http.HandlerFunc
		auth bool
	}{
		{"rejected token", reply(http.StatusUnauthorized, `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`), true},
		{"no result", reply(http.StatusOK, okBody(`null`)), false},
		{"no status", reply(http.StatusOK, okBody(`{"id":"tok1"}`)), false},
		{"bad expiry", reply(http.StatusOK, okBody(`{"id":"tok1","status":"active","expires_on":"soon"}`)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, tt.h)
			got, err := env.c.VerifyToken(context.Background())
			require.Error(t, err)
			require.Equal(t, tt.auth, IsAuth(err))
			require.Equal(t, TokenStatus{}, got)
		})
	}
}

const rejectedToken = `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}]}`

// tokenServer answers the user form of the token check with user, the listing
// of the accounts with the accounts of ids, and the account form of each
// account with what byAccount holds for it, or a rejection.
func tokenServer(user http.HandlerFunc, byAccount map[string]http.HandlerFunc, ids ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/client/v4")
		switch path {
		case "/user/tokens/verify":
			user(w, r)
		case "/accounts":
			items := make([]string, len(ids))
			for i, id := range ids {
				items[i] = fmt.Sprintf(`{"id":%q,"name":"Account %s"}`, id, id)
			}
			reply(http.StatusOK, okBody(`[`+strings.Join(items, ",")+`]`))(w, r)
		default:
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/tokens/verify")
			if h, ok := byAccount[id]; ok {
				h(w, r)
				return
			}
			reply(http.StatusUnauthorized, rejectedToken)(w, r)
		}
	}
}

func requestedPaths(t *testing.T, env *testEnv) []string {
	t.Helper()
	var paths []string
	for _, r := range env.requests() {
		paths = append(paths, strings.TrimPrefix(r.path(t), "/client/v4"))
	}
	return paths
}

func TestVerifyAccountOwnedToken(t *testing.T) {
	active := reply(http.StatusOK, okBody(`{"id":"tok9","status":"active"}`))
	env := setup(t, tokenServer(reply(http.StatusUnauthorized, rejectedToken), map[string]http.HandlerFunc{"a2": active, "a3": active}, "a1", "a2", "a3"))

	got, err := env.c.VerifyToken(context.Background())

	require.NoError(t, err)
	require.Equal(t, TokenStatus{ID: "tok9", Status: "active"}, got)
	require.Equal(t, []string{
		"/user/tokens/verify",
		"/accounts",
		"/accounts/a1/tokens/verify",
		"/accounts/a2/tokens/verify",
	}, requestedPaths(t, env), "the first account that verifies it is the answer")
}

func TestVerifyTokenTriesTheAccountFormOnAnyRefusal(t *testing.T) {
	// What Cloudflare answers an account-owned token at the user form is not
	// known for certain, so every answer that is not a 429 counts.
	for name, user := range map[string]http.HandlerFunc{
		"401":                         reply(http.StatusUnauthorized, rejectedToken),
		"403":                         reply(http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`),
		"400 with an auth code":       reply(http.StatusBadRequest, `{"success":false,"errors":[{"code":6003,"message":"Invalid request headers"}]}`),
		"404":                         reply(http.StatusNotFound, ``),
		"success false":               reply(http.StatusOK, `{"success":false,"errors":[{"code":1001,"message":"no"}]}`),
		"no status":                   reply(http.StatusOK, okBody(`{"id":"tok1"}`)),
		"an answer that is not JSON":  reply(http.StatusOK, `<html></html>`),
		"a result of the wrong shape": reply(http.StatusOK, okBody(`{"id":"tok1","status":"active","expires_on":"soon"}`)),
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, tokenServer(user, map[string]http.HandlerFunc{
				"a1": reply(http.StatusOK, okBody(`{"id":"tok9","status":"active"}`)),
			}, "a1"))

			got, err := env.c.VerifyToken(context.Background())

			require.NoError(t, err)
			require.Equal(t, "tok9", got.ID)
			require.Len(t, env.requests(), 3)
		})
	}
}

func TestVerifyTokenKeepsToTheUserFormWhenItCannotTell(t *testing.T) {
	for name, user := range map[string]http.HandlerFunc{
		"rate limited":        reply(http.StatusTooManyRequests, `{"success":false,"errors":[{"code":971,"message":"slow down"}]}`),
		"no answer":           func(w http.ResponseWriter, _ *http.Request) { dropConnection(w) },
		"cut short":           dropMidBody(http.StatusOK, ""),
		"a server error":      reply(http.StatusInternalServerError, `{"success":false,"errors":[{"code":1000,"message":"boom"}]}`),
		"a gateway":           reply(http.StatusBadGateway, `bad gateway`),
		"service unavailable": reply(http.StatusServiceUnavailable, ``),
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, tokenServer(user, nil, "a1"))

			_, err := env.c.VerifyToken(context.Background())

			require.Error(t, err)
			require.False(t, IsAuth(err))
			require.NotContains(t, err.Error(), "account token")
			require.Equal(t, []string{"/user/tokens/verify"}, requestedPaths(t, env))
		})
	}
}

func TestVerifyTokenFailsBothWays(t *testing.T) {
	user := reply(http.StatusUnauthorized, rejectedToken)
	tests := []struct {
		name        string
		h           http.HandlerFunc
		says        string // about the account form
		auth        bool
		rateLimited bool
		askedA2     bool
	}{
		{"every account rejects it", tokenServer(user, nil, "a1", "a2"),
			"as an account token: no account it sees verifies it: account a1: cloudflare api: HTTP 401: Invalid API Token (codes 1000); account a2: cloudflare api: HTTP 401",
			true, false, true},
		{"it sees no account", tokenServer(user, nil),
			"as an account token: it sees no account", true, false, false},
		{"the accounts cannot be listed", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/client/v4/accounts" {
				reply(http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`)(w, r)
				return
			}
			user(w, r)
		}, "as an account token: listing accounts", true, false, false},
		{"the account form is rate limited", tokenServer(user, map[string]http.HandlerFunc{
			"a1": reply(http.StatusTooManyRequests, `{"success":false,"errors":[{"code":971,"message":"slow down"}]}`),
		}, "a1", "a2"), "as an account token: account a1: cloudflare api: HTTP 429", false, true, false},
		{"the account form gets no answer", tokenServer(user, map[string]http.HandlerFunc{
			"a1": func(w http.ResponseWriter, _ *http.Request) { dropConnection(w) },
		}, "a1", "a2"), "as an account token: account a1: sending request", false, false, false},
		{"the account form fails on the server", tokenServer(user, map[string]http.HandlerFunc{
			"a1": reply(http.StatusServiceUnavailable, ``),
		}, "a1", "a2"), "as an account token: account a1: cloudflare api: HTTP 503", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, tt.h)

			got, err := env.c.VerifyToken(context.Background())

			require.Equal(t, TokenStatus{}, got)
			require.ErrorContains(t, err, "verifying token: as a user token: cloudflare api: HTTP 401: Invalid API Token")
			require.ErrorContains(t, err, tt.says)
			require.Equal(t, tt.auth, IsAuth(err), "IsAuth")
			require.Equal(t, tt.rateLimited, IsRateLimited(err), "IsRateLimited")
			require.Equal(t, tt.askedA2, slices.Contains(requestedPaths(t, env), "/accounts/a2/tokens/verify"),
				"an account that could not answer stops the search")
		})
	}
}

func TestVerifyTokenTriesAtMostFiveAccounts(t *testing.T) {
	ids := make([]string, 40)
	for i := range ids {
		ids[i] = fmt.Sprintf("a%02d", i)
	}
	long := `{"success":false,"errors":[{"code":1000,"message":"` + strings.Repeat("no ", 160) + `"}]}`
	env := setup(t, tokenServer(reply(http.StatusUnauthorized, rejectedToken), map[string]http.HandlerFunc{
		"a00": reply(http.StatusUnauthorized, long),
		"a01": reply(http.StatusUnauthorized, long),
		"a02": reply(http.StatusUnauthorized, long),
		"a03": reply(http.StatusUnauthorized, long),
		"a04": reply(http.StatusUnauthorized, long),
		"a05": reply(http.StatusOK, okBody(`{"id":"tok9","status":"active"}`)),
	}, ids...))

	got, err := env.c.VerifyToken(context.Background())

	require.Equal(t, TokenStatus{}, got, "the sixth account is not asked")
	require.True(t, IsAuth(err))
	require.Equal(t, []string{
		"/user/tokens/verify", "/accounts",
		"/accounts/a00/tokens/verify", "/accounts/a01/tokens/verify", "/accounts/a02/tokens/verify",
		"/accounts/a03/tokens/verify", "/accounts/a04/tokens/verify",
	}, requestedPaths(t, env))
	require.ErrorContains(t, err, "as an account token: none of the first 5 of its 40 accounts verifies it: account a00: cloudflare api: HTTP 401: no no")
	require.LessOrEqual(t, len(err.Error()), 1300, "the refusals are cut short: %d bytes", len(err.Error()))
	require.True(t, utf8.ValidString(err.Error()))
	require.NotContains(t, err.Error(), "account a04", "the message is cut before the last refusal")
}

func TestVerifyTokenSkipsAnAccountIDThatCannotBeAPath(t *testing.T) {
	env := setup(t, tokenServer(reply(http.StatusUnauthorized, rejectedToken), map[string]http.HandlerFunc{
		"a1": reply(http.StatusOK, okBody(`{"id":"tok9","status":"active"}`)),
	}, "..", "a1"))

	got, err := env.c.VerifyToken(context.Background())

	require.NoError(t, err)
	require.Equal(t, "tok9", got.ID)
	require.Equal(t, []string{"/user/tokens/verify", "/accounts", "/accounts/a1/tokens/verify"}, requestedPaths(t, env))
}

func TestAccounts(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(
		`[{"id":"a1","name":"First","type":"standard","settings":{}},{"id":"a2","name":"Second"}]`)))

	got, err := env.c.Accounts(context.Background())

	require.NoError(t, err)
	require.Equal(t, []Account{{ID: "a1", Name: "First"}, {ID: "a2", Name: "Second"}}, got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/accounts", req.path(t))
	require.Equal(t, url.Values{"page": {"1"}, "per_page": {"50"}}, req.query(t))
}

func TestAccountsRejectsAnItemWithoutID(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[{"id":"a1","name":"First"},{"name":"Nameless"}]`)))
	got, err := env.c.Accounts(context.Background())
	require.Error(t, err)
	require.Nil(t, got)
}

func TestZones(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[
		{"id":"z1","name":"example.com","status":"active","account":{"id":"a1","name":"First"},"name_servers":["x"]},
		{"id":"z2","name":"example.org","status":"pending","account":{"id":"a2","name":"Second"}}]`)))

	got, err := env.c.Zones(context.Background())

	require.NoError(t, err)
	require.Equal(t, []Zone{
		{ID: "z1", Name: "example.com", Status: "active", AccountID: "a1"},
		{ID: "z2", Name: "example.org", Status: "pending", AccountID: "a2"},
	}, got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/zones", req.path(t))
	require.Equal(t, url.Values{"page": {"1"}, "per_page": {"50"}}, req.query(t))
}

func TestZonesAreNotShortenedByAFailedPage(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			reply(http.StatusBadGateway, `bad gateway`)(w, r)
			return
		}
		reply(http.StatusOK, `{"success":true,"result":[{"id":"z1","name":"example.com","status":"active","account":{"id":"a1"}}],`+
			`"result_info":{"page":1,"per_page":100,"total_pages":2}}`)(w, r)
	})

	got, err := env.c.Zones(context.Background())

	require.Error(t, err)
	require.Nil(t, got)
}

func TestZonesRejectsAnItemWithoutIdentity(t *testing.T) {
	for _, item := range []string{
		`{"name":"example.com","status":"active","account":{"id":"a1"}}`,
		`{"id":"z1","status":"active","account":{"id":"a1"}}`,
		`{"id":"z1","name":"example.com","status":"active"}`,
		`{"id":"z1","name":"example.com","status":"active","account":{"name":"First"}}`,
	} {
		env := setup(t, reply(http.StatusOK, okBody(`[`+item+`]`)))
		got, err := env.c.Zones(context.Background())
		require.Error(t, err, item)
		require.Nil(t, got)
	}
}

const tunnelJSON = `{"id":"11111111-2222-4333-8444-555555555555","name":"pco-abc","status":"healthy",` +
	`"created_at":"2026-02-03T04:05:06.789Z","deleted_at":null,"connections":[]}`

func wantTunnel(t *testing.T) Tunnel {
	t.Helper()
	return Tunnel{
		ID:        "11111111-2222-4333-8444-555555555555",
		Name:      "pco-abc",
		Status:    "healthy",
		CreatedAt: ts(t, "2026-02-03T04:05:06.789Z"),
	}
}

func TestFindTunnel(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[`+tunnelJSON+`]`)))

	got, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, wantTunnel(t), got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel", req.path(t))
	require.Equal(t, url.Values{
		"name": {"pco-abc"}, "is_deleted": {"false"}, "page": {"1"}, "per_page": {"50"},
	}, req.query(t))
}

func TestFindTunnelNone(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[]`)))

	got, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

	require.NoError(t, err)
	require.False(t, found)
	require.Equal(t, Tunnel{}, got)
}

func TestFindTunnelWantsTheExactNameThatIsNotDeleted(t *testing.T) {
	// The name filter of the API matches more than the name, and a server that
	// ignored the deleted filter must not make a dead tunnel the live one.
	env := setup(t, reply(http.StatusOK, okBody(`[
		{"id":"id-long","name":"pco-abc-probe-1","status":"inactive","created_at":"2026-02-03T04:05:06Z"},
		{"id":"id-case","name":"PCO-ABC","status":"inactive","created_at":"2026-02-03T04:05:06Z"},
		{"id":"id-dead","name":"pco-abc","status":"inactive","created_at":"2026-02-03T04:05:06Z","deleted_at":"2026-02-04T00:00:00Z"},
		`+tunnelJSON+`]`)))

	got, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, wantTunnel(t), got)
}

func TestFindTunnelTakesAZeroDeletionTimeForNone(t *testing.T) {
	live := strings.Replace(tunnelJSON, `"deleted_at":null`, `"deleted_at":"0001-01-01T00:00:00Z"`, 1)
	env := setup(t, reply(http.StatusOK, okBody(`[`+live+`]`)))

	got, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, wantTunnel(t), got)
}

func TestFindTunnelOnlyOtherNames(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[{"id":"id-long","name":"pco-abc-probe-1","status":"inactive"}]`)))

	_, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

	require.NoError(t, err)
	require.False(t, found)
}

func TestFindTunnelSeveralMatchesIsAnError(t *testing.T) {
	second := strings.Replace(tunnelJSON, "11111111-2222-4333-8444-555555555555", "99999999-2222-4333-8444-555555555555", 1)
	env := setup(t, reply(http.StatusOK, okBody(`[`+tunnelJSON+`,`+second+`]`)))

	got, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

	require.ErrorContains(t, err, "pco-abc")
	require.False(t, found)
	require.Equal(t, Tunnel{}, got)
}

func TestFindTunnelFailsWholeOnAFailedPage(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			reply(http.StatusInternalServerError, `{"success":false,"errors":[{"code":1000,"message":"boom"}]}`)(w, r)
			return
		}
		// A full page of other names: the one asked for may be on the next.
		reply(http.StatusOK, tunnelPage(named("pco-abc-x", 100), `{"page":1,"per_page":100,"count":100,"total_count":120}`))(w, r)
	})

	_, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")

	require.Error(t, err)
	require.False(t, found, "an incomplete listing is not proof that the tunnel is missing")
}

func TestFindTunnelMatchWithoutID(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[{"name":"pco-abc","status":"inactive"}]`)))
	_, found, err := env.c.FindTunnel(context.Background(), "a1", "pco-abc")
	require.Error(t, err)
	require.False(t, found)
}

func TestTunnels(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[
		{"id":"id-1","name":"pco-abc_probe_1","status":"inactive","created_at":"2026-02-03T04:05:06Z"},
		{"id":"id-dead","name":"pco-abc_probe_2","status":"inactive","created_at":"2026-02-03T04:05:06Z","deleted_at":"2026-02-04T00:00:00Z"},
		{"id":"id-3","name":"pco-abc_probe_3","status":"healthy","created_at":"2026-02-05T04:05:06Z","deleted_at":null}]`)))

	got, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")

	require.NoError(t, err)
	require.Equal(t, []Tunnel{
		{ID: "id-1", Name: "pco-abc_probe_1", Status: "inactive", CreatedAt: ts(t, "2026-02-03T04:05:06Z")},
		{ID: "id-3", Name: "pco-abc_probe_3", Status: "healthy", CreatedAt: ts(t, "2026-02-05T04:05:06Z")},
	}, got, "a deleted tunnel is none")
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel", req.path(t))
	require.Equal(t, url.Values{
		"include_prefix": {"pco-abc_probe_"}, "is_deleted": {"false"}, "page": {"1"}, "per_page": {"50"},
	}, req.query(t))
}

func TestTunnelsNone(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[]`)))
	got, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestTunnelsBadAnswers(t *testing.T) {
	for name, item := range map[string]string{
		"a name outside the prefix":  `{"id":"id-1","name":"pco-abc-node1","status":"inactive"}`,
		"the prefix in another case": `{"id":"id-1","name":"PCO-ABC_probe_1","status":"inactive"}`,
		"a tunnel without an id":     `{"name":"pco-abc_probe_1","status":"inactive"}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`[{"id":"id-0","name":"pco-abc_probe_0"},`+item+`]`)))
			got, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")
			require.ErrorIs(t, err, errUnexpected)
			require.Nil(t, got, "nothing of a listing that cannot be trusted")
		})
	}
}

func TestTunnelsFailWholeOnAFailedPage(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			reply(http.StatusBadGateway, `bad gateway`)(w, r)
			return
		}
		reply(http.StatusOK, tunnelPage(named("pco-abc_probe_", 50), `{"page":1,"per_page":50,"count":50,"total_count":120}`))(w, r)
	})
	got, err := env.c.Tunnels(context.Background(), "a1", "pco-abc_probe_")
	require.Error(t, err)
	require.Nil(t, got)
}

func TestCreateTunnel(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(tunnelJSON)))

	got, err := env.c.CreateTunnel(context.Background(), "a1", "pco-abc")

	require.NoError(t, err)
	require.Equal(t, wantTunnel(t), got)
	req := only(t, env)
	require.Equal(t, http.MethodPost, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel", req.uri)
	require.Equal(t, "application/json", req.contentType)
	require.JSONEq(t, `{"name":"pco-abc","config_src":"cloudflare"}`, req.body)
}

func TestCreateTunnelNameTaken(t *testing.T) {
	env := setup(t, reply(http.StatusConflict,
		`{"success":false,"errors":[{"code":1013,"message":"tunnel with name already exists"}]}`))

	got, err := env.c.CreateTunnel(context.Background(), "a1", "pco-abc")

	require.True(t, IsConflict(err))
	require.Equal(t, Tunnel{}, got)
}

func TestCreateTunnelAnswerWithoutID(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{"name":"pco-abc","status":"inactive"}`)))
	got, err := env.c.CreateTunnel(context.Background(), "a1", "pco-abc")
	require.Error(t, err)
	require.Equal(t, Tunnel{}, got)
}

func TestDeleteTunnel(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(tunnelJSON)))

	require.NoError(t, env.c.DeleteTunnel(context.Background(), "a1", "t1"))

	req := only(t, env)
	require.Equal(t, http.MethodDelete, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel/t1", req.uri)
	require.Empty(t, req.body)
}

func TestDeleteTunnelFailure(t *testing.T) {
	env := setup(t, reply(http.StatusNotFound, `{"success":false,"errors":[{"code":1003,"message":"no such tunnel"}]}`))
	err := env.c.DeleteTunnel(context.Background(), "a1", "t1")
	require.True(t, IsNotFound(err))
}

func TestTunnelToken(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`"eyJhIjoiYSJ9"`)))

	got, err := env.c.TunnelToken(context.Background(), "a1", "t1")

	require.NoError(t, err)
	require.Equal(t, "eyJhIjoiYSJ9", got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel/t1/token", req.uri)
}

func TestTunnelTokenWithoutAToken(t *testing.T) {
	for name, body := range map[string]string{
		"null":   okBody(`null`),
		"empty":  okBody(`""`),
		"object": okBody(`{"token":"x"}`),
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, body))
			got, err := env.c.TunnelToken(context.Background(), "a1", "t1")
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}

func TestRotateTunnelSecret(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(tunnelJSON)))
	secret := []byte("0123456789abcdef0123456789abcdef")

	err := env.c.RotateTunnelSecret(context.Background(), "a1", "11111111-2222-4333-8444-555555555555", secret)

	require.NoError(t, err)
	req := only(t, env)
	require.Equal(t, http.MethodPatch, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel/11111111-2222-4333-8444-555555555555", req.uri)
	require.Equal(t, "application/json", req.contentType)
	require.JSONEq(t, `{"tunnel_secret":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}`, req.body)
}

func TestRotateTunnelSecretAnswers(t *testing.T) {
	secret := make([]byte, 32)
	for name, tt := range map[string]struct {
		h  http.HandlerFunc
		is func(error) bool
	}{
		"another tunnel":  {reply(http.StatusOK, okBody(`{"id":"99999999-2222-4333-8444-555555555555","name":"x"}`)), func(err error) bool { return err != nil }},
		"no result":       {reply(http.StatusOK, okBody(`null`)), func(err error) bool { return err != nil }},
		"refused":         {reply(http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`), IsAuth},
		"no such tunnel":  {reply(http.StatusNotFound, `{"success":false,"errors":[{"code":1003,"message":"no such tunnel"}]}`), IsNotFound},
		"a short secret":  {reply(http.StatusBadRequest, `{"success":false,"errors":[{"code":1001,"message":"secret too short"}]}`), func(err error) bool { return err != nil }},
		"rate limited":    {reply(http.StatusTooManyRequests, `{"success":false,"errors":[]}`), IsRateLimited},
		"a server failed": {reply(http.StatusBadGateway, `{"success":false,"errors":[]}`), func(err error) bool { return err != nil }},
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, tt.h)
			err := env.c.RotateTunnelSecret(context.Background(), "a1", "11111111-2222-4333-8444-555555555555", secret)
			require.True(t, tt.is(err), "%v", err)
		})
	}
}

// Cloudflare wants 32 bytes at least; a shorter secret never leaves.
func TestRotateTunnelSecretRefusesAShortSecret(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(tunnelJSON)))

	err := env.c.RotateTunnelSecret(context.Background(), "a1", "t1", make([]byte, 31))

	require.ErrorIs(t, err, ErrInvalidArgument)
	require.Empty(t, env.requests())
}

func TestCleanUpConnections(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`null`)))

	require.NoError(t, env.c.CleanUpConnections(context.Background(), "a1", "t1"))

	req := only(t, env)
	require.Equal(t, http.MethodDelete, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel/t1/connections", req.uri)
	require.Empty(t, req.body)
}

func TestCleanUpConnectionsFailure(t *testing.T) {
	env := setup(t, reply(http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
	require.True(t, IsAuth(env.c.CleanUpConnections(context.Background(), "a1", "t1")))
}

// ruleCases pair the wire form of an ingress rule with the rule it maps to.
var ruleCases = []struct {
	name string
	wire string
	rule planner.IngressRule
}{
	{
		"plain host",
		`{"hostname":"app.example.com","service":"http://10.0.0.5:8080"}`,
		planner.IngressRule{Hostname: "app.example.com", Service: "http://10.0.0.5:8080"},
	},
	{
		"catch-all has no hostname",
		`{"service":"http_status:404"}`,
		planner.IngressRule{Service: "http_status:404"},
	},
	{
		"blocked host",
		`{"hostname":"held.example.com","service":"http_status:503"}`,
		planner.IngressRule{Hostname: "held.example.com", Service: "http_status:503"},
	},
	{
		"origin server name",
		`{"hostname":"app.example.com","service":"https://10.0.0.5:8443","originRequest":{"originServerName":"app.example.com"}}`,
		planner.IngressRule{Hostname: "app.example.com", Service: "https://10.0.0.5:8443", OriginServerName: "app.example.com"},
	},
	{
		"match sni to host",
		`{"hostname":"*.example.com","service":"https://10.0.0.5:8443","originRequest":{"matchSNItoHost":true}}`,
		planner.IngressRule{Hostname: "*.example.com", Service: "https://10.0.0.5:8443", MatchSNIToHost: true},
	},
	{
		"no tls verify",
		`{"hostname":"app.example.com","service":"https://10.0.0.5:8443","originRequest":{"noTLSVerify":true}}`,
		planner.IngressRule{Hostname: "app.example.com", Service: "https://10.0.0.5:8443", NoTLSVerify: true},
	},
	{
		"host header",
		`{"hostname":"app.example.com","service":"http://10.0.0.5:8080","originRequest":{"httpHostHeader":"internal.lan"}}`,
		planner.IngressRule{Hostname: "app.example.com", Service: "http://10.0.0.5:8080", HTTPHostHeader: "internal.lan"},
	},
	{
		"every option",
		`{"hostname":"app.example.com","service":"https://10.0.0.5:8443","originRequest":` +
			`{"originServerName":"sni.lan","matchSNItoHost":true,"noTLSVerify":true,"httpHostHeader":"internal.lan"}}`,
		planner.IngressRule{
			Hostname: "app.example.com", Service: "https://10.0.0.5:8443",
			OriginServerName: "sni.lan", MatchSNIToHost: true, NoTLSVerify: true, HTTPHostHeader: "internal.lan",
		},
	},
}

func ruleCaseWires() string {
	wires := make([]string, len(ruleCases))
	for i, tc := range ruleCases {
		wires[i] = tc.wire
	}
	return strings.Join(wires, ",")
}

func ruleCaseRules() []planner.IngressRule {
	rules := make([]planner.IngressRule, len(ruleCases))
	for i, tc := range ruleCases {
		rules[i] = tc.rule
	}
	return rules
}

func configBody(version int, ingress string) string {
	return fmt.Sprintf(`{"tunnel_id":"t1","version":%d,"source":"cloudflare","config":{"ingress":[%s]},"created_at":"2026-01-01T00:00:00Z"}`,
		version, ingress)
}

func TestTunnelConfig(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(configBody(7, ruleCaseWires()))))

	got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")

	require.NoError(t, err)
	require.Equal(t, TunnelConfig{Version: 7, Ingress: ruleCaseRules()}, got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel/t1/configurations", req.uri)
	require.Empty(t, req.body)
}

func TestTunnelConfigRules(t *testing.T) {
	for _, tc := range ruleCases {
		t.Run(tc.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(configBody(1, tc.wire))))
			got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")
			require.NoError(t, err)
			require.Equal(t, []planner.IngressRule{tc.rule}, got.Ingress)
		})
	}
}

func TestTunnelConfigForeign(t *testing.T) {
	const rule = `{"hostname":"app.example.com","service":"http://10.0.0.5:8080"}`
	tests := []struct {
		name   string
		result string
		want   bool
	}{
		{"nothing but what pco manages", configBody(2, ruleCaseWires()), false},
		{"no configuration", `{"version":2,"config":null}`, false},
		{"empty configuration", `{"version":2,"config":{}}`, false},
		// Cloudflare sets warp-routing itself, from the routes of the tunnel,
		// and a write cannot clear it: it must never count as drift.
		{"warp routing on", `{"version":2,"config":{"ingress":[` + rule + `],"warp-routing":{"enabled":true}}}`, false},
		{"warp routing off", `{"version":2,"config":{"ingress":[` + rule + `],"warp-routing":{"enabled":false}}}`, false},
		{"warp routing with anything else", `{"version":2,"config":{"ingress":[` + rule + `],"warp-routing":{"enabled":true,"x":[1]}}}`, false},
		{"warp routing next to a foreign key", `{"version":2,"config":{"ingress":[` + rule + `],"warp-routing":{"enabled":true},"x":1}}`, true},
		{"warp routing empty", `{"version":2,"config":{"ingress":[` + rule + `],"warp-routing":{}}}`, false},
		{"top level origin request empty", `{"version":2,"config":{"ingress":[` + rule + `],"originRequest":{}}}`, false},
		{"top level origin request of defaults", `{"version":2,"config":{"ingress":[` + rule + `],"originRequest":{"noTLSVerify":false,"connectTimeout":0}}}`, false},
		{"top level key that is null", `{"version":2,"config":{"ingress":[` + rule + `],"x":null}}`, false},
		{"rule path empty", `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","path":"","service":"http://10.0.0.5:80"}]}}`, false},
		{"rule origin request of defaults", `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","service":"http://10.0.0.5:80",` +
			`"originRequest":{"noTLSVerify":false,"connectTimeout":0,"access":{"required":false,"teamName":"","audTag":[]}}}]}}`, false},
		{"mapped options are not foreign", `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","service":"https://10.0.0.5:443",` +
			`"originRequest":{"noTLSVerify":true,"originServerName":"x","matchSNItoHost":true,"httpHostHeader":"h"}}]}}`, false},

		{"rule path", `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","path":"/api","service":"http://10.0.0.5:80"}]}}`, true},
		{"rule option pco does not map", `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","service":"http://10.0.0.5:80",` +
			`"originRequest":{"connectTimeout":30}}]}}`, true},
		{"rule option that is a block", `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","service":"http://10.0.0.5:80",` +
			`"originRequest":{"access":{"required":true,"teamName":"team"}}}]}}`, true},
		{"rule option next to mapped ones", `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","service":"http://10.0.0.5:80",` +
			`"originRequest":{"httpHostHeader":"h","noHappyEyeballs":true}}]}}`, true},
		{"rule field of a later rule", `{"version":2,"config":{"ingress":[` + rule + `,{"hostname":"b.example.com","service":"http://10.0.0.6:80","x":1},{"service":"http_status:404"}]}}`, true},
		{"top level origin request", `{"version":2,"config":{"ingress":[` + rule + `],"originRequest":{"noTLSVerify":true}}}`, true},
		{"unknown top level key", `{"version":2,"config":{"ingress":[` + rule + `],"x":1}}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(tt.result)))

			got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")

			require.NoError(t, err)
			require.Equal(t, tt.want, got.Foreign)
			require.Equal(t, 2, got.Version)
		})
	}
}

func TestTunnelConfigForeignStillShowsWhatPcoManages(t *testing.T) {
	wire := `{"hostname":"app.example.com","path":"/api","service":"http://10.0.0.5:8080","originRequest":` +
		`{"connectTimeout":30,"noTLSVerify":false,"httpHostHeader":"internal.lan"}}`
	env := setup(t, reply(http.StatusOK, okBody(configBody(2, wire))))

	got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")

	require.NoError(t, err)
	require.True(t, got.Foreign)
	require.Equal(t, []planner.IngressRule{
		{Hostname: "app.example.com", Service: "http://10.0.0.5:8080", HTTPHostHeader: "internal.lan"},
	}, got.Ingress)
}

func TestTunnelConfigDoesNotFitTheShape(t *testing.T) {
	for name, result := range map[string]string{
		"config is a list":          `{"version":2,"config":[]}`,
		"ingress is an object":      `{"version":2,"config":{"ingress":{}}}`,
		"rule is a string":          `{"version":2,"config":{"ingress":["x"]}}`,
		"rule is null":              `{"version":2,"config":{"ingress":[null]}}`,
		"null among good rules":     `{"version":2,"config":{"ingress":[{"hostname":"a.example.com","service":"http://10.0.0.5:80"},null,{"service":"http_status:404"}]}}`,
		"origin request is a list":  `{"version":2,"config":{"ingress":[{"service":"x","originRequest":[]}]}}`,
		"option has the wrong type": `{"version":2,"config":{"ingress":[{"service":"x","originRequest":{"noTLSVerify":"yes"}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(result)))
			got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")
			require.Error(t, err)
			require.Equal(t, TunnelConfig{}, got)
		})
	}
}

func TestConfigurationAnswerWithoutAVersion(t *testing.T) {
	const noVersion = `{"tunnel_id":"t1","config":{"ingress":[{"service":"http_status:404"}]}}`
	rules := []planner.IngressRule{{Service: "http_status:404"}}

	env := setup(t, reply(http.StatusOK, okBody(noVersion)))
	cfg, err := env.c.TunnelConfig(context.Background(), "a1", "t1")
	require.Error(t, err)
	require.Equal(t, TunnelConfig{}, cfg)

	env = setup(t, reply(http.StatusOK, okBody(noVersion)))
	version, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", rules)
	require.Error(t, err)
	require.Zero(t, version)
}

func TestTunnelConfigEmptyOriginRequest(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(configBody(2, `{"hostname":"a.example.com","service":"http://10.0.0.5:80","originRequest":{}}`))))
	got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")
	require.NoError(t, err)
	require.Equal(t, []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}}, got.Ingress)
}

func TestTunnelConfigWithoutConfiguration(t *testing.T) {
	for name, result := range map[string]string{
		"config null":    `{"tunnel_id":"t1","version":3,"config":null}`,
		"config missing": `{"tunnel_id":"t1","version":3}`,
		"no ingress":     `{"tunnel_id":"t1","version":3,"config":{}}`,
		"empty ingress":  `{"tunnel_id":"t1","version":3,"config":{"ingress":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(result)))
			got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")
			require.NoError(t, err)
			require.Equal(t, 3, got.Version)
			require.Empty(t, got.Ingress)
		})
	}
}

func TestTunnelConfigResultMissing(t *testing.T) {
	for name, body := range map[string]string{
		"null":    okBody(`null`),
		"missing": `{"success":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, body))
			got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")
			require.Error(t, err)
			require.Equal(t, TunnelConfig{}, got)
		})
	}
}

func TestPutTunnelConfig(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(configBody(8, ruleCaseWires()))))

	version, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", ruleCaseRules())

	require.NoError(t, err)
	require.Equal(t, 8, version)
	req := only(t, env)
	require.Equal(t, http.MethodPut, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel/t1/configurations", req.uri)
	require.Equal(t, "application/json", req.contentType)
	require.JSONEq(t, `{"config":{"ingress":[`+ruleCaseWires()+`]}}`, req.body)
}

func TestPutTunnelConfigRules(t *testing.T) {
	for _, tc := range ruleCases {
		t.Run(tc.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(configBody(1, tc.wire))))
			_, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", []planner.IngressRule{tc.rule})
			require.NoError(t, err)
			require.JSONEq(t, `{"config":{"ingress":[`+tc.wire+`]}}`, only(t, env).body)
		})
	}
}

func TestPutTunnelConfigWithoutRulesIsRefusedLocally(t *testing.T) {
	// Cloudflare refuses an ingress that does not end in a catch-all rule, so
	// nothing is sent for one that has no rule at all.
	for name, rules := range map[string][]planner.IngressRule{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(configBody(1, ``))))

			version, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", rules)

			require.ErrorIs(t, err, ErrInvalidArgument)
			require.Zero(t, version)
			require.Empty(t, env.requests())
		})
	}
}

func TestPutTunnelConfigRejected(t *testing.T) {
	env := setup(t, reply(http.StatusBadRequest,
		`{"success":false,"errors":[{"code":1022,"message":"the last ingress rule must match all URLs"}]}`))

	version, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", []planner.IngressRule{{Hostname: "a.example.com", Service: "http://10.0.0.5:80"}})

	require.ErrorContains(t, err, "last ingress rule")
	require.Zero(t, version)
}

func TestPutTunnelConfigWithoutResult(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`null`)))
	version, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", []planner.IngressRule{{Service: "http_status:404"}})
	require.Error(t, err)
	require.Zero(t, version)
}

func TestConnectors(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[
		{"id":"c1","version":"2026.9.0","config_version":4,"arch":"linux_amd64","run_at":"2026-01-01T00:00:00Z",
		 "conns":[{"id":"x","colo_name":"PRG","origin_ip":"203.0.113.10"},{"id":"y","colo_name":"FRA","origin_ip":"203.0.113.10"},
		          {"id":"z","colo_name":"VIE","origin_ip":"203.0.113.10"},{"id":"w","colo_name":"AMS","origin_ip":"203.0.113.10"}]},
		{"id":"c2","version":"2026.8.1","config_version":3,"conns":[{"id":"v","origin_ip":"2001:db8::7"},{"id":"u","origin_ip":"198.51.100.7"}]},
		{"id":"c3","version":"2026.8.1","config_version":0,"conns":[]},
		{"id":"c4","version":"2026.8.1"}]`)))

	got, err := env.c.Connectors(context.Background(), "a1", "t1")

	require.NoError(t, err)
	require.Equal(t, []Connector{
		{ID: "c1", Version: "2026.9.0", ConfigVersion: 4, Connections: 4, OriginIP: "203.0.113.10"},
		{ID: "c2", Version: "2026.8.1", ConfigVersion: 3, Connections: 2, OriginIP: "198.51.100.7, 2001:db8::7"},
		{ID: "c3", Version: "2026.8.1"},
		{ID: "c4", Version: "2026.8.1"},
	}, got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/accounts/a1/cfd_tunnel/t1/connections", req.uri)
}

func TestConnectorsNone(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[]`)))
	got, err := env.c.Connectors(context.Background(), "a1", "t1")
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestConnectorsNullResultIsNone(t *testing.T) {
	// The listing only feeds a status display, and Cloudflare answers a tunnel
	// that nothing connects to in either way.
	for name, body := range map[string]string{
		"null":    okBody(`null`),
		"missing": `{"success":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, body))
			got, err := env.c.Connectors(context.Background(), "a1", "t1")
			require.NoError(t, err)
			require.Empty(t, got)
		})
	}
}

func TestConnectorsBadAnswers(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"entry without an id": reply(http.StatusOK, okBody(`[{"id":"c1","conns":[]},{"version":"2026.8.1","conns":[{}]}]`)),
		"not a list":          reply(http.StatusOK, okBody(`{"id":"c1"}`)),
		"forbidden":           reply(http.StatusForbidden, `{"success":false,"errors":[]}`),
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, h)
			got, err := env.c.Connectors(context.Background(), "a1", "t1")
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

const recordJSON = `{"id":"r1","type":"CNAME","name":"app.example.com","content":"t1.cfargotunnel.com",` +
	`"proxied":true,"ttl":1,"comment":"pco:abc","created_on":"2026-01-01T00:00:00Z","modified_on":"2026-02-03T04:05:06.5Z"}`

func wantRecord(t *testing.T) Record {
	t.Helper()
	return Record{
		ID: "r1", Type: "CNAME", Name: "app.example.com", Content: "t1.cfargotunnel.com",
		Proxied: true, TTL: 1, Comment: "pco:abc", ModifiedOn: ts(t, "2026-02-03T04:05:06.5Z"),
	}
}

func TestRecords(t *testing.T) {
	tests := []struct {
		name   string
		filter RecordFilter
		query  url.Values
	}{
		{"no filter", RecordFilter{}, url.Values{}},
		{"type", RecordFilter{Type: "CNAME"}, url.Values{"type": {"CNAME"}}},
		{"name", RecordFilter{Name: "app.example.com"}, url.Values{"name": {"app.example.com"}}},
		{"comment prefix", RecordFilter{CommentPrefix: "pco:abc"}, url.Values{"comment.startswith": {"pco:abc"}}},
		{
			"all", RecordFilter{Type: "CNAME", Name: "app.example.com", CommentPrefix: "pco:abc"},
			url.Values{"type": {"CNAME"}, "name": {"app.example.com"}, "comment.startswith": {"pco:abc"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`[`+recordJSON+`]`)))

			got, err := env.c.Records(context.Background(), "z1", tt.filter)

			require.NoError(t, err)
			require.Equal(t, []Record{wantRecord(t)}, got)
			req := only(t, env)
			require.Equal(t, http.MethodGet, req.method)
			require.Equal(t, "/client/v4/zones/z1/dns_records", req.path(t))
			want := url.Values{"page": {"1"}, "per_page": {"5000"}}
			for k, v := range tt.query {
				want[k] = v
			}
			require.Equal(t, want, req.query(t))
		})
	}
}

func TestRecordsNullFieldsAndKinds(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[
		{"id":"r2","type":"TXT","name":"_pco-probe-x.example.com","content":"\"probe\"","proxied":false,"comment":null,"modified_on":null},
		{"id":"r3","type":"A","name":"a.example.com","content":"192.0.2.7"}]`)))

	got, err := env.c.Records(context.Background(), "z1", RecordFilter{})

	require.NoError(t, err)
	require.Equal(t, []Record{
		{ID: "r2", Type: "TXT", Name: "_pco-probe-x.example.com", Content: `"probe"`},
		{ID: "r3", Type: "A", Name: "a.example.com", Content: "192.0.2.7"},
	}, got)
}

func TestRecordsReadsEveryPage(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		item := func(id string) string {
			return fmt.Sprintf(`{"id":%q,"type":"CNAME","name":"%s.example.com","content":"x","comment":"pco:abc"}`, id, id)
		}
		switch r.URL.Query().Get("page") {
		case "1":
			reply(http.StatusOK, `{"success":true,"result":[`+item("a")+`,`+item("b")+`],"result_info":{"total_pages":2}}`)(w, r)
		case "2":
			reply(http.StatusOK, `{"success":true,"result":[`+item("c")+`],"result_info":{"total_pages":2}}`)(w, r)
		default:
			reply(http.StatusNotFound, `{"success":false,"errors":[]}`)(w, r)
		}
	})

	got, err := env.c.Records(context.Background(), "z1", RecordFilter{CommentPrefix: "pco:abc"})

	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, []string{"a", "b", "c"}, []string{got[0].ID, got[1].ID, got[2].ID})
}

func TestRecordsAreNotShortenedByAFailure(t *testing.T) {
	// A deleter that reads fewer records than exist would take the rest for
	// gone, so a listing that fails on any page must fail as a whole.
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			reply(http.StatusTooManyRequests, `{"success":false,"errors":[]}`)(w, r)
			return
		}
		reply(http.StatusOK, `{"success":true,"result":[`+recordJSON+`],"result_info":{"total_pages":2}}`)(w, r)
	})

	got, err := env.c.Records(context.Background(), "z1", RecordFilter{})

	require.Error(t, err)
	require.True(t, IsRateLimited(err))
	require.Nil(t, got)
}

func TestRecordsRejectsAnItemWithoutID(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[`+recordJSON+`,{"type":"A","name":"b.example.com","content":"192.0.2.1"}]`)))
	got, err := env.c.Records(context.Background(), "z1", RecordFilter{})
	require.Error(t, err)
	require.Nil(t, got)
}

func TestRecordsOutsideTheFilterFailTheWholeCall(t *testing.T) {
	// The records returned decide what gets deleted, so a server that ignores
	// part of the filter must not be believed.
	good := `{"id":"r1","type":"CNAME","name":"app.example.com","content":"x","comment":"pco:abc ok"}`
	tests := []struct {
		name   string
		filter RecordFilter
		bad    string
	}{
		{"other type", RecordFilter{Type: "CNAME"}, `{"id":"r2","type":"A","name":"app.example.com","content":"192.0.2.1","comment":"pco:abc"}`},
		{"other name", RecordFilter{Name: "app.example.com"}, `{"id":"r2","type":"CNAME","name":"www.example.com","content":"x","comment":"pco:abc"}`},
		{"name that only contains it", RecordFilter{Name: "app.example.com"}, `{"id":"r2","type":"CNAME","name":"x.app.example.com","content":"x","comment":"pco:abc"}`},
		{"other comment", RecordFilter{CommentPrefix: "pco:abc"}, `{"id":"r2","type":"CNAME","name":"app.example.com","content":"x","comment":"pco:other"}`},
		{"comment that only contains it", RecordFilter{CommentPrefix: "pco:abc"}, `{"id":"r2","type":"CNAME","name":"app.example.com","content":"x","comment":"by pco:abc"}`},
		{"no comment", RecordFilter{CommentPrefix: "pco:abc"}, `{"id":"r2","type":"CNAME","name":"app.example.com","content":"x","comment":null}`},
		{"one of three filters", RecordFilter{Type: "CNAME", Name: "app.example.com", CommentPrefix: "pco:abc"},
			`{"id":"r2","type":"CNAME","name":"app.example.com","content":"x","comment":"mine"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`[`+good+`,`+tt.bad+`]`)))

			got, err := env.c.Records(context.Background(), "z1", tt.filter)

			require.ErrorContains(t, err, "cloudflare returned a record outside the requested filter")
			require.Nil(t, got, "the records that did match are not returned either")
		})
	}
}

func TestRecordsOutsideTheFilterOnALaterPage(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		rec := `{"id":"r1","type":"CNAME","name":"app.example.com","content":"x","comment":"pco:abc"}`
		if r.URL.Query().Get("page") == "2" {
			rec = `{"id":"r2","type":"CNAME","name":"app.example.com","content":"x","comment":"mine"}`
		}
		reply(http.StatusOK, `{"success":true,"result":[`+rec+`],"result_info":{"total_pages":2}}`)(w, r)
	})

	got, err := env.c.Records(context.Background(), "z1", RecordFilter{CommentPrefix: "pco:abc"})

	require.ErrorContains(t, err, "outside the requested filter")
	require.Nil(t, got)
}

func TestRecordsFilterIgnoresCase(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(
		`[{"id":"r1","type":"cname","name":"APP.Example.com","content":"x","comment":"PCO:ABC probe"}]`)))

	got, err := env.c.Records(context.Background(), "z1",
		RecordFilter{Type: "CNAME", Name: "app.example.COM", CommentPrefix: "pco:abc"})

	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestRecordFilterMatches(t *testing.T) {
	r := Record{Type: "CNAME", Name: "app.example.com", Comment: "pco:abc moved"}
	tests := []struct {
		name string
		f    RecordFilter
		want bool
	}{
		{"empty filter", RecordFilter{}, true},
		{"type", RecordFilter{Type: "cname"}, true},
		{"other type", RecordFilter{Type: "A"}, false},
		{"name", RecordFilter{Name: "APP.example.com"}, true},
		{"longer name", RecordFilter{Name: "x.app.example.com"}, false},
		{"shorter name", RecordFilter{Name: "app.example"}, false},
		{"comment prefix", RecordFilter{CommentPrefix: "PCO:abc"}, true},
		{"whole comment", RecordFilter{CommentPrefix: "pco:abc moved"}, true},
		{"longer than the comment", RecordFilter{CommentPrefix: "pco:abc moved!"}, false},
		{"not a prefix", RecordFilter{CommentPrefix: "abc"}, false},
		{"all", RecordFilter{Type: "CNAME", Name: "app.example.com", CommentPrefix: "pco:"}, true},
		{"one of all fails", RecordFilter{Type: "CNAME", Name: "app.example.com", CommentPrefix: "x"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, tt.f.Matches(r))
		})
	}
	require.False(t, RecordFilter{CommentPrefix: "pco:"}.Matches(Record{Type: "A", Name: "a"}), "no comment")
}

func TestRecordsDenied(t *testing.T) {
	env := setup(t, reply(http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`))
	got, err := env.c.Records(context.Background(), "z1", RecordFilter{})
	require.True(t, IsAuth(err))
	require.Nil(t, got)
}

func newRecord() Record {
	return Record{
		Type: "CNAME", Name: "app.example.com", Content: "t1.cfargotunnel.com",
		Proxied: true, Comment: "pco:abc",
		ID: "ignored-on-create", ModifiedOn: time.Unix(1, 0),
	}
}

func TestCreateRecord(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(recordJSON)))

	got, err := env.c.CreateRecord(context.Background(), "z1", newRecord())

	require.NoError(t, err)
	require.Equal(t, wantRecord(t), got)
	req := only(t, env)
	require.Equal(t, http.MethodPost, req.method)
	require.Equal(t, "/client/v4/zones/z1/dns_records", req.uri)
	require.Equal(t, "application/json", req.contentType)
	require.JSONEq(t, `{"type":"CNAME","name":"app.example.com","content":"t1.cfargotunnel.com","proxied":true,"comment":"pco:abc","ttl":1}`, req.body)
}

func TestCreateRecordSendsEveryField(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(recordJSON)))

	_, err := env.c.CreateRecord(context.Background(), "z1", Record{Type: "TXT", Name: "_pco-probe-x.example.com", Content: `"probe"`})

	require.NoError(t, err)
	require.JSONEq(t, `{"type":"TXT","name":"_pco-probe-x.example.com","content":"\"probe\"","proxied":false,"comment":"","ttl":1}`, only(t, env).body)
}

func TestRecordWritesSendTheTTL(t *testing.T) {
	tests := []struct {
		name string
		ttl  int
		sent int
	}{
		{"automatic", 1, 1},
		{"unset means automatic", 0, 1},
		{"five minutes", 300, 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Record{ID: "r1", Type: "A", Name: "a.example.com", Content: "192.0.2.7", TTL: tt.ttl}
			want := fmt.Sprintf(`{"type":"A","name":"a.example.com","content":"192.0.2.7","proxied":false,"comment":"","ttl":%d}`, tt.sent)

			env := setup(t, reply(http.StatusOK, okBody(recordJSON)))
			_, err := env.c.CreateRecord(context.Background(), "z1", r)
			require.NoError(t, err)
			require.JSONEq(t, want, only(t, env).body, "create")

			env = setup(t, reply(http.StatusOK, okBody(recordJSON)))
			_, err = env.c.UpdateRecord(context.Background(), "z1", r)
			require.NoError(t, err)
			require.JSONEq(t, want, only(t, env).body, "update")
		})
	}
}

func TestRecordsCarryTheTTL(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[{"id":"r3","type":"A","name":"a.example.com","content":"192.0.2.7","ttl":300}]`)))

	got, err := env.c.Records(context.Background(), "z1", RecordFilter{})

	require.NoError(t, err)
	require.Equal(t, []Record{{ID: "r3", Type: "A", Name: "a.example.com", Content: "192.0.2.7", TTL: 300}}, got)
}

func TestCreateRecordConflict(t *testing.T) {
	env := setup(t, reply(http.StatusBadRequest,
		`{"success":false,"errors":[{"code":81053,"message":"An A, AAAA, or CNAME record with that host already exists."}]}`))

	got, err := env.c.CreateRecord(context.Background(), "z1", newRecord())

	require.True(t, IsConflict(err))
	require.Equal(t, Record{}, got)
}

func TestCreateRecordAnswerWithoutID(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{"type":"CNAME","name":"app.example.com"}`)))
	got, err := env.c.CreateRecord(context.Background(), "z1", newRecord())
	require.Error(t, err)
	require.Equal(t, Record{}, got)
}

func TestUpdateRecord(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(recordJSON)))
	r := newRecord()
	r.ID = "r1"

	got, err := env.c.UpdateRecord(context.Background(), "z1", r)

	require.NoError(t, err)
	require.Equal(t, wantRecord(t), got)
	req := only(t, env)
	require.Equal(t, http.MethodPatch, req.method)
	require.Equal(t, "/client/v4/zones/z1/dns_records/r1", req.uri)
	require.JSONEq(t, `{"type":"CNAME","name":"app.example.com","content":"t1.cfargotunnel.com","proxied":true,"comment":"pco:abc","ttl":1}`, req.body)
}

func TestUpdateRecordNotFound(t *testing.T) {
	env := setup(t, reply(http.StatusNotFound, `{"success":false,"errors":[{"code":81044,"message":"Record does not exist."}]}`))
	r := newRecord()
	r.ID = "r1"
	_, err := env.c.UpdateRecord(context.Background(), "z1", r)
	require.True(t, IsNotFound(err))
}

func TestDeleteRecord(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{"id":"r1"}`)))

	require.NoError(t, env.c.DeleteRecord(context.Background(), "z1", "r1"))

	req := only(t, env)
	require.Equal(t, http.MethodDelete, req.method)
	require.Equal(t, "/client/v4/zones/z1/dns_records/r1", req.uri)
	require.Empty(t, req.body)
}

func TestDeleteRecordFailure(t *testing.T) {
	for name, status := range map[string]int{
		"forbidden": http.StatusForbidden,
		"not found": http.StatusNotFound,
		"server":    http.StatusInternalServerError,
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(status, `{"success":false,"errors":[{"code":1,"message":"no"}]}`))
			require.Error(t, env.c.DeleteRecord(context.Background(), "z1", "r1"))
		})
	}
}

func TestEmptyArgumentsAreRejectedBeforeAnyRequest(t *testing.T) {
	ctx := context.Background()
	withID := newRecord()
	withID.ID = "r1"
	noID := newRecord()
	noID.ID = ""
	noName := newRecord()
	noName.Name = ""
	noType := newRecord()
	noType.Type = ""
	catchAll := []planner.IngressRule{{Service: "http_status:404"}}

	tests := []struct {
		name string
		call func(c *Client) error
	}{
		{"FindTunnel without account", func(c *Client) error { _, _, err := c.FindTunnel(ctx, "", "n"); return err }},
		{"FindTunnel without name", func(c *Client) error { _, _, err := c.FindTunnel(ctx, "a1", ""); return err }},
		{"FindTunnel blank name", func(c *Client) error { _, _, err := c.FindTunnel(ctx, "a1", "  "); return err }},
		{"Tunnels without account", func(c *Client) error { _, err := c.Tunnels(ctx, "", "pco-"); return err }},
		{"Tunnels without prefix", func(c *Client) error { _, err := c.Tunnels(ctx, "a1", ""); return err }},
		{"Tunnels blank prefix", func(c *Client) error { _, err := c.Tunnels(ctx, "a1", " "); return err }},
		{"CreateTunnel without account", func(c *Client) error { _, err := c.CreateTunnel(ctx, "", "n"); return err }},
		{"CreateTunnel without name", func(c *Client) error { _, err := c.CreateTunnel(ctx, "a1", ""); return err }},
		{"DeleteTunnel without account", func(c *Client) error { return c.DeleteTunnel(ctx, "", "t1") }},
		{"DeleteTunnel without tunnel", func(c *Client) error { return c.DeleteTunnel(ctx, "a1", "") }},
		{"TunnelToken without account", func(c *Client) error { _, err := c.TunnelToken(ctx, "", "t1"); return err }},
		{"TunnelToken without tunnel", func(c *Client) error { _, err := c.TunnelToken(ctx, "a1", ""); return err }},
		{"TunnelConfig without account", func(c *Client) error { _, err := c.TunnelConfig(ctx, "", "t1"); return err }},
		{"TunnelConfig without tunnel", func(c *Client) error { _, err := c.TunnelConfig(ctx, "a1", ""); return err }},
		{"PutTunnelConfig without account", func(c *Client) error { _, err := c.PutTunnelConfig(ctx, "", "t1", catchAll); return err }},
		{"PutTunnelConfig without tunnel", func(c *Client) error { _, err := c.PutTunnelConfig(ctx, "a1", "", catchAll); return err }},
		{"PutTunnelConfig without rules", func(c *Client) error { _, err := c.PutTunnelConfig(ctx, "a1", "t1", nil); return err }},
		{"Connectors without account", func(c *Client) error { _, err := c.Connectors(ctx, "", "t1"); return err }},
		{"Connectors without tunnel", func(c *Client) error { _, err := c.Connectors(ctx, "a1", ""); return err }},
		{"RotateTunnelSecret without account", func(c *Client) error { return c.RotateTunnelSecret(ctx, "", "t1", make([]byte, 32)) }},
		{"RotateTunnelSecret without tunnel", func(c *Client) error { return c.RotateTunnelSecret(ctx, "a1", "", make([]byte, 32)) }},
		{"RotateTunnelSecret without secret", func(c *Client) error { return c.RotateTunnelSecret(ctx, "a1", "t1", nil) }},
		{"CleanUpConnections without account", func(c *Client) error { return c.CleanUpConnections(ctx, "", "t1") }},
		{"CleanUpConnections without tunnel", func(c *Client) error { return c.CleanUpConnections(ctx, "a1", "") }},
		{"Records without zone", func(c *Client) error { _, err := c.Records(ctx, "", RecordFilter{}); return err }},
		{"CreateRecord without zone", func(c *Client) error { _, err := c.CreateRecord(ctx, "", withID); return err }},
		{"CreateRecord without name", func(c *Client) error { _, err := c.CreateRecord(ctx, "z1", noName); return err }},
		{"CreateRecord without type", func(c *Client) error { _, err := c.CreateRecord(ctx, "z1", noType); return err }},
		{"UpdateRecord without zone", func(c *Client) error { _, err := c.UpdateRecord(ctx, "", withID); return err }},
		{"UpdateRecord without id", func(c *Client) error { _, err := c.UpdateRecord(ctx, "z1", noID); return err }},
		{"UpdateRecord without name", func(c *Client) error {
			r := noName
			r.ID = "r1"
			_, err := c.UpdateRecord(ctx, "z1", r)
			return err
		}},
		{"DeleteRecord without zone", func(c *Client) error { return c.DeleteRecord(ctx, "", "r1") }},
		{"DeleteRecord without id", func(c *Client) error { return c.DeleteRecord(ctx, "z1", "") }},
		{"DeleteRecord blank id", func(c *Client) error { return c.DeleteRecord(ctx, "z1", " \t") }},
		{"DeleteRecord id with a slash", func(c *Client) error { return c.DeleteRecord(ctx, "z1", "a/b") }},
		{"DeleteTunnel account with a slash", func(c *Client) error { return c.DeleteTunnel(ctx, "a/b", "t1") }},
		{"UpdateRecord id with a slash", func(c *Client) error {
			r := newRecord()
			r.ID = "a/b"
			_, err := c.UpdateRecord(ctx, "z1", r)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`{}`)))
			require.ErrorIs(t, tt.call(env.c), ErrInvalidArgument)
			require.Empty(t, env.requests())
		})
	}
}

func TestIDsCannotChangeThePath(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		call     func(c *Client) error
		wantPath string // as the server sees it, still escaped
	}{
		{"question mark is escaped", func(c *Client) error { return c.DeleteRecord(ctx, "z1", "a?b=c") }, "/client/v4/zones/z1/dns_records/a%3Fb=c"},
		{"percent is escaped", func(c *Client) error { return c.DeleteTunnel(ctx, "a%2e", "t1") }, "/client/v4/accounts/a%252e/cfd_tunnel/t1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`{}`)))
			require.NoError(t, tt.call(env.c))
			require.Equal(t, tt.wantPath, only(t, env).uri)
		})
	}

	// What cannot stay one segment of the path is refused before anything is sent.
	for _, id := range []string{".", "..", "a/b", "/", "../x"} {
		t.Run("refused "+id, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`{}`)))
			require.ErrorIs(t, env.c.DeleteRecord(ctx, "z1", id), ErrInvalidArgument)
			require.ErrorIs(t, env.c.DeleteTunnel(ctx, id, "t1"), ErrInvalidArgument)
			require.ErrorIs(t, env.c.DeleteTunnel(ctx, "a1", id), ErrInvalidArgument)
			require.ErrorIs(t, env.c.DeleteRecord(ctx, id, "r1"), ErrInvalidArgument)
			require.Empty(t, env.requests())
		})
	}
}

func TestCheckID(t *testing.T) {
	for _, id := range []string{"023e105f4ecef8ad9ca31a8372d0c353", "a b", "100%", "q?x#y", "a..b", "...", "%2e%2e", "ünï"} {
		require.NoError(t, CheckID("zone id", id), id)
	}
	for _, id := range []string{"", " ", " \t\n", ".", "..", "a/b", "/"} {
		err := CheckID("zone id", id)
		require.ErrorIs(t, err, ErrInvalidArgument, id)
		require.ErrorContains(t, err, "zone id", id)
	}
}

func TestCheckName(t *testing.T) {
	require.NoError(t, CheckName("tunnel name", "pco-abc"))
	require.NoError(t, CheckName("tunnel name", "a/b"), "a name is not a path segment")
	for _, name := range []string{"", " ", "\t"} {
		err := CheckName("tunnel name", name)
		require.ErrorIs(t, err, ErrInvalidArgument)
		require.ErrorContains(t, err, "tunnel name")
	}
}
