package cffake

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

const (
	basePath     = "/client/v4"
	maxBodyBytes = 1 << 20
)

// Option changes how Handler serves the fake.
type Option func(*handler)

// WithToken accepts token as a bearer token. It may be given more than once;
// every token given is accepted and no other. Without it, or with an empty
// token, any non-empty bearer token is accepted. The fake knows a single token
// state, so the token that is accepted makes no difference to what it
// answers.
func WithToken(token string) Option {
	return func(h *handler) {
		if token != "" {
			h.tokens = append(h.tokens, token)
		}
	}
}

// WithFilteredTunnelTotals makes the total_count of a tunnel listing count the
// tunnels that matched the filters. By default it counts every tunnel of the
// account, which is what the API description says and what the client reads
// those listings for.
func WithFilteredTunnelTotals() Option {
	return func(h *handler) { h.filteredTotals = true }
}

// WithIgnoredIsDeleted makes the listing of tunnels ignore is_deleted and show
// the tunnels that were deleted whatever the request says, as a server does
// that does not know the filter. A client has to skip them itself.
func WithIgnoredIsDeleted() Option {
	return func(h *handler) { h.ignoreIsDeleted = true }
}

// Handler serves f under /client/v4 in the wire format of the Cloudflare API,
// so that the real client, and anything that uses it, can talk to the fake over
// HTTP. It serves exactly what the client calls:
//
//	GET    /user/tokens/verify
//	GET    /accounts/{account}/tokens/verify
//	GET    /accounts
//	GET    /zones
//	GET    /accounts/{account}/cfd_tunnel      (name, is_deleted, include_prefix)
//	POST   /accounts/{account}/cfd_tunnel
//	DELETE /accounts/{account}/cfd_tunnel/{tunnel}
//	GET    /accounts/{account}/cfd_tunnel/{tunnel}/token
//	GET    /accounts/{account}/cfd_tunnel/{tunnel}/configurations
//	PUT    /accounts/{account}/cfd_tunnel/{tunnel}/configurations
//	GET    /accounts/{account}/cfd_tunnel/{tunnel}/connections
//	GET    /zones/{zone}/dns_records           (type, name, comment.startswith)
//	POST   /zones/{zone}/dns_records
//	PATCH  /zones/{zone}/dns_records/{record}
//	DELETE /zones/{zone}/dns_records/{record}
//
// Every request is one call of the fake, so Deny, FailNext and Calls apply to
// it as they do to a call of the API. Errors keep the status of the error the
// fake returned, which for FailNext is the one it was given; an error that is
// not an *cfapi.Error is a 500. A 429 carries a Retry-After.
//
// The handler is strict about what it is sent, as Cloudflare is, and a client
// that drifts from the wire format fails against it instead of being
// forgiven. A query parameter, a field or a method it does not know is
// refused, and so is a body that is not sent as application/json (415). A
// record has a TTL of 1 or from 30 to 86400, a page of zones or accounts
// holds from 5 to 50, and a PATCH of a record has to carry every field, as the
// client sends them. What the handler keeps is what the fake keeps: a
// configuration holds the ingress and nothing else, and a tunnel is always
// managed by Cloudflare.
//
// A tunnel that was deleted stays in the listing of tunnels, with the time it
// was deleted, unless the request asks for is_deleted=false.
//
// Cloudflare sets warp-routing in a configuration itself. The handler always
// sends it, switched on, so that a client that took it for a setting somebody
// else made would show. A configuration that SetForeign marked holds a
// top-level originRequest.
//
// A token that expired or was disabled shows only at the token check.
// Cloudflare refuses every call made with such a token; the handler keeps
// answering them. That is a simplification: Deny is the way to model a token
// that is refused.
func Handler(f *Fake, opts ...Option) http.Handler {
	h := &handler{f: f}
	for _, opt := range opts {
		opt(h)
	}
	h.routes = []route{
		{http.MethodGet, "user/tokens/verify", h.verifyUser},
		{http.MethodGet, "accounts/{account}/tokens/verify", h.verifyAccount},
		{http.MethodGet, "accounts", h.listAccounts},
		{http.MethodGet, "zones", h.listZones},
		{http.MethodGet, "accounts/{account}/cfd_tunnel", h.listTunnels},
		{http.MethodPost, "accounts/{account}/cfd_tunnel", h.createTunnel},
		{http.MethodDelete, "accounts/{account}/cfd_tunnel/{tunnel}", h.deleteTunnel},
		{http.MethodGet, "accounts/{account}/cfd_tunnel/{tunnel}/token", h.tunnelToken},
		{http.MethodGet, "accounts/{account}/cfd_tunnel/{tunnel}/configurations", h.getConfig},
		{http.MethodPut, "accounts/{account}/cfd_tunnel/{tunnel}/configurations", h.putConfig},
		{http.MethodGet, "accounts/{account}/cfd_tunnel/{tunnel}/connections", h.listConnections},
		{http.MethodGet, "zones/{zone}/dns_records", h.listRecords},
		{http.MethodPost, "zones/{zone}/dns_records", h.createRecord},
		{http.MethodPatch, "zones/{zone}/dns_records/{record}", h.updateRecord},
		{http.MethodDelete, "zones/{zone}/dns_records/{record}", h.deleteRecord},
	}
	return h
}

