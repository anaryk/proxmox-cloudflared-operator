package cfapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

const testToken = "cf-token-7d41c9e0-secret"

type seenRequest struct {
	method      string
	uri         string
	auth        string
	accept      string
	contentType string
	userAgent   string
	body        string
}

// testEnv is a client wired to a test server, a fake clock and a recorder.
type testEnv struct {
	c     *Client
	clock *fakeClock
	url   string

	mu   sync.Mutex
	seen []seenRequest
}

func (e *testEnv) requests() []seenRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]seenRequest(nil), e.seen...)
}

func (e *testEnv) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, seenRequest{
		method:      r.Method,
		uri:         r.URL.RequestURI(),
		auth:        r.Header.Get("Authorization"),
		accept:      r.Header.Get("Accept"),
		contentType: r.Header.Get("Content-Type"),
		userAgent:   r.Header.Get("User-Agent"),
		body:        string(body),
	})
}

// setup starts a plain-HTTP server on a loopback address. The limiter runs on
// a fake clock, so no test ever sleeps for real.
func setup(t *testing.T, h http.HandlerFunc, mods ...func(*Options)) *testEnv {
	t.Helper()
	env := &testEnv{clock: newFakeClock()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env.record(r)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	env.url = srv.URL + "/client/v4"

	opts := Options{
		BaseURL: env.url,
		Token:   testToken,
		Limiter: newLimiter(300, 5*time.Minute, 20, env.clock.now, env.clock.sleep),
	}
	for _, mod := range mods {
		mod(&opts)
	}
	c, err := New(opts)
	require.NoError(t, err)
	env.c = c
	return env
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func okBody(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

func TestNewChecksOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    Options
		wantErr string
	}{
		{"defaults", Options{Token: testToken}, ""},
		{"https host", Options{Token: testToken, BaseURL: "https://cf.example.com/client/v4"}, ""},
		{"http on ipv4 loopback", Options{Token: testToken, BaseURL: "http://127.0.0.1:8787/client/v4"}, ""},
		{"http on ipv6 loopback", Options{Token: testToken, BaseURL: "http://[::1]:8787"}, ""},
		{"https on loopback", Options{Token: testToken, BaseURL: "https://127.0.0.1:8787"}, ""},
		{"empty token", Options{}, "token is empty"},
		{"blank token", Options{Token: " \t"}, "token is empty"},
		{"http on a named host", Options{Token: testToken, BaseURL: "http://cf.example.com"}, "https"},
		{"http on a private address", Options{Token: testToken, BaseURL: "http://10.0.0.5"}, "https"},
		{"localhost by name is not trusted", Options{Token: testToken, BaseURL: "http://localhost:8787"}, "https"},
		{"other scheme", Options{Token: testToken, BaseURL: "ftp://127.0.0.1"}, "https"},
		{"no scheme", Options{Token: testToken, BaseURL: "api.cloudflare.com"}, "no host"},
		{"no host", Options{Token: testToken, BaseURL: "https:///client/v4"}, "no host"},
		{"credentials in the url", Options{Token: testToken, BaseURL: "https://user:pw@cf.example.com"}, "credentials"},
		{"query in the url", Options{Token: testToken, BaseURL: "https://cf.example.com/?a=b"}, "a query"},
		{"fragment in the url", Options{Token: testToken, BaseURL: "https://cf.example.com/#x"}, "a fragment"},
		{"unparsable url", Options{Token: testToken, BaseURL: "https://cf.example.com:bad"}, "parsing base url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(tt.opts)
			if tt.wantErr == "" {
				require.NoError(t, err)
				require.NotNil(t, c)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
			require.Nil(t, c)
		})
	}
}

func TestNewErrorsDoNotRepeatTheURL(t *testing.T) {
	// A base URL may carry a secret; none of the errors about it may echo it.
	for _, raw := range []string{
		"http://user:" + testToken + "@cf.example.com",
		"https://user:" + testToken + "@cf.example.com",
		"https://cf.example.com:" + testToken,
		"https://cf.example.com/?key=" + testToken,
		"ftp://" + testToken,
	} {
		_, err := New(Options{Token: testToken, BaseURL: raw})
		require.Error(t, err, raw)
		require.NotContains(t, err.Error(), testToken, raw)
	}
}

func TestNewDefaults(t *testing.T) {
	c, err := New(Options{Token: testToken})
	require.NoError(t, err)

	require.Equal(t, "https://api.cloudflare.com/client/v4", c.base.String())
	require.Equal(t, 30*time.Second, c.hc.Timeout)
	require.NotNil(t, c.limiter)
	require.Equal(t, "pco/"+version.Version, c.userAgent)

	// The API never redirects, and following one would send the token elsewhere.
	require.NotNil(t, c.hc.CheckRedirect)
}

func TestDoSendsTheRequest(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{}`)))

	query := url.Values{"name": {"app.example.com"}, "type": {"CNAME"}}
	body := map[string]any{"content": "x.cfargotunnel.com", "ttl": 1}
	require.NoError(t, env.c.do(context.Background(), http.MethodPost, "/zones/z1/dns_records", query, body, nil))

	reqs := env.requests()
	require.Len(t, reqs, 1)
	got := reqs[0]
	require.Equal(t, http.MethodPost, got.method)
	require.Equal(t, "/client/v4/zones/z1/dns_records?name=app.example.com&type=CNAME", got.uri)
	require.Equal(t, "Bearer "+testToken, got.auth)
	require.Equal(t, "application/json", got.accept)
	require.Equal(t, "application/json", got.contentType)
	require.Equal(t, "pco/"+version.Version, got.userAgent)
	require.JSONEq(t, `{"content":"x.cfargotunnel.com","ttl":1}`, got.body)
}

func TestDoWithoutBody(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{}`)))
	require.NoError(t, env.c.do(context.Background(), http.MethodGet, "zones", nil, nil, nil))

	got := env.requests()[0]
	require.Equal(t, "/client/v4/zones", got.uri)
	require.Empty(t, got.contentType)
	require.Empty(t, got.body)
}

