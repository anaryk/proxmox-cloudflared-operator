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
	PutConfig    ActionKind = "put-config"
	CreateRecord ActionKind = "create-record"
	UpdateRecord ActionKind = "update-record"
	DeleteRecord ActionKind = "delete-record"
)

// Action is a change a run made or would have made.
type Action struct {
	Kind        ActionKind
	Credential  string
	Target      string // tunnel name or record name
	Detail      string
	Destructive bool
	Applied     bool
	Held        string // why it was not applied (observe mode, guard, error)
}

// TunnelState is what a run knows about the tunnel of one account.
type TunnelState struct {
	AccountID    string
	CredentialID string
	Name         string
	ID           string
	Version      int
	Exists       bool
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
