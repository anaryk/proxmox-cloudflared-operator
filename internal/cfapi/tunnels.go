package cfapi

import (
	"context"
	"encoding/json"
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

func tunnelsPath(accountID string) (string, error) {
	if err := CheckID("account id", accountID); err != nil {
		return "", err
	}
	return joinPath("accounts", accountID, "cfd_tunnel")
}

// tunnelPath is the path of a tunnel, or of what lies below it.
func tunnelPath(accountID, tunnelID string, below ...string) (string, error) {
	if err := CheckID("account id", accountID); err != nil {
		return "", err
	}
	if err := CheckID("tunnel id", tunnelID); err != nil {
		return "", err
	}
	return joinPath(append([]string{"accounts", accountID, "cfd_tunnel", tunnelID}, below...)...)
}

// FindTunnel returns the tunnel of that exact name that is not deleted. Two of
// them are an error: pco cannot tell which one it owns.
func (c *Client) FindTunnel(ctx context.Context, accountID, name string) (Tunnel, bool, error) {
	path, err := tunnelsPath(accountID)
	if err != nil {
		return Tunnel{}, false, fmt.Errorf("finding tunnel: %w", err)
	}
	if err := CheckName("tunnel name", name); err != nil {
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
	if err := CheckName("tunnel name", name); err != nil {
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
	path, err := tunnelPath(accountID, tunnelID, "token")
	if err != nil {
		return "", fmt.Errorf("reading run token: %w", err)
	}
	var token string
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &token); err != nil {
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
	path, err := tunnelPath(accountID, tunnelID, "configurations")
	if err != nil {
		return TunnelConfig{}, fmt.Errorf("reading tunnel configuration: %w", err)
	}
	var got wireConfig
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &got); err != nil {
		return TunnelConfig{}, fmt.Errorf("reading configuration of tunnel %s: %w", tunnelID, err)
	}
	cfg, err := got.tunnelConfig()
	if err != nil {
		return TunnelConfig{}, fmt.Errorf("reading configuration of tunnel %s: %w", tunnelID, err)
	}
	return cfg, nil
}

// PutTunnelConfig replaces the ingress configuration of a tunnel and returns
// the version Cloudflare gave it. Without rules there is nothing to write:
// Cloudflare wants the ingress to end in a rule that matches everything.
func (c *Client) PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []planner.IngressRule) (int, error) {
	path, err := tunnelPath(accountID, tunnelID, "configurations")
	if err != nil {
		return 0, fmt.Errorf("writing tunnel configuration: %w", err)
	}
	if len(rules) == 0 {
		return 0, fmt.Errorf("writing tunnel configuration: %w: no ingress rules", ErrInvalidArgument)
	}
	wires := make([]wireRule, len(rules))
	for i, r := range rules {
		wires[i] = toWireRule(r)
	}
	body := struct {
		Config wireIngress `json:"config"`
	}{wireIngress{Ingress: wires}}

	var got wireConfig
	if err := c.do(ctx, http.MethodPut, path, nil, body, &got); err != nil {
		return 0, fmt.Errorf("writing configuration of tunnel %s: %w", tunnelID, err)
	}
	version, err := got.version()
	if err != nil {
		return 0, fmt.Errorf("writing configuration of tunnel %s: %w", tunnelID, err)
	}
	return version, nil
}

// Connectors lists the cloudflared instances connected to a tunnel. A result of
// null is none: the listing only feeds a status display.
func (c *Client) Connectors(ctx context.Context, accountID, tunnelID string) ([]Connector, error) {
	path, err := tunnelPath(accountID, tunnelID, "connections")
	if err != nil {
		return nil, fmt.Errorf("listing connectors: %w", err)
	}
	// The endpoint does not page.
	env, err := c.roundTrip(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("listing connectors of tunnel %s: %w", tunnelID, err)
	}
	if isNull(env.Result) {
		return nil, nil
	}
	var got []struct {
		ID            string     `json:"id"`
		Version       string     `json:"version"`
		ConfigVersion int        `json:"config_version"`
		Conns         []struct{} `json:"conns"`
	}
	if err := json.Unmarshal(env.Result, &got); err != nil {
		return nil, fmt.Errorf("listing connectors of tunnel %s: decoding result: %w", tunnelID, err)
	}
	var out []Connector
	for _, w := range got {
		if w.ID == "" {
			return nil, fmt.Errorf("listing connectors of tunnel %s: %w: connector without an id", tunnelID, errUnexpected)
		}
		out = append(out, Connector{
			ID:            w.ID,
			Version:       w.Version,
			ConfigVersion: w.ConfigVersion,
			Connections:   len(w.Conns),
		})
	}
	return out, nil
}
