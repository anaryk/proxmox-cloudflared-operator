package api

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// fakeRole is what the daemon says about the node.
type fakeRole struct {
	controller bool
	node, addr string
	since      time.Time
	ok         bool
}

func (r fakeRole) Controller() bool { return r.controller }
func (r fakeRole) ControllerAddr() (string, string, time.Time, bool) {
	return r.node, r.addr, r.since, r.ok
}

var (
	otherNode = fakeRole{node: "node2", addr: "10.9.0.2", since: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), ok: true}
	noAnswer  = fakeRole{node: "node2", since: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	noHolder  = fakeRole{}
)

// localReads are the routes a node answers whether or not it is the
// controller: they describe the daemon's own process and its node, and
// act on its own agent.
var localReads = []string{
	"GET /v1/version",
	"GET /v1/state",
	"GET /v1/events",
	"GET /v1/stream",
	"GET /v1/traffic",
	"GET /v1/traffic/route",
	"GET /v1/doctor",
	"GET /v1/local/agent",
	"POST /v1/local/agent/reset-epoch",
}

func TestEveryRouteIsEitherLocalOrTheControllers(t *testing.T) {
	s := newServer(&fakeEngine{})
	var seen []string
	for _, r := range s.router.Routes() {
		route := r.Method + " " + r.Path
		seen = append(seen, route)
		require.Equal(t, slices.Contains(localReads, route), answeredLocally(r.Method, r.Path), route)
	}
	for _, l := range localReads {
		require.Contains(t, seen, l, "the local route %s is not served", l)
	}
}

func TestRoutesOfTheLocalAgentAndTheEgressFilterAreLocal(t *testing.T) {
	for _, route := range []string{
		"GET /v1/local/agent",
		"POST /v1/local/agent/reset-epoch",
		"GET /v1/egress",
		"POST /v1/egress/block",
		"POST /v1/egress/unblock",
	} {
		method, path, _ := strings.Cut(route, " ")
		require.True(t, answeredLocally(method, path), route)
	}
	for _, route := range []string{"GET /v1/localagent", "GET /v1/egressive", "POST /v1/nodes/node2/forget", "GET /v1/cluster"} {
		method, path, _ := strings.Cut(route, " ")
		require.False(t, answeredLocally(method, path), route)
	}
}

func TestANonControllerPointsTheRestAtTheController(t *testing.T) {
	cases := []struct {
		name   string
		role   Role
		status int
		code   string
		msg    string
		hint   string
	}{
		{"a controller is known", otherNode, http.StatusConflict, "not_controller",
			"this node is not the controller; run it on node2 (10.9.0.2)", "10.9.0.2"},
		{"the holder does not answer", noAnswer, http.StatusServiceUnavailable, "no_controller",
			"no controller is reachable (lease held by node2 since 2026-10-05T12:00:00Z)", ""},
		{"no node holds the lease", noHolder, http.StatusServiceUnavailable, "no_controller",
			"no controller is reachable (no node holds the lease)", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{}
			s := newServer(f)
			s.SetRole(tt.role)
			for _, req := range []struct{ method, target, body string }{
				{http.MethodPost, "/v1/apply", `{}`},
				{http.MethodPut, "/v1/settings", `{"rev":1,"settings":{}}`},
				{http.MethodDelete, "/v1/credentials/a1b2c3d4", ""},
				{http.MethodGet, "/v1/credentials", ""},
				{http.MethodGet, "/v1/settings", ""},
				{http.MethodPost, "/v1/promote", `{"confirm":true}`},
				{http.MethodPost, "/v1/managed/enable", `{}`},
			} {
				rec := do(s, req.method, req.target, req.body)
				require.Equal(t, tt.status, rec.Code, "%s %s: %s", req.method, req.target, rec.Body.String())
				a := parseError(t, rec)
				require.Equal(t, tt.code, a.Code, "%s %s", req.method, req.target)
				require.Equal(t, tt.msg, a.Message, "%s %s", req.method, req.target)
				require.Equal(t, tt.hint, rec.Header().Get(controllerHeader), "the hint for the page, %s %s", req.method, req.target)
				require.Empty(t, rec.Header().Get("Connection"), "the connection stays open")
			}
			require.Empty(t, f.called(), "the engine was asked nothing")
		})
	}
}

