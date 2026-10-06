package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// tableRow is how a rule of the table is tested: what the page sends, what
// the socket must get, and for a rule with a filter what the daemon answers
// and the golden of what a reader gets of it.
type tableRow struct {
	path    string // the browser's path under /api/v1, for a pattern with parameters
	query   string // the browser's query, with a parameter the rule does not pass on
	body    string
	upQuery string // the query the socket gets
	answer  any    // the daemon's answer to the filtered call
	golden  string // under testdata/filter
	local   bool   // served by the gateway itself: the state and the stream
}

// tableRows has one row per rule of Table, keyed "METHOD path": a rule added
// without its row fails TestEveryRuleHasARow.
var tableRows = map[string]tableRow{
	"GET /state":  {local: true},
	"GET /stream": {local: true},
	"GET /events": {
		query:   "?after=3&route=www.example.com&history=1&until=2026-10-01T12:00:00Z&cookie=x",
		upQuery: "after=3&history=1&route=www.example.com&until=2026-10-01T12%3A00%3A00Z",
		answer:  eventsAnswer, golden: "events.json",
	},
	"GET /traffic": {answer: trafficAnswer, golden: "traffic.json"},
	"GET /traffic/route": {
		query: "?hostname=www.example.com", upQuery: "hostname=www.example.com",
		answer: engine.RouteSeries{Hostname: "www.example.com", Target: "10.0.0.11:8080", Shared: 2, Samples: []engine.RouteSample{{At: t0, FlowsPerSec: 2.4}}},
		golden: "traffic_route.json",
	},
	"GET /credentials":                   {},
	"GET /claims":                        {answer: claimsAnswer, golden: "claims.json"},
	"GET /approvals":                     {answer: approvalsAnswer, golden: "approvals.json"},
	"GET /guests":                        {answer: guestsAnswer, golden: "guests.json"},
	"GET /guests/:kind/:vmid/annotation": {path: "/guests/qemu/101/annotation"},
	"GET /settings":                      {},
	"GET /routes/manual":                 {},
	"GET /version":                       {},
	"POST /diagnose":                     {body: `{"hostname":"www.example.com"}`, upQuery: "hostname=www.example.com"},
	"POST /doctor":                       {body: `{}`, answer: doctorAnswer, golden: "doctor_counts.json"},
	"POST /sync":                         {body: `{}`},
	"POST /apply":                        {body: `{"confirmDeletes":true,"offer":"9298960fb3d77f2d"}`},
	"POST /adopt":                        {body: `{"name":"api.example.com"}`},
	"POST /credentials":                  {body: `{"label":"main","token":"0123456789abcdefghij0123456789"}`},
	"POST /credentials/:id/check":        {path: "/credentials/cred1/check", body: `{"deep":true}`},
	"POST /claims/resolve":               {body: `{"hostname":"www.example.com","owner":"qemu/101"}`},
	"POST /guests/approve":               {body: `{"owner":"lxc/201","identity":"uuid:201"}`},
	"POST /guests/revoke":                {body: `{"owner":"qemu/101"}`},
	"POST /routes/manual":                {body: `{"hostname":"status.example.com","target":{"kind":"address","scheme":"http","addr":"10.0.5.20","port":9000}}`},
	"POST /daemon/restart":               {body: `{}`},
	"PUT /settings":                      {body: `{"rev":7,"settings":{"pollInterval":"10s"}}`},
	"PUT /routes/manual/:id":             {path: "/routes/manual/status", body: `{"rev":2,"hostname":"status.example.com"}`},
	"DELETE /credentials/:id":            {path: "/credentials/cred2"},
	"DELETE /routes/manual/:id":          {path: "/routes/manual/status", query: "?rev=2&force=1", upQuery: "rev=2"},
}

