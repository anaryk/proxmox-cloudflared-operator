package store

import (
	"fmt"
	"time"
)

// The profiles an installation is made for.
const (
	ProfileHost      = "host"
	ProfileAppliance = "appliance"
)

// Install identifies this installation of pco.
type Install struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	// Profile is ProfileHost or ProfileAppliance. An install made before
	// profiles existed has none, which means the host profile.
	Profile string `json:"profile,omitempty"`
}

// ProfileName returns the profile of the install, never an empty one.
func (i Install) ProfileName() string {
	if i.Profile == "" {
		return ProfileHost
	}
	return i.Profile
}

// NodeEntry is a node that runs pco.
type NodeEntry struct {
	Name    string    `json:"name"`
	Version string    `json:"version"`
	Since   time.Time `json:"since"`
}

// Credential is a Cloudflare API token the daemon may use. Its token is a
// Secret, which neither prints nor encodes as the text.
type Credential struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Kind    string    `json:"kind"` // "scoped"
	Token   Secret    `json:"token"`
	AddedAt time.Time `json:"addedAt"`
}

// String describes the credential without its token.
func (c Credential) String() string {
	return fmt.Sprintf("credential{id: %s, label: %q, kind: %s, token: %s}", c.ID, c.Label, c.Kind, c.Token)
}

// GoString is String for the %#v verb.
func (c Credential) GoString() string { return c.String() }

// credentialFile is what is written for a Credential: the only place where its
// token is a plain string.
type credentialFile struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Kind    string    `json:"kind"`
	Token   string    `json:"token"`
	AddedAt time.Time `json:"addedAt"`
}

func (c Credential) file() credentialFile {
	return credentialFile{ID: c.ID, Label: c.Label, Kind: c.Kind, Token: c.Token.Reveal(), AddedAt: c.AddedAt}
}

func (f credentialFile) credential() Credential {
	return Credential{ID: f.ID, Label: f.Label, Kind: f.Kind, Token: NewSecret(f.Token), AddedAt: f.AddedAt}
}

// PVEToken is the Proxmox API token the daemon uses.
type PVEToken struct {
	TokenID string `json:"tokenId"`
	Secret  Secret `json:"secret"`
}

// String describes the token without its secret.
func (t PVEToken) String() string {
	return fmt.Sprintf("pve token{id: %s, secret: %s}", t.TokenID, t.Secret)
}

// GoString is String for the %#v verb.
func (t PVEToken) GoString() string { return t.String() }

// pveTokenFile is what is written for a PVEToken.
type pveTokenFile struct {
	TokenID string `json:"tokenId"`
	Secret  string `json:"secret"`
}

// Paths are the three roots of the store.
type Paths struct {
	Cluster string // state every node shares: /etc/pve/pco
	Private string // secrets every node shares: /etc/pve/priv/pco
	Local   string // state of this node: /var/lib/pco

	// MountCheck is a file that exists only while the cluster filesystem is
	// mounted. While it is missing, every operation on the cluster and private
	// roots fails with ErrNotMounted. An empty value switches the check off,
	// which is for tests: callers start from DefaultPaths and override the
	// roots, not build a Paths from nothing.
	MountCheck string
}

// DefaultPaths returns the roots on a Proxmox node.
func DefaultPaths() Paths {
	return Paths{
		Cluster:    "/etc/pve/pco",
		Private:    "/etc/pve/priv/pco",
		Local:      "/var/lib/pco",
		MountCheck: "/etc/pve/.version",
	}
}

// approval is the file of one approved owner.
type approval struct {
	Owner    string `json:"owner"`
	Identity string `json:"identity"`
}
