package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vishvananda/netns"
)

const (
	// Namespace is the name the gateway namespace is pinned under, in
	// /run/netns.
	Namespace = "pco"

	hostTransit = "pcotr0"   // the host's end of the transit link
	nsTransit   = "transit0" // the namespace's end
	// transitBits is the length of the transit link's prefix.
	transitBits = 30

	bridgePortPrefix = "vethpco"
	// The temporary port a probe of a network without a leg goes through.
	probePort  = "vethpcop"
	probeIface = "probe0"

	ruleBase  = 1000 // a leg's rule is at 1000+mark
	tableBase = 100  // and looks up table 100+mark
	mainTable = 254

	// fallbackMetric is the metric of the unreachable route of the service
	// prefix, which keeps requests to it off the default route while the
	// transit link is missing.
	fallbackMetric = 1000
)

var (
	// The MACs of the probe port: the namespace end sends, the bridge end
	// has fe as its first octet like every bridge end of pco.
	probeMAC       = net.HardwareAddr{0x02, 0x50, 0x43, 0x4f, 0x00, 0xfe}
	probeBridgeMAC = net.HardwareAddr{0xfe, 0x50, 0x43, 0x4f, 0x00, 0xfe}
)

// bridgePort is the bridge end of the leg with a mark.
func bridgePort(mark int) string { return bridgePortPrefix + strconv.Itoa(mark) }

// markOfPort returns the mark of a bridge end of a leg, by its name.
func markOfPort(name string) (int, bool) {
	return markOf(name, bridgePortPrefix)
}

// markOfIface returns the mark of a leg's interface, by its name.
func markOfIface(name string) (int, bool) { return markOf(name, "leg") }

func markOf(name, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || rest == "" || strings.HasPrefix(rest, "0") {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 1 && n <= MaxMark
}

// bridgeEndMAC is the MAC of the bridge end of a leg: the namespace end's
// with fe as its first octet, which never becomes the bridge's own MAC (a
// bridge takes the lowest of its ports').
func bridgeEndMAC(mac net.HardwareAddr) net.HardwareAddr {
	out := slices.Clone(mac)
	if len(out) > 0 {
		out[0] = 0xfe
	}
	return out
}

// side is the network namespace an operation is in: the host's own, or the
// gateway namespace.
type side int

const (
	onHost side = iota
	inNS
)

func (s side) String() string {
	if s == onHost {
		return "the host"
	}
	return "the namespace " + Namespace
}

// link is a network interface as the backend compares it.
type link struct {
	name    string
	kind    string // veth, bridge, vlan, dummy, ...
	mac     net.HardwareAddr
	mtu     int
	up      bool // set up
	carrier bool // its lower layer is up
	master  string
	// A bridge that filters VLANs; a bridge port that is isolated.
	vlanFiltering bool
	isolated      bool
}

type address struct {
	prefix        netip.Prefix
	noPrefixRoute bool
}

// route is an IPv4 route. The zero dst is the default route, and the only
// way it is written.
type route struct {
	table       int
	dst         netip.Prefix
	dev         string
	src         netip.Addr
	gw          netip.Addr
	unreachable bool
	metric      int
}

func (r route) String() string {
	var b strings.Builder
	if r.unreachable {
		b.WriteString("unreachable ")
	}
	if !r.dst.IsValid() || r.dst.Bits() == 0 {
		b.WriteString("default")
	} else {
		b.WriteString(r.dst.String())
	}
	if r.gw.IsValid() {
		fmt.Fprintf(&b, " via %s", r.gw)
	}
	if r.dev != "" {
		fmt.Fprintf(&b, " dev %s", r.dev)
	}
	if r.src.IsValid() {
		fmt.Fprintf(&b, " src %s", r.src)
	}
	if r.metric != 0 {
		fmt.Fprintf(&b, " metric %d", r.metric)
	}
	if r.table != mainTable {
		fmt.Fprintf(&b, " table %d", r.table)
	}
	return b.String()
}

// rule is an IPv4 policy rule on a mark.
type rule struct {
	pref, mark, table int
}

func (r rule) String() string {
	return fmt.Sprintf("the rule %d: fwmark %d lookup %d", r.pref, r.mark, r.table)
}

type fdbEntry struct {
	mac            net.HardwareAddr
	vlan           int
	static, sticky bool
}

type portVLAN struct {
	vid            int
	pvid, untagged bool
}

type neighbour struct {
	addr netip.Addr
	mac  net.HardwareAddr
}

// system is what the host backend reads and changes of the node's network:
// the host's namespace and the gateway namespace. Every change is one call,
// so that what a run changed can be told from the calls.
type system interface {
	// pinned reports whether the namespace is pinned; mayPin whether this
	// process can pin one where the node's other processes see it.
	pinned() (bool, error)
	mayPin() bool
	createNamespace() error
	// openNamespace opens the pinned namespace for the calls that follow.
	openNamespace() error
	removeNamespace() error
	handle() netns.NsHandle

	sysctl(s side, key string) (string, error)
	setSysctl(s side, key, value string) error

	link(s side, name string) (link, bool, error)
	links(s side) ([]link, error)
	// addVeth creates a pair: the host end with its MAC and the namespace
	// end, in the namespace, with its own, both with the MTU, in one call. A
	// nil MAC or a zero MTU is the kernel's choice.
	addVeth(hostName string, hostMAC net.HardwareAddr, nsName string, nsMAC net.HardwareAddr, mtu int) error
	delLink(s side, name string) error
	setMTU(s side, name string, mtu int) error
	setMaster(name, master string) error
	setUp(s side, name string) error
	setIsolated(name string, isolated bool) error

	portVLANs(port string) ([]portVLAN, error)
	addPortVLAN(port string, v portVLAN) error
	delPortVLAN(port string, vid int) error

	fdb(port string) ([]fdbEntry, error)
	// addSticky replaces the entry of the MAC on the port with a static
	// sticky one; a zero VLAN leaves the VLAN to the kernel.
	addSticky(port string, mac net.HardwareAddr, vlan int) error
	delFDB(port string, e fdbEntry) error

	addrs(s side, dev string) ([]address, error)
	addAddr(s side, dev string, a address) error
	delAddr(s side, dev string, p netip.Prefix) error

	// routes lists the IPv4 routes of a table, of every table for 0.
	routes(s side, table int) ([]route, error)
	addRoute(s side, r route) error
	delRoute(s side, r route) error

	rules(s side) ([]rule, error)
	addRule(s side, r rule) error
	delRule(s side, r rule) error

	// neighbours lists the permanent IPv4 neighbours of an interface of the
	// namespace.
	neighbours(dev string) ([]neighbour, error)
	setNeighbour(dev string, n neighbour) error
	delNeighbour(dev string, addr netip.Addr) error

	// arpProbe sends probes for addr with the sender 0.0.0.0 through an
	// interface of the namespace and returns the MACs that claim it within
	// the window.
	arpProbe(ctx context.Context, dev string, addr netip.Addr, window time.Duration) ([]net.HardwareAddr, error)
	// dial connects from a socket of the namespace, with the mark, from src.
	dial(ctx context.Context, mark int, src netip.Addr, dst netip.AddrPort) (net.Conn, error)
}
