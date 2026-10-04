package cffake_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// The tests of this file look at the wire itself: the requests the handler
// answers and how it answers them. What the client makes of the answers is the
// business of the contract test.

// do makes one request of the handler.
func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doAs(t, h, token, method, target, body)
}

func doAs(t *testing.T, h http.Handler, bearer, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doTyped(t, h, bearer, "application/json", method, target, body)
}

// doTyped makes a request that says what its body is, when it has a kind of
// request that has one.
func doTyped(t *testing.T, h http.Handler, bearer, contentType, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if contentType != "" && (method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch) {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The codes the handler gives an error that has none, which the tests spell
// out so that a change of them is noticed.
const (
	codeBadRequest      = 1400
	codeConflict        = 1409
	codeBodyTooLarge    = 1413
	codeUnsupportedType = 1415
	codeServerError     = 1500
)

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type answer struct {
	Success    bool            `json:"success"`
	Errors     []wireError     `json:"errors"`
	Messages   []any           `json:"messages"`
	Result     json.RawMessage `json:"result"`
	ResultInfo json.RawMessage `json:"result_info"`
}

// decode reads the answer, which must be an envelope as Cloudflare makes them.
func decode(t *testing.T, rec *httptest.ResponseRecorder) answer {
	t.Helper()
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &keys), rec.Body.String())
	for _, key := range []string{"success", "errors", "messages", "result"} {
		require.Contains(t, keys, key, rec.Body.String())
	}
	require.True(t, strings.HasPrefix(string(keys["errors"]), "["), "errors is a list: %s", rec.Body.String())
	require.True(t, strings.HasPrefix(string(keys["messages"]), "["), "messages is a list: %s", rec.Body.String())
	var a answer
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &a))
	require.Equal(t, rec.Code < 300, a.Success, "success follows the status: %s", rec.Body.String())
	if !a.Success {
		require.NotEmpty(t, a.Errors, "a refusal says why: %s", rec.Body.String())
		require.JSONEq(t, "null", string(a.Result))
	}
	return a
}

func requireRefused(t *testing.T, rec *httptest.ResponseRecorder, status, code int, says string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	a := decode(t, rec)
	require.Equal(t, code, a.Errors[0].Code, rec.Body.String())
	require.Contains(t, a.Errors[0].Message, says)
}

// wireFixture is a fake with a tunnel and two records, and ways to name them.
type wireFixture struct {
	f      *cffake.Fake
	h      http.Handler
	tunnel string
	doomed string
	record string
	spare  string
}

func newWireFixture(opts ...cffake.Option) wireFixture {
	f := cffake.New()
	f.SetNow(func() time.Time { return t0 })
	std(f)
	fx := wireFixture{f: f, h: cffake.Handler(f, opts...)}
	fx.tunnel = f.SeedTunnel(acct, "pco-abc", nil).ID
	fx.doomed = f.SeedTunnel(acct, "pco-doomed", nil).ID
	fx.record = f.SeedRecord(zone, rec("A", "a.example.com", "192.0.2.1")).ID
	fx.spare = f.SeedRecord(zone, rec("A", "b.example.com", "192.0.2.2")).ID
	return fx
}

type endpoint struct {
	method, path, body string
	status             int
	call               string // what the fake logs
}

// endpoints is every request the handler serves, in an order in which each
// one succeeds, with the call of the fake it is.
func (fx wireFixture) endpoints() []endpoint {
	tun := "/client/v4/accounts/" + acct + "/cfd_tunnel"
	rules := `{"config":{"ingress":[{"hostname":"a.example.com","service":"http://10.0.0.5:80"},{"service":"http_status:404"}]}}`
	return []endpoint{
		{http.MethodGet, "/client/v4/user/tokens/verify", "", 200, "VerifyToken"},
		{http.MethodGet, "/client/v4/accounts/" + acct + "/tokens/verify", "", 401, "VerifyToken"},
		{http.MethodGet, "/client/v4/accounts", "", 200, "Accounts"},
		{http.MethodGet, "/client/v4/zones", "", 200, "Zones"},
		{http.MethodGet, tun + "?name=pco-abc&is_deleted=false", "", 200, "FindTunnel acct1 pco-abc"},
		{http.MethodGet, tun + "?include_prefix=pco-&is_deleted=false", "", 200, "Tunnels acct1 pco-"},
		{http.MethodPost, tun, `{"name":"pco-new","config_src":"cloudflare"}`, 200, "CreateTunnel acct1 pco-new"},
		{http.MethodGet, tun + "/" + fx.tunnel + "/token", "", 200, "TunnelToken acct1 " + fx.tunnel},
		{http.MethodGet, tun + "/" + fx.tunnel + "/configurations", "", 200, "TunnelConfig acct1 " + fx.tunnel},
		{http.MethodPut, tun + "/" + fx.tunnel + "/configurations", rules, 200, "PutTunnelConfig acct1 " + fx.tunnel},
		{http.MethodGet, tun + "/" + fx.tunnel + "/connections", "", 200, "Connectors acct1 " + fx.tunnel},
		{http.MethodDelete, tun + "/" + fx.doomed, "", 200, "DeleteTunnel acct1 " + fx.doomed},
		{http.MethodGet, "/client/v4/zones/" + zone + "/dns_records?type=A&name=a.example.com&comment.startswith=x", "", 200, "Records zone1"},
		{http.MethodPost, "/client/v4/zones/" + zone + "/dns_records",
			`{"type":"A","name":"new.example.com","content":"192.0.2.9","proxied":false,"comment":"","ttl":1}`, 200, "CreateRecord zone1 new.example.com"},
		{http.MethodPatch, "/client/v4/zones/" + zone + "/dns_records/" + fx.record,
			`{"type":"A","name":"a.example.com","content":"192.0.2.3","proxied":false,"comment":"","ttl":1}`, 200, "UpdateRecord zone1 " + fx.record},
		{http.MethodDelete, "/client/v4/zones/" + zone + "/dns_records/" + fx.spare, "", 200, "DeleteRecord zone1 " + fx.spare},
	}
}

func TestEveryEndpointIsOneCallOfTheFake(t *testing.T) {
	fx := newWireFixture()
	for _, ep := range fx.endpoints() {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			before := len(fx.f.Calls())

			rec := do(t, fx.h, ep.method, ep.path, ep.body)

			require.Equal(t, ep.status, rec.Code, rec.Body.String())
			decode(t, rec)
			calls := fx.f.Calls()
			require.Equal(t, []string{ep.call}, calls[before:])
		})
	}
}

