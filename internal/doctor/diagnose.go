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

	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// Step is one link of a route's chain.
type Step struct {
	Name   string `json:"name"` // "route", "zone", "dns", "ingress", "connector", "identity", "tcp", "http"
	Level  Level  `json:"level"`
	Detail string `json:"detail"`
}

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
	wildcardLabel  = "pco-diagnose"
	blockedService = "http_status:503"

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

// DiagnoseRoute walks the chain of the route that holds a hostname in st:
// the route and its state, its zone, its DNS record, its ingress rule in the
// tunnel's verified configuration, the connector, the identity and the port
// of its target, and last an HTTP(S) request to that target as the tunnel
// would make it. The first step that fails leaves the steps after it skipped.
//
// The request goes to nothing but the address and port of the route's rule,
// and only when the state shows that address verified for the route's owner
// and its port answering; the Host header, the name TLS asks for and the
// verification are those of the rule. It follows no redirect, ends after
// diagnoseTimeout and reads at most maxBodyRead bytes, none of which are
// returned: only the status, a hint and the class of a TLS failure are.
//
// httpc only lends the certificate authorities it trusts; nil trusts those of
// the system. An unknown hostname is engine.ErrNotFound.
func DiagnoseRoute(ctx context.Context, st engine.State, name string, httpc *http.Client) ([]Step, error) {
	host, err := hostname.Normalize(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrInvalid, err)
	}
	rt, found := holderOf(st, host)
	if !found {
		return nil, fmt.Errorf("%w: the last cycle has no route for %s", engine.ErrNotFound, host)
	}
	d := &diagnosis{st: st, rt: rt, host: host}
	if i := slices.IndexFunc(st.Tunnels, func(t engine.TunnelView) bool { return rt.Account != "" && t.AccountID == rt.Account }); i >= 0 {
		d.tunnel = &st.Tunnels[i]
	}
	links := []struct {
		name  string
		check func() Step
	}{
		{"route", d.route}, {"zone", d.zone}, {"dns", d.dns}, {"ingress", d.ingress},
		{"connector", d.connector}, {"identity", d.identity}, {"tcp", d.tcp},
		{"http", func() Step { return d.http(ctx, httpc) }},
	}
	steps := make([]Step, 0, len(links))
	failed := false
	for _, l := range links {
		if failed {
			steps = append(steps, Step{Name: l.name, Level: LevelWarn, Detail: "skipped"})
			continue
		}
		s := l.check()
		s.Name = l.name
		failed = s.Level == LevelFail
		steps = append(steps, s)
	}
	return steps, nil
}

// holderOf returns the route of host that did not lose it to another owner,
// or, failing that, any route of host.
func holderOf(st engine.State, host string) (engine.RouteView, bool) {
	var fallback *engine.RouteView
	for i, r := range st.Routes {
		if !strings.EqualFold(r.Hostname, host) {
			continue
		}
		if r.State != planner.StateConflict {
			return r, true
		}
		if fallback == nil {
			fallback = &st.Routes[i]
		}
	}
	if fallback == nil {
		return engine.RouteView{}, false
	}
	return *fallback, true
}

type diagnosis struct {
	st     engine.State
	rt     engine.RouteView
	host   string
	tunnel *engine.TunnelView // of the route's account, when the state has it
}

func passed(detail string) Step { return Step{Level: LevelOK, Detail: detail} }
func warned(detail string) Step { return Step{Level: LevelWarn, Detail: detail} }
func failed(detail string) Step { return Step{Level: LevelFail, Detail: detail} }

func (d *diagnosis) route() Step {
	who := d.rt.Owner
	if g := d.rt.Guest; g != nil && g.Name != "" {
		who += " (" + g.Name + ")"
	}
	switch d.rt.State {
	case planner.StateConflict:
		return failed(d.rt.Reason)
	case planner.StateHeld:
		return failed(fmt.Sprintf("%s holds it, but nobody serves it: %s", who, d.rt.Reason))
	}
	detail := fmt.Sprintf("%s holds it; state %s", who, d.rt.State)
	if d.rt.Reason != "" {
		detail += ": " + d.rt.Reason
	}
	return passed(detail)
}

