// Package cfapi is a small client for the Cloudflare API calls the operator
// needs. Every call goes through one transport that takes care of
// authentication, rate limiting, the response envelope and paging.
//
// The operator deletes only on certain data, so a failed or partial answer is
// always an error here, never a shorter successful result.
package cfapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/version"
)

const (
	defaultBaseURL = "https://api.cloudflare.com/client/v4"
	defaultTimeout = 30 * time.Second
	maxBodyBytes   = 16 << 20

	// The budget of one credential: 300 requests per 5 minutes.
	defaultLimit  = 300
	defaultWindow = 5 * time.Minute
	defaultBurst  = 20

	// How a Retry-After is read: missing or unreadable means a minute, and
	// whatever the server asks is held between a second and an hour.
	defaultRetryAfter = time.Minute
	minRetryAfter     = time.Second
	maxRetryAfter     = time.Hour

	// Longest message taken from a response. Messages come from the server and
	// end up in logs.
	maxMessageBytes = 512
	redacted        = "[redacted]"
)

// Options says how to reach the API. Only Token is required.
type Options struct {
	BaseURL string // default https://api.cloudflare.com/client/v4; plain http only for loopback addresses
	Token   string // API token, sent as a bearer token; must not contain whitespace or control characters
	// HTTPClient is used for every request. The client keeps a copy of it that
	// refuses redirects whatever its CheckRedirect says, so that the token never
	// follows a redirect, and that times out after 30 s when it has no timeout
	// of its own; the value passed in is not changed. Default: a client with a
	// 30 s timeout.
	HTTPClient *http.Client

	// Limiter paces the requests. Cloudflare counts its rate limit per user,
	// not per token, so share one limiter between all clients of one
	// Cloudflare user (owner of the tokens). Default: 300 requests per 5
	// minutes, burst 20, for this client alone.
	Limiter *Limiter

	UserAgent string // default "pco/<version>"
}

// Client talks to the Cloudflare API. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	token     string
	hc        *http.Client
	limiter   *Limiter
	userAgent string
}

// New checks opts and returns a client for them. Errors never repeat the
// token or the base URL.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, errors.New("api token is empty")
	}
	if strings.ContainsFunc(opts.Token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return nil, errors.New("api token contains whitespace or control characters")
	}
	base, err := parseBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}

	hc := &http.Client{Timeout: defaultTimeout}
	if opts.HTTPClient != nil {
		given := *opts.HTTPClient
		hc = &given
	}
	if hc.Timeout == 0 {
		// A request that never ends would hold its caller, and whatever lock
		// it holds, for good.
		hc.Timeout = defaultTimeout
	}
	// The API never redirects; following one would carry the token to
	// wherever it points.
	hc.CheckRedirect = refuseRedirect

	c := &Client{
		base:      base,
		token:     opts.Token,
		hc:        hc,
		limiter:   opts.Limiter,
		userAgent: opts.UserAgent,
	}
	if c.limiter == nil {
		c.limiter = NewLimiter(defaultLimit, defaultWindow, defaultBurst, time.Now)
	}
	if c.userAgent == "" {
		c.userAgent = "pco/" + version.Version
	}
	return c, nil
}

func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// parseBaseURL returns the base URL to use. The messages do not quote raw,
// which may carry a secret.
func parseBaseURL(raw string) (*url.URL, error) {
	if raw == "" {
		raw = defaultBaseURL
	}
	base, err := url.Parse(raw)
	if err != nil {
		// The parse error would repeat part of the URL.
		return nil, errors.New("parsing base url: not a valid URL")
	}
	host := base.Hostname()
	switch {
	case host == "":
		return nil, errors.New("base url has no host")
	case base.User != nil:
		return nil, errors.New("base url must not carry credentials")
	case base.RawQuery != "" || base.ForceQuery:
		return nil, errors.New("base url must not carry a query")
	case base.Fragment != "":
		return nil, errors.New("base url must not carry a fragment")
	}
	switch {
	case base.Scheme == "https":
	case base.Scheme == "http" && isLoopback(host):
	default:
		return nil, errors.New("base url must use https (http only for a loopback address)")
	}
	return base, nil
}

