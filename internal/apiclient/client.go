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
	"net/url"
	"syscall"
	"time"

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

// ErrUnknownRequest is what an error unwraps to when the daemon does not know
// the request: it is another version than the client.
var ErrUnknownRequest = errors.New("the daemon does not know the request")

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

// Events returns the events after since; the zero time asks for all of them.
func (c *Client) Events(ctx context.Context, since time.Time) ([]engine.Event, error) {
	path := "/v1/events"
	if !since.IsZero() {
		path += "?" + url.Values{"since": {since.UTC().Format(time.RFC3339Nano)}}.Encode()
	}
	var events []engine.Event
	err := c.call(ctx, c.short, http.MethodGet, path, nil, &events)
	return events, err
}

// Apply leaves observe-only mode; confirmDeletes lets the next run delete more
// DNS records than the mass delete guard allows.
func (c *Client) Apply(ctx context.Context, confirmDeletes bool) error {
	body := struct {
		ConfirmDeletes bool `json:"confirmDeletes"`
	}{confirmDeletes}
	return c.call(ctx, c.long, http.MethodPost, "/v1/apply", body, nil)
}

// Adopt asks the daemon to take over the DNS record of someone else that holds
// name.
func (c *Client) Adopt(ctx context.Context, name string) error {
	body := struct {
		Name string `json:"name"`
	}{name}
	return c.call(ctx, c.long, http.MethodPost, "/v1/adopt", body, nil)
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
		return c.transportError(err)
	}
	defer func() { _ = res.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	if err != nil {
		return c.transportError(err)
	}
	if len(data) > maxResponse {
		return errors.New("the answer of the pco daemon is too large")
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return c.statusError(res.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding the answer of the pco daemon: %w", err)
	}
	return nil
}

// daemonError is an error the daemon answered with. Its text is the message of
// the daemon, and it unwraps to the sentinel of the engine that the code of the
// answer stands for, if there is one.
type daemonError struct {
	msg        string
	cause      error
	credential *engine.CredentialView // the report of a refused token
}

func (e *daemonError) Error() string { return e.msg }
func (e *daemonError) Unwrap() error { return e.cause }

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
		return &daemonError{msg: "permission denied on " + c.socket + ": run as root", cause: fs.ErrPermission}
	case answer.Code == "no_route", answer.Code == "method_not_allowed",
		answer.Code == "" && (status == http.StatusNotFound || status == http.StatusMethodNotAllowed):
		return &daemonError{
			msg:   "the pco daemon at " + c.socket + " does not know this request: is it a different version than this pco?",
			cause: ErrUnknownRequest,
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
	return &daemonError{msg: msg, cause: cause, credential: answer.Credential}
}

// transportError explains a request that did not get an answer.
func (c *Client) transportError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ECONNREFUSED):
		return &daemonError{msg: "cannot reach the pco daemon at " + c.socket + ": is it running?", cause: err}
	case errors.Is(err, fs.ErrPermission):
		return &daemonError{msg: "permission denied on " + c.socket + ": run as root", cause: err}
	}
	return fmt.Errorf("talking to the pco daemon at %s: %w", c.socket, err)
}
