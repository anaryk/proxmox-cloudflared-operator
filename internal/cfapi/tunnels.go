package cfapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

type wireTunnel struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at"`
}

func (w wireTunnel) tunnel() Tunnel {
	return Tunnel{ID: w.ID, Name: w.Name, Status: w.Status, CreatedAt: w.CreatedAt}
}

// deleted reports whether the tunnel carries a time of deletion. The zero time
// is what a field that was never set looks like, not a deletion.
func (w wireTunnel) deleted() bool {
	return w.DeletedAt != nil && !w.DeletedAt.IsZero()
}

// wireRule is an ingress rule as the API spells it: the origin options live
// in a nested object that is left out when none is set.
type wireRule struct {
	Hostname      string             `json:"hostname,omitempty"` // none for the catch-all
	Service       string             `json:"service"`
	OriginRequest *wireOriginRequest `json:"originRequest,omitempty"`
}

type wireOriginRequest struct {
	OriginServerName string `json:"originServerName,omitempty"`
	MatchSNIToHost   bool   `json:"matchSNItoHost,omitempty"`
	NoTLSVerify      bool   `json:"noTLSVerify,omitempty"`
	HTTPHostHeader   string `json:"httpHostHeader,omitempty"`
}

func toWireRule(r planner.IngressRule) wireRule {
	w := wireRule{Hostname: r.Hostname, Service: r.Service}
	opts := wireOriginRequest{
		OriginServerName: r.OriginServerName,
		MatchSNIToHost:   r.MatchSNIToHost,
		NoTLSVerify:      r.NoTLSVerify,
		HTTPHostHeader:   r.HTTPHostHeader,
	}
	if opts != (wireOriginRequest{}) {
		w.OriginRequest = &opts
	}
	return w
}

// rule maps back to the flat form. Options of the API that pco does not set
// are not carried over.
func (w wireRule) rule() planner.IngressRule {
	r := planner.IngressRule{Hostname: w.Hostname, Service: w.Service}
	if o := w.OriginRequest; o != nil {
		r.OriginServerName = o.OriginServerName
		r.MatchSNIToHost = o.MatchSNIToHost
		r.NoTLSVerify = o.NoTLSVerify
		r.HTTPHostHeader = o.HTTPHostHeader
	}
	return r
}

type wireIngress struct {
	Ingress []wireRule `json:"ingress"`
}

// wireConfig is the answer to reading or writing a tunnel configuration.
// Config is null for a tunnel that has none yet.
type wireConfig struct {
	Version int          `json:"version"`
	Config  *wireIngress `json:"config"`
}

func tunnelsPath(accountID string) (string, error) {
	acct, err := pathID("account id", accountID)
	if err != nil {
		return "", err
	}
	return "/accounts/" + acct + "/cfd_tunnel", nil
}

func tunnelPath(accountID, tunnelID string) (string, error) {
	base, err := tunnelsPath(accountID)
	if err != nil {
		return "", err
	}
	id, err := pathID("tunnel id", tunnelID)
	if err != nil {
		return "", err
	}
	return base + "/" + id, nil
}

// FindTunnel returns the tunnel of that exact name that is not deleted. Two of
// them are an error: pco cannot tell which one it owns.
func (c *Client) FindTunnel(ctx context.Context, accountID, name string) (Tunnel, bool, error) {
	path, err := tunnelsPath(accountID)
	if err != nil {
		return Tunnel{}, false, fmt.Errorf("finding tunnel: %w", err)
	}
	if err := required("tunnel name", name); err != nil {
		return Tunnel{}, false, fmt.Errorf("finding tunnel: %w", err)
	}

	// The name filter of the API matches more than the exact name and a server
	// may ignore a filter, so the answer is checked here too.
	var matches []Tunnel
	query := url.Values{"name": {name}, "is_deleted": {"false"}}
	err = listEach(ctx, c, path, query, func(w wireTunnel) error {
		if w.Name != name || w.deleted() {
			return nil
		}
		if w.ID == "" {
			return fmt.Errorf("%w: tunnel without an id", errUnexpected)
		}
		matches = append(matches, w.tunnel())
		return nil
	})
	if err != nil {
		return Tunnel{}, false, fmt.Errorf("finding tunnel %q: %w", name, err)
	}
	switch len(matches) {
	case 0:
		return Tunnel{}, false, nil
	case 1:
		return matches[0], true, nil
	}
	return Tunnel{}, false, fmt.Errorf("finding tunnel %q: %d tunnels have that name", name, len(matches))
}