func TestAuthentication(t *testing.T) {
	t.Run("a request without a token is refused before anything is done", func(t *testing.T) {
		fx := newWireFixture()
		for _, ep := range fx.endpoints() {
			rec := doAs(t, fx.h, "", ep.method, ep.path, ep.body)
			requireRefused(t, rec, http.StatusUnauthorized, 10000, "Authentication error")
		}
		require.Empty(t, fx.f.Calls())
		require.Len(t, fx.f.RecordsIn(zone), 2)
	})

	t.Run("what is not a bearer token", func(t *testing.T) {
		fx := newWireFixture()
		for _, header := range []string{"", "Bearer", "Bearer ", "Basic abc", "abc", token, "Token " + token} {
			req := httptest.NewRequest(http.MethodGet, "/client/v4/zones", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			rec := httptest.NewRecorder()
			fx.h.ServeHTTP(rec, req)
			requireRefused(t, rec, http.StatusUnauthorized, 10000, "Authentication error")
		}
	})

	t.Run("any token is accepted by default, and the scheme is not case sensitive", func(t *testing.T) {
		fx := newWireFixture()
		for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
			req := httptest.NewRequest(http.MethodGet, "/client/v4/zones", nil)
			req.Header.Set("Authorization", scheme+" anything-at-all")
			rec := httptest.NewRecorder()
			fx.h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
		}
	})

	t.Run("the tokens that were named are the only ones", func(t *testing.T) {
		fx := newWireFixture(cffake.WithToken("first"), cffake.WithToken("second"))
		for bearer, status := range map[string]int{"first": 200, "second": 200, "third": 401, "firs": 401, "firstt": 401, "FIRST": 401} {
			rec := doAs(t, fx.h, bearer, http.MethodGet, "/client/v4/zones", "")
			require.Equal(t, status, rec.Code, bearer)
		}
	})

	t.Run("an empty token names nothing", func(t *testing.T) {
		fx := newWireFixture(cffake.WithToken(""))
		require.Equal(t, http.StatusOK, do(t, fx.h, http.MethodGet, "/client/v4/zones", "").Code)
	})

	t.Run("the client with another token is refused", func(t *testing.T) {
		fx := newWireFixture(cffake.WithToken("right"))
		srv := httptest.NewServer(fx.h)
		t.Cleanup(srv.Close)
		c, err := cfapi.New(cfapi.Options{BaseURL: srv.URL + "/client/v4", Token: "wrong", Limiter: cfapi.NewLimiter(1000, time.Minute, 100, nil)})
		require.NoError(t, err)

		_, err = c.VerifyToken(ctx)
		require.True(t, cfapi.IsAuth(err), "%v", err)
		_, err = c.Zones(ctx)
		require.True(t, cfapi.IsAuth(err), "%v", err)
		require.NotContains(t, fmt.Sprint(err), "wrong")
		require.Empty(t, fx.f.Calls())
	})
}

func TestUnknownPathsAndMethods(t *testing.T) {
	fx := newWireFixture()

	for _, path := range []string{
		"/", "/client", "/client/v4", "/client/v4/", "/client/v4/nope", "/client/v3/zones", "/v4/zones", "/other/client/v4/zones",
		"/client/v4/zones/", "/client/v4/zones/z1", "/client/v4/zones/z1/dns_records/r1/extra", "/client/v4/zones/z1/nope",
		"/client/v4/accounts//cfd_tunnel", "/client/v4/accounts/a1", "/client/v4/accounts/a1/cfd_tunnel/t1/nope",
		"/client/v4/accounts/a1/cfd_tunnel/t1/token/more", "/client/v4/user", "/client/v4/user/tokens", "/client/v4/user/tokens/verify/x",
		"/client/v4/zones//dns_records", "/client/v4/Zones",
	} {
		t.Run("GET "+path, func(t *testing.T) {
			requireRefused(t, do(t, fx.h, http.MethodGet, path, ""), http.StatusNotFound, 7000, "No route")
			requireRefused(t, doAs(t, fx.h, "", http.MethodGet, path, ""), http.StatusNotFound, 7000, "No route")
		})
	}

	for _, tc := range []struct{ method, path, allow string }{
		{http.MethodPost, "/client/v4/zones", "GET"},
		{http.MethodDelete, "/client/v4/accounts", "GET"},
		{http.MethodPut, "/client/v4/user/tokens/verify", "GET"},
		{http.MethodPost, "/client/v4/accounts/a1/tokens/verify", "GET"},
		{http.MethodPut, "/client/v4/accounts/a1/cfd_tunnel", "GET, POST"},
		{http.MethodPatch, "/client/v4/accounts/a1/cfd_tunnel", "GET, POST"},
		{http.MethodGet, "/client/v4/accounts/a1/cfd_tunnel/t1", "DELETE, PATCH"},
		{http.MethodPost, "/client/v4/accounts/a1/cfd_tunnel/t1", "DELETE, PATCH"},
		{http.MethodPost, "/client/v4/accounts/a1/cfd_tunnel/t1/token", "GET"},
		{http.MethodPost, "/client/v4/accounts/a1/cfd_tunnel/t1/configurations", "GET, PUT"},
		{http.MethodPatch, "/client/v4/accounts/a1/cfd_tunnel/t1/configurations", "GET, PUT"},
		{http.MethodPut, "/client/v4/accounts/a1/cfd_tunnel/t1/connections", "DELETE, GET"},
		{http.MethodPut, "/client/v4/zones/z1/dns_records", "GET, POST"},
		{http.MethodGet, "/client/v4/zones/z1/dns_records/r1", "DELETE, PATCH"},
		{http.MethodPut, "/client/v4/zones/z1/dns_records/r1", "DELETE, PATCH"},
		{http.MethodPost, "/client/v4/zones/z1/dns_records/r1", "DELETE, PATCH"},
		{http.MethodHead, "/client/v4/zones", "GET"},
		{http.MethodOptions, "/client/v4/zones", "GET"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, fx.h, tc.method, tc.path, "")
			requireRefused(t, rec, http.StatusMethodNotAllowed, 10405, "Method not allowed")
			require.Equal(t, tc.allow, rec.Header().Get("Allow"))
			requireRefused(t, doAs(t, fx.h, "", tc.method, tc.path, ""), http.StatusMethodNotAllowed, 10405, "Method not allowed")
		})
	}
	require.Empty(t, fx.f.Calls())
}

