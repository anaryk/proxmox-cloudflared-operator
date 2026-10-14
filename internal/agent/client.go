package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/clusterapi"
)

const (
	// discoverEvery is how often the client asks again where the controller
	// is: a host agent reads leader.json that often, so it follows a new
	// holder, or a new CA, within two seconds.
	discoverEvery = 2 * time.Second
	// statusEvery is the period of the status, and statusDebounce the least
	// time between two, which a change sends sooner.
	statusEvery    = 10 * time.Second
	statusDebounce = time.Second
	// pollSlack is how much longer than the controller's long-poll a poll
	// may take before the client gives up on it.
	pollSlack = 15 * time.Second
	// standDownWait is how long a node that stood down waits before it asks
	// the controller again, so that one enrolled again comes back without a
	// restart.
	standDownWait = time.Minute
	maxBackoff    = 30 * time.Second
	dialTimeout   = 10 * time.Second
	// maxAnswer bounds a heartbeat, a refusal or a certificate the client
	// reads.
	maxAnswer = 1 << 20
	// defaultPollInterval is how often the agent proves its targets again
	// while no desired state says otherwise.
	defaultPollInterval = time.Minute
)

var (
	errMoved = errors.New("the controller moved")
	errRenew = errors.New("the certificate is to be issued again")
)

// refusedError is a refusal of the controller, inside a verified session.
type refusedError struct{ r agentproto.Refusal }

func (e *refusedError) Error() string {
	return fmt.Sprintf("the controller refused this agent (%s): %s", e.r.Code, e.r.Message)
}

// ClientDeps is what a Client is built from.
type ClientDeps struct {
	Discover Discoverer
	// Files hold the agent's certificate. On a host Reissue issues it again
	// from the install's CA where it must be; in the appliance it is nil,
	// and the controller renews the certificate over the channel.
	Files   TLSFiles
	Reissue *Reissuer
	// Events are what the agent sends with its status; nil makes a log of
	// its own. Deps.Event of the agent should be its Add.
	Events *EventLog
}

// Client connects the agent of a node to the controller of its install over
// the network: it long-polls its desired state and applies it, reports the
// agent's status and hands it the heartbeat, follows the controller where
// it moves, and stands the agent down on a definitive refusal. While this
// node holds the lease, its controller drives the agent in process and the
// client waits for the lease to move.
type Client struct {
	a        *Agent
	d        ClientDeps
	protocol agentproto.Range

	discoverEvery, statusEvery, debounce, standDownWait time.Duration
	backoff                                             func(attempt int) time.Duration
	trigger                                             chan struct{}

	mu          sync.Mutex
	problem     string
	following   Endpoint
	creds       Creds
	stoodFor    string
	lastApplied time.Time
}

// NewClient returns the client of an agent.
func NewClient(a *Agent, d ClientDeps) (*Client, error) {
	switch {
	case a == nil:
		return nil, errors.New("agent client: no agent")
	case d.Discover == nil:
		return nil, errors.New("agent client: nothing to find the controller with")
	case d.Files.Dir == "":
		return nil, errors.New("agent client: no directory of the agent's certificate")
	case a.d.Kind == pki.KindHost && d.Reissue == nil:
		return nil, errors.New("agent client: a host agent issues its own certificate, and has nothing to do it with")
	}
	if d.Events == nil {
		d.Events = NewEventLog(a.d.Now)
	}
	return &Client{
		a: a, d: d,
		protocol:      agentproto.Range{Min: agentproto.ProtocolMin, Max: agentproto.ProtocolMax},
		discoverEvery: discoverEvery, statusEvery: statusEvery, debounce: statusDebounce, standDownWait: standDownWait,
		backoff: func(n int) time.Duration { return min(maxBackoff, time.Second<<min(n, 5)) },
		trigger: make(chan struct{}, 1),
	}, nil
}

