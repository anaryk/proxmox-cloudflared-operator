package store

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
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
	// Appliance is set for ProfileAppliance and for no other profile.
	Appliance *ApplianceInstall `json:"appliance,omitempty"`
	// Cluster is the Proxmox cluster the store was made in, on a host. An
	// install made before it was recorded has none.
	Cluster *ClusterIdentity `json:"cluster,omitempty"`
}

// ClusterIdentity is the Proxmox cluster a store belongs to: a store restored
// into another cluster, or a node image copied elsewhere, writes nothing
// before an admin says so.
type ClusterIdentity struct {
	// CASHA256 is the SHA-256 of /etc/pve/pve-root-ca.pem, in lower-case
	// hex: made once per cluster or standalone node and never rotated, it
	// is the identity.
	CASHA256 string `json:"caSHA256"`
	// Name is the cluster's name, for messages only. A store made on a
	// standalone node has none until it starts inside a cluster.
	Name string `json:"name,omitempty"`
}

// ProfileName returns the profile of the install, never an empty one.
func (i Install) ProfileName() string {
	if i.Profile == "" {
		return ProfileHost
	}
	return i.Profile
}

// NodeEntry is a node that runs pco. It is a frozen object, as leader.json
// is: every field after the first three was added for a cluster, is left out
// while it is empty, and is read by the previous release as a key it does
// not know.
type NodeEntry struct {
	Name    string    `json:"name"`
	Version string    `json:"version"`
	Since   time.Time `json:"since"`

	// Index is the node's number for its leg MACs, 0 to 254, the same for a
	// name for as long as the install lives; 255 is the managed leg's. The
	// controller gives it at routing admission; an entry has none before.
	Index *int `json:"index,omitempty"`
	// Kind is the kind of the node's agent: "host" or "appliance", or
	// "controller" for the controller container of an appliance.
	Kind string `json:"kind,omitempty"`
	// VMID is the container of an appliance agent.
	VMID int `json:"vmid,omitempty"`
	// AgentAddress is where the node's agent listens and is dialled from, on
	// a host.
	AgentAddress netip.Addr `json:"agentAddress,omitzero"`
	// Protocol is the range of versions of the agent protocol the node
	// speaks, and Schema the store schema its build reads.
	Protocol ProtocolRange `json:"protocol,omitzero"`
	Schema   int           `json:"schema,omitempty"`
	// CertSerial and CertSHA256 name the agent's client certificate.
	CertSerial string `json:"certSerial,omitempty"`
	CertSHA256 string `json:"certSHA256,omitempty"`
	// State is the node's state, as of StateSince: joining, active,
	// draining, unknown, down, contested or removed.
	State      string    `json:"state,omitempty"`
	StateSince time.Time `json:"stateSince,omitzero"`
	// ControllerEligible says whether the node may hold the lease; unset is
	// not yet decided.
	ControllerEligible *bool `json:"controllerEligible,omitempty"`
	// ContestedUses are the uses of the node's join token that made it
	// contested.
	ContestedUses []JoinUse `json:"contestedUses,omitempty"`
}

// ProtocolRange is a range of versions of the agent protocol, both ends in.
type ProtocolRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

const (
	// maxNodeIndex is the highest index of a node: the next is the managed
	// leg's.
	maxNodeIndex = 254

	// stateRemoved is the state of a node that was forgotten.
	stateRemoved = "removed"
)

// check refuses an entry with no name, or with a field out of its range.
func (n NodeEntry) check() error {
	switch {
	case n.Name == "":
		return errors.New("node name is empty")
	case n.Index != nil && (*n.Index < 0 || *n.Index > maxNodeIndex):
		return fmt.Errorf("node %s: index %d is not 0 to %d", n.Name, *n.Index, maxNodeIndex)
	case n.VMID < 0:
		return fmt.Errorf("node %s: VMID %d is negative", n.Name, n.VMID)
	case n.Protocol.Min < 0 || n.Protocol.Min > n.Protocol.Max:
		return fmt.Errorf("node %s: protocol %d to %d is no range", n.Name, n.Protocol.Min, n.Protocol.Max)
	}
	return nil
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

	// MountCheck is a file that exists only while the filesystem of the
	// cluster and private roots is mounted: pmxcfs on a node, the state volume
	// in the appliance. While it is missing, every operation on those roots
	// fails with ErrNotMounted. An empty value switches the check off, which
	// is for tests: callers start from PathsFor and override the roots, not
	// build a Paths from nothing.
	MountCheck string

	// Durable makes every write of the shared roots fsync its directory
	// after the rename. Off on pmxcfs, which refuses it; on for a volume.
	Durable bool
}

// NodeLock is the file a daemon holds locked for as long as it runs on the
// node, below the local root.
func (p Paths) NodeLock() string { return filepath.Join(p.Local, "daemon.lock") }

// DefaultPaths returns the roots on a Proxmox node.
func DefaultPaths() Paths {
	return Paths{
		Cluster:    "/etc/pve/pco",
		Private:    "/etc/pve/priv/pco",
		Local:      "/var/lib/pco",
		MountCheck: "/etc/pve/.version",
	}
}

// Approval is what an admin approved of a guest: its identity, and at the
// observed level the MACs that may answer for its routes and the soft-denied
// addresses it may be published at.
type Approval struct {
	Owner     string       `json:"owner"`
	Identity  string       `json:"identity"`
	MACs      []string     `json:"macs,omitempty"` // normalised, sorted
	Addresses []netip.Addr `json:"addresses,omitempty"`
}