func TestMalformedRequestsAreRefusedBeforeTheFakeIsAsked(t *testing.T) {
	fx := newWireFixture()
	tunnels := "/client/v4/accounts/" + acct + "/cfd_tunnel"
	config := tunnels + "/" + fx.tunnel + "/configurations"
	records := "/client/v4/zones/" + zone + "/dns_records"
	oneRecord := records + "/" + fx.record
	full := `"type":"A","name":"a.example.com","content":"192.0.2.3","proxied":false,"comment":"","ttl":1`
	rule := `{"hostname":"a.example.com","service":"http://10.0.0.5:80"`

	for _, tc := range []struct {
		name, method, path, body, says string
	}{
		{"tunnel: not JSON", "POST", tunnels, `{`, "not a JSON object"},
		{"tunnel: no body", "POST", tunnels, ``, "not a JSON object"},
		{"tunnel: a list", "POST", tunnels, `[]`, "not a JSON object"},
		{"tunnel: null", "POST", tunnels, `null`, "expected a JSON object"},
		{"tunnel: a string", "POST", tunnels, `"x"`, "not a JSON object"},
		{"tunnel: two documents", "POST", tunnels, `{"name":"a"} {}`, "not a JSON object"},
		{"tunnel: no name", "POST", tunnels, `{}`, "name is required"},
		{"tunnel: a name that is no string", "POST", tunnels, `{"name":5}`, "does not fit"},
		{"tunnel: a null name", "POST", tunnels, `{"name":null}`, "name is required"},
		{"tunnel: another field", "POST", tunnels, `{"name":"a","tunnel_secret":"x"}`, `unknown field "tunnel_secret"`},
		{"tunnel: a field in another case", "POST", tunnels, `{"Name":"a"}`, `unknown field "Name"`},
		{"tunnel: managed locally", "POST", tunnels, `{"name":"a","config_src":"local"}`, "config_src must be cloudflare"},
		{"tunnel: a query", "POST", tunnels + "?x=1", `{"name":"a"}`, `unknown query parameter "x"`},

		{"config: not JSON", "PUT", config, `{`, "not a JSON object"},
		{"config: nothing", "PUT", config, `{}`, "config is required"},
		{"config: null", "PUT", config, `{"config":null}`, "config is required"},
		{"config: a list", "PUT", config, `{"config":[]}`, "not a JSON object"},
		{"config: another field", "PUT", config, `{"config":{"ingress":[]},"x":1}`, `unknown field "x"`},
		{"config: warp routing", "PUT", config, `{"config":{"ingress":[],"warp-routing":{"enabled":true}}}`, `unknown field "warp-routing"`},
		{"config: a top level origin request", "PUT", config, `{"config":{"ingress":[],"originRequest":{}}}`, `unknown field "originRequest"`},
		{"config: ingress is no list", "PUT", config, `{"config":{"ingress":{}}}`, "does not fit"},
		{"config: a rule that is a string", "PUT", config, `{"config":{"ingress":["x"]}}`, "ingress rule #1"},
		{"config: a rule that is null", "PUT", config, `{"config":{"ingress":[null]}}`, "ingress rule #1: expected a JSON object"},
		{"config: a path on a rule", "PUT", config, `{"config":{"ingress":[` + rule + `,"path":"/x"}]}}`, `ingress rule #1: unknown field "path"`},
		{"config: an option that is not one", "PUT", config, `{"config":{"ingress":[{"service":"x","originRequest":{"connectTimeout":3}}]}}`, `ingress rule #1: originRequest: unknown field "connectTimeout"`},
		{"config: an option in another case", "PUT", config, `{"config":{"ingress":[{"service":"x","originRequest":{"matchSNIToHost":true}}]}}`, `unknown field "matchSNIToHost"`},
		{"config: an option of the wrong type", "PUT", config, `{"config":{"ingress":[{"service":"x","originRequest":{"noTLSVerify":"yes"}}]}}`, "ingress rule #1: originRequest"},
		{"config: a second rule that is wrong", "PUT", config, `{"config":{"ingress":[{"service":"x"},{"service":"y","x":1}]}}`, "ingress rule #2"},
		{"config: a service of the wrong type", "PUT", config, `{"config":{"ingress":[{"service":5}]}}`, "ingress rule #1"},

		{"record: not JSON", "POST", records, `{`, "not a JSON object"},
		{"record: nothing", "POST", records, `{}`, "type is required"},
		{"record: no name", "POST", records, `{"type":"A","content":"x"}`, "name is required"},
		{"record: no content", "POST", records, `{"type":"A","name":"a"}`, "content is required"},
		{"record: a TTL that is a string", "POST", records, `{"type":"A","name":"a","content":"x","ttl":"1"}`, "does not fit"},
		{"record: a TTL that is a fraction", "POST", records, `{"type":"A","name":"a","content":"x","ttl":1.5}`, "does not fit"},
		{"record: proxied that is a string", "POST", records, `{"type":"A","name":"a","content":"x","proxied":"yes"}`, "does not fit"},
		{"record: another field", "POST", records, `{"type":"A","name":"a","content":"x","tags":[]}`, `unknown field "tags"`},
		{"record: a field in another case", "POST", records, `{"Type":"A","name":"a","content":"x"}`, `unknown field "Type"`},
		{"record: a query", "POST", records + "?x=1", `{"type":"A","name":"a","content":"x"}`, "unknown query parameter"},
		{"change: without a proxied", "PATCH", oneRecord, `{"type":"A","name":"a.example.com","content":"x","comment":"","ttl":1}`, "proxied is required"},
		{"change: without a comment", "PATCH", oneRecord, `{"type":"A","name":"a.example.com","content":"x","proxied":false,"ttl":1}`, "comment is required"},
		{"change: without a TTL", "PATCH", oneRecord, `{"type":"A","name":"a.example.com","content":"x","proxied":false,"comment":""}`, "ttl is required"},
		{"change: a null comment", "PATCH", oneRecord, `{"type":"A","name":"a.example.com","content":"x","proxied":false,"comment":null,"ttl":1}`, "comment is required"},
		{"change: only a content", "PATCH", oneRecord, `{"content":"x"}`, "type is required"},
		{"change: not JSON", "PATCH", oneRecord, `{` + full, "not a JSON object"},
		{"change: nothing", "PATCH", oneRecord, ``, "not a JSON object"},

		{"a body that is too large", "POST", tunnels, `{"name":"` + strings.Repeat("x", 2<<20) + `"}`, "too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, fx.h, tc.method, tc.path, tc.body)

			status, code := http.StatusBadRequest, codeBadRequest
			if strings.Contains(tc.says, "too large") {
				status, code = http.StatusRequestEntityTooLarge, codeBodyTooLarge
			}
			requireRefused(t, rec, status, code, tc.says)
		})
	}
	require.Empty(t, fx.f.Calls(), "nothing that was malformed reached the fake")
}