// Trigger asks for a status soon: within a second, and at most once a
// second. The agent's Deps.Changed may call it.
func (c *Client) Trigger() {
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run follows the controller until ctx ends.
func (c *Client) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { c.reverify(ctx) })
	attempt := 0
	for ctx.Err() == nil {
		wait, reset := c.round(ctx, attempt)
		if reset {
			attempt = 0
		} else {
			attempt++
		}
		if !sleep(ctx, wait) {
			break
		}
	}
	return nil
}

// round finds the controller and follows it until it moves or fails, and
// returns how long to wait before the next and whether it got anywhere.
func (c *Client) round(ctx context.Context, attempt int) (time.Duration, bool) {
	ep, err := c.d.Discover.Endpoint(ctx)
	if err != nil {
		c.note("no controller to follow: " + err.Error())
		return c.backoff(attempt), false
	}
	c.follow(ep)
	if ep.Self {
		// The controller of this node serves with the server certificate,
		// which is issued again here as well when it is due.
		problem := ""
		if _, err := c.credentials(ep); err != nil {
			problem = "this node's certificate: " + err.Error()
		}
		c.note(problem)
		c.waitMove(ctx, ep)
		return 0, true
	}
	creds, err := c.credentials(ep)
	if err != nil {
		c.note("this node's certificate: " + err.Error())
		return c.backoff(attempt), false
	}
	served, err := c.session(ctx, ep, creds)
	var refused *refusedError
	switch {
	case ctx.Err() != nil:
		return 0, true
	case errors.Is(err, errMoved), errors.Is(err, errRenew):
		return 0, true
	case errors.As(err, &refused) && refused.r.Definitive():
		c.standDown(ctx, refused.r)
		return c.standDownWait, true
	case errors.As(err, &refused):
		c.note(err.Error() + "; this node serves what it was last told")
	case err != nil:
		c.note(err.Error())
	}
	c.a.d.Log.Warn().Err(err).Msg("the connection to the controller ended")
	return c.backoff(attempt), served
}

// credentials is the certificate to dial with: on a host issued again where
// it must be, in the appliance the one the controller issued.
func (c *Client) credentials(ep Endpoint) (Creds, error) {
	var creds Creds
	var err error
	if c.d.Reissue != nil {
		var issued bool
		creds, issued, err = c.d.Reissue.Ensure(ep.CASHA256)
		if issued {
			c.a.event(levelInfo, kindNode, c.a.d.Node, fmt.Sprintf(
				"issued this node's certificates again under the CA %s; the new client certificate ends %s",
				creds.CASHA256(), creds.Cert.NotAfter.UTC().Format(time.RFC3339)))
		}
	} else {
		creds, err = c.d.Files.Client()
	}
	if err != nil {
		return Creds{}, err
	}
	c.mu.Lock()
	c.creds = creds
	c.mu.Unlock()
	return creds, nil
}

// standDown takes the node out of service once per refusal, until a
// controller serves it again.
func (c *Client) standDown(ctx context.Context, r agentproto.Refusal) {
	c.mu.Lock()
	again := c.stoodFor == r.Code
	c.stoodFor = r.Code
	c.mu.Unlock()
	c.note("")
	if again {
		return
	}
	if err := c.a.StandDown(ctx, r); err != nil {
		c.a.d.Log.Error().Err(err).Msg("standing down")
	}
}

