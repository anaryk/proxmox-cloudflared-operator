package gateway

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/apiclient"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/auth"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/web/wire"
)

// The codes of the gateway's own error answers that are the daemon's as
// well: the page reads them alike from either.
const (
	codeNotFound         = "not_found"
	codeNoRoute          = "no_route"
	codeMethodNotAllowed = "method_not_allowed"
	codeUnavailable      = "unavailable"
	codeInternal         = "internal"
)

const (
	// maxAnswer bounds what the gateway reads of an answer of the daemon: a
	// state of a few thousand routes is a few megabytes.
	maxAnswer = 32 << 20
	// gzipOver is the size from which an answer is compressed for a browser
	// that accepts it.
	gzipOver = 1 << 10
	// bodyTimeout is how long a browser has to send the body of a call.
	bodyTimeout = 30 * time.Second

	baseURL     = "http://pco"
	actorHeader = "Pco-Actor"
)

// Gateway forwards the calls of Table to the daemon's socket and serves the
// shared stream.
type Gateway struct {
	auth   *auth.Auth
	log    zerolog.Logger
	client *http.Client
	// subscribe follows the daemon's stream, from the boot and the seq of the
	// last event the gateway holds.
	subscribe func(ctx context.Context, boot string, after uint64) (<-chan engine.Notice, engine.Hello, error)

	states *stateCache
	hub    *hub
	slots  *streamSlots
	limits map[string]*userLimit
	// inbox is what the subscription took and the worker has not handed on.
	inbox chan upstreamItem

	// Tests set these.
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) bool
	ticker      func(every time.Duration) (<-chan time.Time, func())
	checkWindow time.Duration
	timeoutOf   func(Rule) time.Duration
}

// New returns the gateway to the daemon that listens on socket, for the
// sessions of a.
func New(socket string, a *auth.Auth, log zerolog.Logger) *Gateway {
	var d net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socket)
		},
		// Accept-Encoding is no header of the browser's to forward, and the
		// daemon does not compress.
		DisableCompression:  true,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}
	g := &Gateway{
		auth: a,
		log:  log,
		client: &http.Client{
			Transport: transport,
			// The daemon does not redirect; an answer that does is not its.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		subscribe: apiclient.New(socket).Stream,
		states:    newStateCache(),
		slots:     newStreamSlots(),
		limits: map[string]*userLimit{
			limitDiagnose: newUserLimit("diagnoses", diagnosesPerMinute, 1, time.Minute),
			limitDoctor:   newUserLimit("doctor runs", doctorsPerMinute, 0, time.Minute),
		},
		inbox:       make(chan upstreamItem, inboxSize),
		now:         time.Now,
		sleep:       sleep,
		ticker:      startTicker,
		checkWindow: checkWindow,
		timeoutOf:   func(r Rule) time.Duration { return r.Timeout },
	}
	g.hub = newHub(g)
	// A stream ends with its session at once; the ping also finds a session
	// that is over, or whose role or guests changed.
	a.OnEnd(g.hub.endSession)
	return g
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func startTicker(every time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(every)
	return t.C, t.Stop
}

// Mount adds the calls of Table under /api/v1, each behind the session's
// check of its role, and answers whatever else comes under /api/ with a
// JSON error: the page reads the code of every answer it gets there.
func (g *Gateway) Mount(r gin.IRouter) {
	r.Use(unrouted)
	v1 := r.Group("/api/v1")
	for _, rule := range Table {
		v1.Handle(rule.Method, rule.Path, g.auth.Require(rule.Min), g.handle(rule))
	}
}

// unrouted answers a request under /api/ that has no route. It runs before
// every handler; one that has a route passes.
func unrouted(c *gin.Context) {
	if c.FullPath() != "" || !strings.HasPrefix(c.Request.URL.Path, "/api/") {
		c.Next()
		return
	}
	if c.Writer.Status() == http.StatusMethodNotAllowed {
		refuse(c, http.StatusMethodNotAllowed, wire.Error{Error: "this call takes another method", Code: codeMethodNotAllowed})
		return
	}
	refuse(c, http.StatusNotFound, wire.Error{Error: "pco has no such call", Code: codeNoRoute})
}

func refuse(c *gin.Context, status int, body wire.Error) {
	if body.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(body.RetryAfter))
	}
	c.AbortWithStatusJSON(status, body)
}

// handle answers one rule: for a reader with what the reader may see.
func (g *Gateway) handle(rule Rule) gin.HandlerFunc {
	return func(c *gin.Context) {
		s := auth.SessionOf(c)
		var r *Reader
		if s.Principal.Role < auth.RoleAdmin {
			visible, hash, err := g.auth.Visible(c)
			if err != nil {
				g.log.Warn().Err(err).Str("path", c.Request.URL.Path).Msg("listing the guests of a reader")
				refuse(c, http.StatusServiceUnavailable, wire.Error{
					Error: "Proxmox VE on this node does not answer, so pco cannot tell which guests you may see",
					Code:  wire.CodeProxmoxUnreachable,
				})
				return
			}
			r = &Reader{Visible: visible, Hash: hash, g: g, ctx: c.Request.Context(), actor: s.Principal.Actor()}
		}
		if rule.serve != nil {
			rule.serve(g, c, rule, r)
			return
		}
		g.forward(c, rule, r)
	}
}