func TestQueriesAreChecked(t *testing.T) {
	fx := newWireFixture()
	for _, tc := range []struct{ path, says string }{
		{"/client/v4/zones?order=name", `unknown query parameter "order"`},
		{"/client/v4/zones?name=example.com", `unknown query parameter "name"`},
		{"/client/v4/accounts?type=x", `unknown query parameter "type"`},
		{"/client/v4/user/tokens/verify?x=1", `unknown query parameter "x"`},
		{"/client/v4/accounts/acct1/tokens/verify?x=1", `unknown query parameter "x"`},
		{"/client/v4/accounts/acct1/cfd_tunnel?uuid=x", `unknown query parameter "uuid"`},
		{"/client/v4/accounts/acct1/cfd_tunnel/x/token?x=1", `unknown query parameter "x"`},
		{"/client/v4/accounts/acct1/cfd_tunnel/x/configurations?x=1", `unknown query parameter "x"`},
		{"/client/v4/accounts/acct1/cfd_tunnel/x/connections?page=1", `unknown query parameter "page"`},
		{"/client/v4/zones/zone1/dns_records?content=x", `unknown query parameter "content"`},
		{"/client/v4/zones/zone1/dns_records?type=A&type=TXT", "more than once"},
		{"/client/v4/zones?page=0", "page must be"},
		{"/client/v4/zones?page=-1", "page must be"},
		{"/client/v4/zones?page=one", "page must be"},
		{"/client/v4/zones?page=99999999999999999999", "page must be"},
		{"/client/v4/zones?page=1&page=2", "more than once"},
		{"/client/v4/zones?per_page=0", "per_page must be an integer from 5 to 50"},
		{"/client/v4/zones?per_page=4", "per_page must be an integer from 5 to 50"},
		{"/client/v4/zones?per_page=51", "per_page must be an integer from 5 to 50"},
		{"/client/v4/accounts?per_page=1", "per_page must be an integer from 5 to 50"},
		{"/client/v4/accounts?per_page=4", "per_page must be an integer from 5 to 50"},
		{"/client/v4/accounts?per_page=51", "per_page must be an integer from 5 to 50"},
		{"/client/v4/accounts/acct1/cfd_tunnel?per_page=0", "per_page must be an integer from 1 to 1000"},
		{"/client/v4/accounts/acct1/cfd_tunnel?per_page=1001", "per_page must be an integer from 1 to 1000"},
		{"/client/v4/zones/zone1/dns_records?per_page=0", "per_page must be an integer from 1 to 5000000"},
		{"/client/v4/zones/zone1/dns_records?per_page=5000001", "per_page must be an integer from 1 to 5000000"},
		{"/client/v4/zones/zone1/dns_records?per_page=x", "per_page must be"},
		{"/client/v4/accounts/acct1/cfd_tunnel?is_deleted=maybe", "is_deleted must be"},
		{"/client/v4/accounts/acct1/cfd_tunnel?is_deleted=1", "is_deleted must be"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			requireRefused(t, do(t, fx.h, http.MethodGet, tc.path, ""), http.StatusBadRequest, codeBadRequest, tc.says)
		})
	}
	require.Empty(t, fx.f.Calls())

	for _, path := range []string{
		"/client/v4/zones?per_page=50&page=1", "/client/v4/zones?per_page=5", "/client/v4/accounts?per_page=5",
		"/client/v4/accounts/acct1/cfd_tunnel?per_page=1", "/client/v4/accounts/acct1/cfd_tunnel?per_page=1000",
		"/client/v4/zones/zone1/dns_records?per_page=1", "/client/v4/zones/zone1/dns_records?per_page=5000000",
		"/client/v4/zones/zone1/dns_records?per_page=100&page=7",
	} {
		require.Equal(t, http.StatusOK, do(t, fx.h, http.MethodGet, path, "").Code, path)
	}
}

func TestIDsInThePathAreChecked(t *testing.T) {
	fx := newWireFixture()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/client/v4/zones/../dns_records"},
		{"GET", "/client/v4/zones/%2e%2e/dns_records"},
		{"GET", "/client/v4/zones/%20/dns_records"},
		{"GET", "/client/v4/zones/a%2Fb/dns_records"},
		{"DELETE", "/client/v4/zones/zone1/dns_records/.."},
		{"DELETE", "/client/v4/zones/zone1/dns_records/%2E"},
		{"GET", "/client/v4/accounts/%2F/cfd_tunnel"},
		{"GET", "/client/v4/accounts/acct1/cfd_tunnel/a%2Fb/token"},
		{"GET", "/client/v4/accounts/acct1/cfd_tunnel/./configurations"},
		{"GET", "/client/v4/accounts/../tokens/verify"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(t, fx.h, tc.method, tc.path, "")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			decode(t, rec)
		})
	}
}