// session follows one controller at ep: a long-poll, the statuses and a
// watch of where the controller is run side by side, and the first of them
// to end ends the others. served reports whether a status was taken.
func (c *Client) session(ctx context.Context, ep Endpoint, creds Creds) (served bool, err error) {
	pin := ep.CASHA256
	if pin == "" {
		pin = creds.CASHA256()
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	tr := &http.Transport{
		TLSClientConfig:   pki.ClientTLS(creds.TLS(), pin, ep.ServerName),
		ForceAttemptHTTP2: true,
		// The URL names the server, not an address: each connection tries
		// the controller's addresses in order.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var errs []error
			for _, addr := range ep.Addrs {
				conn, err := dialer.DialContext(ctx, "tcp", addr)
				if err == nil {
					return conn, nil
				}
				errs = append(errs, err)
			}
			return nil, errors.Join(errs...)
		},
		TLSHandshakeTimeout:    dialTimeout,
		IdleConnTimeout:        2 * time.Minute,
		MaxResponseHeaderBytes: 64 << 10,
	}
	defer tr.CloseIdleConnections()
	s := &session{c: c, hc: httpClient(tr), base: "https://" + ep.ServerName, creds: creds}

	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	for _, run := range []func(context.Context) error{
		func(ctx context.Context) error { return c.watch(ctx, ep, creds) },
		s.poll,
		s.report,
	} {
		wg.Go(func() {
			if err := run(sctx); err != nil {
				cancel(err)
			}
		})
	}
	<-sctx.Done()
	wg.Wait()
	if ctx.Err() != nil {
		return s.served.Load(), nil
	}
	return s.served.Load(), context.Cause(sctx)
}

// watch asks every two seconds where the controller is, and ends the session
// when it moved, its CA changed, or a host's certificate is due.
func (c *Client) watch(ctx context.Context, ep Endpoint, creds Creds) error {
	t := time.NewTicker(c.discoverEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		// A discovery that fails keeps the session: the controller that
		// serves it is still the one to follow until another is named.
		if next, err := c.d.Discover.Endpoint(ctx); err == nil && !next.same(ep) {
			return errMoved
		}
		now := c.a.d.Now()
		if c.d.Reissue != nil && (pki.DueForRenewal(creds.Cert, now) || pki.DueForSelfRenewal(creds.Cert, now)) {
			return errRenew
		}
	}
}

// waitMove waits while this node holds the lease.
func (c *Client) waitMove(ctx context.Context, ep Endpoint) {
	t := time.NewTicker(c.discoverEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if next, err := c.d.Discover.Endpoint(ctx); err != nil || !next.same(ep) {
			return
		}
	}
}

// httpClient is the client of a session. The controller never redirects, so
// a redirect is taken as the answer it is and never followed elsewhere.
func httpClient(tr http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// session is one connection to a controller.
type session struct {
	c      *Client
	hc     *http.Client
	base   string
	creds  Creds
	served atomic.Bool
}

func (s *session) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(clusterapi.ProtocolHeader, fmt.Sprintf("%d-%d", s.c.protocol.Min, s.c.protocol.Max))
	return req, nil
}