func TestUserAgentOverride(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{}`)), func(o *Options) { o.UserAgent = "pco-test/9" })
	require.NoError(t, env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil))
	require.Equal(t, "pco-test/9", env.requests()[0].userAgent)
}

func TestDoRejectsBodyThatCannotBeEncoded(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{}`)))
	err := env.c.do(context.Background(), http.MethodPost, "/zones", nil, make(chan int), nil)
	require.ErrorContains(t, err, "encoding request")
	require.Empty(t, env.requests())
}

func TestDoDecodesResult(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{"id":"abc","count":3,"ignored":true}`)))

	var out struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
	}
	require.NoError(t, env.c.do(context.Background(), http.MethodGet, "/zones/abc", nil, nil, &out))
	require.Equal(t, "abc", out.ID)
	require.Equal(t, 3, out.Count)
}

func TestDoIgnoresResultWithoutOut(t *testing.T) {
	for _, body := range []string{
		okBody(`{"id":"abc"}`),
		okBody(`null`),
		okBody(`"anything"`),
		`{"success":true}`,
	} {
		env := setup(t, reply(http.StatusOK, body))
		require.NoError(t, env.c.do(context.Background(), http.MethodDelete, "/zones/abc", nil, nil, nil), body)
	}
}

func TestDoErrorEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr Error
	}{
		{
			name:   "success false with two codes",
			status: http.StatusOK,
			body:   `{"success":false,"errors":[{"code":1003,"message":"first problem"},{"code":1004,"message":"second problem"}],"result":null}`,
			wantErr: Error{
				Status: http.StatusOK, Codes: []int{1003, 1004}, Message: "first problem",
			},
		},
		{
			name:   "client error with codes",
			status: http.StatusBadRequest,
			body:   `{"success":false,"errors":[{"code":9106,"message":"Missing header"},{"code":9109,"message":"Invalid token"}]}`,
			wantErr: Error{
				Status: http.StatusBadRequest, Codes: []int{9106, 9109}, Message: "Missing header",
			},
		},
		{
			name:   "first non-empty message",
			status: http.StatusBadRequest,
			body:   `{"success":false,"errors":[{"code":1,"message":" "},{"code":2,"message":"second"}]}`,
			wantErr: Error{
				Status: http.StatusBadRequest, Codes: []int{1, 2}, Message: "second",
			},
		},
		{
			name:    "non-2xx without a body",
			status:  http.StatusNotFound,
			body:    ``,
			wantErr: Error{Status: http.StatusNotFound, Message: "Not Found"},
		},
		{
			name:    "non-2xx with an html page",
			status:  http.StatusBadGateway,
			body:    `<html><body>bad gateway</body></html>`,
			wantErr: Error{Status: http.StatusBadGateway, Message: "Bad Gateway"},
		},
		{
			name:    "non-2xx with an envelope without errors",
			status:  http.StatusInternalServerError,
			body:    `{"success":false,"errors":[],"result":null}`,
			wantErr: Error{Status: http.StatusInternalServerError, Message: "Internal Server Error"},
		},
		{
			name:    "non-2xx that claims success",
			status:  http.StatusForbidden,
			body:    `{"success":true,"result":{}}`,
			wantErr: Error{Status: http.StatusForbidden, Message: "Forbidden"},
		},
		{
			name:    "unknown status code",
			status:  599,
			body:    `nope`,
			wantErr: Error{Status: 599, Message: "unknown error"},
		},
		{
			name:    "success false without any message",
			status:  http.StatusOK,
			body:    `{"success":false}`,
			wantErr: Error{Status: http.StatusOK, Message: "request was not successful"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(tt.status, tt.body))
			var out map[string]any
			err := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, &out)

			var apiErr *Error
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, tt.wantErr, *apiErr)
			require.Nil(t, out)
		})
	}
}

func TestErrorString(t *testing.T) {
	tests := []struct {
		name string
		err  *Error
		want string
	}{
		{"message only", &Error{Status: 404, Message: "Not Found"}, "cloudflare api: HTTP 404: Not Found"},
		{"with codes", &Error{Status: 400, Codes: []int{1003, 1004}, Message: "bad"}, "cloudflare api: HTTP 400: bad (codes 1003, 1004)"},
		{"rate limited", &Error{Status: 429, Message: "slow down", RetryAfter: 7 * time.Second}, "cloudflare api: HTTP 429: slow down (retry after 7s)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.EqualError(t, tt.err, tt.want)
		})
	}
}

func TestClassifiers(t *testing.T) {
	apiErr := func(status int, codes ...int) error {
		return &Error{Status: status, Codes: codes, Message: "x"}
	}
	tests := []struct {
		name                                         string
		err                                          error
		auth, notFound, rateLimited, conflict, other bool
	}{
		{name: "401", err: apiErr(401), auth: true},
		{name: "403", err: apiErr(403), auth: true},
		{name: "404", err: apiErr(404), notFound: true},
		{name: "429", err: apiErr(429), rateLimited: true},
		{name: "409", err: apiErr(409), conflict: true},
		{name: "record already exists, code 81057", err: apiErr(400, 81057), conflict: true},
		{name: "cname already exists, code 81053", err: apiErr(400, 81053), conflict: true},
		{name: "identical record, code 81058", err: apiErr(400, 81058), conflict: true},
		{name: "conflicting code among others", err: apiErr(400, 1004, 81057), conflict: true},
		{name: "success false with a conflict code", err: apiErr(200, 81058), conflict: true},
		{name: "unrelated code", err: apiErr(400, 81000)},
		{name: "500", err: apiErr(500)},
		{name: "wrapped 403", err: fmt.Errorf("listing: %w", apiErr(403)), auth: true},
		{name: "wrapped 404", err: fmt.Errorf("a: %w", fmt.Errorf("b: %w", apiErr(404))), notFound: true},
		{name: "plain error", err: errors.New("403 forbidden")},
		{name: "nil", err: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.auth, IsAuth(tt.err), "IsAuth")
			require.Equal(t, tt.notFound, IsNotFound(tt.err), "IsNotFound")
			require.Equal(t, tt.rateLimited, IsRateLimited(tt.err), "IsRateLimited")
			require.Equal(t, tt.conflict, IsConflict(tt.err), "IsConflict")
		})
	}
}

func TestStatusesClassifyOverTheWire(t *testing.T) {
	tests := []struct {
		status int
		check  func(error) bool
	}{
		{http.StatusUnauthorized, IsAuth},
		{http.StatusForbidden, IsAuth},
		{http.StatusNotFound, IsNotFound},
		{http.StatusTooManyRequests, IsRateLimited},
		{http.StatusConflict, IsConflict},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			env := setup(t, reply(tt.status, `{"success":false,"errors":[{"code":10000,"message":"nope"}]}`))
			err := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil)
			require.Error(t, err)
			require.True(t, tt.check(err))
		})
	}
}

func TestDoUnexpectedResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
		out  any
	}{
		{"empty body", ``, nil},
		{"not json", `<html>captive portal</html>`, nil},
		{"truncated json", `{"success":true,"result":{"id":`, nil},
		{"json array", `[]`, nil},
		{"json null", `null`, nil},
		{"empty object", `{}`, nil},
		{"no success field", `{"result":{"id":"abc"},"errors":[]}`, new(map[string]any)},
		{"success of the wrong type", `{"success":"true","result":{}}`, nil},
		{"no result where one is wanted", `{"success":true}`, new(map[string]any)},
		{"null result where one is wanted", okBody(`null`), new(map[string]any)},
		{"errors of the wrong shape", `{"success":false,"errors":"boom"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, tt.body))
			err := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, tt.out)

			require.ErrorIs(t, err, errUnexpected)
			require.ErrorContains(t, err, "unexpected response")
			var apiErr *Error
			require.False(t, errors.As(err, &apiErr), "an unreadable answer is not an API error")
		})
	}
}