func TestListingsDescribeTheirPages(t *testing.T) {
	type info struct {
		Page       int  `json:"page"`
		PerPage    int  `json:"per_page"`
		Count      int  `json:"count"`
		TotalCount int  `json:"total_count"`
		TotalPages *int `json:"total_pages"`
	}
	list := func(t *testing.T, h http.Handler, target string) (items []map[string]any, in info, raw map[string]json.RawMessage) {
		t.Helper()
		rec := do(t, h, http.MethodGet, target, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		a := decode(t, rec)
		require.True(t, strings.HasPrefix(string(a.Result), "["), "a page is a list, even an empty one: %s", a.Result)
		require.NoError(t, json.Unmarshal(a.Result, &items))
		require.NoError(t, json.Unmarshal(a.ResultInfo, &in))
		require.NoError(t, json.Unmarshal(a.ResultInfo, &raw))
		return items, in, raw
	}
	pages := func(n int) *int { return &n }

	t.Run("zones and accounts", func(t *testing.T) {
		f := cffake.New()
		for i := range 7 {
			f.AddAccount(fmt.Sprintf("a%d", i), fmt.Sprintf("Account %d", i))
			f.AddZone(fmt.Sprintf("z%d", i), fmt.Sprintf("zone%d.example", i), fmt.Sprintf("a%d", i))
		}
		h := cffake.Handler(f)
		for _, kind := range []string{"zones", "accounts"} {
			items, in, _ := list(t, h, "/client/v4/"+kind)
			require.Len(t, items, 7)
			require.Equal(t, info{Page: 1, PerPage: 20, Count: 7, TotalCount: 7, TotalPages: pages(1)}, in, "the page size is 20 unless asked")

			items, in, _ = list(t, h, "/client/v4/"+kind+"?per_page=5&page=2")
			require.Len(t, items, 2)
			require.Equal(t, kind[:1]+"5", items[0]["id"])
			require.Equal(t, kind[:1]+"6", items[1]["id"])
			require.Equal(t, info{Page: 2, PerPage: 5, Count: 2, TotalCount: 7, TotalPages: pages(2)}, in)

			items, in, _ = list(t, h, "/client/v4/"+kind+"?per_page=5&page=3")
			require.Empty(t, items, "past the last page")
			require.Equal(t, info{Page: 3, PerPage: 5, Count: 0, TotalCount: 7, TotalPages: pages(2)}, in)

			_, in, _ = list(t, h, "/client/v4/"+kind+"?per_page=7")
			require.Equal(t, info{Page: 1, PerPage: 7, Count: 7, TotalCount: 7, TotalPages: pages(1)}, in)
		}
	})

	t.Run("nothing to list", func(t *testing.T) {
		h := cffake.Handler(cffake.New())
		items, in, _ := list(t, h, "/client/v4/zones")
		require.Empty(t, items)
		require.Equal(t, info{Page: 1, PerPage: 20, TotalPages: pages(0)}, in)
	})

	t.Run("records", func(t *testing.T) {
		fx := newWireFixture()
		for i := range 130 {
			fx.f.SeedRecord(zone, cfapi.Record{Type: "TXT", Name: fmt.Sprintf("t%03d.example.com", i), Content: "x", Comment: "pco:abc"})
		}
		items, in, _ := list(t, fx.h, "/client/v4/zones/"+zone+"/dns_records")
		require.Len(t, items, 100)
		require.Equal(t, info{Page: 1, PerPage: 100, Count: 100, TotalCount: 132, TotalPages: pages(2)}, in, "the page size is 100 unless asked")

		items, in, _ = list(t, fx.h, "/client/v4/zones/"+zone+"/dns_records?page=2&per_page=100&comment.startswith=pco:")
		require.Len(t, items, 30)
		require.Equal(t, info{Page: 2, PerPage: 100, Count: 30, TotalCount: 130, TotalPages: pages(2)}, in, "totals count what the filters left")

		items, in, _ = list(t, fx.h, "/client/v4/zones/"+zone+"/dns_records?type=MX")
		require.Empty(t, items)
		require.Equal(t, info{Page: 1, PerPage: 100, TotalPages: pages(0)}, in)
	})

	// 5 tunnels, 2 of which start with "pco-".
	seedTunnels := func(opts ...cffake.Option) http.Handler {
		f := cffake.New()
		std(f)
		for _, name := range []string{"other-1", "pco-a", "other-2", "pco-b", "other-3"} {
			f.SeedTunnel(acct, name, nil)
		}
		return cffake.Handler(f, opts...)
	}
	const listing = "/client/v4/accounts/" + acct + "/cfd_tunnel"

	t.Run("tunnels count every tunnel of the account and have no pages", func(t *testing.T) {
		h := seedTunnels()
		items, in, raw := list(t, h, listing+"?include_prefix=pco-&is_deleted=false")
		require.Len(t, items, 2)
		require.Equal(t, info{Page: 1, PerPage: 20, Count: 2, TotalCount: 5}, in)
		require.NotContains(t, raw, "total_pages")

		items, in, _ = list(t, h, listing+"?name=pco-b")
		require.Len(t, items, 1)
		require.Equal(t, info{Page: 1, PerPage: 20, Count: 1, TotalCount: 5}, in)

		items, in, _ = list(t, h, listing+"?name=pco&include_prefix=pco")
		require.Empty(t, items, "the name is exact")
		require.Equal(t, 5, in.TotalCount)

		items, _, _ = list(t, h, listing+"?name=pco-b&include_prefix=other-")
		require.Empty(t, items, "both filters apply")

		items, in, _ = list(t, h, listing+"?per_page=2&page=3")
		require.Len(t, items, 1)
		require.Equal(t, info{Page: 3, PerPage: 2, Count: 1, TotalCount: 5}, in)

		items, in, _ = list(t, h, listing+"?is_deleted=true")
		require.Empty(t, items, "nothing the fake deleted is kept")
		require.Equal(t, 5, in.TotalCount)

		items, _, _ = list(t, h, listing+"?is_deleted=false")
		require.Len(t, items, 5)
		items, _, _ = list(t, h, listing)
		require.Len(t, items, 5, "without is_deleted every tunnel is listed")
	})

	t.Run("tunnels can count what the filters left", func(t *testing.T) {
		h := seedTunnels(cffake.WithFilteredTunnelTotals())
		_, in, raw := list(t, h, listing+"?include_prefix=pco-")
		require.Equal(t, info{Page: 1, PerPage: 20, Count: 2, TotalCount: 2}, in)
		require.NotContains(t, raw, "total_pages")

		_, in, _ = list(t, h, listing+"?name=nothing")
		require.Equal(t, info{Page: 1, PerPage: 20, Count: 0, TotalCount: 0}, in)

		_, in, _ = list(t, h, listing+"?is_deleted=true")
		require.Equal(t, 0, in.TotalCount)
	})
}

func TestShapesOfTheAnswers(t *testing.T) {
	fx := newWireFixture()
	tun := "/client/v4/accounts/" + acct + "/cfd_tunnel"
	result := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return string(decode(t, rec).Result)
	}

	t.Run("a tunnel", func(t *testing.T) {
		want := func(id, name string) string {
			return `{"id":"` + id + `","account_tag":"acct1","name":"` + name + `","status":"inactive",` +
				`"created_at":"2026-03-04T05:06:07.123456789Z","deleted_at":null,"tun_type":"cfd_tunnel","remote_config":true}`
		}
		created := result(t, do(t, fx.h, http.MethodPost, tun, `{"name":"pco-new"}`))
		var got struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(created), &got))
		require.JSONEq(t, want(got.ID, "pco-new"), created)
		require.JSONEq(t, `[`+want(fx.tunnel, "pco-abc")+`]`, result(t, do(t, fx.h, http.MethodGet, tun+"?name=pco-abc", "")))
	})

	t.Run("a token is a string", func(t *testing.T) {
		require.JSONEq(t, `"`+cffake.RunToken(acct, fx.tunnel)+`"`, result(t, do(t, fx.h, http.MethodGet, tun+"/"+fx.tunnel+"/token", "")))
	})

	t.Run("a deletion names what it deleted", func(t *testing.T) {
		require.JSONEq(t, `{"id":"`+fx.doomed+`"}`, result(t, do(t, fx.h, http.MethodDelete, tun+"/"+fx.doomed, "")))
		require.JSONEq(t, `{"id":"`+fx.spare+`"}`, result(t, do(t, fx.h, http.MethodDelete, "/client/v4/zones/"+zone+"/dns_records/"+fx.spare, "")))
	})

	t.Run("a configuration that was never written is none", func(t *testing.T) {
		require.JSONEq(t, `{"tunnel_id":"`+fx.tunnel+`","version":0,"source":"cloudflare","config":null}`,
			result(t, do(t, fx.h, http.MethodGet, tun+"/"+fx.tunnel+"/configurations", "")))
	})

	t.Run("a configuration has the warp-routing that Cloudflare sets itself", func(t *testing.T) {
		put := `{"config":{"ingress":[{"hostname":"a.example.com","service":"https://10.0.0.5:8443","originRequest":` +
			`{"originServerName":"sni.lan","matchSNItoHost":true,"noTLSVerify":true,"httpHostHeader":"internal.lan"}},` +
			`{"service":"http_status:404"}]}}`
		want := `{"tunnel_id":"` + fx.tunnel + `","version":1,"source":"cloudflare","config":{"ingress":[` +
			`{"hostname":"a.example.com","service":"https://10.0.0.5:8443","originRequest":` +
			`{"originServerName":"sni.lan","matchSNItoHost":true,"noTLSVerify":true,"httpHostHeader":"internal.lan"}},` +
			`{"service":"http_status:404"}],"warp-routing":{"enabled":true}}}`
		require.JSONEq(t, want, result(t, do(t, fx.h, http.MethodPut, tun+"/"+fx.tunnel+"/configurations", put)), "what a write answers")
		require.JSONEq(t, want, result(t, do(t, fx.h, http.MethodGet, tun+"/"+fx.tunnel+"/configurations", "")), "what a read answers")
	})

	t.Run("settings that pco does not manage sit at the top of the configuration", func(t *testing.T) {
		fx.f.SetForeign(acct, fx.tunnel, true)
		require.JSONEq(t, `{"tunnel_id":"`+fx.tunnel+`","version":1,"source":"cloudflare","config":{"ingress":[`+
			`{"hostname":"a.example.com","service":"https://10.0.0.5:8443","originRequest":`+
			`{"originServerName":"sni.lan","matchSNItoHost":true,"noTLSVerify":true,"httpHostHeader":"internal.lan"}},`+
			`{"service":"http_status:404"}],"warp-routing":{"enabled":true},"originRequest":{"connectTimeout":30}}}`,
			result(t, do(t, fx.h, http.MethodGet, tun+"/"+fx.tunnel+"/configurations", "")))

		fresh := fx.f.SeedTunnel(acct, "pco-foreign", nil)
		fx.f.SetForeign(acct, fresh.ID, true)
		require.JSONEq(t, `{"tunnel_id":"`+fresh.ID+`","version":0,"source":"cloudflare","config":`+
			`{"warp-routing":{"enabled":true},"originRequest":{"connectTimeout":30}}}`,
			result(t, do(t, fx.h, http.MethodGet, tun+"/"+fresh.ID+"/configurations", "")), "also where no rule was written")
	})

	t.Run("connectors", func(t *testing.T) {
		fx.f.SetConnectors(acct, fx.tunnel, []cfapi.Connector{{ID: "c1", Version: "2026.9.0", ConfigVersion: 4, Connections: 2}, {ID: "c2", Version: "2026.8.1"}})
		require.JSONEq(t, `[{"id":"c1","version":"2026.9.0","config_version":4,"conns":[{"id":"c1-1"},{"id":"c1-2"}]},
			{"id":"c2","version":"2026.8.1","config_version":0,"conns":[]}]`,
			result(t, do(t, fx.h, http.MethodGet, tun+"/"+fx.tunnel+"/connections", "")))

		bare := fx.f.SeedTunnel(acct, "pco-bare", nil)
		require.JSONEq(t, `[]`, result(t, do(t, fx.h, http.MethodGet, tun+"/"+bare.ID+"/connections", "")), "no connectors is a list")
	})

	t.Run("a record", func(t *testing.T) {
		fx.f.SeedRecord(zone, cfapi.Record{ID: "commented", Type: "CNAME", Name: "c.example.com", Content: "x.example.com", Proxied: true, Comment: "pco:abc"})
		items := result(t, do(t, fx.h, http.MethodGet, "/client/v4/zones/"+zone+"/dns_records?name=c.example.com", ""))
		require.JSONEq(t, `[{"id":"commented","type":"CNAME","name":"c.example.com","content":"x.example.com","proxied":true,"ttl":1,
			"comment":"pco:abc","modified_on":"2026-03-04T05:06:07.123456789Z"}]`, items)

		items = result(t, do(t, fx.h, http.MethodGet, "/client/v4/zones/"+zone+"/dns_records?name=a.example.com", ""))
		require.JSONEq(t, `[{"id":"`+fx.record+`","type":"A","name":"a.example.com","content":"192.0.2.1","proxied":false,"ttl":1,
			"comment":null,"modified_on":"2026-03-04T05:06:07.123456789Z"}]`, items, "no comment is null")
	})

	t.Run("accounts and zones", func(t *testing.T) {
		require.JSONEq(t, `[{"id":"acct1","name":"Acme","type":"standard"}]`, result(t, do(t, fx.h, http.MethodGet, "/client/v4/accounts", "")))
		require.JSONEq(t, `[{"id":"zone1","name":"example.com","status":"active","account":{"id":"acct1"}}]`,
			result(t, do(t, fx.h, http.MethodGet, "/client/v4/zones", "")))
	})

	t.Run("a token", func(t *testing.T) {
		require.JSONEq(t, `{"id":"token-1","status":"active"}`, result(t, do(t, fx.h, http.MethodGet, "/client/v4/user/tokens/verify", "")))

		expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		fx.f.SetTokenStatus("expired", &expires)
		require.JSONEq(t, `{"id":"token-1","status":"expired","expires_on":"2030-01-02T03:04:05Z"}`,
			result(t, do(t, fx.h, http.MethodGet, "/client/v4/user/tokens/verify", "")))
	})
}