// forward makes a new request of the rule to the socket: the upstream
// method, the path, the query of the rule and its body, with no header of
// the browser's; and answers with what the daemon answered, filtered for a
// reader.
func (g *Gateway) forward(c *gin.Context, rule Rule, r *Reader) {
	var body []byte
	if rule.Body > 0 {
		// The server has no read timeout, which would cut the streams: a
		// body that does not come in time does not hold the request.
		rc := http.NewResponseController(c.Writer)
		_ = rc.SetReadDeadline(time.Now().Add(bodyTimeout))
		var err error
		body, err = io.ReadAll(io.LimitReader(c.Request.Body, rule.Body+1))
		_ = rc.SetReadDeadline(time.Time{})
		switch {
		case err != nil:
			refuse(c, http.StatusBadRequest, wire.Error{Error: "the request body could not be read", Code: wire.CodeInvalid})
			return
		case int64(len(body)) > rule.Body:
			refuse(c, http.StatusRequestEntityTooLarge, wire.Error{Error: "the request is too large", Code: wire.CodeTooLarge})
			return
		}
	}
	query := url.Values{}
	if rule.Query != nil {
		var err error
		if query, err = rule.Query(body); err != nil {
			refuse(c, http.StatusBadRequest, wire.Error{Error: err.Error(), Code: wire.CodeInvalid})
			return
		}
		body = nil
	} else {
		for _, p := range rule.Params {
			if vs, ok := c.Request.URL.Query()[p]; ok {
				query[p] = vs
			}
		}
	}
	if r != nil && rule.Check != nil {
		if err := rule.Check(r, c, query); err != nil {
			g.fail(c, err)
			return
		}
	}
	if l := g.limits[rule.limit]; l != nil {
		done, wait, ok := l.start(auth.SessionOf(c).Principal.User, g.now(), rule.Timeout)
		if !ok {
			after := int(math.Ceil(wait.Seconds()))
			refuse(c, http.StatusTooManyRequests, wire.Error{
				Error: fmt.Sprintf("too many %s; try again in %d s", l.what, after), Code: wire.CodeRateLimited, RetryAfter: after,
			})
			return
		}
		defer done()
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), g.timeoutOf(rule))
	defer cancel()
	up := upstream{
		method: cmp.Or(rule.Upstream, rule.Method),
		path:   "/v1" + strings.TrimPrefix(c.Request.URL.Path, "/api/v1"),
		query:  query,
		body:   body,
		actor:  auth.SessionOf(c).Principal.Actor(),
	}
	res, err := g.call(ctx, up)
	if err != nil {
		g.fail(c, err)
		return
	}
	if r != nil && rule.Filter != nil && res.status/100 == 2 {
		if res.body, err = rule.Filter(res.body, r); err != nil {
			g.fail(c, err)
			return
		}
	}
	g.write(c, res, nil)
}

// upstream is a request to the socket.
type upstream struct {
	method, path string
	query        url.Values
	body         []byte // nil for none
	actor        string // Pco-Actor; empty for the gateway's own calls
	ifNoneMatch  string
}

// reply is what the daemon answered, of which the browser gets the status,
// the content type, the ETag and the body.
type reply struct {
	status int
	ctype  string
	etag   string
	body   []byte
}

// errUnreachable and errNoAnswer are calls that got no answer: nothing
// listens on the socket, or the daemon did not answer within the timeout.
var (
	errUnreachable = errors.New("the pco daemon cannot be reached")
	errNoAnswer    = errors.New("the pco daemon did not answer in time")
)

