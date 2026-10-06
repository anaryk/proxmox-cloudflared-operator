// Package gateway is the way from the browser to the daemon's socket: a
// table of the calls the page may make, each with the role it needs, its
// body limit and its timeout; the readers' filter of what the daemon
// answers; and one shared subscription to the daemon's stream, handed out
// to every browser filtered for its session. Nothing outside the table is
// forwarded.
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
)

// The timeouts of the calls (P5). apply and the credential calls wait past
// the daemon's own write timeout of 70 s, so that its answer comes first.
const (
	readTimeout     = 10 * time.Second
	writeTimeout    = 60 * time.Second
	longTimeout     = 75 * time.Second
	diagnoseTimeout = 30 * time.Second
	doctorTimeout   = 60 * time.Second
)

// The largest bodies the page may send: a settings import may be larger
// than anything else.
const (
	maxBody     = 64 << 10
	maxSettings = 1 << 20
)

// The per-user limits of Rule.limit.
const (
	limitDiagnose = "diagnose"
	limitDoctor   = "doctor"
)

// Rule is a call the browser may make, and how it is forwarded.
type Rule struct {
	Method string // as the browser sends it
	Path   string // gin pattern under /api/v1
	// Upstream is the socket's method when it differs: POST /diagnose and
	// /doctor go up as GET.
	Upstream string
	// Query makes the query of those two from the body, which then does not
	// go up; Params are the parameters of the browser's query that go up
	// for the others, and no other does.
	Query  func(body []byte) (url.Values, error)
	Params []string
	Min    auth.Role
	// Body is the most the body may have; 0 for a call that takes none.
	Body    int64
	Timeout time.Duration
	// Check refuses a reader's call before it goes up: a guest or a hostname
	// the reader may not see is not found. nil for none.
	Check func(r *Reader, c *gin.Context, q url.Values) error
	// Filter rewrites the answer for readers; nil passes it as is.
	Filter func(raw []byte, r *Reader) ([]byte, error)

	limit string // the per-user limit of the call: limitDiagnose or limitDoctor
	// serve answers the call in the gateway instead of forwarding it: the
	// state, from its cache, and the stream, from the shared subscription.
	serve func(g *Gateway, c *gin.Context, rule Rule, r *Reader)
}

// eventParams are the parameters of GET /v1/events.
var eventParams = []string{"since", "after", "boot", "route", "guest", "tunnel", "account", "kind", "level", "limit", "history"}

// Table is every call the page may make, the rows of spec-ui 9.3. The
// networking milestone adds its own here, one entry each with its test;
// until then /networks and /managed answer 404 like anything else that is
// not here.
var Table = []Rule{
	{Method: http.MethodGet, Path: "/state", Min: auth.RoleReader, Timeout: readTimeout, serve: (*Gateway).state},
	{Method: http.MethodGet, Path: "/stream", Min: auth.RoleReader, serve: (*Gateway).stream},
	{Method: http.MethodGet, Path: "/events", Params: eventParams, Min: auth.RoleReader, Timeout: readTimeout, Filter: filterEvents},
	{Method: http.MethodGet, Path: "/traffic", Min: auth.RoleReader, Timeout: readTimeout, Filter: filterTraffic},
	{
		Method: http.MethodGet, Path: "/traffic/route", Params: []string{"hostname"}, Min: auth.RoleReader, Timeout: readTimeout,
		Check: checkHolder, Filter: filterRouteSeries,
	},
	{Method: http.MethodGet, Path: "/credentials", Min: auth.RoleReader, Timeout: readTimeout},
	{Method: http.MethodGet, Path: "/claims", Min: auth.RoleReader, Timeout: readTimeout, Filter: filterClaims},
	{Method: http.MethodGet, Path: "/approvals", Min: auth.RoleReader, Timeout: readTimeout, Filter: filterApprovals},
	{Method: http.MethodGet, Path: "/guests", Min: auth.RoleReader, Timeout: readTimeout, Filter: filterGuests},
	{Method: http.MethodGet, Path: "/guests/:kind/:vmid/annotation", Min: auth.RoleReader, Timeout: readTimeout, Check: checkAnnotation},
	{Method: http.MethodGet, Path: "/settings", Min: auth.RoleReader, Timeout: readTimeout},
	{Method: http.MethodGet, Path: "/routes/manual", Min: auth.RoleReader, Timeout: readTimeout},
	{Method: http.MethodGet, Path: "/version", Min: auth.RoleReader, Timeout: readTimeout},
	{
		Method: http.MethodPost, Path: "/diagnose", Upstream: http.MethodGet, Query: hostnameQuery, Min: auth.RoleReader,
		Body: maxBody, Timeout: diagnoseTimeout, Check: checkHolder, limit: limitDiagnose,
	},
	{
		Method: http.MethodPost, Path: "/doctor", Upstream: http.MethodGet, Query: emptyQuery, Min: auth.RoleReader,
		Body: maxBody, Timeout: doctorTimeout, Filter: filterDoctor, limit: limitDoctor,
	},
	{Method: http.MethodPost, Path: "/sync", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodPost, Path: "/apply", Min: auth.RoleAdmin, Body: maxBody, Timeout: longTimeout},
	{Method: http.MethodPost, Path: "/adopt", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodPost, Path: "/credentials", Min: auth.RoleAdmin, Body: maxBody, Timeout: longTimeout},
	{Method: http.MethodPost, Path: "/credentials/:id/check", Min: auth.RoleAdmin, Body: maxBody, Timeout: longTimeout},
	{Method: http.MethodPost, Path: "/claims/resolve", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodPost, Path: "/guests/approve", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodPost, Path: "/guests/revoke", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodPost, Path: "/routes/manual", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodPost, Path: "/daemon/restart", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodPut, Path: "/settings", Min: auth.RoleAdmin, Body: maxSettings, Timeout: writeTimeout},
	{Method: http.MethodPut, Path: "/routes/manual/:id", Min: auth.RoleAdmin, Body: maxBody, Timeout: writeTimeout},
	{Method: http.MethodDelete, Path: "/credentials/:id", Min: auth.RoleAdmin, Timeout: writeTimeout},
	{Method: http.MethodDelete, Path: "/routes/manual/:id", Params: []string{"rev"}, Min: auth.RoleAdmin, Timeout: writeTimeout},
}

var errNotTheCall = errors.New("the request is not the JSON this call takes")

// hostnameQuery is the query of a diagnosis: {"hostname": ...} as
// ?hostname=.
func hostnameQuery(body []byte) (url.Values, error) {
	var req struct {
		Hostname string `json:"hostname"`
	}
	if err := strictJSON(body, &req); err != nil || req.Hostname == "" {
		return nil, errNotTheCall
	}
	return url.Values{"hostname": {req.Hostname}}, nil
}

// emptyQuery is the query of the doctor, which takes {} or nothing.
func emptyQuery(body []byte) (url.Values, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return url.Values{}, nil
	}
	if err := strictJSON(body, &struct{}{}); err != nil {
		return nil, errNotTheCall
	}
	return url.Values{}, nil
}

// strictJSON decodes body, one JSON object of the fields of v and nothing
// else.
func strictJSON(body []byte, v any) error {
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return errNotTheCall
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errNotTheCall
	}
	return nil
}
