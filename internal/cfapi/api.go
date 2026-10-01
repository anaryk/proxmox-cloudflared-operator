package cfapi

import (
	"context"
	"encoding/json"
	"errors"
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

	// Foreign is true when the configuration holds settings pco does not
	// manage and Ingress therefore does not show: a path on a rule, origin
	// options other than the ones of IngressRule, or settings outside the
	// ingress such as a top-level originRequest. Ingress equal to what pco wants
	// does not make the configuration equal to it while Foreign is set.
	// The read-only warp-routing key, which Cloudflare sets itself, is not counted.
	Foreign bool
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
	TTL        int // in seconds; 1 means automatic, the only TTL a proxied record has
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

// Matches reports whether r is a record the filter asks for. Type, name and
// comment prefix are compared without regard to case, as Cloudflare does.
func (f RecordFilter) Matches(r Record) bool {
	return (f.Type == "" || strings.EqualFold(r.Type, f.Type)) &&
		(f.Name == "" || strings.EqualFold(r.Name, f.Name)) &&
		(f.CommentPrefix == "" || strings.HasPrefix(strings.ToLower(r.Comment), strings.ToLower(f.CommentPrefix)))
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
	Tunnels(ctx context.Context, accountID, namePrefix string) ([]Tunnel, error) // not deleted, name starts with namePrefix
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

// ErrInvalidArgument marks a call refused before any request was made because
// an id or a name it was given cannot be used.
var ErrInvalidArgument = errors.New("invalid argument")

// CheckID checks an id that is about to become a segment of a request path:
// it must not be blank, "." or "..", nor contain a slash. kind names the id in
// the error, which matches ErrInvalidArgument. The fake in cffake applies the
// same check, so that both refuse the same calls.
func CheckID(kind, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidArgument, kind)
	}
	if checkSegment(id) != nil {
		return fmt.Errorf("%w: %s %q cannot be part of a request path", ErrInvalidArgument, kind, id)
	}
	return nil
}

// CheckName checks a value that goes into a query or a body, such as the name
// of a tunnel or a record: it must not be blank.
func CheckName(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: %s is empty", ErrInvalidArgument, kind)
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