func TestTimesAreWrittenInUTC(t *testing.T) {
	f := cffake.New()
	cest := time.FixedZone("CEST", 2*3600)
	f.SetNow(func() time.Time { return time.Date(2026, 3, 4, 7, 6, 7, 0, cest) })
	std(f)
	expires := time.Date(2030, 1, 2, 5, 4, 5, 0, cest)
	f.SetTokenStatus("active", &expires)
	f.SeedRecord(zone, rec("A", "a.example.com", "192.0.2.1"))
	f.SeedTunnel(acct, "pco-abc", nil)
	h := cffake.Handler(f)

	for target, want := range map[string]string{
		"/client/v4/user/tokens/verify":                            `"expires_on":"2030-01-02T03:04:05Z"`,
		"/client/v4/zones/" + zone + "/dns_records":                `"modified_on":"2026-03-04T05:06:07Z"`,
		"/client/v4/accounts/" + acct + "/cfd_tunnel?name=pco-abc": `"created_at":"2026-03-04T05:06:07Z"`,
	} {
		require.Contains(t, do(t, h, http.MethodGet, target, "").Body.String(), want, target)
	}
}

func TestTheTwoFormsOfTheTokenCheck(t *testing.T) {
	user, byAccount := "/client/v4/user/tokens/verify", "/client/v4/accounts/"+acct+"/tokens/verify"

	t.Run("a user's token is verified for the user", func(t *testing.T) {
		fx := newWireFixture()
		require.Equal(t, http.StatusOK, do(t, fx.h, http.MethodGet, user, "").Code)
		requireRefused(t, do(t, fx.h, http.MethodGet, byAccount, ""), http.StatusUnauthorized, 1000, "Invalid API Token")
	})

	t.Run("an account's token is verified for its account", func(t *testing.T) {
		fx := newWireFixture()
		fx.f.AddAccount("acct2", "Other")
		fx.f.SetTokenOwner(acct)
		requireRefused(t, do(t, fx.h, http.MethodGet, user, ""), http.StatusUnauthorized, 1000, "Invalid API Token")
		require.Equal(t, http.StatusOK, do(t, fx.h, http.MethodGet, byAccount, "").Code)
		requireRefused(t, do(t, fx.h, http.MethodGet, "/client/v4/accounts/acct2/tokens/verify", ""), http.StatusUnauthorized, 1000, "Invalid API Token")
	})

	t.Run("a denied check is denied in both forms", func(t *testing.T) {
		fx := newWireFixture()
		fx.f.Deny("verify")
		requireRefused(t, do(t, fx.h, http.MethodGet, user, ""), http.StatusForbidden, 10000, "Authentication error")
		requireRefused(t, do(t, fx.h, http.MethodGet, byAccount, ""), http.StatusForbidden, 10000, "Authentication error")
	})
}