func (d *diagnosis) zone() Step {
	switch {
	case d.rt.State == planner.StateNoZone, d.rt.State == engine.RouteFrozen:
		return failed(cmp.Or(d.rt.Reason, "its zone cannot be served"))
	case d.rt.Zone == "":
		return failed("it is in no zone pco serves")
	}
	return passed("zone " + d.rt.Zone + " is active")
}

func (d *diagnosis) same(name string) bool {
	return strings.EqualFold(strings.TrimSuffix(name, "."), d.host)
}

// unchecked is the held reason of the route's tunnel when the last cycle did
// not check Cloudflare, and empty otherwise. What the state shows of the
// records, the rules and the connectors is then not what is there now.
func (d *diagnosis) unchecked() string {
	if d.tunnel != nil && strings.HasPrefix(d.tunnel.Held, engine.HeldUnchecked) {
		return d.tunnel.Held
	}
	return ""
}

func (d *diagnosis) dns() Step {
	if why := d.unchecked(); why != "" {
		return warned(why)
	}
	if i := slices.IndexFunc(d.st.Conflicts, func(c reconcile.Conflict) bool { return d.same(c.Name) }); i >= 0 {
		c := d.st.Conflicts[i]
		return failed(fmt.Sprintf("a record of someone else holds the name in zone %s: %s %s; pco adopt %s replaces it", c.Zone, c.Type, c.Content, d.host))
	}
	if slices.ContainsFunc(d.st.Lost, d.same) {
		return failed(fmt.Sprintf("its record points at the tunnel but lost the marker of this install; pco adopt %s takes it back", d.host))
	}
	for _, a := range d.st.Actions {
		if (a.Kind == reconcile.CreateRecord || a.Kind == reconcile.UpdateRecord) && !a.Applied && d.same(a.Target) {
			return failed(fmt.Sprintf("%s is not applied: %s", a.Kind, cmp.Or(a.Held, "it waits for the next run")))
		}
	}
	if d.rt.Service == "" && d.rt.State != planner.StateWithdrawn {
		return warned("no record is published for it while its target is not verified")
	}
	return passed("its record points at the tunnel")
}

func (d *diagnosis) ingress() Step {
	r, t := d.rt.Rule, d.tunnel
	switch {
	case r == nil || d.rt.Account == "":
		return failed("the plan has no rule for it")
	case d.unchecked() != "":
		return warned(d.unchecked())
	case t == nil || !t.Exists && !t.Unknown:
		return failed(fmt.Sprintf("the tunnel of account %s does not exist yet", d.rt.Account))
	case t.Unknown:
		return failed(fmt.Sprintf("the state of the tunnel of account %s is not known", d.rt.Account))
	case t.Held != "":
		return failed(fmt.Sprintf("tunnel %s in account %s is left as it is: %s", t.Name, t.AccountID, t.Held))
	case !t.Verified:
		return failed(fmt.Sprintf("the configuration of tunnel %s is not verified: the last write was held or failed (pco plan shows why)", t.Name))
	case r.Service == blockedService && d.targetIsTheCause():
		return warned(fmt.Sprintf("tunnel %s answers 503 for it until its target is verified", t.Name))
	case r.Service == blockedService:
		return failed(fmt.Sprintf("tunnel %s answers 503 for it", t.Name))
	}
	return passed(fmt.Sprintf("tunnel %s sends it to %s (configuration version %d)", t.Name, r.Service, t.Version))
}

// targetIsTheCause reports whether the route is not served because of its
// target, which the identity and tcp steps tell more of.
func (d *diagnosis) targetIsTheCause() bool {
	return d.rt.State == planner.StateUnreachable || d.rt.State == planner.StateWithdrawn
}

