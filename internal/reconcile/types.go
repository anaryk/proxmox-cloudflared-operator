// Package reconcile brings what Cloudflare holds in line with a plan: the
// tunnel of each account with its ingress, and the DNS records that point at
// it. A writer that finds the mark of a newer writer in a tunnel's
// configuration stops instead of overwriting it.
package reconcile

import "github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"

// Mode says whether a run may change anything.
type Mode int

const (
	Observe Mode = iota // compute actions, change nothing
	Enforce
)

// ActionKind is the change an action makes at Cloudflare.
type ActionKind string

const (
	CreateTunnel ActionKind = "create-tunnel"
	DeleteTunnel ActionKind = "delete-tunnel" // only ever a probe tunnel the credential check left behind
	PutConfig    ActionKind = "put-config"
	CreateRecord ActionKind = "create-record"
	UpdateRecord ActionKind = "update-record"
	DeleteRecord ActionKind = "delete-record"
)

// Action is a change a run made or would have made.
type Action struct {
	Kind        ActionKind `json:"kind"`
	Credential  string     `json:"credential"`
	Target      string     `json:"target"` // tunnel name or record name
	Detail      string     `json:"detail"`
	Destructive bool       `json:"destructive"`
	Applied     bool       `json:"applied"`
	Held        string     `json:"held,omitempty"` // why it was not applied (observe mode, guard, error)
}

// TunnelState is what a run knows about the tunnel of one account.
type TunnelState struct {
	AccountID    string `json:"accountId"`
	CredentialID string `json:"credentialId"`
	Name         string `json:"name"`
	ID           string `json:"id,omitempty"` // empty while the tunnel is not known to exist

	// Version is the version of the configuration. Without a write it is the
	// version read. After a write it is the version read back when the
	// read-back equals the plan, and otherwise the version the write
	// returned.
	Version int  `json:"version"`
	Exists  bool `json:"exists"`

	// Verified is true when the configuration at Version was read and equals
	// the plan: nothing needed writing, or the read-back after a write
	// matched. It is false when a write was held or failed, or when its
	// read-back failed or differed.
	Verified bool `json:"verified"`

	// Unknown is true when the run could not find out whether the tunnel
	// exists; Exists is then false. Nothing may be pruned for such a tunnel.
	Unknown bool `json:"unknown"`
}

// WriterVerdict is the outcome of the sentinel decision table.
type WriterVerdict int

const (
	WriterProceed WriterVerdict = iota
	WriterStale                 // a newer generation of this install wrote: stop and re-acquire
	WriterForeign               // another installation uses this id: stop and alert
)

// Clients are the Cloudflare clients a run may use.
type Clients map[string]cfapi.API // by credential id