func TestErrorsKeepTheStatusTheFakeGave(t *testing.T) {
	zones := "/client/v4/zones"

	t.Run("the status, the codes and the message of an injected error", func(t *testing.T) {
		for _, status := range []int{400, 401, 403, 404, 409, 410, 422, 500, 502, 503, 504, 599} {
			fx := newWireFixture()
			fx.f.FailNext("zones", 1, &cfapi.Error{Status: status, Codes: []int{1111, 2222}, Message: "injected"})

			rec := do(t, fx.h, http.MethodGet, zones, "")

			require.Equal(t, status, rec.Code)
			a := decode(t, rec)
			require.Equal(t, []wireError{{1111, "injected"}, {2222, "injected"}}, a.Errors)
			require.Empty(t, rec.Header().Get("Retry-After"))
		}
	})

	t.Run("a 429 says when to try again", func(t *testing.T) {
		for retry, header := range map[time.Duration]string{
			0: "1", time.Millisecond: "1", time.Second: "1", 1500 * time.Millisecond: "2", 90 * time.Second: "90", time.Hour: "3600",
		} {
			fx := newWireFixture()
			fx.f.FailNext("zones", 1, &cfapi.Error{Status: 429, Codes: []int{971}, Message: "slow down", RetryAfter: retry})

			rec := do(t, fx.h, http.MethodGet, zones, "")

			requireRefused(t, rec, http.StatusTooManyRequests, 971, "slow down")
			require.Equal(t, header, rec.Header().Get("Retry-After"), retry)
		}
	})

	t.Run("an error that is not one of the API is a 500", func(t *testing.T) {
		fx := newWireFixture()
		fx.f.FailNext("zones", 1, errors.New("boom"))
		requireRefused(t, do(t, fx.h, http.MethodGet, zones, ""), http.StatusInternalServerError, codeServerError, "boom")

		fx.f.FailNext("zones", 1, nil)
		requireRefused(t, do(t, fx.h, http.MethodGet, zones, ""), http.StatusInternalServerError, codeServerError, "injected failure")
	})

	t.Run("a status that is not an error is a 500", func(t *testing.T) {
		for _, status := range []int{0, 200, 204, 302, 399, 600} {
			fx := newWireFixture()
			fx.f.FailNext("zones", 1, &cfapi.Error{Status: status, Message: "odd"})
			requireRefused(t, do(t, fx.h, http.MethodGet, zones, ""), http.StatusInternalServerError, codeServerError, "odd")
		}
	})

	t.Run("an error with no code and no message gets both", func(t *testing.T) {
		for status, code := range map[int]int{400: codeBadRequest, 401: 10000, 403: 10000, 404: 7003, 409: codeConflict, 410: codeBadRequest, 429: 971, 503: codeServerError} {
			fx := newWireFixture()
			fx.f.FailNext("zones", 1, &cfapi.Error{Status: status})
			requireRefused(t, do(t, fx.h, http.MethodGet, zones, ""), status, code, http.StatusText(status))
		}
	})

	t.Run("a failure is used up by the request it fails", func(t *testing.T) {
		fx := newWireFixture()
		fx.f.FailNext("zones", 2, &cfapi.Error{Status: 503, Message: "down"})
		require.Equal(t, 503, do(t, fx.h, http.MethodGet, zones, "").Code)
		require.Equal(t, 503, do(t, fx.h, http.MethodGet, zones, "").Code)
		require.Equal(t, 200, do(t, fx.h, http.MethodGet, zones, "").Code)
	})

	t.Run("what is denied is a 403", func(t *testing.T) {
		fx := newWireFixture()
		fx.f.Deny("dns.write")
		rec := do(t, fx.h, http.MethodDelete, "/client/v4/zones/"+zone+"/dns_records/"+fx.record, "")
		requireRefused(t, rec, http.StatusForbidden, 10000, "Authentication error")
		require.Len(t, fx.f.RecordsIn(zone), 2)
		require.Equal(t, 200, do(t, fx.h, http.MethodGet, "/client/v4/zones/"+zone+"/dns_records", "").Code, "reading is not denied")
	})

	t.Run("the refusals of the fake keep the codes the client reads", func(t *testing.T) {
		fx := newWireFixture()
		tun := "/client/v4/accounts/" + acct + "/cfd_tunnel"
		records := "/client/v4/zones/" + zone + "/dns_records"

		requireRefused(t, do(t, fx.h, http.MethodPost, tun, `{"name":"pco-abc"}`), http.StatusConflict, 1013, "already exists")
		requireRefused(t, do(t, fx.h, http.MethodPost, records, `{"type":"CNAME","name":"a.example.com","content":"x.example.com"}`),
			http.StatusBadRequest, 81053, "already exists")
		requireRefused(t, do(t, fx.h, http.MethodPost, records, `{"type":"A","name":"a.example.com","content":"192.0.2.1"}`),
			http.StatusBadRequest, 81058, "identical record")
		requireRefused(t, do(t, fx.h, http.MethodDelete, records+"/nope", ""), http.StatusNotFound, 7003, "not found")
		requireRefused(t, do(t, fx.h, http.MethodGet, "/client/v4/zones/nope/dns_records", ""), http.StatusNotFound, 7003, "not found")
		requireRefused(t, do(t, fx.h, http.MethodGet, tun+"/nope/token", ""), http.StatusNotFound, 7003, "not found")
		requireRefused(t, do(t, fx.h, http.MethodGet, "/client/v4/accounts/nope/cfd_tunnel", ""), http.StatusNotFound, 7003, "not found")

		fx.f.SetConnectors(acct, fx.doomed, []cfapi.Connector{{ID: "c1", Connections: 1}})
		requireRefused(t, do(t, fx.h, http.MethodDelete, tun+"/"+fx.doomed, ""), http.StatusBadRequest, codeBadRequest, "active connections")

		requireRefused(t, do(t, fx.h, http.MethodPut, tun+"/"+fx.tunnel+"/configurations", `{"config":{"ingress":[]}}`),
			http.StatusBadRequest, codeBadRequest, "no rules")
		requireRefused(t, do(t, fx.h, http.MethodPut, tun+"/"+fx.tunnel+"/configurations", `{"config":{"ingress":[{"hostname":"a.example.com","service":"x"}]}}`),
			http.StatusBadRequest, codeBadRequest, "last ingress rule")
		requireRefused(t, do(t, fx.h, http.MethodPost, tun, `{"name":" "}`), http.StatusBadRequest, codeBadRequest, "tunnel name is empty")
	})
}

// failingAtPage makes the fake fail its operation on the request for page.
func failingAtPage(f *cffake.Fake, h http.Handler, op string, page string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == page {
			f.FailNext(op, 1, &cfapi.Error{Status: http.StatusBadGateway, Message: "bad gateway"})
		}
		h.ServeHTTP(w, r)
	})
}