var (
	eventsAnswer = []engine.Event{
		{Seq: 4, Boot: bootA, At: t0, Level: "info", Kind: "route", Subject: "www.example.com", Message: "qemu/101: active", Route: "www.example.com", Guest: "qemu/101"},
		{Seq: 5, Boot: bootA, At: t0, Level: "warn", Kind: "route", Subject: "www.example.com", Message: "qemu/102: conflict", Route: "www.example.com", Guest: "qemu/102"},
		{Seq: 6, Boot: bootA, At: t0, Level: "info", Kind: "action", Subject: "old.example.com", Message: "delete-record in zone example.com", Route: "old.example.com"},
		{Seq: 7, Boot: bootA, At: t0, Level: "info", Kind: "action", Subject: "www.example.com", Message: "create-record in zone example.com", Route: "www.example.com"},
		{Seq: 8, Boot: bootA, At: t0, Level: "info", Kind: "admin", Subject: "lxc/202", Message: "lxc/202 (dns-1) is approved in identity uuid:202", Actor: "alice@pve (ticket)"},
		{Seq: 9, Boot: bootA, At: t0, Level: "info", Kind: "admin", Subject: "lxc/201", Message: "lxc/201 (new-1) is approved in identity uuid:201", Actor: "alice@pve (ticket)"},
		{Seq: 10, Boot: bootA, At: t0, Level: "warn", Kind: "problem", Subject: "", Message: "a problem"},
		{Seq: 11, Boot: bootA, At: t0, Level: "info", Kind: "rollout", Subject: "pco-abc123", Message: "version 3 runs on 1 connector", Tunnel: "pco-abc123", Account: "acc1"},
	}
	trafficAnswer = engine.TrafficView{
		At: t0, Interval: "5s", Tunnels: []engine.TunnelTraffic{{TunnelID: "00000000-0000-4000-8000-000000000001", Node: "pve1", Samples: []engine.TrafficSample{}}},
		Routes: []engine.RouteTraffic{
			{Hostname: "shop.example.com", Owner: "lxc/200", Target: "10.0.0.11:8080", FlowsPerSec: 1, Shared: 2},
			{Hostname: "status.example.com", Owner: "manual/status", Target: "10.0.5.20:9000", FlowsPerSec: 0.5},
			{Hostname: "web.example.com", Owner: "qemu/101", Target: "10.0.0.11:8080", FlowsPerSec: 1, Shared: 2},
			{Hostname: "www.example.com", Owner: "qemu/101", Target: "10.0.0.11:8080", FlowsPerSec: 1, Shared: 2},
		},
		RoutesTotal: 4,
	}
	claimsAnswer = []engine.ClaimView{
		{Hostname: "api.example.com", Holder: "manual/api", Since: t0, State: "serving", Waiting: []engine.ClaimantView{}},
		{Hostname: "new.example.com", Holder: "qemu/102", Guest: guestView(model.KindQEMU, 102, "web-2"), Since: t0, State: "pending", Waiting: []engine.ClaimantView{}},
		{Hostname: "www.example.com", Holder: "qemu/101", Guest: guestView(model.KindQEMU, 101, "web-1"), Since: t0, State: "conflict", Waiting: []engine.ClaimantView{
			{Owner: "qemu/102", Guest: guestView(model.KindQEMU, 102, "web-2"), Since: t0},
			{Owner: "lxc/201", Guest: guestView(model.KindLXC, 201, "new-1"), Since: t0},
		}},
	}
	approvalsAnswer = []engine.ApprovalView{
		{Owner: "qemu/101", Guest: guestView(model.KindQEMU, 101, "web-1"), Identity: "uuid:101", Current: "uuid:101", Matches: true},
		{Owner: "lxc/300", Guest: guestView(model.KindLXC, 300, ""), Identity: "uuid:300", MACs: []string{"bc:24:11:00:03:00"}},
	}
	guestsAnswer = []engine.GuestListView{
		{Ref: "qemu/101", Name: "web-1", Node: "pve1", Running: true, Tagged: true, Approval: "approved", Routes: 2},
		{Ref: "lxc/200", Node: "pve1", Approval: "not-needed"},
		{Ref: "lxc/201", Name: "new-1", Node: "pve1", Tagged: true, Approval: "waiting", Routes: 1},
	}
	// doctorAnswer are the findings of cmd/pco/testdata/doctor.golden.
	doctorAnswer = []doctor.Finding{
		{Check: "cloudflared", Level: doctor.LevelFail, Detail: "cloudflared does not run: exec: no such file", Fix: "install cloudflared from the package repository of Cloudflare"},
		{Check: "credential cred1", Level: doctor.LevelWarn, Detail: "not checked yet", Fix: "pco credential check cred1"},
		{Check: "mode", Level: doctor.LevelOK, Detail: "enforce: changes are applied"},
		{Check: "store", Level: doctor.LevelOK, Detail: "the store is mounted and set up"},
	}
)

func guestView(kind model.GuestKind, vmid int, name string) *engine.GuestView {
	return &engine.GuestView{GuestRef: model.GuestRef{Kind: kind, VMID: vmid}, Name: name}
}

func ruleKey(r Rule) string { return r.Method + " " + r.Path }

func (row tableRow) url(r Rule) string {
	p := row.path
	if p == "" {
		p = r.Path
	}
	return "/api/v1" + p + row.query
}