// CreateTunnel creates a tunnel whose configuration is kept by Cloudflare. A
// name that is taken is an error for which IsConflict is true.
func (c *Client) CreateTunnel(ctx context.Context, accountID, name string) (Tunnel, error) {
	path, err := tunnelsPath(accountID)
	if err != nil {
		return Tunnel{}, fmt.Errorf("creating tunnel: %w", err)
	}
	if err := required("tunnel name", name); err != nil {
		return Tunnel{}, fmt.Errorf("creating tunnel: %w", err)
	}

	body := map[string]string{"name": name, "config_src": "cloudflare"}
	var got wireTunnel
	if err := c.do(ctx, http.MethodPost, path, nil, body, &got); err != nil {
		return Tunnel{}, fmt.Errorf("creating tunnel %q: %w", name, err)
	}
	if got.ID == "" {
		return Tunnel{}, fmt.Errorf("creating tunnel %q: %w: no tunnel id", name, errUnexpected)
	}
	return got.tunnel(), nil
}

// DeleteTunnel deletes a tunnel.
func (c *Client) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	path, err := tunnelPath(accountID, tunnelID)
	if err != nil {
		return fmt.Errorf("deleting tunnel: %w", err)
	}
	if err := c.do(ctx, http.MethodDelete, path, nil, nil, nil); err != nil {
		return fmt.Errorf("deleting tunnel %s: %w", tunnelID, err)
	}
	return nil
}

// TunnelToken returns the token a cloudflared needs to run the tunnel.
func (c *Client) TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	path, err := tunnelPath(accountID, tunnelID)
	if err != nil {
		return "", fmt.Errorf("reading run token: %w", err)
	}
	var token string
	if err := c.do(ctx, http.MethodGet, path+"/token", nil, nil, &token); err != nil {
		return "", fmt.Errorf("reading run token of tunnel %s: %w", tunnelID, err)
	}
	if token == "" {
		return "", fmt.Errorf("reading run token of tunnel %s: %w: token is empty", tunnelID, errUnexpected)
	}
	return token, nil
}

// TunnelConfig returns the ingress configuration of a tunnel. A tunnel that
// has none yet has no rules and the version Cloudflare reports for it.
func (c *Client) TunnelConfig(ctx context.Context, accountID, tunnelID string) (TunnelConfig, error) {
	path, err := tunnelPath(accountID, tunnelID)
	if err != nil {
		return TunnelConfig{}, fmt.Errorf("reading tunnel configuration: %w", err)
	}
	var got wireConfig
	if err := c.do(ctx, http.MethodGet, path+"/configurations", nil, nil, &got); err != nil {
		return TunnelConfig{}, fmt.Errorf("reading configuration of tunnel %s: %w", tunnelID, err)
	}
	return got.tunnelConfig(), nil
}

func (w wireConfig) tunnelConfig() TunnelConfig {
	cfg := TunnelConfig{Version: w.Version}
	if w.Config == nil {
		return cfg
	}
	for _, rule := range w.Config.Ingress {
		cfg.Ingress = append(cfg.Ingress, rule.rule())
	}
	return cfg
}

// PutTunnelConfig replaces the ingress configuration of a tunnel and returns
// the version Cloudflare gave it.
func (c *Client) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []planner.IngressRule) (int, error) {
	path, err := tunnelPath(accountID, tunnelID)
	if err != nil {
		return 0, fmt.Errorf("writing tunnel configuration: %w", err)
	}
	// A list, never null, however few rules there are.
	wires := make([]wireRule, len(rules))
	for i, r := range rules {
		wires[i] = toWireRule(r)
	}
	body := struct {
		Config wireIngress `json:"config"`
	}{wireIngress{Ingress: wires}}

	var got wireConfig
	if err := c.do(ctx, http.MethodPut, path+"/configurations", nil, body, &got); err != nil {
		return 0, fmt.Errorf("writing configuration of tunnel %s: %w", tunnelID, err)
	}
	return got.Version, nil
}

// Connectors lists the cloudflared instances connected to a tunnel.
func (c *Client) Connectors(ctx context.Context, accountID, tunnelID string) ([]Connector, error) {
	path, err := tunnelPath(accountID, tunnelID)
	if err != nil {
		return nil, fmt.Errorf("listing connectors: %w", err)
	}
	// The endpoint does not page.
	var got []struct {
		ID            string     `json:"id"`
		Version       string     `json:"version"`
		ConfigVersion int        `json:"config_version"`
		Conns         []struct{} `json:"conns"`
	}
	if err := c.do(ctx, http.MethodGet, path+"/connections", nil, nil, &got); err != nil {
		return nil, fmt.Errorf("listing connectors of tunnel %s: %w", tunnelID, err)
	}
	var out []Connector
	for _, w := range got {
		out = append(out, Connector{
			ID:            w.ID,
			Version:       w.Version,
			ConfigVersion: w.ConfigVersion,
			Connections:   len(w.Conns),
		})
	}
	return out, nil
}