func TestAListingThatFailsHalfWayIsAnError(t *testing.T) {
	// The fake can only be made to fail the next call, so the page that is to
	// fail is announced in front of the handler.
	fx := newWireFixture()
	for i := range 120 {
		fx.f.AddAccount(fmt.Sprintf("a%03d", i), "x")
		fx.f.AddZone(fmt.Sprintf("z%03d", i), fmt.Sprintf("z%03d.example", i), acct)
		fx.f.SeedTunnel(acct, fmt.Sprintf("other-%03d", i), nil)
		fx.f.SeedRecord(zone, rec("TXT", fmt.Sprintf("t%03d.example.com", i), "x"))
	}
	// The client reads records 5000 to a page.
	for i := 120; i <= 5000; i++ {
		fx.f.SeedRecord(zone, rec("TXT", fmt.Sprintf("t%04d.example.com", i), "x"))
	}
	for _, tc := range []struct {
		name string
		op   string
		page string
		call call
	}{
		{"Accounts", "accounts", "2", accounts()},
		{"Zones", "zones", "2", zones()},
		{"Zones on the last page", "zones", "3", zones()},
		{"Records", "dns.read", "2", records(zone, cfapi.RecordFilter{})},
		{"Tunnels", "tunnel.read", "2", tunnels(acct, "other-")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(failingAtPage(fx.f, fx.h, tc.op, tc.page))
			t.Cleanup(srv.Close)
			c, err := cfapi.New(cfapi.Options{BaseURL: srv.URL + "/client/v4", Token: token, Limiter: cfapi.NewLimiter(1000, time.Minute, 100, nil)})
			require.NoError(t, err)

			got, err := tc.call(c)

			var apiErr *cfapi.Error
			require.ErrorAs(t, err, &apiErr, "a listing with a page missing is no listing")
			require.Equal(t, http.StatusBadGateway, apiErr.Status)
			require.True(t, got == nil || reflect.ValueOf(got).IsZero(), "nothing of the pages that were read: %#v", got)
		})
	}
}

// The places where the fake and the client do not see eye to eye. They are
// kept out of the contract, and pinned here so that they are known.

func TestAnAccountTokenIsOnlyLookedForAtTheFirstFiveAccounts(t *testing.T) {
	f := cffake.New()
	for i := range 7 {
		f.AddAccount(fmt.Sprintf("a%d", i), "x")
	}
	f.SetTokenOwner("a5")

	_, err := f.VerifyToken(ctx)
	require.NoError(t, err, "the fake verifies the token wherever its account is")

	_, err = client(t, f).VerifyToken(ctx)
	require.True(t, cfapi.IsAuth(err), "the client asks the first five accounts and no more: %v", err)
}

func TestACheckThatFailsOnceIsMadeGoodByTheAccountForm(t *testing.T) {
	f := cffake.New()
	f.AddAccount(acct, "Acme")
	f.SetTokenOwner(acct)
	f.FailNext("verify", 1, &cfapi.Error{Status: http.StatusForbidden, Message: "no"})

	_, err := f.VerifyToken(ctx)
	require.True(t, cfapi.IsAuth(err), "the fake fails the call")

	f.FailNext("verify", 1, &cfapi.Error{Status: http.StatusForbidden, Message: "no"})
	st, err := client(t, f).VerifyToken(ctx)
	require.NoError(t, err, "the client goes on to the account form, which is not failed")
	require.Equal(t, "active", st.Status)
}

func TestAnAccountTokenAsksForTheAccountsToo(t *testing.T) {
	f := cffake.New()
	f.AddAccount(acct, "Acme")
	f.SetTokenOwner(acct)
	f.Deny("accounts")

	_, err := f.VerifyToken(ctx)
	require.NoError(t, err, "the fake does not list accounts to verify a token")

	_, err = client(t, f).VerifyToken(ctx)
	require.True(t, cfapi.IsAuth(err), "the client does: %v", err)
}

func TestNoRulesAreRefusedByTheClientBeforeTheWire(t *testing.T) {
	fx := newWireFixture()

	_, err := fx.f.PutTunnelConfig(ctx, acct, fx.tunnel, nil)
	require.True(t, isStatus(http.StatusBadRequest)(err), "the fake refuses them as Cloudflare does: %v", err)
	require.False(t, isInvalid(err))

	before := len(fx.f.Calls())
	_, err = client(t, fx.f).PutTunnelConfig(ctx, acct, fx.tunnel, nil)
	require.True(t, isInvalid(err), "the client refuses them itself: %v", err)
	require.Len(t, fx.f.Calls(), before, "and sends nothing")
}

func TestAZoneWithoutAnAccountIsNotAZoneToTheClient(t *testing.T) {
	f := cffake.New()
	f.AddZone("z1", "example.com", "")

	zones, err := f.Zones(ctx)
	require.NoError(t, err)
	require.Len(t, zones, 1, "the fake lists it")

	_, err = client(t, f).Zones(ctx)
	require.ErrorContains(t, err, "zone without an id, a name or an account")
}

func TestHandlerIsSafeForConcurrentUse(t *testing.T) {
	fx := newWireFixture()
	c := client(t, fx.f)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("host%d.example.com", i)
			created, err := c.CreateRecord(ctx, zone, rec("A", name, fmt.Sprintf("192.0.2.%d", 100+i)))
			if err == nil {
				_, err = c.Records(ctx, zone, cfapi.RecordFilter{Name: name})
			}
			if err == nil {
				err = c.DeleteRecord(ctx, zone, created.ID)
			}
			if err == nil {
				_, err = c.PutTunnelConfig(ctx, acct, fx.tunnel, []planner.IngressRule{planner.CatchAllRule()})
			}
			if err == nil {
				_, err = c.Tunnels(ctx, acct, "pco-")
			}
			assertNoError(t, err)
		}()
	}
	wg.Wait()
	require.Len(t, fx.f.RecordsIn(zone), 2)
}

// assertNoError is require.NoError for a goroutine other than the test's.
func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func FuzzHandler(f *testing.F) {
	for _, seed := range []struct{ method, path, query, body string }{
		{"GET", "zones", "page=2&per_page=3", ""},
		{"GET", "accounts/acct1/cfd_tunnel", "name=pco-abc&include_prefix=pco&is_deleted=false", ""},
		{"POST", "accounts/acct1/cfd_tunnel", "", `{"name":"x","config_src":"cloudflare"}`},
		{"PUT", "accounts/acct1/cfd_tunnel/t/configurations", "", `{"config":{"ingress":[{"service":"http_status:404"}]}}`},
		{"POST", "zones/zone1/dns_records", "", `{"type":"A","name":"a","content":"x"}`},
		{"PATCH", "zones/zone1/dns_records/r", "", `{"type":"A","name":"a","content":"x","proxied":false,"comment":"","ttl":1}`},
		{"DELETE", "zones/zone1/dns_records/%2e%2e", "", ""},
		{"GET", "zones/%00/dns_records", "type=%ff&name=%", ""},
		{"GET", "accounts/ü/cfd_tunnel/\U0001f600/token", "", ""},
		{"POST", "zones/zone1/dns_records", "", strings.Repeat("[", 10000)},
		{"GET", "", "page=" + strings.Repeat("9", 400), ""},
	} {
		f.Add(seed.method, seed.path, seed.query, seed.body)
	}
	fx := newWireFixture()
	f.Fuzz(func(t *testing.T, method, path, query, body string) {
		u := url.URL{Path: "/client/v4/" + path, RawQuery: query}
		req, err := http.NewRequest(method, u.String(), strings.NewReader(body))
		if err != nil {
			t.Skip()
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()

		fx.h.ServeHTTP(rec, req)

		require.GreaterOrEqual(t, rec.Code, 200)
		require.Less(t, rec.Code, 600)
		decode(t, rec)
	})
}
