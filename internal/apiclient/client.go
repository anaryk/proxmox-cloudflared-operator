// Package apiclient talks to the API of the daemon over its unix socket. It is
// what the command line uses.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const (
	baseURL = "http://pco"
	// Whatever changes something may wait for the cycle that is running, and a
	// credential check or an apply may take a minute at Cloudflare. Only what
	// reads takes the short time.
	shortTimeout = 10 * time.Second
	longTimeout  = 60 * time.Second
	maxResponse  = 32 << 20
)

var (
	// ErrUnknownRequest is what an error unwraps to when the daemon does not
	// know the request: it is another version than the client.
	ErrUnknownRequest = errors.New("the daemon does not know the request")
	// ErrNoAnswer is what an error unwraps to when no usable answer came
	// from the daemon: it could not be reached, its socket refused the
	// caller, it did not answer in time or gave up waiting for a cycle, it
	// did not know the request, or what came was no answer of it. A command
	// that gets one could not ask; one the daemon refused is not one.
	ErrNoAnswer = errors.New("no usable answer from the pco daemon")
)

// NotRunning reports whether err says that nothing listens on the socket: it
// is not there, or no daemon is behind it. A daemon that did not answer, or
// whose socket refused the caller, is not one that is not running.
func NotRunning(err error) bool {
	var d *daemonError
	return errors.As(err, &d) && d.notRunning
}

// Client calls the daemon on a unix socket.
type Client struct {
	socket string
	http   *http.Client
	short  time.Duration
	long   time.Duration
}

// New returns a client for the daemon that listens on socketPath.
func New(socketPath string) *Client {
	var d net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", socketPath)
		},
		// One call per process, so there is nothing to keep alive.
		DisableKeepAlives: true,
	}
	return &Client{
		socket: socketPath,
		http: &http.Client{
			Transport: transport,
			// The daemon does not redirect. An answer that does is not one of its.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		short: shortTimeout,
		long:  longTimeout,
	}
}

// Status returns the state of the last cycle.
func (c *Client) Status(ctx context.Context) (engine.State, error) {
	var st engine.State
	err := c.call(ctx, c.short, http.MethodGet, "/v1/state", nil, &st)
	return st, err
}

// StatusRaw returns the state of the last cycle as the daemon sent it, for a
// caller that prints it unchanged.
func (c *Client) StatusRaw(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.call(ctx, c.short, http.MethodGet, "/v1/state", nil, &raw)
	return raw, err
}

// Credentials returns the stored credentials with the last check of each. The
// answer has no token.
func (c *Client) Credentials(ctx context.Context) ([]engine.CredentialView, error) {
	var views []engine.CredentialView
	err := c.call(ctx, c.short, http.MethodGet, "/v1/credentials", nil, &views)
	return views, err
}

// CredentialsRaw is Credentials as the daemon sent it.
func (c *Client) CredentialsRaw(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.call(ctx, c.short, http.MethodGet, "/v1/credentials", nil, &raw)
	return raw, err
}

// Events returns the events after since, oldest first; the zero time asks for
// all of them.
func (c *Client) Events(ctx context.Context, since time.Time) ([]engine.Event, error) {
	var events []engine.Event
	err := c.call(ctx, c.short, http.MethodGet, eventsPath(since), nil, &events)
	return events, err
}

// EventsRaw is Events as the daemon sent it.
func (c *Client) EventsRaw(ctx context.Context, since time.Time) (json.RawMessage, error) {
	return c.raw(ctx, c.short, eventsPath(since))
}

func eventsPath(since time.Time) string {
	path := "/v1/events"
	if !since.IsZero() {
		path += "?" + url.Values{"since": {since.UTC().Format(time.RFC3339Nano)}}.Encode()
	}
	return path
}

