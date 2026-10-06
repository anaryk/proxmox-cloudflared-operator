package agentproto

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Snapshot is the whole desired state of one node, never a change to an
// earlier one. Seq grows within an epoch, and only when the content changed.
type Snapshot struct {
	Protocol int       `json:"protocol"`
	Epoch    Epoch     `json:"epoch"`
	Seq      uint64    `json:"seq"`
	Node     string    `json:"node"`
	State    NodeState `json:"state"`
	Settings Settings  `json:"settings"`
	Nodes    []Node    `json:"nodes"` // for the denylist and the resolver's LocalNode
	Deny     DenyList  `json:"deny"`
	Tunnels  []Tunnel  `json:"tunnels"`
	Targets  []Target  `json:"targets"`
	// Networks are the node's legs, in the gateway's own form.
	Networks  json.RawMessage `json:"networks,omitempty"`
	Blocks    []Block         `json:"blocks"`
	Actions   []Action        `json:"actions"`
	Endpoints []string        `json:"endpoints"` // where the controller answers
}

// Node is a cluster node as the agent needs it.
type Node struct {
	Name   string       `json:"name"`
	Addr   netip.Addr   `json:"addr,omitzero"`
	Online bool         `json:"online"`
	Local  bool         `json:"local,omitempty"`
	Addrs  []netip.Addr `json:"addrs,omitempty"` // every address the node is configured with
}

// Settings are the settings of the install an agent works by.
type Settings struct {
	PollInterval     time.Duration  `json:"pollInterval"`
	IdentityMinimum  string         `json:"identityMinimum"`
	TrustStatic      bool           `json:"trustStatic,omitempty"`
	TrustedCIDRs     []netip.Prefix `json:"trustedCIDRs,omitempty"`
	ReverifyInterval time.Duration  `json:"reverifyInterval"`
}

// DenyList are the addresses no route may reach.
type DenyList struct {
	Denied []netip.Addr      `json:"denied"`
	Soft   map[string]string `json:"soft,omitempty"` // address -> why
}

// The kinds of tunnel.
const (
	TunnelShared = "shared" // every replica runs a connector of it
	TunnelPinned = "pinned" // one node's, for what only that node can reach
)

// Tunnel is a tunnel the node runs a connector of, or keeps the targets of.
type Tunnel struct {
	AccountID string `json:"accountId"`
	TunnelID  string `json:"tunnelId"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // shared or pinned
	Run       bool   `json:"run"`
	Token     Secret `json:"token,omitzero"`
	// Verified are the targets of the configuration last verified at
	// Cloudflare, which stay in the egress set while it still names them.
	Verified []netip.AddrPort `json:"verified"`
}

// Attestation is a node's word that a guest's card sits on a port of its
// bridge: its forwarding table learned the card's MAC there.
type Attestation struct {
	Node   string         `json:"node"`
	Guest  model.GuestRef `json:"guest"`
	NIC    int            `json:"nic"`
	MAC    string         `json:"mac"`
	Bridge string         `json:"bridge"`
	VLAN   int            `json:"vlan,omitempty"`
	Port   string         `json:"port"`
	Since  time.Time      `json:"since"`
}

// Target is a hostname whose target the node resolves or verifies.
type Target struct {
	Key     string       `json:"key"` // the hostname
	Tunnels []string     `json:"tunnels"`
	Route   model.Route  `json:"route"`
	Guest   *model.Guest `json:"guest,omitempty"`
	// Addr is the chosen address; it is zero when this node resolves the
	// target and none is chosen yet.
	Addr        netip.Addr `json:"addr,omitzero"`
	Service     netip.Addr `json:"service,omitzero"` // the address behind a leg
	Port        uint16     `json:"port"`
	HostingNode string     `json:"hostingNode,omitempty"`
	MACs        []string   `json:"macs,omitempty"`
	Required    string     `json:"required"` // observed, filtered or port
	Resolve     bool       `json:"resolve"`  // this node chooses the address
	// Attestation is the hosting node's, which this node may rely on.
	Attestation *Attestation `json:"attestation,omitempty"`
	// AttestedFresh is whether the attesting node's status was fresh at the
	// controller as of its last heartbeat.
	AttestedFresh bool `json:"attestedFresh,omitempty"`
	// Covered is whether every online node runs its forwarding-table watch,
	// without which no attestation stands.
	Covered   bool `json:"covered,omitempty"`
	AllowNode bool `json:"allowNode,omitempty"` // the route may reach an address of a node
}

// The reasons of a Block.
const (
	BlockConflict = "conflict" // a watch saw a bound MAC on another guest's port
	BlockAdmin    = "admin"
)

// Block is an address no node may let a connector reach.
type Block struct {
	Addr   netip.Addr `json:"addr"`
	Reason string     `json:"reason"`
	Until  time.Time  `json:"until,omitzero"`
}

// The kinds of Action.
const (
	ActionProbe            = "probe"
	ActionDiagnose         = "diagnose"
	ActionDoctor           = "doctor"
	ActionPVEWrite         = "pveWrite"
	ActionRestartConnector = "restartConnector"
	ActionUninstall        = "uninstall"
)

// Action is a request the agent carries out once, under the epoch that issued
// it, and answers in a later status.
type Action struct {
	ID   string          `json:"id"`
	Kind string          `json:"kind"`
	Args json.RawMessage `json:"args"`
}

// PVEWrite is the request of a pveWrite action: a change of one network card
// of a guest on the agent's node, which the agent makes with qm or pct.
type PVEWrite struct {
	Guest    model.GuestRef `json:"guest"`
	NIC      int            `json:"nic"`
	Bridge   string         `json:"bridge,omitempty"`
	Tag      int            `json:"tag,omitempty"`
	MAC      string         `json:"mac,omitempty"` // one of pco's, 06:50:43:4f:xx:xx
	Firewall *bool          `json:"firewall,omitempty"`
	LinkDown *bool          `json:"linkDown,omitempty"`
	Remove   bool           `json:"remove,omitempty"`
	Digest   string         `json:"digest"` // of the guest's config the change was made against
}

// wireSnapshot and wireTunnel are the snapshot as EncodeSnapshot writes it.
// An outer field hides the inner one of the same JSON name, so the tokens are
// written as their text and everything else as Snapshot says.
type wireSnapshot struct {
	Snapshot
	Tunnels []wireTunnel `json:"tunnels"`
}

type wireTunnel struct {
	Tunnel
	Token string `json:"token,omitempty"`
}

// EncodeSnapshot writes s as JSON with the text of its tokens, for the agent
// and for the agent's copy on disk. json.Marshal writes them "[redacted]".
// This is also the form to hash when a new token must count as a change.
func EncodeSnapshot(w io.Writer, s Snapshot) error {
	wire := wireSnapshot{Snapshot: s}
	if s.Tunnels != nil {
		wire.Tunnels = make([]wireTunnel, len(s.Tunnels))
		for i, t := range s.Tunnels {
			wire.Tunnels[i] = wireTunnel{Tunnel: t, Token: t.Token.Reveal()}
		}
	}
	if err := json.NewEncoder(w).Encode(wire); err != nil {
		return fmt.Errorf("encoding the snapshot: %w", err)
	}
	return nil
}