func TestDoResultThatDoesNotFitOut(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{"id":42}`)))
	var out struct {
		ID string `json:"id"`
	}
	err := env.c.do(context.Background(), http.MethodGet, "/zones/abc", nil, nil, &out)
	require.ErrorContains(t, err, "decoding result")
}

func TestDoResponseBodyLimit(t *testing.T) {
	const prefix, suffix = `{"success":true,"result":"`, `"}`
	bodyOfSize := func(n int) string {
		return prefix + strings.Repeat("a", n-len(prefix)-len(suffix)) + suffix
	}

	t.Run("a body at the limit is read", func(t *testing.T) {
		env := setup(t, reply(http.StatusOK, bodyOfSize(maxBodyBytes)))
		var out string
		require.NoError(t, env.c.do(context.Background(), http.MethodGet, "/big", nil, nil, &out))
		require.Len(t, out, maxBodyBytes-len(prefix)-len(suffix))
	})
	t.Run("a larger success is an error", func(t *testing.T) {
		env := setup(t, reply(http.StatusOK, bodyOfSize(maxBodyBytes+1)))
		var out string
		err := env.c.do(context.Background(), http.MethodGet, "/big", nil, nil, &out)
		require.ErrorContains(t, err, "exceeds")
		require.Empty(t, out)
	})
	t.Run("a larger failure keeps its status", func(t *testing.T) {
		body := `{"success":false,"errors":[{"code":1,"message":"` + strings.Repeat("a", maxBodyBytes) + `"}]}`
		env := setup(t, reply(http.StatusBadRequest, body))
		err := env.c.do(context.Background(), http.MethodGet, "/big", nil, nil, nil)

		var apiErr *Error
		require.ErrorAs(t, err, &apiErr)
		require.Equal(t, Error{Status: http.StatusBadRequest, Message: "Bad Request"}, *apiErr)
	})
}

func TestRateLimited(t *testing.T) {
	// Every test clock starts at the same instant.
	start := newFakeClock().now()
	inThirtySeconds := start.Add(30 * time.Second).UTC().Format(http.TimeFormat)
	aMinuteAgo := start.Add(-time.Minute).UTC().Format(http.TimeFormat)

	tests := []struct {
		name       string
		retryAfter string
		want       time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"seconds with spaces", " 12 ", 12 * time.Second},
		{"absent", "", time.Minute},
		{"not a number or a date", "soon", time.Minute},
		{"negative", "-3", time.Minute},
		{"fraction", "1.5", time.Minute},
		{"http date", inThirtySeconds, 30 * time.Second},
		{"http date in the past", aMinuteAgo, 0},
		{"zero", "0", 0},
		{"absurdly long", "99999999999999", time.Hour},
		{"does not fit an int64", "99999999999999999999999", time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := setup(t, func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"success":false,"errors":[{"code":971,"message":"Rate limited"}]}`)
			})

			err := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil)

			require.True(t, IsRateLimited(err))
			var apiErr *Error
			require.ErrorAs(t, err, &apiErr)
			require.Equal(t, Error{
				Status: http.StatusTooManyRequests, Codes: []int{971}, Message: "Rate limited", RetryAfter: tt.want,
			}, *apiErr)
			require.Len(t, env.requests(), 1, "do does not retry")

			// The limiter now holds every caller back for that long.
			var wantSleeps []time.Duration
			if tt.want > 0 {
				wantSleeps = []time.Duration{tt.want}
			}
			require.Equal(t, wantSleeps, takeAfterWait(t, env))
		})
	}
}