// Apply leaves observe-only mode. With confirmDeletes it confirms what waits
// for a confirmation, as the state with that offer showed it, such as more DNS
// deletes than the mass delete guard allows; the daemon refuses an offer that
// is not of what waits now. The result says what was accepted.
func (c *Client) Apply(ctx context.Context, confirmDeletes bool, offer string) (engine.ApplyResult, error) {
	body := struct {
		ConfirmDeletes bool   `json:"confirmDeletes"`
		Offer          string `json:"offer,omitempty"`
	}{confirmDeletes, offer}
	var res engine.ApplyResult
	err := c.call(ctx, c.long, http.MethodPost, "/v1/apply", body, &res)
	return res, err
}

// Adopt asks the daemon to take over the DNS record of someone else that holds
// name.
func (c *Client) Adopt(ctx context.Context, name string) error {
	body := struct {
		Name string `json:"name"`
	}{name}
	return c.call(ctx, c.long, http.MethodPost, "/v1/adopt", body, nil)
}

// RotateTunnel asks the daemon to rotate the secret of the tunnel of the
// install in account, or of its only tunnel when account is empty. Only root
// may ask.
func (c *Client) RotateTunnel(ctx context.Context, account string) (engine.TunnelRotation, error) {
	body := struct {
		Account string `json:"account,omitempty"`
	}{account}
	var res engine.TunnelRotation
	err := c.call(ctx, c.long, http.MethodPost, "/v1/tunnels/rotate", body, &res)
	return res, err
}

// Sync asks for a reconcile cycle now.
func (c *Client) Sync(ctx context.Context) error {
	return c.call(ctx, c.short, http.MethodPost, "/v1/sync", nil, nil)
}

// AddCredential checks a token and stores it as a credential. The token
// travels in the body of the request only. When the daemon refuses the token it
// answers with an error and with the view that says what the token can do, and
// so does AddCredential.
func (c *Client) AddCredential(ctx context.Context, label, token string) (engine.CredentialView, error) {
	body := struct {
		Label string `json:"label"`
		Token string `json:"token"`
	}{label, token}
	var view engine.CredentialView
	err := c.call(ctx, c.long, http.MethodPost, "/v1/credentials", body, &view)
	var de *daemonError
	if errors.As(err, &de) && de.credential != nil {
		return *de.credential, err
	}
	return view, err
}

// CheckCredential checks a stored credential again; deep proves the write
// permissions.
func (c *Client) CheckCredential(ctx context.Context, id string, deep bool) (engine.CredentialView, error) {
	body := struct {
		Deep bool `json:"deep"`
	}{deep}
	var view engine.CredentialView
	err := c.call(ctx, c.long, http.MethodPost, "/v1/credentials/"+url.PathEscape(id)+"/check", body, &view)
	return view, err
}

// RemoveCredential deletes a credential, if nothing is left that it manages.
func (c *Client) RemoveCredential(ctx context.Context, id string) error {
	return c.call(ctx, c.long, http.MethodDelete, "/v1/credentials/"+url.PathEscape(id), nil, nil)
}

// Claims returns the claims on the hostnames, with who holds each and who
// waits for it.
func (c *Client) Claims(ctx context.Context) ([]engine.ClaimView, error) {
	var claims []engine.ClaimView
	err := c.call(ctx, c.short, http.MethodGet, "/v1/claims", nil, &claims)
	return claims, err
}

// ClaimsRaw is Claims as the daemon sent it.
func (c *Client) ClaimsRaw(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, c.short, "/v1/claims")
}

// ResolveClaim hands the claim on hostname to owner, which has to claim it.
func (c *Client) ResolveClaim(ctx context.Context, hostname, owner string) error {
	body := struct {
		Hostname string `json:"hostname"`
		Owner    string `json:"owner"`
	}{hostname, owner}
	return c.call(ctx, c.long, http.MethodPost, "/v1/claims/resolve", body, nil)
}

// Approvals returns the approved guests, with the identity each was approved
// in.
func (c *Client) Approvals(ctx context.Context) ([]engine.ApprovalView, error) {
	var approvals []engine.ApprovalView
	err := c.call(ctx, c.short, http.MethodGet, "/v1/approvals", nil, &approvals)
	return approvals, err
}

