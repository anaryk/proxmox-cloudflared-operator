package cfapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// TokenStatus is what Cloudflare says about the token in use.
type TokenStatus struct {
	ID        string
	Status    string     // "active", "disabled", "expired"
	ExpiresOn *time.Time // nil when the token does not expire
}

// Account is a Cloudflare account the token can see.
type Account struct{ ID, Name string }

// Zone is a DNS zone the token can see.
type Zone struct {
	ID        string
	Name      string
	Status    string // "active", "pending", ...
	AccountID string
}

// Tunnel is a Cloudflare tunnel.
type Tunnel struct {
	ID        string
	Name      string
	Status    string // "inactive", "healthy", "degraded", "down"
	CreatedAt time.Time
}

// TunnelConfig is the ingress configuration stored for a tunnel.
type TunnelConfig struct {
	Version int
	Ingress []planner.IngressRule // empty when the tunnel has no config yet
}

// Connector is one running cloudflared of a tunnel.
type Connector struct {
	ID            string
	Version       string // cloudflared version
	ConfigVersion int
	Connections   int
}

// Record is a DNS record.
type Record struct {
	ID         string
	Type       string
	Name       string
	Content    string
	Proxied    bool
	Comment    string
	ModifiedOn time.Time
}

// RecordFilter narrows a listing of DNS records. Fields left empty do not
// filter.
type RecordFilter struct {
	Type          string
	Name          string // exact
	CommentPrefix string // comment.startswith
}

// API is everything pco asks of Cloudflare for one credential.
//
// Every call that does not return an error has seen the whole answer: a
// listing that could not be read to its end is an error, never a shorter list.
type API interface {
	VerifyToken(ctx context.Context) (TokenStatus, error)
	Accounts(ctx context.Context) ([]Account, error)
	Zones(ctx context.Context) ([]Zone, error)

	FindTunnel(ctx context.Context, accountID, name string) (Tunnel, bool, error)
	CreateTunnel(ctx context.Context, accountID, name string) (Tunnel, error)
	DeleteTunnel(ctx context.Context, accountID, tunnelID string) error
	TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error)
	TunnelConfig(ctx context.Context, accountID, tunnelID string) (TunnelConfig, error)
	PutTunnelConfig(ctx context.Context, accountID, tunnelID string, rules []planner.IngressRule) (int, error)
	Connectors(ctx context.Context, accountID, tunnelID string) ([]Connector, error)

	Records(ctx context.Context, zoneID string, f RecordFilter) ([]Record, error)
	CreateRecord(ctx context.Context, zoneID string, r Record) (Record, error)
	UpdateRecord(ctx context.Context, zoneID string, r Record) (Record, error) // by r.ID
	DeleteRecord(ctx context.Context, zoneID, recordID string) error
}

var _ API = (*Client)(nil)

// pathID checks an id that is about to become a path segment and escapes it.
// An id of "." or ".." would be resolved away when the URL is joined and send
// the call to another resource.
func pathID(what, id string) (string, error) {
	switch {
	case strings.TrimSpace(id) == "":
		return "", fmt.Errorf("%s is empty", what)
	case id == "." || id == "..":
		return "", fmt.Errorf("%s %q is not valid", what, id)
	}
	return url.PathEscape(id), nil
}

// required checks a value that goes into a query or a body, not a path.
func required(what, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is empty", what)
	}
	return nil
}

// listEach reads every page of the collection at path and calls each with
// every item decoded into a W, in order. It is list for a caller that wants
// typed items; each runs only after the whole listing was read.
func listEach[W any](ctx context.Context, c *Client, path string, query url.Values, each func(W) error) error {
	return c.list(ctx, path, query, func(raw json.RawMessage) error {
		var item W
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("decoding item: %w", err)
		}
		return each(item)
	})
}