func isLoopback(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

// envelope is what every Cloudflare answer is wrapped in.
type envelope struct {
	Success    *bool           `json:"success"` // a pointer, so that a missing field is not read as false
	Errors     []apiMessage    `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo *resultInfo     `json:"result_info"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// resultInfo describes the page of a listing. Which of the counts an endpoint
// sends differs, so each one is a pointer: absent is not zero.
type resultInfo struct {
	Page       *int `json:"page"`
	Count      *int `json:"count"`
	PerPage    int  `json:"per_page"`
	TotalCount *int `json:"total_count"`
	TotalPages *int `json:"total_pages"`
}

// do sends one request and decodes the result of the answer into out, which
// may be nil when the result is not needed. body, when not nil, is sent as
// JSON. path is below the base URL and must already be escaped (see
// joinPath); a path with an empty, "." or ".." segment is refused.
//
// An answer with a result of null or none is an error when out is given: no
// result is not an empty one.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	env, err := c.roundTrip(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if isNull(env.Result) {
		return fmt.Errorf("%w: no result", errUnexpected)
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("decoding result: %w", err)
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// roundTrip waits for the limiter, sends the request and returns the envelope
// of a successful answer. Every other outcome is an error; while the limiter
// is paused after a 429 that is a 429 of its own, and nothing is sent.
func (c *Client) roundTrip(ctx context.Context, method, path string, query url.Values, body any) (*envelope, error) {
	if err := checkPath(path); err != nil {
		return nil, fmt.Errorf("invalid request path: %w", err)
	}
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("waiting for the rate limit: %w", err)
	}

	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	return c.readAnswer(resp, raw, readErr)
}

// readAnswer turns the answer into an envelope when it is a success, and into
// an error in every other case. The status is looked at before a failure to
// read the body is reported, so that a 429 always pauses the limiter.
func (c *Client) readAnswer(resp *http.Response, raw []byte, readErr error) (*envelope, error) {
	tooBig := len(raw) > maxBodyBytes
	var env envelope
	var decodeErr error
	if !tooBig && readErr == nil {
		decodeErr = json.Unmarshal(raw, &env)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A body that could not be read, is too big or is not an envelope is
		// not worth quoting: the status says enough.
		if tooBig || readErr != nil || decodeErr != nil {
			env = envelope{}
		}
		apiErr := c.newError(resp.StatusCode, &env)
		if resp.StatusCode == http.StatusTooManyRequests {
			apiErr.RetryAfter = c.retryAfter(resp.Header)
			c.limiter.Pause(apiErr.RetryAfter)
		}
		return nil, apiErr
	}

	switch {
	case readErr != nil:
		return nil, fmt.Errorf("reading response: %w", readErr)
	case tooBig:
		return nil, fmt.Errorf("%w: body exceeds %d bytes", errUnexpected, maxBodyBytes)
	case decodeErr != nil:
		return nil, fmt.Errorf("%w: body is not a valid envelope: %w", errUnexpected, decodeErr)
	case env.Success == nil:
		return nil, fmt.Errorf("%w: no success field", errUnexpected)
	case !*env.Success:
		return nil, c.newError(resp.StatusCode, &env)
	}
	return &env, nil
}

func (c *Client) newError(status int, env *envelope) *Error {
	e := &Error{Status: status}
	message := ""
	for _, m := range env.Errors {
		e.Codes = append(e.Codes, m.Code)
		if message == "" {
			message = strings.TrimSpace(m.Message)
		}
	}
	e.Message = c.cleanMessage(message)
	if e.Message == "" {
		e.Message = fallbackMessage(status)
	}
	return e
}

// cleanMessage makes a message from the server safe to keep: control
// characters are dropped, so that the message cannot break or forge a line of
// a log, the token is blanked out, in case the server echoes the request, and
// the length is bounded. The control characters go before the token is looked
// for, which cannot contain any, so that one inside the echoed token does not
// hide it; the token goes before the cut so that a cut cannot leave part of
// it behind.
func (c *Client) cleanMessage(msg string) string {
	msg = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, msg)
	msg = strings.ReplaceAll(msg, c.token, redacted)
	if len(msg) <= maxMessageBytes {
		return msg
	}
	cut := maxMessageBytes
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}

func fallbackMessage(status int) string {
	switch text := http.StatusText(status); {
	case status >= 200 && status <= 299:
		// The status text of a 2xx would read as good news.
		return "request was not successful"
	case text != "":
		return text
	default:
		return "unknown error"
	}
}

// retryAfter reads the Retry-After header of a 429: a number of seconds or an
// HTTP date. A header that is missing or unreadable means a minute. The result
// is kept between a second and an hour: a server that says "now" has not made
// the next request any more likely to succeed, and one that says "never" must
// not stop the operator for good.
func (c *Client) retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	d := defaultRetryAfter
	if secs, err := strconv.ParseUint(v, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		// On overflow ParseUint returns the largest value, which is capped below.
		d = time.Duration(min(secs, uint64(maxRetryAfter/time.Second))) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(c.limiter.now())
	}
	return min(max(d, minRetryAfter), maxRetryAfter)
}