func TestRateLimitedWithoutEnvelope(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	err := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil)

	var apiErr *Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, Error{Status: 429, Message: "Too Many Requests", RetryAfter: 7 * time.Second}, *apiErr)
	require.Equal(t, []time.Duration{7 * time.Second}, takeAfterWait(t, env))
}

func takeAfterWait(t *testing.T, env *testEnv) []time.Duration {
	t.Helper()
	require.NoError(t, env.c.limiter.Wait(context.Background()))
	return env.clock.takeSleeps()
}

func TestDoWaitsForTheLimiter(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{}`)))
	env.c.limiter.Pause(5 * time.Second)

	require.NoError(t, env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil))
	require.Equal(t, []time.Duration{5 * time.Second}, env.clock.takeSleeps())
	require.Len(t, env.requests(), 1)
}

func TestDoStopsWhenContextEnds(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`{}`)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := env.c.do(ctx, http.MethodGet, "/zones", nil, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, env.requests())
}

func TestDoDoesNotFollowRedirects(t *testing.T) {
	var elsewhereHits int
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhereHits++ }))
	t.Cleanup(elsewhere.Close)

	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	})
	err := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil)

	var apiErr *Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusFound, apiErr.Status)
	require.Zero(t, elsewhereHits)
}

func TestDoUsesTheGivenHTTPClient(t *testing.T) {
	var used bool
	env := setup(t, reply(http.StatusOK, okBody(`{}`)), func(o *Options) {
		o.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			used = true
			return http.DefaultTransport.RoundTrip(r)
		})}
	})
	require.NoError(t, env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil))
	require.True(t, used)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestErrorsNeverContainTheToken(t *testing.T) {
	// The token is also what a careless server or proxy might echo back, but
	// nothing in this package ever puts it in a message of its own.
	scenarios := map[string]http.HandlerFunc{
		"401":            reply(401, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`),
		"403 html":       reply(403, `<html>forbidden</html>`),
		"429":            reply(429, `{"success":false,"errors":[{"code":971,"message":"slow down"}]}`),
		"500":            reply(500, ``),
		"success false":  reply(200, `{"success":false,"errors":[{"code":1003,"message":"bad"}]}`),
		"not json":       reply(200, `nope`),
		"no success":     reply(200, `{}`),
		"wrong result":   reply(200, okBody(`"text"`)),
		"dropped":        func(w http.ResponseWriter, _ *http.Request) { dropConnection(w) },
		"redirect":       func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"huge, unusable": reply(200, `{"success":true,"result":"`+strings.Repeat("a", maxBodyBytes)+`"}`),
	}
	for name, h := range scenarios {
		t.Run(name, func(t *testing.T) {
			env := setup(t, h)
			var out struct{ ID int }
			doErr := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, &out)
			listErr := env.c.list(context.Background(), "/zones", nil, func(json.RawMessage) error { return nil })

			require.Error(t, doErr)
			require.Error(t, listErr)
			require.NotContains(t, doErr.Error(), testToken)
			require.NotContains(t, listErr.Error(), testToken)
		})
	}

	t.Run("server is gone", func(t *testing.T) {
		env := setup(t, reply(200, okBody(`{}`)))
		srv := httptest.NewServer(http.NotFoundHandler())
		env.c.base = mustParse(t, srv.URL)
		srv.Close()

		err := env.c.do(context.Background(), http.MethodGet, "/zones", nil, nil, nil)
		require.Error(t, err)
		require.NotContains(t, err.Error(), testToken)
	})

	t.Run("request cannot be built", func(t *testing.T) {
		env := setup(t, reply(200, okBody(`{}`)))
		err := env.c.do(context.Background(), "BAD METHOD", "/zones", nil, nil, nil)
		require.Error(t, err)
		require.NotContains(t, err.Error(), testToken)
	})
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// dropConnection closes the connection without answering.
func dropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// pagedAnswer answers page n of a listing whose pages hold the given items.
func pagedAnswer(pages [][]string, w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 || page > len(pages) {
		reply(http.StatusNotFound, `{"success":false,"errors":[{"code":7003,"message":"no such page"}]}`)(w, r)
		return
	}
	items := make([]string, len(pages[page-1]))
	for i, item := range pages[page-1] {
		items[i] = strconv.Quote(item)
	}
	reply(http.StatusOK, fmt.Sprintf(
		`{"success":true,"errors":[],"result":[%s],"result_info":{"page":%d,"per_page":100,"count":%d,"total_count":5,"total_pages":%d}}`,
		strings.Join(items, ","), page, len(items), len(pages)))(w, r)
}