// ApprovalsRaw is Approvals as the daemon sent it.
func (c *Client) ApprovalsRaw(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, c.short, "/v1/approvals")
}

// ApproveGuest approves a guest in the identity the daemon sees it in now.
// identity, when it is not empty, is the identity the admin was shown, which
// the guest has to have still, and macs are the MACs it was shown to wait
// for, which it has to wait for still; addrs are the soft-denied addresses
// the admin allows it. The answer is the approval as it was made.
func (c *Client) ApproveGuest(ctx context.Context, owner, identity string, macs []string, addrs []netip.Addr) (engine.Approval, error) {
	body := struct {
		Owner     string       `json:"owner"`
		Identity  string       `json:"identity,omitempty"`
		MACs      []string     `json:"macs,omitempty"`
		Addresses []netip.Addr `json:"addresses,omitempty"`
	}{owner, identity, macs, addrs}
	var approved engine.Approval
	err := c.call(ctx, c.long, http.MethodPost, "/v1/guests/approve", body, &approved)
	return approved, err
}

// RevokeGuest removes the approval of a guest.
func (c *Client) RevokeGuest(ctx context.Context, owner string) error {
	return c.call(ctx, c.long, http.MethodPost, "/v1/guests/revoke", ownerBody{owner}, nil)
}

type ownerBody struct {
	Owner string `json:"owner"`
}

// Segments returns the segments routes at observed were proven on, and those
// acknowledged.
func (c *Client) Segments(ctx context.Context) ([]engine.SegmentView, error) {
	var segments []engine.SegmentView
	err := c.call(ctx, c.short, http.MethodGet, "/v1/segments", nil, &segments)
	return segments, err
}

// SegmentsRaw is Segments as the daemon sent it.
func (c *Client) SegmentsRaw(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, c.short, "/v1/segments")
}

// AcknowledgeSegment lets routes at observed be served on a bridge and VLAN,
// 0 for untagged.
func (c *Client) AcknowledgeSegment(ctx context.Context, bridge string, vlan int) error {
	return c.call(ctx, c.long, http.MethodPost, "/v1/segments/acknowledge", segmentBody{bridge, vlan}, nil)
}

// RevokeSegment takes the acknowledgement of a segment back.
func (c *Client) RevokeSegment(ctx context.Context, bridge string, vlan int) error {
	return c.call(ctx, c.long, http.MethodPost, "/v1/segments/revoke", segmentBody{bridge, vlan}, nil)
}

type segmentBody struct {
	Bridge string `json:"bridge"`
	VLAN   int    `json:"vlan,omitempty"`
}

// Diagnose walks the chain of the route of hostname, in the daemon.
func (c *Client) Diagnose(ctx context.Context, hostname string) ([]doctor.Step, error) {
	var steps []doctor.Step
	err := c.call(ctx, c.long, http.MethodGet, diagnosePath(hostname), nil, &steps)
	return steps, err
}

// DiagnoseRaw is Diagnose as the daemon sent it.
func (c *Client) DiagnoseRaw(ctx context.Context, hostname string) (json.RawMessage, error) {
	return c.raw(ctx, c.long, diagnosePath(hostname))
}

func diagnosePath(hostname string) string {
	return "/v1/diagnose?" + url.Values{"hostname": {hostname}}.Encode()
}

// Doctor checks the installation, in the daemon.
func (c *Client) Doctor(ctx context.Context) ([]doctor.Finding, error) {
	var findings []doctor.Finding
	err := c.call(ctx, c.long, http.MethodGet, "/v1/doctor", nil, &findings)
	return findings, err
}

// DoctorRaw is Doctor as the daemon sent it.
func (c *Client) DoctorRaw(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, c.long, "/v1/doctor")
}

// raw returns the answer to a GET as the daemon sent it.
func (c *Client) raw(ctx context.Context, timeout time.Duration, path string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.call(ctx, timeout, http.MethodGet, path, nil, &raw)
	return raw, err
}

