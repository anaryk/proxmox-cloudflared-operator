package agentproto

import (
	"encoding/json"
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Status is what an agent reports to the controller, every 10 seconds and
// soon after a change. The controller answers it with a Heartbeat.
type Status struct {
	Protocol       Range     `json:"protocol"`
	Build          string    `json:"build"`
	Node           string    `json:"node"`
	Kind           pki.Kind  `json:"kind"`           // host or appliance, as its certificate says
	VMID           int       `json:"vmid,omitempty"` // the appliance container's
	AgentStartedAt time.Time `json:"agentStartedAt"`
	// WatchRunningSince is zero while the forwarding-table watch is not
	// running.
	WatchRunningSince time.Time          `json:"watchRunningSince,omitzero"`
	EpochSeen         Epoch              `json:"epochSeen"`
	SeqApplied        uint64             `json:"seqApplied"`
	Targets           []TargetReport     `json:"targets"`
	Attestations      []Attestation      `json:"attestations,omitempty"`
	Observations      []Observation      `json:"observations,omitempty"`
	Connectors        []connector.Status `json:"connectors"`
	Egress            string             `json:"egress"` // on, off, not loaded or changed
	LocalBlocks       []netip.Addr       `json:"localBlocks,omitempty"`
	ServiceAddrs      []netip.Addr       `json:"serviceAddrs,omitempty"` // what its gateway maps
	Addrs             []netip.Addr       `json:"addrs,omitempty"`        // the node's runtime addresses (host)
	Clock             time.Time          `json:"clock"`
	// Events are the agent's events since the cursor the controller last
	// acknowledged, at most MaxEventBatch of them; EventCursor is the cursor
	// of the last one.
	Events      []json.RawMessage `json:"events,omitempty"`
	EventCursor uint64            `json:"eventCursor"`
	// Resend is, while the agent sends the events of the last 24 hours again
	// to a new epoch, how many that resend holds in all by its own count; zero
	// otherwise. DecodeStatus refuses a count over MaxResend, but an agent
	// could say less than it sends: the controller counts what it receives
	// from each node and holds it to the bound itself.
	Resend   int            `json:"resend,omitempty"`
	Actions  []ActionResult `json:"actions,omitempty"`
	Problems []string       `json:"problems,omitempty"`
}

// TargetReport is how a target of the snapshot fares on the agent's node.
type TargetReport struct {
	Key      string     `json:"key"`
	Addr     netip.Addr `json:"addr,omitzero"`
	Level    string     `json:"level,omitempty"`
	Attested bool       `json:"attested,omitempty"`
	// AttestedBy is the node whose attestation the level stands on, when
	// Attested.
	AttestedBy  string    `json:"attestedBy,omitempty"`
	OK          bool      `json:"ok"`
	Reason      string    `json:"reason,omitempty"`
	VerifiedAt  time.Time `json:"verifiedAt,omitzero"`
	InEgressSet bool      `json:"inEgressSet"`
	// Result is the resolving node's resolve.Result, in its own form.
	Result json.RawMessage `json:"result,omitempty"`
}

// The kinds of Observation.
const (
	ObservationMoved    = "moved"    // a bound MAC left the guest's own port
	ObservationConflict = "conflict" // a bound MAC showed up on another guest's port
)

// Observation is what a host agent's forwarding-table watch saw of a MAC the
// desired state binds to a guest.
type Observation struct {
	Kind   string          `json:"kind"`
	MAC    string          `json:"mac"`
	Bridge string          `json:"bridge"`
	VLAN   int             `json:"vlan,omitempty"`
	From   string          `json:"from,omitempty"`
	Port   string          `json:"port"`
	Owner  *model.GuestRef `json:"owner,omitempty"` // the guest of Port, for a conflict
	At     time.Time       `json:"at"`
}

// ActionResult answers the Action of the same id.
type ActionResult struct {
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// AgentView is what pco agent status shows of the node's own agent.
type AgentView struct {
	Node       string   `json:"node"`
	Kind       pki.Kind `json:"kind"`
	Controller string   `json:"controller,omitempty"` // the node or address it follows
	// Remote says that the controller it follows runs on another node and
	// is reached over the network, not in this process.
	Remote          bool          `json:"remote,omitempty"`
	SilentFor       time.Duration `json:"silentFor"`
	Epoch           Epoch         `json:"epoch"`
	Seq             uint64        `json:"seq"`
	SeqAge          time.Duration `json:"seqAge"`
	Connectors      int           `json:"connectors"`
	EgressTargets   int           `json:"egressTargets"`
	StoodDown       string        `json:"stoodDown,omitempty"`
	CertFingerprint string        `json:"certFingerprint,omitempty"`
	CertExpires     time.Time     `json:"certExpires,omitzero"`
	// Heard says that a heartbeat came; ReadyReplicas and HeartbeatAge are
	// of the last one.
	Heard bool `json:"heard"`
	// ReadyReplicas are, by tunnel id, the ready replicas the last heartbeat
	// counted.
	ReadyReplicas map[string]int `json:"readyReplicas,omitempty"`
	HeartbeatAge  time.Duration  `json:"heartbeatAge"`
	Problems      []string       `json:"problems"`
}
