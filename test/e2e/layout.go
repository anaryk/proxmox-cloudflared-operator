//go:build e2e

package e2e

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The profiles the suite runs in: pco installed on the node, or the appliance
// in a container of the node.
const (
	profileHost      = "host"
	profileAppliance = "appliance"
)

var profileFlag = flag.String("profile", "", "host or appliance (default: PCO_E2E_PROFILE, else host)")

// layout is where the run puts things: the ids and addresses of its guests,
// the node's address on the bridge of the run and, in the appliance profile,
// the appliance's own.
type layout struct {
	profile string
	// first and last bound the ids of the guests; a guest at offset n is
	// first+n.
	first, last int
	// netFirst and netLast are the range of the networking scenarios of the
	// host profile; the appliance has one range for everything.
	netFirst, netLast int
	// nodeAddr is the node's address on the bridge of the run, which the
	// run adds and takes away again.
	nodeAddr string
	// appliance is the id of the appliance, applianceAddr its address on the
	// bridge of the run and applianceGW the gateway it is given there, which
	// nothing answers for until A-S6 makes a guest of it.
	appliance     int
	applianceAddr string
	applianceGW   string
	// guestNet is the first three octets of the guests' addresses, and
	// guestBase the last octet of offset 0.
	guestNet  string
	guestBase int
}

// The second segment of the appliance (A-S13): a runtime bridge with an
// address of the appliance and one guest.
const (
	segBridge      = "pcote1"
	segNet         = "10.92.1"
	segApplianceIP = segNet + ".150"
)

func chooseProfile() (string, error) {
	p := *profileFlag
	if p == "" {
		p = envOr("PCO_E2E_PROFILE", profileHost)
	}
	switch p {
	case profileHost, profileAppliance:
		return p, nil
	}
	return "", fmt.Errorf("the profile %q is neither %s nor %s", p, profileHost, profileAppliance)
}

func layoutFor(profile string) (layout, error) {
	if profile == profileHost {
		return layout{
			profile: profileHost, first: 9100, last: 9199, netFirst: 9350, netLast: 9399,
			nodeAddr: "10.77.0.1", guestNet: "10.77.0", guestBase: 100,
		}, nil
	}
	l := layout{
		profile: profileAppliance, nodeAddr: "10.92.0.100",
		applianceAddr: "10.92.0.150", applianceGW: "10.92.0.199", guestNet: "10.92.0", guestBase: 150,
	}
	ids := envOr("PCO_E2E_VMIDS", "9250-9299")
	lo, hi, ok := strings.Cut(ids, "-")
	var err1, err2 error
	l.first, err1 = strconv.Atoi(lo)
	l.last, err2 = strconv.Atoi(hi)
	if !ok || err1 != nil || err2 != nil || l.last-l.first != 49 {
		return layout{}, fmt.Errorf("PCO_E2E_VMIDS %q: want a range of 50 ids, as 9250-9299", ids)
	}
	l.appliance = l.first
	if raw := os.Getenv("PCO_E2E_APPLIANCE_VMID"); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id < l.first || id > l.last || usedOffset(id-l.first) {
			return layout{}, fmt.Errorf("PCO_E2E_APPLIANCE_VMID %q: want an id of %s at an offset no guest takes (0, 4, 9, 10 or 19)", raw, ids)
		}
		l.appliance = id
	}
	return l, nil
}

// usedOffset says whether a scenario puts a guest at an offset of the range:
// the host scenarios at 1 to 18 but 4, 9 and 10, the appliance's at 20 to 29
// and 45 to 49, its networking scenarios at 30 to 44.
func usedOffset(n int) bool {
	return (n >= 1 && n <= 18 && n != 4 && n != 9 && n != 10) || (n >= 20 && n <= 49)
}

func (l layout) isAppliance() bool { return l.profile == profileAppliance }

// vmid is the id of the guest at offset n.
func (l layout) vmid(n int) int { return l.first + n }

// addr is the address of the guest at offset n on the bridge of the run.
func (l layout) addr(n int) string { return fmt.Sprintf("%s.%d", l.guestNet, l.guestBase+n) }

// contains says whether the suite may use the id.
func (l layout) contains(vmid int) bool {
	return (vmid >= l.first && vmid <= l.last) || (l.netFirst != 0 && vmid >= l.netFirst && vmid <= l.netLast)
}

// nodeCIDR is the node's address on the bridge with its prefix.
func (l layout) nodeCIDR() string { return l.nodeAddr + "/24" }

// pcoAddr is the address pco answers ARP from on the bridge of the run: the
// node's, or the appliance's.
func (l layout) pcoAddr() string {
	if l.isAppliance() {
		return l.applianceAddr
	}
	return l.nodeAddr
}

// level is the identity level a route on the bridge of the run is served at:
// the node proves a guest's port, the appliance only what it observes.
func (l layout) level() string {
	if l.isAppliance() {
		return "observed"
	}
	return "port"
}

// netVMID is the id of a guest of the networking scenarios, given as the id
// the host profile gives it: in the appliance's one range those sit at the
// offsets from 31 up.
func (l layout) netVMID(hostID int) int {
	if !l.isAppliance() {
		return hostID
	}
	k := hostID - 9350
	if k > 10 {
		k -= 5
	}
	return l.first + 30 + k
}