// Version returns the version of the running daemon.
func (c *Client) Version(ctx context.Context) (string, error) {
	var out struct {
		Version string `json:"version"`
	}
	err := c.call(ctx, c.short, http.MethodGet, "/v1/version", nil, &out)
	return out.Version, err
}

// call sends one request and decodes a successful answer into out, unless out
// is nil.
func (c *Client) call(ctx context.Context, timeout time.Duration, method, path string, in, out any) error {
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding the request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return c.transportError(caller, err, timeout)
	}
	defer func() { _ = res.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return c.transportError(caller, err, timeout)
	}
	if len(data) > maxResponse {
		return &daemonError{msg: "the answer of the pco daemon is too large", noAnswer: true}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return c.statusError(res.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &daemonError{msg: "decoding the answer of the pco daemon: " + err.Error(), cause: err, noAnswer: true}
	}
	return nil
}

// daemonError is an error the daemon answered with, or one that says why no
// usable answer came. Its text is the message of the daemon, or says what
// went wrong; it unwraps to the sentinel of the engine that the code of the
// answer stands for, or to the cause, and to ErrNoAnswer when it is no
// answer.
type daemonError struct {
	msg        string
	cause      error
	noAnswer   bool
	notRunning bool                   // nothing listens on the socket
	credential *engine.CredentialView // the report of a refused token
}

func (e *daemonError) Error() string { return e.msg }

func (e *daemonError) Unwrap() []error {
	var out []error
	if e.cause != nil {
		out = append(out, e.cause)
	}
	if e.noAnswer {
		out = append(out, ErrNoAnswer)
	}
	return out
}

// statusError makes the error of an answer that is not a success. What an
// answer stands for is told by its code, not by its status: an unknown route
// is a 404 too, and is not "not found".
func (c *Client) statusError(status int, body []byte) error {
	var answer struct {
		Error      string                 `json:"error"`
		Code       string                 `json:"code"`
		Credential *engine.CredentialView `json:"credential"`
	}
	if json.Unmarshal(body, &answer) != nil {
		answer.Error, answer.Code, answer.Credential = "", "", nil
	}
	switch {
	case status == http.StatusForbidden, answer.Code == "forbidden":
		return &daemonError{msg: "permission denied on " + c.socket + ": run as root", cause: fs.ErrPermission, noAnswer: true}
	case answer.Code == "no_route", answer.Code == "method_not_allowed",
		answer.Code == "" && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed):
		return &daemonError{
			msg:      "the pco daemon at " + c.socket + " does not know this request: is it a different version than this pco?",
			cause:    ErrUnknownRequest,
			noAnswer: true,
		}
	}
	msg := answer.Error
	if msg == "" {
		msg = fmt.Sprintf("the pco daemon answered %d %s", status, http.StatusText(status))
	}
	var cause error
	switch answer.Code {
	case "invalid":
		cause = engine.ErrInvalid
	case "not_found":
		cause = engine.ErrNotFound
	case "refused":
		cause = engine.ErrRefused
	}
	// A daemon that gave up, as on a cycle that ran too long, said why.
	unavailable := answer.Code == "unavailable" || answer.Code == "" && status == http.StatusServiceUnavailable
	return &daemonError{msg: msg, cause: cause, noAnswer: unavailable, credential: answer.Credential}
}

// transportError explains a request that did not get an answer. caller is the
// context the call was made with: when that one still runs, a deadline that
// passed is the timeout of the call.
func (c *Client) transportError(caller context.Context, err error, timeout time.Duration) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := fmt.Sprintf("talking to the pco daemon at %s: %v", c.socket, err)
	notRunning := errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
	switch {
	case notRunning:
		msg = "cannot reach the pco daemon at " + c.socket + ": is it running?"
	case errors.Is(err, fs.ErrPermission):
		msg = "permission denied on " + c.socket + ": run as root"
	case errors.Is(err, context.DeadlineExceeded) && caller.Err() == nil:
		msg = fmt.Sprintf("the pco daemon at %s did not answer within %s", c.socket, timeout)
	}
	return &daemonError{msg: msg, cause: err, noAnswer: true, notRunning: notRunning}
}
