// Package wire is the JSON of the web process's own answers and notices, those
// that do not come from the daemon. The interface's TypeScript types are
// generated from these, as from the daemon's.
package wire

import "time"

// Session is the answer of GET /api/session for a signed-in user.
type Session struct {
	User          string    `json:"user"`
	Method        string    `json:"method"` // "ticket", "token", "password" (appliance)
	Role          string    `json:"role"`   // "admin", "reader"
	CSRF          string    `json:"csrf"`
	IdleExpiresAt time.Time `json:"idleExpiresAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Profile       string    `json:"profile"`
	Node          string    `json:"node"`
	NodeZone      string    `json:"nodeZone"` // the node's time zone, e.g. "Europe/Prague"
	Version       string    `json:"version"`
}

// Unauthenticated is the answer to a request without a session: how the user
// may sign in.
type Unauthenticated struct {
	Code    string   `json:"code"`             // "unauthenticated"
	Methods []string `json:"methods"`          // "ticket", "token", "password"
	Ticket  bool     `json:"ticket"`           // a ticket cookie is present
	Realms  []string `json:"realms,omitempty"` // appliance: from /access/domains
}

// Error is the daemon's ErrorBody shape, with the web's own codes.
type Error struct {
	Error      string `json:"error"`
	Code       string `json:"code"`
	Field      string `json:"field,omitempty"`
	Missing    string `json:"missing,omitempty"`    // "Sys.Audit"
	RetryAfter int    `json:"retryAfter,omitempty"` // seconds, with 429
	// Kinds are the second factors Proxmox VE takes for the account, with
	// second_factor and second_factor_key: "totp", "recovery", "webauthn".
	Kinds []string `json:"kinds,omitempty"`
}

// DoctorCounts is what a reader gets of the doctor: how many checks ended in
// each verdict, and when they ran.
type DoctorCounts struct {
	OK   int       `json:"ok"`
	Warn int       `json:"warn"`
	Fail int       `json:"fail"`
	At   time.Time `json:"at"`
}

// Upstream says whether the web process reaches the daemon, and since when.
type Upstream struct {
	Up    bool      `json:"up"`
	Since time.Time `json:"since"`
}

// The codes of Error and Unauthenticated.
const (
	CodeUnauthenticated      = "unauthenticated"
	CodeTicketInvalid        = "ticket_invalid"
	CodeProxmoxUnreachable   = "proxmox_unreachable"
	CodeDaemonUnreachable    = "daemon_unreachable"
	CodeRateLimited          = "rate_limited"
	CodeForbidden            = "forbidden"
	CodeInvalid              = "invalid"
	CodeTooLarge             = "too_large"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeSecondFactor         = "second_factor"     // appliance: Proxmox asks for a code
	CodeSecondFactorKey      = "second_factor_key" // appliance: a security key, which cannot work here
)