// collect runs list and returns what the callback saw as strings.
func collect(t *testing.T, env *testEnv, path string, query url.Values) ([]string, error) {
	t.Helper()
	var got []string
	err := env.c.list(context.Background(), path, query, func(raw json.RawMessage) error {
		var s string
		require.NoError(t, json.Unmarshal(raw, &s))
		got = append(got, s)
		return nil
	})
	return got, err
}

func TestListCollectsPagesInOrder(t *testing.T) {
	pages := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer(pages, w, r) })

	got, err := collect(t, env, "/zones/z1/dns_records", url.Values{"type": {"CNAME"}, "page": {"9"}, "per_page": {"5"}})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c", "d", "e"}, got)

	var uris []string
	for _, r := range env.requests() {
		require.Equal(t, http.MethodGet, r.method)
		require.Equal(t, "Bearer "+testToken, r.auth)
		uris = append(uris, r.uri)
	}
	require.Equal(t, []string{
		"/client/v4/zones/z1/dns_records?page=1&per_page=100&type=CNAME",
		"/client/v4/zones/z1/dns_records?page=2&per_page=100&type=CNAME",
		"/client/v4/zones/z1/dns_records?page=3&per_page=100&type=CNAME",
	}, uris)
}

func TestListDoesNotChangeTheCallersQuery(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}, {"b"}}, w, r) })
	query := url.Values{"type": {"A"}}
	_, err := collect(t, env, "/zones", query)
	require.NoError(t, err)
	require.Equal(t, url.Values{"type": {"A"}}, query)
}

