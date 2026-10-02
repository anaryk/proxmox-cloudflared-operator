// Package pve is a small client for the Proxmox VE API calls the operator
// needs. It reads guests, their configuration, their addresses and the
// network layout of the nodes; it never changes anything.
package pve

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultTimeout = 10 * time.Second
	maxBodyBytes   = 8 << 20
	apiPath        = "api2/json"
)

// DefaultURL is the Proxmox VE API of the node pco runs on.
const DefaultURL = "https://127.0.0.1:8006"

// ErrAgentUnavailable means the QEMU guest agent cannot answer: it is not
// configured, not running, or the guest is stopped.
var ErrAgentUnavailable = errors.New("guest agent not available")

// Config says how to reach the Proxmox VE API.
type Config struct {
	BaseURL string        // e.g. "https://127.0.0.1:8006"
	TokenID string        // "user@realm!tokenid"
	Secret  string        // the token's secret value
	CAFile  string        // PEM bundle; required unless the host is a loopback address
	Timeout time.Duration // per request, default 10s
	// NodeCertDir is where the certificates of this node are, which the API
	// on a loopback address must present; default DefaultNodeCertDir.
	NodeCertDir string
}

// Client talks to one Proxmox VE API endpoint. It is safe for concurrent use.
type Client struct {
	base    *url.URL
	auth    string
	hc      *http.Client
	timeout time.Duration
}

// New checks cfg and returns a client for it. A loopback host must present
// the certificate this node serves, as it is in cfg.NodeCertDir; any other
// host is verified against cfg.CAFile.
func New(cfg Config) (*Client, error) {
	base, err := parseConfig(cfg)
	if err != nil {
		return nil, err
	}
	hc, err := newHTTPClient(base, cfg.CAFile, cmp.Or(cfg.NodeCertDir, DefaultNodeCertDir))
	if err != nil {
		return nil, err
	}
	return newClient(cfg, base, hc), nil
}

// newWithHTTPClient is New with the HTTP client supplied, so that tests can
// trust the certificate of a test server.
func newWithHTTPClient(cfg Config, hc *http.Client) (*Client, error) {
	base, err := parseConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newClient(cfg, base, hc), nil
}

func newClient(cfg Config, base *url.URL, hc *http.Client) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Client{
		base:    base,
		auth:    "PVEAPIToken=" + cfg.TokenID + "=" + cfg.Secret,
		hc:      hc,
		timeout: timeout,
	}
}

// parseConfig validates cfg and returns the parsed base URL. Errors never
// repeat the token or the secret.
func parseConfig(cfg Config) (*url.URL, error) {
	base, err := url.Parse(cfg.BaseURL)
	if err != nil {
		// The underlying error would repeat the URL, which may carry credentials.
		return nil, fmt.Errorf("parsing base url: %w", errors.Unwrap(err))
	}
	if base.Scheme != "https" {
		return nil, errors.New("base url must use https")
	}
	host := base.Hostname()
	if host == "" {
		return nil, errors.New("base url has no host")
	}
	if base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, errors.New("base url must not carry credentials, a query or a fragment")
	}
	user, name, ok := strings.Cut(cfg.TokenID, "!")
	if !ok || user == "" || name == "" {
		return nil, errors.New("token id must look like user@realm!tokenid")
	}
	if cfg.Secret == "" {
		return nil, errors.New("token secret is empty")
	}
	if cfg.CAFile == "" && !isLoopback(host) {
		return nil, fmt.Errorf("CA file is required for host %q, which is not a loopback address", host)
	}
	return base, nil
}

func isLoopback(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func newHTTPClient(base *url.URL, caFile, nodeCertDir string) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	// A CA file that is given is always read, so that a wrong path is
	// reported even where it would not be used.
	if caFile != "" {
		pool, err := loadCAPool(caFile)
		if err != nil {
			return nil, err
		}
		tlsCfg.RootCAs = pool
	}
	if isLoopback(base.Hostname()) {
		// The certificate is pinned instead: it is self-signed, by the CA
		// of the cluster, and checked in VerifyConnection.
		tlsCfg.InsecureSkipVerify = true
		tlsCfg.VerifyConnection = (&nodePin{dir: nodeCertDir}).verify
	}
	return &http.Client{
		// No proxy on purpose: the API is local or on the management network.
		Transport: &http.Transport{
			TLSClientConfig:     tlsCfg,
			TLSHandshakeTimeout: defaultTimeout,
			IdleConnTimeout:     90 * time.Second,
		},
		// The API never redirects; following one would carry the token
		// to wherever it points.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("CA file %q contains no certificates", path)
	}
	return pool, nil
}

// APIError is a non-2xx answer from the API.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("proxmox api: HTTP %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err says the thing asked for is not there: a
// 404, or the 500 Proxmox uses when a guest's configuration does not exist.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusNotFound ||
		(apiErr.Status == http.StatusInternalServerError && containsFold(apiErr.Message, "does not exist"))
}

// IsForbidden reports whether err is a rejected token or missing privilege.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

func containsFold(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), substr)
}

// get calls GET <base>/api2/json/<endpoint> and decodes the "data" member of
// the answer into out.
func (c *Client) get(ctx context.Context, endpoint string, query url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	u := c.base.JoinPath(apiPath, endpoint)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	tooBig := len(body) > maxBodyBytes

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if tooBig {
			body = nil
		}
		return newAPIError(resp.StatusCode, body)
	}
	if tooBig {
		return fmt.Errorf("response exceeds %d bytes", maxBodyBytes)
	}
	return decodeData(body, out)
}

func newAPIError(status int, body []byte) *APIError {
	var payload struct {
		Message string `json:"message"`
	}
	msg := ""
	if json.Unmarshal(body, &payload) == nil {
		msg = strings.TrimSpace(payload.Message)
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	if msg == "" {
		msg = "unknown error"
	}
	return &APIError{Status: status, Message: msg}
}

func decodeData(body []byte, out any) error {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("unexpected response: no data")
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decoding response data: %w", err)
	}
	return nil
}