func (d *diagnosis) connector() Step {
	if why := d.unchecked(); why != "" {
		return warned(why)
	}
	t := d.tunnel
	if t == nil || t.ID == "" {
		return failed("the tunnel has no id yet")
	}
	i := slices.IndexFunc(d.st.Connectors, func(c connector.Status) bool { return c.TunnelID == t.ID })
	switch {
	case i < 0:
		return failed(fmt.Sprintf("no connector runs for tunnel %s", t.Name))
	case !d.st.Connectors[i].Active:
		return failed(fmt.Sprintf("the connector of tunnel %s is not running", t.Name))
	case !d.st.Connectors[i].Ready:
		return failed(fmt.Sprintf("the connector of tunnel %s is not connected to Cloudflare", t.Name))
	}
	return passed(connections(d.st.Connectors[i].Connections))
}

func (d *diagnosis) identity() Step {
	switch {
	case d.rt.State == planner.StateWithdrawn:
		return failed(cmp.Or(d.rt.Reason, "the identity check failed"))
	case d.rt.Service == "":
		return failed(cmp.Or(d.rt.Reason, "no verified address") + tried(d.rt.Candidates))
	}
	_, ap, err := parseService(d.rt.Service)
	if err != nil {
		return failed(fmt.Sprintf("the target %s is no address and port", d.rt.Service))
	}
	level := ""
	if d.rt.Level != "" {
		level = " at identity level " + d.rt.Level
	}
	if c, ok := d.candidate(ap.Addr()); ok {
		return passed(fmt.Sprintf("%s is the address of %s, verified%s (%s)", ap.Addr(), d.rt.Owner, level, c.Source))
	}
	return passed(fmt.Sprintf("%s is the address verified for %s%s", ap.Addr(), d.rt.Owner, level))
}

// tried lists the candidates resolution tried, and how each fared.
func tried(cands []resolve.CandidateResult) string {
	if len(cands) == 0 {
		return ""
	}
	parts := make([]string, len(cands))
	for i, c := range cands {
		parts[i] = fmt.Sprintf("%s (%s): %s", c.Addr, c.Source, cmp.Or(c.Reason, outcome(c.OK)))
	}
	return "; tried " + strings.Join(parts, ", ")
}

func outcome(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}

func (d *diagnosis) candidate(addr netip.Addr) (resolve.CandidateResult, bool) {
	i := slices.IndexFunc(d.rt.Candidates, func(c resolve.CandidateResult) bool { return c.Addr == addr })
	if i < 0 {
		return resolve.CandidateResult{}, false
	}
	return d.rt.Candidates[i], true
}

func (d *diagnosis) tcp() Step {
	_, ap, err := parseService(d.rt.Service)
	if err != nil {
		return failed(fmt.Sprintf("the target %s is no address and port", d.rt.Service))
	}
	c, ok := d.candidate(ap.Addr())
	switch {
	case ok && c.OK:
		return passed(ap.String() + " answers")
	case ok:
		return failed(cmp.Or(c.Reason, d.rt.Reason, "the port does not answer"))
	}
	return failed(cmp.Or(d.rt.Reason, "the last cycle did not try "+ap.String()))
}

// target is where the request of the diagnosis goes, and how.
type target struct {
	scheme string
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
func parseService(service string) (scheme string, ap netip.AddrPort, err error) {
	scheme, rest, found := strings.Cut(service, "://")
	if !found || scheme != "http" && scheme != "https" {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.scheme+"://"+t.addr.String()+"/", nil)
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
func answered(scheme string, code int, body []byte) Step {
	status := strings.TrimSpace(fmt.Sprintf("%d %s", code, http.StatusText(code)))
	switch {
	case scheme == "http" && code == http.StatusBadRequest && slices.ContainsFunc(plainOnTLS, func(s string) bool { return strings.Contains(string(body), s) }):
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
func requestError(err error, scheme string) string {
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
	case scheme == "http" && answeredTLS(err):
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