func TestListWithoutQuery(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}}, w, r) })
	got, err := collect(t, env, "/zones", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"a"}, got)
	require.Equal(t, "/client/v4/zones?page=1&per_page=100", env.requests()[0].uri)
}

func TestListWithoutResultInfoIsOnePage(t *testing.T) {
	env := setup(t, reply(http.StatusOK, okBody(`["a","b"]`)))
	got, err := collect(t, env, "/zones", nil)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, got)
	require.Len(t, env.requests(), 1)
}

func TestListWithEmptyResult(t *testing.T) {
	for _, info := range []string{
		`{"page":1,"per_page":100,"count":0,"total_count":0,"total_pages":0}`,
		`{"page":1,"per_page":100,"count":0,"total_count":0,"total_pages":1}`,
		`{}`,
		`null`,
	} {
		env := setup(t, reply(http.StatusOK, `{"success":true,"errors":[],"result":[],"result_info":`+info+`}`))
		got, err := collect(t, env, "/zones", nil)
		require.NoError(t, err, info)
		require.Empty(t, got, info)
		require.Len(t, env.requests(), 1, info)
	}
}

func TestListFailsWholeOnPageError(t *testing.T) {
	// The first page is fine and the second one fails. Whatever page 1 held
	// must not be taken for the listing.
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			reply(http.StatusInternalServerError, `{"success":false,"errors":[{"code":1000,"message":"boom"}]}`)(w, r)
			return
		}
		pagedAnswer([][]string{{"a", "b"}, {"c"}, {"d"}}, w, r)
	})

	got, err := collect(t, env, "/zones", nil)

	require.Error(t, err)
	var apiErr *Error
	require.ErrorAs(t, err, &apiErr)
	require.Equal(t, http.StatusInternalServerError, apiErr.Status)
	require.Empty(t, got, "no callback may run for a listing that did not complete")
	require.Len(t, env.requests(), 2, "page 3 is not requested after page 2 failed")
}

