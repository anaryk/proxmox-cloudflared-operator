package pve

import (
	"cmp"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	// answered, or when nothing was asked of it yet.
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
// verify, or answers a read with a server error; a write moves on only
// while nothing of it was sent, since the other node might apply it twice
// otherwise. A client error, a rejected token above all, is the answer of
// every endpoint alike and is returned from the first. A deadline is for
// the whole request, so an endpoint that hangs is not fallen back from.
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
	if host == "" || port == "" {
		return nil, errors.New("not an address with an optional port")
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
		i := (start + k) % len(f.targets)
		if k > 0 && req.Context().Err() != nil {
			break
		}
		o := outcome{i: i}
		o.resp, o.err = f.try(i, req)
		f.note(o)
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

// try sends req to target i. The request is cloned so that each target
// gets its own copy, with the body read anew.
func (f *fallback) try(i int, req *http.Request) (*http.Response, error) {
	t := f.targets[i]
	r := req.Clone(req.Context())
	r.URL.Host = t.host
	r.Host = ""
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		r.Body = body
	}
	return t.rt.RoundTrip(r)
}

// note keeps how the attempt went, and makes its target the current one
// when it answered.
func (f *fallback) note(o outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case o.err != nil:
		f.errs[o.i] = o.err
	case o.resp.StatusCode >= http.StatusInternalServerError:
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
}

// final says that the attempt decides the request: the endpoint answered,
// the request's context is done, or the request is a write that may have
// reached the endpoint, which must not be sent elsewhere as well.
func (o outcome) final(req *http.Request) bool {
	read := req.Method == http.MethodGet || req.Method == http.MethodHead
	switch {
	case o.err != nil:
		return req.Context().Err() != nil || !read && !beforeSending(o.err)
	case o.resp.StatusCode >= http.StatusInternalServerError:
		return !read
	}
	return true
}

// discard drops a response that is not returned.
func (o outcome) discard() {
	if o.resp != nil {
		_, _ = io.Copy(io.Discard, o.resp.Body)
		_ = o.resp.Body.Close()
	}
}

// beforeSending says that err happened before anything of the request was
// sent: the connection could not be made, or the certificate did not
// verify, which ends the handshake.
func beforeSending(err error) bool {
	var op *net.OpError
	return (errors.As(err, &op) && op.Op == "dial") || notVerified(err)
}