func TestEveryRuleHasARow(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Table {
		key := ruleKey(r)
		require.False(t, seen[key], "%s is in the table twice", key)
		seen[key] = true
		row, ok := tableRows[key]
		require.True(t, ok, "%s has no row in tableRows: add one, with its filter's golden if it has a filter", key)
		require.Equal(t, r.Filter != nil, row.golden != "", "%s: a rule with a filter has a golden, and only such a rule", key)
		require.Equal(t, r.serve != nil, row.local, "%s: only the state and the stream are served by the gateway itself", key)
	}
	for key := range tableRows {
		require.True(t, seen[key], "tableRows has %s, which is no rule", key)
	}
}

// The table is exactly the calls the page makes, with the role each needs
// and the timeout of its kind: a call that only reads 10 s, one that writes
// 60 s, apply and the credential calls 75 s, past the daemon's own write
// timeout of 70 s, a diagnosis 30 s and the doctor 60 s.
func TestTheTableHoldsExactlyTheCallsOfThePage(t *testing.T) {
	readers := []string{
		"GET /state", "GET /stream", "GET /events", "GET /traffic", "GET /traffic/route",
		"GET /credentials", "GET /claims", "GET /approvals", "GET /guests", "GET /guests/:kind/:vmid/annotation",
		"GET /settings", "GET /routes/manual", "GET /version", "POST /diagnose", "POST /doctor",
	}
	long := map[string]bool{"POST /apply": true, "POST /credentials": true, "POST /credentials/:id/check": true}
	for _, r := range Table {
		key := ruleKey(r)
		reads := false
		for _, k := range readers {
			reads = reads || k == key
		}
		if reads {
			require.Equal(t, auth.RoleReader, r.Min, key)
		} else {
			require.Equal(t, auth.RoleAdmin, r.Min, key)
		}
		switch {
		case key == "GET /stream":
			require.Zero(t, r.Timeout, key)
		case key == "POST /diagnose":
			require.Equal(t, 30*time.Second, r.Timeout)
			require.Equal(t, http.MethodGet, r.Upstream)
		case key == "POST /doctor":
			require.Equal(t, 60*time.Second, r.Timeout)
			require.Equal(t, http.MethodGet, r.Upstream)
		case long[key]:
			require.Equal(t, 75*time.Second, r.Timeout, key)
		case r.Method == http.MethodGet:
			require.Equal(t, 10*time.Second, r.Timeout, key)
		default:
			require.Equal(t, 60*time.Second, r.Timeout, key)
		}
		switch {
		case key == "PUT /settings":
			require.EqualValues(t, 1<<20, r.Body)
		case r.Method == http.MethodPost || r.Method == http.MethodPut:
			require.EqualValues(t, 64<<10, r.Body, key)
		default:
			require.Zero(t, r.Body, key)
		}
	}
	require.Len(t, Table, len(readers)+14)
}

func TestReadersAreRefusedEveryAdminRule(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(reader)
	for _, r := range Table {
		if r.Min != auth.RoleAdmin {
			continue
		}
		row := tableRows[ruleKey(r)]
		rec := b.do(r.Method, row.url(r), row.body)
		requireError(t, rec, http.StatusForbidden, wire.CodeForbidden)
	}
	require.Zero(t, s.daemon.count(""), "a refused request never reaches the socket")
}

func TestEveryRuleForwardsItsCall(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(populated(t))
	b := s.signIn(admin)
	for _, r := range Table {
		key := ruleKey(r)
		row := tableRows[key]
		if row.local {
			continue
		}
		s.daemon.forget()
		rec := b.do(r.Method, row.url(r), row.body)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", key, rec.Body.String())
		upstream := r.Method
		if r.Upstream != "" {
			upstream = r.Upstream
		}
		path := "/v1" + strings.TrimPrefix(row.url(r), "/api/v1")
		path, _, _ = strings.Cut(path, "?")
		got := s.daemon.last(t, upstream+" "+path)
		require.Equal(t, row.upQuery, got.RawQuery, key)
		switch {
		case r.Query != nil, r.Method == http.MethodGet, r.Method == http.MethodDelete:
			require.Empty(t, got.Body, "%s carries no body", key)
		default:
			require.JSONEq(t, row.body, string(got.Body), key)
		}
	}
}

func TestReadersGetTheFilteredAnswers(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(populated(t))
	s.daemon.answer("GET /v1/traffic", http.StatusOK, trafficAnswer)
	s.pve.sees(reader, 101, 201)
	b := s.signIn(reader)
	for _, r := range Table {
		key := ruleKey(r)
		row := tableRows[key]
		if r.Filter == nil {
			continue
		}
		upstream := r.Method
		if r.Upstream != "" {
			upstream = r.Upstream
		}
		path := "/v1" + strings.TrimPrefix(row.url(r), "/api/v1")
		path, _, _ = strings.Cut(path, "?")
		s.daemon.answer(upstream+" "+path, http.StatusOK, row.answer)
		rec := b.do(r.Method, row.url(r), row.body)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", key, rec.Body.String())
		requireGoldenJSON(t, "filter/"+row.golden, rec.Body.Bytes())

		// An admin gets the daemon's answer as it is.
		admin := s.signIn(admin)
		rec = admin.do(r.Method, row.url(r), row.body)
		require.Equal(t, http.StatusOK, rec.Code)
		want, err := json.Marshal(row.answer)
		require.NoError(t, err)
		require.JSONEq(t, string(want), rec.Body.String(), key)
	}
}

