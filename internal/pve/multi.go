package pve

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPort is the port pveproxy listens on.
const DefaultPort = "8006"

// Endpoint is one pveproxy the controller can talk to: an address, on
// DefaultPort unless another is given, and the name the certificate of that
// node is verified under, the host of the address when empty.
type Endpoint struct {
	Address    string
	ServerName string
}

// EndpointStatus is an endpoint and how its last attempt went.
type EndpointStatus struct {
	Endpoint Endpoint
	// Current says that the next request goes to this endpoint first.
	Current bool
	// Err is why the last attempt at the endpoint failed, nil when it
	// answered, or when nothing was asked of it yet. A request the caller
	// gave up on says nothing of the endpoint.
	Err error
}

// Multi is a Client that, when the endpoint of its configuration does not
// answer, tries the other endpoints in order, and keeps talking to the last
// one that answered until that one stops answering. The other endpoints are
// the pveproxies of the other nodes, verified with the CA of the
// configuration under each node's own name; one on a loopback address is
// pinned to the certificate of this node, as the configuration's is.
//
// A request moves on when an endpoint refuses the connection, does not
// verify, gives no answer within its share of the request's deadline, or
// answers a read with a status that says pveproxy could not reach its
// backend; a write moves on only while nothing of it was sent, since the
// other node might apply it twice. A client error, a rejected token above
// all, is the answer of every endpoint alike and is returned from the
// first. An endpoint that gave no answer in time is not asked first again
// until another one has failed.
type Multi struct {
	*Client
	fb *fallback
}

// NewMulti is New for the endpoint of cfg, falling back to endpoints in
// their order. An endpoint that is no loopback address needs cfg.CAFile.
func NewMulti(cfg Config, endpoints []Endpoint) (*Multi, error) {
	base, err := parseConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg.NodeCertDir = cmp.Or(cfg.NodeCertDir, DefaultNodeCertDir)
	pool, err := caPool(cfg, x509.SystemCertPool)
	if err != nil {
		return nil, err
	}
	fb := &fallback{targets: []target{{
		ep:   Endpoint{Address: base.Host, ServerName: cfg.ServerName},
		host: base.Host,
		rt:   newTransport(base, cfg.ServerName, pool, cfg.NodeCertDir),
	}}}
	for _, ep := range endpoints {
		u, err := endpointURL(base, ep)
		if err != nil {
			return nil, fmt.Errorf("endpoint %q: %w", ep.Address, err)
		}
		if cfg.CAFile == "" && !isLoopback(u.Hostname()) {
			return nil, fmt.Errorf("endpoint %q: CA file is required for a host that is not a loopback address", ep.Address)
		}
		fb.targets = append(fb.targets, target{ep: ep, host: u.Host, rt: newTransport(u, ep.ServerName, pool, cfg.NodeCertDir)})
	}
	fb.errs = make([]error, len(fb.targets))
	hc := &http.Client{Transport: fb, CheckRedirect: noRedirect}
	return &Multi{Client: newClient(cfg, base, hc), fb: fb}, nil
}

// endpointURL is where ep is reached: its address, with DefaultPort unless
// it names one, under the scheme and path of base.
func endpointURL(base *url.URL, ep Endpoint) (*url.URL, error) {
	if ep.Address == "" || strings.ContainsAny(ep.Address, "/?#@ ") {
		return nil, errors.New("not an address with an optional port")
	}
	host, port, err := net.SplitHostPort(ep.Address)
	if err != nil {
		host, port = strings.Trim(ep.Address, "[]"), DefaultPort
	}
	if host == "" {
		return nil, errors.New("not an address with an optional port")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("port %q is not between 1 and 65535", port)
	}
	return &url.URL{Scheme: base.Scheme, Host: net.JoinHostPort(host, port), Path: base.Path}, nil
}

// Current is the endpoint the next request goes to first.
func (m *Multi) Current() Endpoint {
	m.fb.mu.Lock()
	defer m.fb.mu.Unlock()
	return m.fb.targets[m.fb.current].ep
}

// Status is every endpoint in order, with how its last attempt went.
func (m *Multi) Status() []EndpointStatus {
	m.fb.mu.Lock()
	defer m.fb.mu.Unlock()
	out := make([]EndpointStatus, len(m.fb.targets))
	for i, t := range m.fb.targets {
		out[i] = EndpointStatus{Endpoint: t.ep, Current: i == m.fb.current, Err: m.fb.errs[i]}
	}
	return out
}

type target struct {
	ep   Endpoint
	host string
	rt   http.RoundTripper
}

// fallback sends each request to the current target and, when that one
// does not answer, to the others in order.
type fallback struct {
	targets []target

	mu      sync.Mutex
	current int
	errs    []error
}

