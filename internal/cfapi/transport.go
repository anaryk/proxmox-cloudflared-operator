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

	pageSize = 100
	// A listing this long is a server that never ends, not a zone.
	maxPages = 1000

	defaultRetryAfter = time.Minute
	maxRetryAfter     = time.Hour
)

// Options says how to reach the API. Only Token is required.
type Options struct {
	BaseURL    string       // default https://api.cloudflare.com/client/v4; plain http only for loopback addresses
	Token      string       // API token, sent as a bearer token
	HTTPClient *http.Client // default: 30 s timeout, never follows a redirect
	Limiter    *Limiter     // default: 300 requests per 5 minutes, burst 20; share one between clients of a credential
	UserAgent  string       // default "pco/<version>"
}

// Client talks to the Cloudflare API. It is safe for concurrent use.
type Client struct {
	base      *url.URL
	auth      string
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
	base, err := parseBaseURL(opts.BaseURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		base:      base,
		auth:      "Bearer " + opts.Token,
		hc:        opts.HTTPClient,
		limiter:   opts.Limiter,
		userAgent: opts.UserAgent,
	}
	if c.hc == nil {
		c.hc = &http.Client{
			Timeout: defaultTimeout,
			// The API never redirects; following one would carry the token
			// to wherever it points.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if c.limiter == nil {
		c.limiter = NewLimiter(defaultLimit, defaultWindow, defaultBurst, time.Now)
	}
	if c.userAgent == "" {
		c.userAgent = "pco/" + version.Version
	}
	return c, nil
}

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

type resultInfo struct {
	TotalPages int `json:"total_pages"`
}

// do sends one request and decodes the result of the answer into out, which
// may be nil when the result is not needed. body, when not nil, is sent as
// JSON. path is below the base URL and must already be escaped.
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

// list reads every page of the collection at path, then calls each with every
// item in order. Nothing is passed to each unless all pages were read: a
// listing that failed half way is an error, never a shorter list.
func (c *Client) list(ctx context.Context, path string, query url.Values, each func(json.RawMessage) error) error {
	var items []json.RawMessage
	totalPages := 1
	for page := 1; page <= totalPages; page++ {
		env, err := c.roundTrip(ctx, http.MethodGet, path, pageQuery(query, page), nil)
		if err != nil {
			return fmt.Errorf("listing %s, page %d: %w", path, page, err)
		}
		var batch []json.RawMessage
		if isNull(env.Result) || json.Unmarshal(env.Result, &batch) != nil {
			return fmt.Errorf("listing %s, page %d: %w: result is not a list", path, page, errUnexpected)
		}
		items = append(items, batch...)

		if page == 1 && env.ResultInfo != nil {
			totalPages = max(env.ResultInfo.TotalPages, 1)
			if totalPages > maxPages {
				return fmt.Errorf("listing %s: %d pages is more than the limit of %d", path, totalPages, maxPages)
			}
		}
	}
	for _, item := range items {
		if err := each(item); err != nil {
			return err
		}
	}
	return nil
}

func pageQuery(query url.Values, page int) url.Values {
	q := make(url.Values, len(query)+2)
	for k, v := range query {
		q[k] = append([]string(nil), v...)
	}
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(pageSize))
	return q
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// roundTrip waits for the limiter, sends the request and returns the envelope
// of a successful answer. Every other outcome is an error.
func (c *Client) roundTrip(ctx context.Context, method, path string, query url.Values, body any) (*envelope, error) {
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
	req.Header.Set("Authorization", c.auth)
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

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return c.readAnswer(resp, raw)
}

// readAnswer turns the answer into an envelope when it is a success, and into
// an error in every other case.
func (c *Client) readAnswer(resp *http.Response, raw []byte) (*envelope, error) {
	tooBig := len(raw) > maxBodyBytes
	var env envelope
	var decodeErr error
	if !tooBig {
		decodeErr = json.Unmarshal(raw, &env)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// A body that is too big or not an envelope is not worth quoting: the
		// status says enough.
		if tooBig || decodeErr != nil {
			env = envelope{}
		}
		apiErr := newError(resp.StatusCode, &env)
		if resp.StatusCode == http.StatusTooManyRequests {
			apiErr.RetryAfter = c.retryAfter(resp.Header)
			c.limiter.Pause(apiErr.RetryAfter)
		}
		return nil, apiErr
	}

	switch {
	case tooBig:
		return nil, fmt.Errorf("%w: body exceeds %d bytes", errUnexpected, maxBodyBytes)
	case decodeErr != nil:
		return nil, fmt.Errorf("%w: body is not a valid envelope: %w", errUnexpected, decodeErr)
	case env.Success == nil:
		return nil, fmt.Errorf("%w: no success field", errUnexpected)
	case !*env.Success:
		return nil, newError(resp.StatusCode, &env)
	}
	return &env, nil
}

func newError(status int, env *envelope) *Error {
	e := &Error{Status: status}
	for _, m := range env.Errors {
		e.Codes = append(e.Codes, m.Code)
		if e.Message == "" {
			e.Message = strings.TrimSpace(m.Message)
		}
	}
	if e.Message == "" {
		e.Message = fallbackMessage(status)
	}
	return e
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
// HTTP date. A header that is missing or unreadable means a minute; one that
// asks for more than an hour is cut to an hour.
func (c *Client) retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		switch {
		case secs < 0:
			return defaultRetryAfter
		case secs > int64(maxRetryAfter/time.Second):
			return maxRetryAfter
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(c.limiter.now()), 0), maxRetryAfter)
	}
	return defaultRetryAfter
}
