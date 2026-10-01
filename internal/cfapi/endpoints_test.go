package cfapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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

func TestAccounts(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(
		`[{"id":"a1","name":"First","type":"standard","settings":{}},{"id":"a2","name":"Second"}]`)))

	got, err := env.c.Accounts(context.Background())

	require.NoError(t, err)
	require.Equal(t, []Account{{ID: "a1", Name: "First"}, {ID: "a2", Name: "Second"}}, got)
	req := only(t, env)
	require.Equal(t, http.MethodGet, req.method)
	require.Equal(t, "/client/v4/accounts", req.path(t))
	require.Equal(t, url.Values{"page": {"1"}, "per_page": {"100"}}, req.query(t))
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
	require.Equal(t, url.Values{"page": {"1"}, "per_page": {"100"}}, req.query(t))
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
		"name": {"pco-abc"}, "is_deleted": {"false"}, "page": {"1"}, "per_page": {"100"},
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
		reply(http.StatusOK, `{"success":true,"result":[],"result_info":{"page":1,"per_page":100,"total_pages":2}}`)(w, r)
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

func TestTunnelConfigIgnoresWhatItDoesNotKnow(t *testing.T) {
	wire := `{"hostname":"app.example.com","path":"/api","service":"http://10.0.0.5:8080","originRequest":` +
		`{"connectTimeout":30,"noTLSVerify":false,"httpHostHeader":"internal.lan"}}`
	env := setup(t, reply(http.StatusOK, okBody(configBody(2, wire))))

	got, err := env.c.TunnelConfig(context.Background(), "a1", "t1")

	require.NoError(t, err)
	require.Equal(t, []planner.IngressRule{
		{Hostname: "app.example.com", Service: "http://10.0.0.5:8080", HTTPHostHeader: "internal.lan"},
	}, got.Ingress)
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

func TestPutTunnelConfigWithoutRulesSendsAList(t *testing.T) {
	for name, rules := range map[string][]planner.IngressRule{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(configBody(1, ``))))
			_, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", rules)
			require.NoError(t, err)
			require.JSONEq(t, `{"config":{"ingress":[]}}`, only(t, env).body)
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
	version, err := env.c.PutTunnelConfig(context.Background(), "a1", "t1", nil)
	require.Error(t, err)
	require.Zero(t, version)
}

func TestConnectors(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`[
		{"id":"c1","version":"2026.9.0","config_version":4,"arch":"linux_amd64","run_at":"2026-01-01T00:00:00Z",
		 "conns":[{"id":"x","colo_name":"PRG"},{"id":"y","colo_name":"FRA"},{"id":"z","colo_name":"VIE"},{"id":"w","colo_name":"AMS"}]},
		{"id":"c2","version":"2026.8.1","config_version":3,"conns":[{"id":"v"}]},
		{"id":"c3","version":"2026.8.1","config_version":0,"conns":[]},
		{"id":"c4","version":"2026.8.1"}]`)))

	got, err := env.c.Connectors(context.Background(), "a1", "t1")

	require.NoError(t, err)
	require.Equal(t, []Connector{
		{ID: "c1", Version: "2026.9.0", ConfigVersion: 4, Connections: 4},
		{ID: "c2", Version: "2026.8.1", ConfigVersion: 3, Connections: 1},
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

func TestConnectorsWithoutResult(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`null`)))
	got, err := env.c.Connectors(context.Background(), "a1", "t1")
	require.Error(t, err)
	require.Nil(t, got)
}

const recordJSON = `{"id":"r1","type":"CNAME","name":"app.example.com","content":"t1.cfargotunnel.com",` +
	`"proxied":true,"ttl":1,"comment":"pco:abc","created_on":"2026-01-01T00:00:00Z","modified_on":"2026-02-03T04:05:06.5Z"}`

func wantRecord(t *testing.T) Record {
	t.Helper()
	return Record{
		ID: "r1", Type: "CNAME", Name: "app.example.com", Content: "t1.cfargotunnel.com",
		Proxied: true, Comment: "pco:abc", ModifiedOn: ts(t, "2026-02-03T04:05:06.5Z"),
	}
}

func TestRecords(t *testing.T) {
	tests := []struct {
		name   string
		filter RecordFilter
		query  url.Values
	}{
		{"no filter", RecordFilter{}, url.Values{}},
		{"type", RecordFilter{Type: "TXT"}, url.Values{"type": {"TXT"}}},
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
			want := url.Values{"page": {"1"}, "per_page": {"100"}}
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

	tests := []struct {
		name string
		call func(c *Client) error
	}{
		{"FindTunnel without account", func(c *Client) error { _, _, err := c.FindTunnel(ctx, "", "n"); return err }},
		{"FindTunnel without name", func(c *Client) error { _, _, err := c.FindTunnel(ctx, "a1", ""); return err }},
		{"FindTunnel blank name", func(c *Client) error { _, _, err := c.FindTunnel(ctx, "a1", "  "); return err }},
		{"CreateTunnel without account", func(c *Client) error { _, err := c.CreateTunnel(ctx, "", "n"); return err }},
		{"CreateTunnel without name", func(c *Client) error { _, err := c.CreateTunnel(ctx, "a1", ""); return err }},
		{"DeleteTunnel without account", func(c *Client) error { return c.DeleteTunnel(ctx, "", "t1") }},
		{"DeleteTunnel without tunnel", func(c *Client) error { return c.DeleteTunnel(ctx, "a1", "") }},
		{"TunnelToken without account", func(c *Client) error { _, err := c.TunnelToken(ctx, "", "t1"); return err }},
		{"TunnelToken without tunnel", func(c *Client) error { _, err := c.TunnelToken(ctx, "a1", ""); return err }},
		{"TunnelConfig without account", func(c *Client) error { _, err := c.TunnelConfig(ctx, "", "t1"); return err }},
		{"TunnelConfig without tunnel", func(c *Client) error { _, err := c.TunnelConfig(ctx, "a1", ""); return err }},
		{"PutTunnelConfig without account", func(c *Client) error { _, err := c.PutTunnelConfig(ctx, "", "t1", nil); return err }},
		{"PutTunnelConfig without tunnel", func(c *Client) error { _, err := c.PutTunnelConfig(ctx, "a1", "", nil); return err }},
		{"Connectors without account", func(c *Client) error { _, err := c.Connectors(ctx, "", "t1"); return err }},
		{"Connectors without tunnel", func(c *Client) error { _, err := c.Connectors(ctx, "a1", ""); return err }},
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`{}`)))
			require.Error(t, tt.call(env.c))
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
		{"slash is escaped", func(c *Client) error { return c.DeleteRecord(ctx, "z1", "a/b") }, "/client/v4/zones/z1/dns_records/a%2Fb"},
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

	for _, id := range []string{".", ".."} {
		t.Run("dot "+id, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, okBody(`{}`)))
			require.Error(t, env.c.DeleteRecord(ctx, "z1", id))
			require.Error(t, env.c.DeleteTunnel(ctx, id, "t1"))
			require.Empty(t, env.requests())
		})
	}
}
