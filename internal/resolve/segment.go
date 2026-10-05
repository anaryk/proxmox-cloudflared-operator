package resolve

import (
	"strconv"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// maxVLAN is the highest VLAN id a tag can carry.
const maxVLAN = 4094

// Segment is the layer-2 segment an interface sits on: a bridge and a VLAN
// (0 for untagged). On the node it is read off the interface name; in the
// appliance the daemon maps the container's NICs to it.
type Segment struct {
	Bridge string `json:"bridge"`
	VLAN   int    `json:"vlan,omitempty"`
}

// IsZero reports whether s is no segment at all.
func (s Segment) IsZero() bool { return s == Segment{} }

// String names the segment as the problems do: "vmbr1", "vmbr1 VLAN 20".
func (s Segment) String() string {
	if s.VLAN == 0 {
		return s.Bridge
	}
	return s.Bridge + " VLAN " + strconv.Itoa(s.VLAN)
}

// SegmentOf is the segment a guest NIC is on: its bridge and its tag.
func SegmentOf(nic model.NIC) Segment { return Segment{Bridge: nic.Bridge, VLAN: nic.VLAN} }

// linkInfo is what the segment of a host interface is read from.
type linkInfo struct {
	name       string
	bridge     bool   // the interface is a Linux bridge
	vlanParent string // for a VLAN interface, the interface it sits on
	vlanID     int
}

// segmentOf returns the segment of a host interface, given the names of the
// bridges of the host: a bridge is its own untagged segment, a VLAN interface
// on a bridge (vmbr0.20) is that VLAN of it, and so is the bridge Proxmox makes
// for a VLAN of a bridge that does not filter VLANs (vmbr0v20). Any other
// interface is on no segment.
func segmentOf(l linkInfo, bridges map[string]bool) Segment {
	switch {
	case l.bridge:
		if base, vlan, ok := vlanBridgeOf(l.name, bridges); ok {
			return Segment{Bridge: base, VLAN: vlan}
		}
		return Segment{Bridge: l.name}
	case bridges[l.vlanParent] && l.vlanID > 0 && l.vlanID <= maxVLAN:
		return Segment{Bridge: l.vlanParent, VLAN: l.vlanID}
	}
	return Segment{}
}

// vlanBridgeOf reads a name of the form <bridge>v<vlan>, as Proxmox names the
// bridge it makes for a VLAN, where <bridge> is a bridge of the host.
func vlanBridgeOf(name string, bridges map[string]bool) (string, int, bool) {
	i := strings.LastIndexByte(name, 'v')
	if i <= 0 || i == len(name)-1 {
		return "", 0, false
	}
	base, digits := name[:i], name[i+1:]
	if strings.Trim(digits, "0123456789") != "" || !bridges[base] {
		return "", 0, false
	}
	vlan, err := strconv.Atoi(digits)
	if err != nil || vlan < 1 || vlan > maxVLAN {
		return "", 0, false
	}
	return base, vlan, true
}