func TestListFailsWholeOnAnyBadLaterPage(t *testing.T) {
	page2 := map[string]http.HandlerFunc{
		"server error":      reply(http.StatusBadGateway, `bad gateway`),
		"rate limited":      reply(http.StatusTooManyRequests, `{"success":false,"errors":[]}`),
		"unauthorized":      reply(http.StatusUnauthorized, `{"success":false,"errors":[{"code":10000,"message":"Authentication error"}]}`),
		"success false":     reply(http.StatusOK, `{"success":false,"errors":[{"code":1003,"message":"bad"}]}`),
		"not json":          reply(http.StatusOK, `<html></html>`),
		"empty body":        reply(http.StatusOK, ``),
		"no success":        reply(http.StatusOK, `{"result":["x"],"result_info":{"total_pages":3}}`),
		"result is null":    reply(http.StatusOK, okBody(`null`)),
		"result is missing": reply(http.StatusOK, `{"success":true}`),
		"result is object":  reply(http.StatusOK, okBody(`{"id":"x"}`)),
		"result is text":    reply(http.StatusOK, okBody(`"x"`)),
		"dropped":           func(w http.ResponseWriter, _ *http.Request) { dropConnection(w) },
	}
	for name, bad := range page2 {
		t.Run(name, func(t *testing.T) {
			env := setup(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "2" {
					bad(w, r)
					return
				}
				pagedAnswer([][]string{{"a"}, {"b"}, {"c"}}, w, r)
			})
			got, err := collect(t, env, "/zones", nil)
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}

func TestListWrapsTheFailingPage(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			reply(http.StatusForbidden, `{"success":false,"errors":[{"code":10000,"message":"nope"}]}`)(w, r)
			return
		}
		pagedAnswer([][]string{{"a"}, {"b"}}, w, r)
	})
	_, err := collect(t, env, "/zones", nil)

	require.ErrorContains(t, err, "page 2")
	require.True(t, IsAuth(err))
}