func TestANonControllerAnswersWhatItsOwnProcessKnows(t *testing.T) {
	f := &fakeEngine{state: testState(), findings: nil}
	s := newServer(f)
	s.SetRole(otherNode)
	for _, target := range []string{"/v1/version", "/v1/state", "/v1/events", "/v1/traffic", "/v1/doctor"} {
		rec := do(s, http.MethodGet, target, "")
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", target, rec.Body.String())
		require.Empty(t, rec.Header().Get(controllerHeader), "%s: no hint on an answer", target)
	}
}

func TestTheRefusalComesBeforeTheBodyIsRead(t *testing.T) {
	s := newServer(&fakeEngine{})
	s.SetRole(otherNode)
	req := request(http.MethodPost, "/v1/apply", strings.Repeat("x", maxBody+1))
	req.Header.Set("Content-Type", "text/plain")
	rec := send(s, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Equal(t, "not_controller", errorCode(t, rec))
}

func TestTheControllerAndANodeWithoutARoleAnswerEverything(t *testing.T) {
	for _, tt := range []struct {
		name string
		role Role
	}{
		{"no role set", nil},
		{"the controller", fakeRole{controller: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeEngine{settings: engine.SettingsView{Rev: 1}}
			s := newServer(f)
			if tt.role != nil {
				s.SetRole(tt.role)
			}
			rec := do(s, http.MethodGet, "/v1/settings", "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rec = do(s, http.MethodPost, "/v1/sync", `{}`)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		})
	}
}

func TestTheRoleChangesWhileTheServerRuns(t *testing.T) {
	s := newServer(&fakeEngine{})
	s.SetRole(otherNode)
	require.Equal(t, http.StatusConflict, do(s, http.MethodPost, "/v1/sync", `{}`).Code)
	s.SetRole(fakeRole{controller: true})
	require.Equal(t, http.StatusAccepted, do(s, http.MethodPost, "/v1/sync", `{}`).Code)
	s.SetRole(nil)
	require.Equal(t, http.StatusAccepted, do(s, http.MethodPost, "/v1/sync", `{}`).Code)
}

func TestThePeerCheckAndTheRouterStillComeFirst(t *testing.T) {
	s := newServer(&fakeEngine{})
	s.checkPeers = true
	s.SetRole(otherNode)
	rec := send(s, requestFrom(4242, http.MethodPost, "/v1/apply", `{}`))
	require.Equal(t, http.StatusForbidden, rec.Code, "a peer that is not allowed learns nothing of the controller")
	require.Empty(t, rec.Header().Get(controllerHeader))
	rec = do(s, http.MethodGet, "/v1/nothing", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, codeNoRoute, errorCode(t, rec))
	rec = do(s, http.MethodDelete, "/v1/apply", "")
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestTheLeaseRole(t *testing.T) {
	since := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddr("10.9.0.2")
	cases := []struct {
		name       string
		state      engine.State
		standalone bool
		controller bool
		node, addr string
		ok         bool
	}{
		{"no lease on a node that stands alone", engine.State{Node: "node1"}, true, true, "", "", false},
		{"no lease on a cluster member", engine.State{Node: "node1"}, false, false, "", "", false},
		{"this node holds it", engine.State{Node: "node1", Lease: &engine.LeaseView{Holder: "node1", Valid: true}}, false, true, "node1", "", false},
		{"another node holds it", engine.State{Node: "node1", Lease: &engine.LeaseView{Holder: "node2", Address: addr, AcquiredAt: since}}, false, false, "node2", "10.9.0.2", true},
		{"the holder has no address yet", engine.State{Node: "node1", Lease: &engine.LeaseView{Holder: "node2", AcquiredAt: since}}, false, false, "node2", "", false},
		{"nobody holds it", engine.State{Node: "node1", Lease: &engine.LeaseView{}}, true, false, "", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := LeaseRole(func() engine.State { return tt.state }, func() bool { return tt.standalone })
			require.Equal(t, tt.controller, r.Controller())
			node, addr, since, ok := r.ControllerAddr()
			require.Equal(t, tt.node, node)
			require.Equal(t, tt.addr, addr)
			require.Equal(t, tt.ok, ok)
			if tt.state.Lease != nil {
				require.Equal(t, tt.state.Lease.AcquiredAt, since)
			}
		})
	}
}