// poll long-polls the desired state and applies what comes.
func (s *session) poll(ctx context.Context) error {
	for ctx.Err() == nil {
		st := s.c.a.Status(ctx)
		q := url.Values{"after": {strconv.FormatUint(st.SeqApplied, 10)}}
		if st.EpochSeen.Generation > 0 {
			q.Set("epoch", st.EpochSeen.String())
		}
		if err := s.pollOnce(ctx, "/v1/agent/desired?"+q.Encode()); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
	return nil
}

func (s *session) pollOnce(ctx context.Context, path string) error {
	pctx, cancel := context.WithTimeout(ctx, clusterapi.LongPoll+pollSlack)
	defer cancel()
	req, err := s.request(pctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("polling the desired state: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return s.take(ctx, resp.Body)
	case http.StatusNoContent:
		s.c.a.hear()
		return nil
	}
	return answerError("polling the desired state", resp)
}

// take applies a snapshot. One the agent does not take ends the session: it
// came from a controller the agent does not follow.
func (s *session) take(ctx context.Context, body io.Reader) error {
	snap, err := agentproto.DecodeSnapshot(body)
	if err != nil {
		return fmt.Errorf("reading the desired state: %w", err)
	}
	_, err = s.c.a.Apply(ctx, snap)
	if st := s.c.a.Status(ctx); st.EpochSeen != snap.Epoch || st.SeqApplied != snap.Seq {
		return fmt.Errorf("the controller's desired state was not taken: %w", err)
	}
	if err != nil {
		s.c.a.d.Log.Warn().Err(err).Msg("the desired state was applied in part")
	}
	s.c.mu.Lock()
	s.c.stoodFor, s.c.lastApplied = "", time.Now()
	s.c.mu.Unlock()
	s.c.Trigger()
	return nil
}

// report sends a status at once, then every ten seconds and soon after a
// change; an appliance agent whose certificate is due renews it.
func (s *session) report(ctx context.Context) error {
	t := time.NewTicker(s.c.statusEvery)
	defer t.Stop()
	for {
		if err := s.send(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		last := time.Now()
		if s.c.d.Reissue == nil && pki.DueForRenewal(s.creds.Cert, s.c.a.d.Now()) {
			if err := s.renew(ctx); err != nil {
				s.c.a.d.Log.Warn().Err(err).Msg("the certificate could not be renewed")
			} else {
				return errRenew
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-s.c.trigger:
			if !sleep(ctx, s.c.debounce-time.Since(last)) {
				return nil
			}
		}
	}
}

// send reports the agent's status and takes the heartbeat.
func (s *session) send(ctx context.Context) error {
	st := s.c.a.Status(ctx)
	st.Protocol = s.c.protocol
	if id, err := pki.IdentityOf(s.creds.Cert); err == nil {
		st.VMID = id.VMID // the container an appliance agent speaks for, as its certificate names it
	}
	st.Events, st.EventCursor, st.Resend = s.c.d.Events.Batch()
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encoding the status: %w", err)
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	req, err := s.request(ctx, http.MethodPost, "/v1/agent/status", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("sending the status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var hb agentproto.Heartbeat
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&hb); err != nil {
			return fmt.Errorf("reading the heartbeat: %w", err)
		}
		s.heartbeat(st, hb)
		return nil
	case http.StatusRequestEntityTooLarge:
		// What the controller will not take is given up, so that the
		// statuses after it go through.
		if st.Resend > 0 {
			s.c.d.Events.ResendRefused()
		} else {
			s.c.d.Events.Skip(st.EventCursor)
		}
		s.c.note(fmt.Sprintf("the controller refused the last status as too large (%d events, a resend of %d); its events were given up, "+
			"and what this node reports reaches the controller again with the next status it takes", len(st.Events), st.Resend))
		return nil
	}
	return answerError("sending the status", resp)
}

// heartbeat hands the agent the controller's answer, and moves the events
// on, unless it is of another install or of an epoch below the one the agent
// follows.
func (s *session) heartbeat(sent agentproto.Status, hb agentproto.Heartbeat) {
	s.served.Store(true)
	s.c.note("")
	if hb.Epoch.InstallID != s.c.a.d.InstallID {
		return
	}
	if order, _ := hb.Epoch.Order(sent.EpochSeen); order < 0 {
		return
	}
	s.c.a.Heartbeat(hb)
	s.c.d.Events.Ack(hb.Epoch, hb.EventAck)
}

// renew asks the controller for the next certificate, for a new key.
func (s *session) renew(ctx context.Context) error {
	id, err := pki.IdentityOf(s.creds.Cert)
	if err != nil {
		return err
	}
	key, err := pki.NewKey(rand.Reader)
	if err != nil {
		return err
	}
	csr, err := pki.NewCSR(key, id, rand.Reader)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(clusterapi.RenewRequest{CSR: csr})
	if err != nil {
		return err
	}
	req, err := s.request(ctx, http.MethodPost, "/v1/agent/renew", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("renewing the certificate: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return answerError("renewing the certificate", resp)
	}
	var answer clusterapi.CertificateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(&answer); err != nil {
		return fmt.Errorf("reading the renewed certificate: %w", err)
	}
	next, err := renewed(key, answer, id, s.creds)
	if err != nil {
		return err
	}
	return s.c.d.Files.SaveClient(next)
}

// renewed checks a renewed certificate: of the new key, the same identity,
// under the CA the agent knows.
func renewed(key *ecdsa.PrivateKey, answer clusterapi.CertificateResponse, id pki.Identity, was Creds) (Creds, error) {
	cert, err := parsePEMCert(answer.CertPEM)
	if err != nil {
		return Creds{}, fmt.Errorf("reading the renewed certificate: %w", err)
	}
	got, err := pki.IdentityOf(cert)
	pub, isEC := cert.PublicKey.(*ecdsa.PublicKey)
	switch {
	case err != nil:
		return Creds{}, err
	case got != id:
		return Creds{}, fmt.Errorf("the renewed certificate is of %s, not %s", got, id)
	case cert.CheckSignatureFrom(was.CA) != nil:
		return Creds{}, errors.New("the renewed certificate is not under the CA this agent knows")
	case !isEC || !pub.Equal(&key.PublicKey):
		return Creds{}, errors.New("the renewed certificate is not of the new key")
	}
	return Creds{Key: key, Cert: cert, CA: was.CA}, nil
}

func parsePEMCert(text string) (*x509.Certificate, error) {
	b, _ := pem.Decode([]byte(text))
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate in PEM")
	}
	return x509.ParseCertificate(b.Bytes)
}

// answerError reads what the controller answered: a refusal, or a failure.
func answerError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode == http.StatusForbidden {
		var r agentproto.Refusal
		if err := json.Unmarshal(body, &r); err == nil && r.Code != "" {
			return &refusedError{r: r}
		}
	}
	return fmt.Errorf("%s: the controller answered %s: %s", what, resp.Status, bytes.TrimSpace(body))
}

// reverify proves the targets again every poll interval while no desired
// state comes, unless this node's controller drives the agent.
func (c *Client) reverify(ctx context.Context) {
	for {
		every := c.a.pollInterval(ctx)
		if !sleep(ctx, every) {
			return
		}
		c.mu.Lock()
		skip := c.following.Self || time.Since(c.lastApplied) < every
		c.mu.Unlock()
		if skip {
			continue
		}
		if _, err := c.a.Reverify(ctx); err != nil && ctx.Err() == nil {
			c.a.d.Log.Warn().Err(err).Msg("proving the targets again")
		}
		c.Trigger()
	}
}

// pollInterval is the desired state's, or the default without one.
func (a *Agent) pollInterval(ctx context.Context) time.Duration {
	if a.acquire(ctx) != nil {
		return defaultPollInterval
	}
	defer a.release()
	if a.desired != nil && a.desired.Settings.PollInterval > 0 {
		return a.desired.Settings.PollInterval
	}
	return defaultPollInterval
}

func (c *Client) note(problem string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if problem != "" && problem != c.problem {
		c.a.d.Log.Warn().Msg(problem)
	}
	c.problem = problem
}

func (c *Client) follow(ep Endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.following = ep
}

// View is the agent's view with what the client knows: whom it follows, its
// certificate, and why it does not reach the controller.
func (c *Client) View() agentproto.AgentView {
	v := c.a.View()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ep := c.following; {
	case ep.Self:
		v.Controller = ep.Node + " (this node)"
	case ep.Node != "":
		v.Controller, v.Remote = ep.Node, true
	case len(ep.Addrs) > 0:
		v.Controller, v.Remote = ep.Addrs[0], true
	}
	if c.creds.Cert != nil {
		v.CertFingerprint, v.CertExpires = pki.Fingerprint(c.creds.Cert), c.creds.Cert.NotAfter
	}
	if c.problem != "" {
		v.Problems = append(v.Problems, c.problem)
	}
	return v
}

// sleep waits d, or until ctx ends; it reports whether ctx is still alive.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