// Nothing outside the table reaches the socket: the root-only rotation, the
// networking milestone's routes before they exist, routes the daemon has and
// the page has no use for, and anything else under /api.
func TestWhatIsNotInTheTableReachesNoSocket(t *testing.T) {
	s := newTestServer(t)
	b := s.signIn(admin)
	cases := []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodPost, "/api/v1/tunnels/rotate", http.StatusNotFound, "no_route"},
		{http.MethodGet, "/api/v1/networks", http.StatusNotFound, "no_route"},
		{http.MethodGet, "/api/v1/networks/candidates", http.StatusNotFound, "no_route"},
		{http.MethodPost, "/api/v1/networks/probe", http.StatusNotFound, "no_route"},
		{http.MethodDelete, "/api/v1/networks/net1", http.StatusNotFound, "no_route"},
		{http.MethodGet, "/api/v1/managed", http.StatusNotFound, "no_route"},
		{http.MethodPost, "/api/v1/managed/approve", http.StatusNotFound, "no_route"},
		{http.MethodGet, "/api/v1/segments", http.StatusNotFound, "no_route"},
		{http.MethodGet, "/api/v1/nothing", http.StatusNotFound, "no_route"},
		{http.MethodGet, "/api/elsewhere", http.StatusNotFound, "no_route"},
		{http.MethodGet, "/api/v1/diagnose?hostname=www.example.com", http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.MethodGet, "/api/v1/doctor", http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.MethodPost, "/api/v1/state", http.StatusMethodNotAllowed, "method_not_allowed"},
		{http.MethodOptions, "/api/v1/state", http.StatusMethodNotAllowed, "method_not_allowed"},
	}
	for _, c := range cases {
		rec := b.do(c.method, c.path, "{}")
		requireError(t, rec, c.status, c.code)
	}
	require.Zero(t, s.daemon.count(""))

	// Pages are not the gateway's: a path outside /api keeps its own answer.
	rec := b.do(http.MethodGet, "/nothing", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
}

// A diagnosis probes guests, so it is a POST at the browser with the checks
// of a write: without the token, from another page or as a GET it never
// reaches the socket; with them it goes up as GET /v1/diagnose?hostname=.
func TestDiagnoseIsAWriteAtTheBrowser(t *testing.T) {
	s := newTestServer(t)
	s.daemon.serveState(populated(t))
	b := s.signIn(admin)
	body := `{"hostname":"www.example.com"}`

	r := b.request(http.MethodPost, "/api/v1/diagnose", body)
	r.Header.Del("Pco-Csrf")
	requireError(t, b.send(r), http.StatusForbidden, wire.CodeForbidden)

	r = b.request(http.MethodPost, "/api/v1/diagnose", body)
	r.Header.Set("Origin", "https://pve2:8643")
	requireError(t, b.send(r), http.StatusForbidden, wire.CodeForbidden)

	r = b.request(http.MethodPost, "/api/v1/diagnose", body)
	r.Header.Set("Content-Type", "text/plain")
	requireError(t, b.send(r), http.StatusUnsupportedMediaType, wire.CodeUnsupportedMediaType)

	requireError(t, b.do(http.MethodGet, "/api/v1/diagnose?hostname=www.example.com", ""), http.StatusMethodNotAllowed, "method_not_allowed")
	require.Zero(t, s.daemon.count(""))

	requireError(t, b.do(http.MethodPost, "/api/v1/diagnose", `{"hostname":"www.example.com","extra":1}`), http.StatusBadRequest, wire.CodeInvalid)
	requireError(t, b.do(http.MethodPost, "/api/v1/diagnose", `{}`), http.StatusBadRequest, wire.CodeInvalid)
	require.Zero(t, s.daemon.count(""))

	rec := b.do(http.MethodPost, "/api/v1/diagnose", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.daemon.last(t, "GET /v1/diagnose")
	require.Equal(t, url.Values{"hostname": {"www.example.com"}}.Encode(), got.RawQuery)
	require.Empty(t, got.Body)
	require.Empty(t, got.Header.Get("Content-Type"), "a GET has no body and so no content type")
}