func TestListFirstPageErrors(t *testing.T) {
	env := setup(t, reply(http.StatusUnauthorized, `{"success":false,"errors":[{"code":10000,"message":"nope"}]}`))
	got, err := collect(t, env, "/zones", nil)
	require.True(t, IsAuth(err))
	require.Empty(t, got)
}

func TestListResultMustBeAList(t *testing.T) {
	for name, body := range map[string]string{
		"object":  okBody(`{"id":"x"}`),
		"null":    okBody(`null`),
		"missing": `{"success":true}`,
		"string":  okBody(`"x"`),
	} {
		t.Run(name, func(t *testing.T) {
			env := setup(t, reply(http.StatusOK, body))
			got, err := collect(t, env, "/zones", nil)
			require.ErrorIs(t, err, errUnexpected)
			require.Empty(t, got)
		})
	}
}

func TestListGuardsAgainstRunawayPaging(t *testing.T) {
	t.Run("more than the limit", func(t *testing.T) {
		env := setup(t, reply(http.StatusOK, `{"success":true,"result":["a"],"result_info":{"total_pages":1001}}`))
		got, err := collect(t, env, "/zones", nil)
		require.ErrorContains(t, err, "1001 pages")
		require.Empty(t, got)
		require.Len(t, env.requests(), 1, "no page after the first is fetched")
	})
	t.Run("exactly the limit", func(t *testing.T) {
		env := setup(t, reply(http.StatusOK, `{"success":true,"result":["a"],"result_info":{"total_pages":1000}}`), func(o *Options) {
			// 1000 requests would otherwise wait on the default budget.
			o.Limiter = NewLimiter(1_000_000, time.Second, 1000, time.Now)
		})
		got, err := collect(t, env, "/zones", nil)
		require.NoError(t, err)
		require.Len(t, got, 1000)
	})
}

func TestListStopsWhenCallbackFails(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a", "b"}, {"c"}}, w, r) })
	errStop := errors.New("stop here")

	var seen []string
	err := env.c.list(context.Background(), "/zones", nil, func(raw json.RawMessage) error {
		seen = append(seen, string(raw))
		if len(seen) == 2 {
			return errStop
		}
		return nil
	})
	require.ErrorIs(t, err, errStop)
	require.Equal(t, []string{`"a"`, `"b"`}, seen)
}

func TestListStopsWhenContextEnds(t *testing.T) {
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}, {"b"}}, w, r) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int
	err := env.c.list(ctx, "/zones", nil, func(json.RawMessage) error {
		calls++
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, calls)

	cancel()
	err = env.c.list(ctx, "/zones", nil, func(json.RawMessage) error {
		calls++
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, calls)
}

func TestListEachRequestWaitsForTheLimiter(t *testing.T) {
	clock := newFakeClock()
	env := setup(t, func(w http.ResponseWriter, r *http.Request) { pagedAnswer([][]string{{"a"}, {"b"}, {"c"}}, w, r) }, func(o *Options) {
		// One token every 10 s, room for one.
		o.Limiter = newLimiter(6, time.Minute, 1, clock.now, clock.sleep)
	})
	_, err := collect(t, env, "/zones", nil)
	require.NoError(t, err)
	require.Len(t, env.requests(), 3)
	require.Equal(t, []time.Duration{10 * time.Second, 10 * time.Second}, clock.takeSleeps())
}
