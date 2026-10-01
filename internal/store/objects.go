package store

import (
	"fmt"
	"time"
)

// Install identifies this installation of pco.
type Install struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
}

// NodeEntry is a node that runs pco.
type NodeEntry struct {
	Name    string    `json:"name"`
	Version string    `json:"version"`
	Since   time.Time `json:"since"`
}

// Credential is a Cloudflare API token the daemon may use.
//
// The JSON form keeps the token, as the store needs it; only the store should
// encode a Credential. Formatting one with fmt leaves the token out.
type Credential struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Kind    string    `json:"kind"` // "scoped"
	Token   string    `json:"token"`
	AddedAt time.Time `json:"addedAt"`
}

// String describes the credential without its token.
func (c Credential) String() string {
	return fmt.Sprintf("credential{id: %s, label: %q, kind: %s, token: <redacted>}", c.ID, c.Label, c.Kind)
}

// GoString is String for the %#v verb.
func (c Credential) GoString() string { return c.String() }

// PVEToken is the Proxmox API token the daemon uses.
type PVEToken struct {
	TokenID string `json:"tokenId"`
	Secret  string `json:"secret"`
}

// String describes the token without its secret.
func (t PVEToken) String() string {
	return fmt.Sprintf("pve token{id: %s, secret: <redacted>}", t.TokenID)
}

// GoString is String for the %#v verb.
func (t PVEToken) GoString() string { return t.String() }

// Paths are the three roots of the store.
type Paths struct {
	Cluster string // state every node shares: /etc/pve/pco
	Private string // secrets every node shares: /etc/pve/priv/pco
	Local   string // state of this node: /var/lib/pco
}

// DefaultPaths returns the roots on a Proxmox node.
func DefaultPaths() Paths {
	return Paths{Cluster: "/etc/pve/pco", Private: "/etc/pve/priv/pco", Local: "/var/lib/pco"}
}

// approval is the file of one approved owner.
type approval struct {
	Owner    string `json:"owner"`
	Identity string `json:"identity"`
}