// call makes one request to the socket. The request carries Content-Type
// when it has a body, If-None-Match when it is given, Pco-Actor, and no
// other header; a failed call is logged with its method, path, status and
// duration, and never with its query or a body.
func (g *Gateway) call(ctx context.Context, up upstream) (reply, error) {
	u := url.URL{Scheme: "http", Host: "pco", Path: up.path, RawQuery: up.query.Encode()}
	var body io.Reader
	if up.body != nil {
		body = bytes.NewReader(up.body)
	}
	req, err := http.NewRequestWithContext(ctx, up.method, u.String(), body)
	if err != nil {
		return reply{}, fmt.Errorf("making the request %s %s: %w", up.method, up.path, errors.Unwrap(err))
	}
	// The transport adds a User-Agent unless the request has one, even
	// an empty one, which it then leaves out.
	req.Header["User-Agent"] = []string{""}
	if up.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if up.ifNoneMatch != "" {
		req.Header.Set("If-None-Match", up.ifNoneMatch)
	}
	if up.actor != "" {
		req.Header.Set(actorHeader, up.actor)
	}
	start := g.now()
	res, err := g.client.Do(req)
	if err != nil {
		cause := errUnreachable
		if ctx.Err() != nil {
			cause = errNoAnswer
		}
		// The error of the client repeats the URL with its query.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		g.log.Warn().Str("method", up.method).Str("path", up.path).Dur("duration", g.now().Sub(start)).
			AnErr("error", err).Msg("a call to the daemon failed")
		return reply{}, cause
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer+1))
	switch {
	case err != nil && ctx.Err() != nil:
		return reply{}, errNoAnswer
	case err != nil:
		return reply{}, errUnreachable
	case len(data) > maxAnswer:
		return reply{}, fmt.Errorf("the answer of %s %s is larger than %d bytes", up.method, up.path, maxAnswer)
	}
	if res.StatusCode >= http.StatusBadRequest {
		g.log.Info().Str("method", up.method).Str("path", up.path).Int("status", res.StatusCode).
			Dur("duration", g.now().Sub(start)).Msg("the daemon refused a call")
	}
	return reply{status: res.StatusCode, ctype: res.Header.Get("Content-Type"), etag: res.Header.Get("ETag"), body: data}, nil
}

// errHidden is a guest or a hostname a reader may not see: it answers as
// one that is not there.
var errHidden = errors.New("not found")

// fail answers a call that did not get through.
func (g *Gateway) fail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errHidden):
		refuse(c, http.StatusNotFound, wire.Error{Error: "not found: pco has no such guest or route", Code: codeNotFound})
	case errors.Is(err, errUnreachable):
		refuse(c, http.StatusBadGateway, wire.Error{Error: "the pco daemon does not answer on its socket", Code: wire.CodeDaemonUnreachable})
	case errors.Is(err, errNoAnswer):
		refuse(c, http.StatusServiceUnavailable, wire.Error{
			Error: "the pco daemon did not answer in time: the outcome is unknown; look again before you try again",
			Code:  codeUnavailable,
		})
	default:
		var refused *daemonRefused
		if errors.As(err, &refused) {
			g.write(c, refused.reply, nil)
			c.Abort()
			return
		}
		g.log.Error().Err(err).Str("path", c.Request.URL.Path).Msg("answering a call")
		refuse(c, http.StatusInternalServerError, wire.Error{Error: "pco web could not read the answer of the daemon", Code: codeInternal})
	}
}

// daemonRefused is an answer of the daemon that is not the one asked for,
// such as an error, passed on as it is.
type daemonRefused struct{ reply reply }

func (e *daemonRefused) Error() string { return fmt.Sprintf("the daemon answered %d", e.reply.status) }

// write answers with what the daemon answered, gzipped when the browser
// takes it and it is over a kibibyte; gz, when given, is body gzipped
// already.
func (g *Gateway) write(c *gin.Context, res reply, gz []byte) {
	h := c.Writer.Header()
	if res.etag != "" {
		h.Set("ETag", res.etag)
	}
	ctype := cmp.Or(res.ctype, "application/json; charset=utf-8")
	if res.status == http.StatusNotModified || res.status == http.StatusNoContent {
		c.Status(res.status)
		return
	}
	body := res.body
	if len(body) > gzipOver || gz != nil {
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(c.Request.Header.Values("Accept-Encoding")) {
			if gz == nil {
				gz = gzipped(body)
			}
			h.Set("Content-Encoding", "gzip")
			body = gz
		} else if body == nil {
			body = gunzipped(gz)
		}
	}
	c.Data(res.status, ctype, body)
}

func gzipped(data []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	// A bytes.Buffer takes every write.
	_, _ = zw.Write(data)
	_ = zw.Close()
	return buf.Bytes()
}

func gunzipped(gz []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil
	}
	data, _ := io.ReadAll(zr)
	return data
}

// acceptsGzip says whether Accept-Encoding gives gzip, or everything, a
// weight above zero.
func acceptsGzip(values []string) bool {
	star := false
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			name, params, _ := strings.Cut(part, ";")
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "gzip", "x-gzip":
				return weighted(params)
			case "*":
				star = weighted(params)
			}
		}
	}
	return star
}

// weighted says whether the parameters of an encoding leave it a weight
// above zero; without a q it has 1.
func weighted(params string) bool {
	for p := range strings.SplitSeq(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return err == nil && q > 0
		}
	}
	return true
}

// isJSON says whether a content type is JSON.
func isJSON(ctype string) bool {
	media, _, err := mime.ParseMediaType(ctype)
	return err == nil && media == "application/json"
}

// get makes a read of the gateway's own, for actor or for the streams, and
// refuses anything but a 200 of JSON.
func (g *Gateway) get(ctx context.Context, actor, path string, query url.Values) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	res, err := g.call(ctx, upstream{method: http.MethodGet, path: path, query: query, actor: actor})
	if err != nil {
		return nil, err
	}
	if res.status != http.StatusOK || !isJSON(res.ctype) {
		return nil, &daemonRefused{reply: res}
	}
	return res.body, nil
}