type handler struct {
	f               *Fake
	tokens          []string // none: any non-empty token
	filteredTotals  bool
	ignoreIsDeleted bool
	routes          []route
}

// params are the segments of a path that a route names, unescaped.
type params map[string]string

// reply is what an endpoint answers with: the result, and for a listing the
// description of the page.
type reply struct {
	result any
	info   *resultInfo
}

type route struct {
	method  string
	pattern string // path segments below /client/v4, a name in braces matches any
	serve   func(r *http.Request, p params) (reply, error)
}

// match returns the parameters of a path that fits the pattern.
func (rt route) match(segments []string) (params, bool) {
	want := strings.Split(rt.pattern, "/")
	if len(want) != len(segments) {
		return nil, false
	}
	p := make(params)
	for i, w := range want {
		switch {
		case strings.HasPrefix(w, "{") && strings.HasSuffix(w, "}"):
			p[w[1:len(w)-1]] = segments[i]
		case w != segments[i]:
			return nil, false
		}
	}
	return p, true
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	segments, ok := splitPath(r.URL.EscapedPath())
	if !ok {
		writeError(w, apiError(http.StatusNotFound, codeNoRoute, "No route for that URI"))
		return
	}
	var allowed []string
	for _, rt := range h.routes {
		p, ok := rt.match(segments)
		switch {
		case !ok:
		case rt.method != r.Method:
			allowed = append(allowed, rt.method)
		default:
			if err := h.authenticate(r); err != nil {
				writeError(w, err)
				return
			}
			rep, err := rt.serve(r, p)
			if err != nil {
				writeError(w, err)
				return
			}
			write(w, http.StatusOK, envelope{Success: true, Result: rep.result, ResultInfo: rep.info})
			return
		}
	}
	if len(allowed) > 0 {
		slices.Sort(allowed)
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeError(w, apiError(http.StatusMethodNotAllowed, codeMethodNotAllowed, "Method not allowed"))
		return
	}
	writeError(w, apiError(http.StatusNotFound, codeNoRoute, "No route for that URI"))
}

// splitPath returns the unescaped segments of a path below /client/v4. A path
// with an empty segment, a bad escape or another root is none.
func splitPath(escaped string) ([]string, bool) {
	rest, ok := strings.CutPrefix(escaped, basePath+"/")
	if !ok {
		return nil, false
	}
	segments := strings.Split(rest, "/")
	for i, s := range segments {
		unescaped, err := url.PathUnescape(s)
		if err != nil || unescaped == "" {
			return nil, false
		}
		segments[i] = unescaped
	}
	return segments, true
}

func (h *handler) authenticate(r *http.Request) error {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return errAuthentication()
	}
	if len(h.tokens) == 0 {
		return nil
	}
	accepted := false
	for _, t := range h.tokens {
		accepted = subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 || accepted
	}
	if !accepted {
		return errAuthentication()
	}
	return nil
}

// Codes of the answers the handler makes up, and of the errors that have none.
// They are Cloudflare's where Cloudflare has one for it, and otherwise in the
// range its own are in, so that none can be taken for an HTTP status. The codes
// of the fake's own refusals are in fake.go.
const (
	codeNoRoute          = 7000
	codeNotFound         = 7003
	codeMethodNotAllowed = 10405
	codeRateLimited      = 971
	codeBadRequest       = 1400
	codeConflict         = 1409
	codeBodyTooLarge     = 1413
	codeUnsupportedType  = 1415
	codeServerError      = 1500
)

func errAuthentication() error {
	return apiError(http.StatusUnauthorized, codeAuthentication, "Authentication error")
}

func apiError(status, code int, message string) *cfapi.Error {
	return &cfapi.Error{Status: status, Codes: []int{code}, Message: message}
}

// badRequest is an error for a request that is not one the handler accepts. It
// carries no code, so the one of its status stands for it.
func badRequest(format string, args ...any) *cfapi.Error {
	return &cfapi.Error{Status: http.StatusBadRequest, Message: fmt.Sprintf(format, args...)}
}

// codeFor is the code of an error that has none.
func codeFor(status int) int {
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return codeAuthentication
	case status == http.StatusNotFound:
		return codeNotFound
	case status == http.StatusMethodNotAllowed:
		return codeMethodNotAllowed
	case status == http.StatusConflict:
		return codeConflict
	case status == http.StatusRequestEntityTooLarge:
		return codeBodyTooLarge
	case status == http.StatusUnsupportedMediaType:
		return codeUnsupportedType
	case status == http.StatusTooManyRequests:
		return codeRateLimited
	case status >= 500:
		return codeServerError
	}
	return codeBadRequest
}

