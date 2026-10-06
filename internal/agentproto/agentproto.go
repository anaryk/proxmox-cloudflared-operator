// Package agentproto is what the controller of an install and the agents on
// its nodes say to each other: the snapshot of desired state the controller
// renders for a node, the status an agent reports, the heartbeat that answers
// it, and the refusals.
//
// The golden files of testdata/v1 pin protocol 1. Within a version fields are
// only added, and optional; readers ignore fields they do not know. Taking a
// field away or changing its meaning needs a new version.
package agentproto

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// ProtocolMin and ProtocolMax are the versions this build speaks. A build
	// speaks its own version and the one before, so that a cluster can be
	// upgraded one node at a time.
	ProtocolMin = 1
	ProtocolMax = 1

	// MaxGeneration is the highest generation an agent accepts. A forged
	// epoch above it cannot wedge the agents, and every generation up to it
	// is exact in a JSON number read as a float.
	MaxGeneration = 1 << 53

	// MaxStatusBytes, MaxEventBatch and MaxResend bound what one agent can
	// make the controller hold: a status body, the events in one status, and
	// the events of the last 24 hours an agent sends again to a new epoch.
	MaxStatusBytes = 1 << 20
	MaxEventBatch  = 500
	MaxResend      = 10000
)

// Epoch is the tenure of one controller. The generation grows with each new
// controller; the nonce tells apart two that drew the same generation.
type Epoch struct {
	InstallID  string `json:"installId"`
	Generation int    `json:"generation"`
	Nonce      string `json:"nonce"`
}

// Order compares e with o by generation. same is true when generation and
// nonce are equal. The install is not compared.
func (e Epoch) Order(o Epoch) (c int, same bool) {
	c = cmp.Compare(e.Generation, o.Generation)
	return c, c == 0 && e.Nonce == o.Nonce
}

// String returns "<generation>.<nonce>", the form of the epoch query
// parameter. The install is left out: the certificates of both ends name it.
func (e Epoch) String() string {
	return strconv.Itoa(e.Generation) + "." + e.Nonce
}

// ParseEpoch reads what String writes. The generation is a decimal from 1 to
// MaxGeneration without a sign or a leading zero, the nonce lower-case letters
// and digits, as setup and the lease draw them.
func ParseEpoch(s string) (Epoch, error) {
	g, nonce, ok := strings.Cut(s, ".")
	if !ok {
		return Epoch{}, fmt.Errorf("epoch %q: want <generation>.<nonce>", s)
	}
	n, err := strconv.ParseUint(g, 10, 64)
	switch {
	case errors.Is(err, strconv.ErrRange) || err == nil && (n < 1 || n > MaxGeneration):
		return Epoch{}, fmt.Errorf("epoch %q: the generation is not between 1 and %d", s, MaxGeneration)
	case err != nil || strconv.FormatUint(n, 10) != g:
		return Epoch{}, fmt.Errorf("epoch %q: the generation is not a decimal number", s)
	case !lowerAlnum(nonce):
		return Epoch{}, fmt.Errorf("epoch %q: the nonce is not lower-case letters and digits", s)
	}
	return Epoch{Generation: int(n), Nonce: nonce}, nil
}

func lowerAlnum(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'z')
	}) < 0
}

// NodeState is the state of a node in the registry of the install.
type NodeState string

const (
	Joining   NodeState = "joining"
	Active    NodeState = "active"
	Draining  NodeState = "draining"
	Unknown   NodeState = "unknown"
	Down      NodeState = "down"
	Contested NodeState = "contested"
	Removed   NodeState = "removed"
)

// The kinds of agent.
const (
	KindHost      = "host"      // pco.service on a node of the cluster
	KindAppliance = "appliance" // a container of the appliance
)

// Range is the protocol versions one end speaks, both included.
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

func (r Range) valid() bool { return r.Min >= 1 && r.Min <= r.Max }

// Negotiate returns the highest version both ends speak. ok is false when they
// share none, or one of them says something that is no range.
func Negotiate(agent, controller Range) (version int, ok bool) {
	if !agent.valid() || !controller.valid() {
		return 0, false
	}
	low, high := max(agent.Min, controller.Min), min(agent.Max, controller.Max)
	if low > high {
		return 0, false
	}
	return high, true
}

// The codes of a Refusal.
const (
	RefusalRemoved      = "removed"       // the node was forgotten
	RefusalContested    = "contested"     // two nodes claim the name or the certificate
	RefusalNotEnrolled  = "not-enrolled"  // the registry does not know the certificate
	RefusalOtherInstall = "other-install" // the certificate is of another install
	RefusalOutOfRange   = "out-of-range"  // the two ends share no protocol version
)

// Refusal is the controller's answer, inside a verified session, to an agent
// it will not serve.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Definitive reports whether the agent must stand down rather than wait for
// the controller to come back. Every code says so but out-of-range: an agent
// whose version the controller does not speak keeps serving what it was last
// told until one of the two is upgraded.
func (r Refusal) Definitive() bool {
	return r.Code != "" && r.Code != RefusalOutOfRange
}