func (f *fallback) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	start := f.current
	f.mu.Unlock()

	// A body that cannot be read again is sent once, to the current target.
	tries := len(f.targets)
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		tries = 1
	}
	var first outcome
	for k := range tries {
		if k > 0 && req.Context().Err() != nil {
			break
		}
		i := (start + k) % len(f.targets)
		o := f.try(i, req, tries-k)
		if o.cancelled {
			first.discard()
			return nil, o.err
		}
		f.note(o, start)
		if o.final(req) {
			first.discard()
			return o.resp, o.err
		}
		if first.resp == nil && first.err == nil {
			first = o
		} else {
			o.discard()
		}
	}
	return first.resp, first.err
}

// try sends req to target i, within its share of what is left of the
// request's deadline, with left targets still to try. The request is cloned
// so that each target gets its own copy, with the body read anew.
func (f *fallback) try(i int, req *http.Request, left int) outcome {
	t := f.targets[i]
	ctx, cancel := context.WithTimeout(req.Context(), share(req.Context(), left))
	// Whether the attempt got a connection, new or kept, decides whether a
	// write may have reached the endpoint.
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { connected.Store(true) },
	})
	r := req.Clone(ctx)
	r.URL.Host = t.host
	r.Host = ""
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			cancel()
			return outcome{i: i, err: err, cancelled: true}
		}
		r.Body = body
	}
	resp, err := t.rt.RoundTrip(r) //nolint:bodyclose // the body goes back to the caller, inside the outcome
	if err != nil {
		// Why the attempt ended is read before its context is cancelled,
		// which would make every failure look like a timeout. The request's
		// deadline is never shorter than the attempt's share of it, so a
		// deadline is the endpoint's failure; only the caller giving up is
		// not.
		o := outcome{i: i, err: err, connected: connected.Load()}
		switch {
		case errors.Is(req.Context().Err(), context.Canceled):
			o.cancelled = true
		case ctx.Err() != nil:
			o.timedOut = true
			o.err = fmt.Errorf("no answer within its share of the deadline: %w", err)
		}
		cancel()
		return o
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return outcome{i: i, resp: resp}
}

// share is the time one attempt gets: what is left of the deadline, divided
// by the targets still to try, so that every one of them is tried.
func share(ctx context.Context, left int) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return defaultTimeout
	}
	return time.Until(deadline) / time.Duration(max(left, 1))
}

// note keeps how the attempt went, and makes its target the current one
// when it answered. The target the request started at, when it gave no
// answer in time, hands the first place to the next one, so that the next
// request does not wait for it again.
func (f *fallback) note(o outcome, start int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case o.err != nil:
		f.errs[o.i] = o.err
		if o.timedOut && o.i == start && f.current == start {
			f.current = (start + 1) % len(f.targets)
		}
	case unreachable(o.resp.StatusCode):
		f.errs[o.i] = &APIError{Status: o.resp.StatusCode, Message: http.StatusText(o.resp.StatusCode)}
	default:
		f.errs[o.i] = nil
		f.current = o.i
	}
}

// outcome is what one attempt gave.
type outcome struct {
	i    int
	resp *http.Response
	err  error
	// connected says the attempt had a connection to the endpoint, so a
	// write may have reached it; timedOut that the attempt ran out of its
	// share of the deadline; cancelled that the request's own context
	// ended, which says nothing of the target.
	connected bool
	timedOut  bool
	cancelled bool
}

// final says that the attempt decides the request: the endpoint answered
// for itself, or the request is a write that may have reached the endpoint,
// which must not be sent elsewhere as well.
func (o outcome) final(req *http.Request) bool {
	read := req.Method == http.MethodGet || req.Method == http.MethodHead
	switch {
	case o.err != nil:
		return !read && !o.beforeSending()
	case unreachable(o.resp.StatusCode):
		return !read
	}
	return true
}

// beforeSending says that the attempt failed before anything of the request
// was sent: no connection was had, within the time or at all, or the
// certificate did not verify, which ends the handshake.
func (o outcome) beforeSending() bool {
	return !o.connected || notVerified(o.err)
}

// discard drops a response that is not returned.
func (o outcome) discard() {
	if o.resp != nil {
		_, _ = io.Copy(io.Discard, o.resp.Body)
		_ = o.resp.Body.Close()
	}
}

// unreachable says that the status is pveproxy's for a backend it could not
// reach, or a gateway's for the same, not an answer of the API: 502, 503,
// 504, and the 595 to 599 pveproxy answers with when its connection to the
// node fails.
func unreachable(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return status >= 595 && status <= 599
}

// cancelOnClose ends the attempt's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}
