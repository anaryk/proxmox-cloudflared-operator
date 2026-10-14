package agentproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// The bounds of a probe answer: an answer over them is not read, so that an
// agent cannot make the controller hold more than a node has.
const (
	// MaxProbeIfaces is the most interfaces an answer lists: the bridges,
	// their uplinks and the interfaces with an address, not the ports of the
	// guests.
	MaxProbeIfaces = 512
	// MaxProbeAddrs is the most addresses of one interface.
	MaxProbeAddrs = 32
	// MaxProbeAnswers is the most MACs that answered for the address.
	MaxProbeAnswers = 64
	// MaxProbeAnswerBytes is the most bytes of an answer.
	MaxProbeAnswerBytes = 128 << 10
	// MaxLegReports is the most legs a status says: one for each mark.
	MaxLegReports = 64

	// maxProbeText bounds a line of an answer.
	maxProbeText = 512
	// maxIfaceName is the longest name Linux gives an interface.
	maxIfaceName = 15
	maxVLAN      = 4094
)

// Probe is the request of a probe action: whether a bridge, or a VLAN of it,
// is on the agent's node, how the node is on it, and who answers there for
// an address. Without an address nothing is probed on the wire.
type Probe struct {
	Bridge string     `json:"bridge"`
	VLAN   int        `json:"vlan,omitempty"`
	Addr   netip.Addr `json:"addr,omitzero"`
}

// ProbeIface is an interface of the node as the agent sees it: a bridge,
// a port of one that is no guest's, or one with an address of the node.
type ProbeIface struct {
	Name   string `json:"name"`
	Master string `json:"master,omitempty"` // the bridge it is a port of
	// Bridge and VLAN are the segment it is on; none for an interface on
	// no bridge.
	Bridge string         `json:"bridge,omitempty"`
	VLAN   int            `json:"vlan,omitempty"`
	Addrs  []netip.Prefix `json:"addrs,omitempty"`
	Via    string         `json:"via,omitempty"` // the network whose leg it is
}

// ProbeAnswer is what the agent found for a probe: the interfaces of its
// node, the MACs that answered for the address, and why the address was not
// probed when it was not.
type ProbeAnswer struct {
	Interfaces []ProbeIface `json:"interfaces"`
	AnsweredBy []string     `json:"answeredBy"`
	NotProbed  string       `json:"notProbed,omitempty"`
}

// Check refuses a probe that names no bridge the way Linux names one, a VLAN
// out of range or an address that is not IPv4.
func (p Probe) Check() error {
	switch {
	case p.Bridge == "":
		return errors.New("the probe names no bridge")
	case !ifaceName(p.Bridge):
		return fmt.Errorf("the probe names the bridge %q: want up to %d letters, digits, - and _", p.Bridge, maxIfaceName)
	case p.VLAN < 0 || p.VLAN > maxVLAN:
		return fmt.Errorf("the probe names VLAN %d: want 0 for untagged, or 1 to %d", p.VLAN, maxVLAN)
	case p.Addr.IsValid() && !p.Addr.Is4():
		return fmt.Errorf("the probe names %s: want an IPv4 address", p.Addr)
	}
	return nil
}

// Check refuses an answer over the bounds, or with a name, a VLAN or a MAC
// that is none.
func (a ProbeAnswer) Check() error {
	switch {
	case len(a.Interfaces) > MaxProbeIfaces:
		return fmt.Errorf("the probe answer lists %d interfaces, at most %d", len(a.Interfaces), MaxProbeIfaces)
	case len(a.AnsweredBy) > MaxProbeAnswers:
		return fmt.Errorf("the probe answer has %d MACs that answered, at most %d", len(a.AnsweredBy), MaxProbeAnswers)
	case len(a.NotProbed) > maxProbeText:
		return fmt.Errorf("the probe answer says why it did not probe in %d bytes, which is too long", len(a.NotProbed))
	}
	for _, i := range a.Interfaces {
		if err := i.check(); err != nil {
			return err
		}
	}
	for _, m := range a.AnsweredBy {
		if hw, err := net.ParseMAC(m); err != nil || len(hw) != 6 {
			return fmt.Errorf("the probe answer says %q answered, which is no MAC", m)
		}
	}
	return nil
}

func (i ProbeIface) check() error {
	for _, name := range []string{i.Name, i.Master, i.Bridge} {
		if name != "" && !ifaceName(name) {
			return fmt.Errorf("the probe answer names the interface %q, which no interface is called", name)
		}
	}
	if i.Via != "" && !networkID(i.Via) {
		return fmt.Errorf("the probe answer names the network %q of interface %s, which no network is called", i.Via, i.Name)
	}
	switch {
	case i.Name == "":
		return errors.New("the probe answer lists an interface without a name")
	case i.VLAN < 0 || i.VLAN > maxVLAN:
		return fmt.Errorf("the probe answer puts %s on VLAN %d", i.Name, i.VLAN)
	case len(i.Addrs) > MaxProbeAddrs:
		return fmt.Errorf("the probe answer gives %s %d addresses, at most %d", i.Name, len(i.Addrs), MaxProbeAddrs)
	}
	return nil
}

// DecodeProbeAnswer reads the data of the answer to a probe action, and
// refuses one over MaxProbeAnswerBytes with ErrTooLarge, one that Check
// refuses, and anything after the answer.
func DecodeProbeAnswer(data []byte) (ProbeAnswer, error) {
	if len(data) > MaxProbeAnswerBytes {
		return ProbeAnswer{}, fmt.Errorf("the probe answer is %w: %d bytes, at most %d", ErrTooLarge, len(data), MaxProbeAnswerBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var a ProbeAnswer
	if err := dec.Decode(&a); err != nil {
		return ProbeAnswer{}, fmt.Errorf("decoding the probe answer: %w", err)
	}
	if dec.More() {
		return ProbeAnswer{}, errors.New("decoding the probe answer: something follows it")
	}
	if err := a.Check(); err != nil {
		return ProbeAnswer{}, err
	}
	return a, nil
}

// LegReport is how the gateway of the agent's node last found the leg of a
// network.
type LegReport struct {
	ID      string `json:"id"`
	Up      bool   `json:"up"`
	Problem string `json:"problem,omitempty"`
}

// ifaceName says that s is a name Linux could give an interface: up to 15
// letters, digits, -, _ and dots.
func ifaceName(s string) bool {
	return s != "" && len(s) <= maxIfaceName && strings.IndexFunc(s, notNameChar) < 0
}

func notNameChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		return false
	}
	return true
}

// networkID says that s is the id of a network: a bridge, or a bridge and
// a VLAN of it, as vmbr1.20.
func networkID(s string) bool {
	bridge, vlan, tagged := strings.Cut(s, ".")
	if !ifaceName(bridge) {
		return false
	}
	n, err := strconv.Atoi(vlan)
	return !tagged || err == nil && n >= 1 && n <= maxVLAN && strconv.Itoa(n) == vlan
}
