package doctor

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	// diagnoseTimeout bounds the request to the origin, connection included.
	diagnoseTimeout = 5 * time.Second
	// maxBodyRead is how much of the answer of the origin is read, to look
	// for the page of a TLS server spoken to in plain HTTP. None of it is
	// returned.
	maxBodyRead    = 4 << 10
	maxHeaderBytes = 16 << 10
	// wildcardLabel stands for the label a wildcard route serves, in the
	// name the origin is asked for.
	wildcardLabel = "pco-diagnose"

	hintTLS    = "origin speaks TLS: use https:// in the route"
	hintPlain  = "origin speaks plain HTTP: use http:// in the route"
	hintVerify = "set sni=<name> in the route when the certificate is of another name, or no-tls-verify"
)

// plainOnTLS are what servers answer when they are spoken to in plain HTTP
// on a port where they speak TLS: nginx, Go and Apache.
var plainOnTLS = []string{
	"The plain HTTP request was sent to HTTPS port",
	"Client sent an HTTP request to an HTTPS server",
	"speaking plain HTTP to an SSL-enabled server port",
}

// target is where the request of the diagnosis goes, and how: as the tunnel
// would make it.
type target struct {
	scheme model.Scheme
	addr   netip.AddrPort
	host   string // the Host header
	sni    string // the name TLS asks for; empty for none
	verify bool
}

// target is the verified target of the route: the address and port of its
// rule, when the route is active, its rule serves what the route says and a
// candidate of that address passed. Anything else has none.
func (d *diagnosis) target() (target, bool) {
	r := d.rt.Rule
	if r == nil || d.rt.State != planner.StateActive || r.Service != d.rt.Service {
		return target{}, false
	}
	scheme, ap, err := parseService(r.Service)
	if err != nil {
		return target{}, false
	}
	if c, ok := d.candidate(ap.Addr()); !ok || !c.OK {
		return target{}, false
	}
	t := target{scheme: scheme, addr: ap, host: cmp.Or(r.HTTPHostHeader, requestHost(d.host)), verify: !r.NoTLSVerify}
	switch {
	case r.OriginServerName != "":
		t.sni = r.OriginServerName
	case r.MatchSNIToHost:
		// The name follows the Host the tunnel sends, after the override.
		t.sni = t.host
	}
	return t, true
}

// requestHost is the name a request for host carries: a wildcard stands for
// any name below it, and one is picked.
func requestHost(host string) string {
	if rest, ok := strings.CutPrefix(host, "*"); ok && hostname.IsWildcard(host) {
		return wildcardLabel + rest
	}
	return host
}

// parseService reads "http://10.0.0.11:8080", as the plan writes a service.
func parseService(service string) (scheme model.Scheme, ap netip.AddrPort, err error) {
	name, rest, found := strings.Cut(service, "://")
	scheme = model.Scheme(name)
	if !found || scheme != model.SchemeHTTP && scheme != model.SchemeHTTPS {
		return "", netip.AddrPort{}, fmt.Errorf("%q is no service of an address", service)
	}
	ap, err = netip.ParseAddrPort(rest)
	if err != nil || !ap.Addr().IsValid() || ap.Addr().IsUnspecified() || ap.Port() == 0 {
		return "", netip.AddrPort{}, fmt.Errorf("%q is no service of an address", service)
	}
	return scheme, ap, nil
}

func (d *diagnosis) http(ctx context.Context, base *http.Client) Step {
	t, ok := d.target()
	if !ok {
		return failed("there is no verified target to ask")
	}
	ctx, cancel := context.WithTimeout(ctx, diagnoseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, string(t.scheme)+"://"+t.addr.String()+"/", nil)
	if err != nil {
		return failed("the request cannot be made")
	}
	req.Host = t.host
	req.Header.Set("User-Agent", "pco-diagnose")
	res, err := newClient(t, base).Do(req)
	if err != nil {
		return failed(requestError(err, t.scheme))
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxBodyRead))
	return answered(t.scheme, res.StatusCode, body)
}

// newClient makes the client of one request to t: it connects to t whatever
// the request names, through no proxy, follows no redirect and gives up after
// diagnoseTimeout. Only the certificate authorities are taken from base.
func newClient(t target, base *http.Client) *http.Client {
	dialer := &net.Dialer{Timeout: diagnoseTimeout}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", t.addr.String())
			},
			TLSClientConfig: &tls.Config{
				ServerName:         t.sni,
				InsecureSkipVerify: !t.verify, // the route says so: no-tls-verify
				RootCAs:            rootsOf(base),
				MinVersion:         tls.VersionTLS12,
			},
			TLSHandshakeTimeout:    diagnoseTimeout,
			ResponseHeaderTimeout:  diagnoseTimeout,
			MaxResponseHeaderBytes: maxHeaderBytes,
			DisableKeepAlives:      true,
			DisableCompression:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       diagnoseTimeout,
	}
}

func rootsOf(base *http.Client) *x509.CertPool {
	if base == nil {
		return nil
	}
	if tr, ok := base.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
		return tr.TLSClientConfig.RootCAs
	}
	return nil
}

// answered judges the answer of the origin by its status, and, for a route
// that speaks plain HTTP, by whether the page says that TLS was expected.
func answered(scheme model.Scheme, code int, body []byte) Step {
	status := strings.TrimSpace(fmt.Sprintf("%d %s", code, http.StatusText(code)))
	switch {
	case scheme == model.SchemeHTTP && code == http.StatusBadRequest && slices.ContainsFunc(plainOnTLS, func(s string) bool { return strings.Contains(string(body), s) }):
		return failed(hintTLS)
	case code >= 500:
		return warned("the origin answered " + status)
	case code >= 300 && code < 400:
		return passed("the origin answered " + status + "; the redirect is not followed")
	}
	return passed("the origin answered " + status)
}

// requestError says what kept the request from an answer, without repeating
// anything the origin sent.
func requestError(err error, scheme model.Scheme) string {
	var (
		unknown x509.UnknownAuthorityError
		name    x509.HostnameError
		invalid x509.CertificateInvalidError
		verify  *tls.CertificateVerificationError
		record  tls.RecordHeaderError
		alert   tls.AlertError
		netErr  net.Error
	)
	switch {
	case errors.As(err, &unknown):
		return verifyFailed("unknown authority")
	case errors.As(err, &name):
		return verifyFailed("name mismatch")
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		return verifyFailed("expired or not yet valid")
	case errors.As(err, &invalid), errors.As(err, &verify):
		return verifyFailed("invalid certificate")
	case errors.Is(err, http.ErrSchemeMismatch), errors.As(err, &record):
		return hintPlain
	case errors.As(err, &alert):
		return fmt.Sprintf("the origin refused the TLS handshake (%s)", alert.Error())
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return fmt.Sprintf("no answer within %s", diagnoseTimeout)
	case scheme == model.SchemeHTTP && answeredTLS(err):
		return hintTLS
	case strings.Contains(err.Error(), "malformed HTTP"):
		return "the origin does not answer in HTTP"
	}
	return "the request failed"
}

func verifyFailed(class string) string {
	return fmt.Sprintf("TLS verification failed (%s): %s", class, hintVerify)
}

// answeredTLS reports whether the answer that did not read as HTTP begins
// with a TLS record: an alert, or a handshake.
func answeredTLS(err error) bool {
	const malformed = `malformed HTTP response "`
	msg := err.Error()
	i := strings.Index(msg, malformed)
	if i < 0 {
		return false
	}
	rest := msg[i+len(malformed):]
	return strings.HasPrefix(rest, `\x15\x03`) || strings.HasPrefix(rest, `\x16\x03`)
}