type envelope struct {
	Success    bool          `json:"success"`
	Errors     []wireMessage `json:"errors"`
	Messages   []wireMessage `json:"messages"`
	Result     any           `json:"result"`
	ResultInfo *resultInfo   `json:"result_info,omitempty"`
}

type wireMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// resultInfo describes a page of a listing. Cloudflare leaves total_pages out
// of the tunnel listing.
type resultInfo struct {
	Page       int  `json:"page"`
	PerPage    int  `json:"per_page"`
	Count      int  `json:"count"`
	TotalCount int  `json:"total_count"`
	TotalPages *int `json:"total_pages,omitempty"`
}

func write(w http.ResponseWriter, status int, env envelope) {
	env.Errors = nonNil(env.Errors)
	env.Messages = nonNil(env.Messages)
	body, err := json.Marshal(env)
	if err != nil {
		http.Error(w, "cannot encode the answer", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// nonNil turns no messages into an empty list, which is how they are sent.
func nonNil(m []wireMessage) []wireMessage {
	if m == nil {
		return []wireMessage{}
	}
	return m
}

// writeError answers with err, in the status it carries when it is an error of
// the API. A 429 says when to try again, at the earliest in a second.
func writeError(w http.ResponseWriter, err error) {
	var apiErr *cfapi.Error
	status, message, codes := http.StatusInternalServerError, err.Error(), []int(nil)
	switch {
	case errors.As(err, &apiErr):
		status, message, codes = apiErr.Status, apiErr.Message, apiErr.Codes
		if status < 400 || status > 599 {
			status = http.StatusInternalServerError
		}
		if status == http.StatusTooManyRequests {
			seconds := max(1, int(math.Ceil(apiErr.RetryAfter.Seconds())))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
	case errors.Is(err, cfapi.ErrInvalidArgument):
		status = http.StatusBadRequest
	}
	if len(codes) == 0 {
		codes = []int{codeFor(status)}
	}
	if message == "" {
		message = http.StatusText(status)
	}
	msgs := make([]wireMessage, len(codes))
	for i, code := range codes {
		msgs[i] = wireMessage{Code: code, Message: message}
	}
	write(w, status, envelope{Errors: msgs})
}

type paging struct{ page, perPage int }

// readPaging reads page and per_page, which default to 1 and def. A page size
// outside of what the API gives for the listing, from least to most, is
// refused.
func readPaging(q url.Values, def, least, most int) (paging, error) {
	p := paging{page: 1, perPage: def}
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return paging{}, badRequest("page must be a positive integer")
		}
		p.page = n
	}
	if v := q.Get("per_page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < least || n > most {
			return paging{}, badRequest("per_page must be an integer from %d to %d", least, most)
		}
		p.perPage = n
	}
	return p, nil
}

// pageOf returns the items of the page. It is never nil, so that an empty page
// is sent as a list.
func pageOf[T any](items []T, p paging) []T {
	if p.page-1 > len(items)/p.perPage {
		return []T{}
	}
	start := (p.page - 1) * p.perPage
	end := min(start+p.perPage, len(items))
	if start >= end {
		return []T{}
	}
	return items[start:end]
}

// info describes the page that holds count of the total items. A listing that
// is counted by pages says how many there are.
func (p paging) info(count, total int, pages bool) *resultInfo {
	info := &resultInfo{Page: p.page, PerPage: p.perPage, Count: count, TotalCount: total}
	if pages {
		n := (total + p.perPage - 1) / p.perPage
		info.TotalPages = &n
	}
	return info
}

// checkQuery refuses a query that has a parameter other than the ones named,
// or one that is given twice.
func checkQuery(q url.Values, allowed ...string) error {
	for _, key := range slices.Sorted(maps.Keys(q)) {
		switch {
		case !slices.Contains(allowed, key):
			return badRequest("unknown query parameter %q", key)
		case len(q[key]) > 1:
			return badRequest("query parameter %q is given more than once", key)
		}
	}
	return nil
}

// readBody returns the body of a request, which has to be sent as
// application/json and must not be larger than the handler is willing to read.
func readBody(r *http.Request) ([]byte, error) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		return nil, &cfapi.Error{Status: http.StatusUnsupportedMediaType, Message: "Content-Type must be application/json"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	switch {
	case err != nil:
		return nil, badRequest("reading the request body: %v", err)
	case len(body) > maxBodyBytes:
		return nil, &cfapi.Error{Status: http.StatusRequestEntityTooLarge, Message: "request body is too large"}
	}
	return body, nil
}

// decodeObject decodes raw into dst, which must be a JSON object that has no
// field other than the ones named. The names are matched as they are spelled,
// not without regard to case as encoding/json does, so that a client that
// writes one wrong is noticed.
func decodeObject(raw []byte, dst any, fields ...string) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return badRequest("the body is not a JSON object: %v", err)
	}
	if keys == nil {
		return badRequest("expected a JSON object")
	}
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		if !slices.Contains(fields, key) {
			return badRequest("unknown field %q", key)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("the body does not fit: %v", err)
	}
	return nil
}
