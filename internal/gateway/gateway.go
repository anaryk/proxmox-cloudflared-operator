// Package gateway is the network backend of the networks pco reaches through
// a leg of its own: a network the node has no address in, attached by the
// admin, or the managed network pco gives guests. Each leg is a foot in one
// bridge and VLAN; each target behind a leg is published under a service
// address of its own, which the connectors dial and which the backend NATs to
// the target through its leg. On the host the legs live in the network
// namespace pco, linked to the host by a transit pair; in the appliance they
// are the container's own network cards.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/vishvananda/netns"
)

// The service prefix is fixed: the connectors dial service addresses from it,
// the egress filter's edge rule excludes it, and the host routes it into the
// namespace. Its first /30 is the transit link.
var (
	ServicePrefix = netip.MustParsePrefix("198.18.0.0/16")
	// TransitHost is the host's transit address, and in the appliance the
	// source of every request to the prefix.
	TransitHost = netip.MustParseAddr("198.18.0.1")
	// TransitNS is the namespace's end of the transit link.
	TransitNS = netip.MustParseAddr("198.18.0.2")
)

const (
	// PortBlockSize is the number of source ports the SNAT through a leg
	// takes. A guest keeps a source port in TIME_WAIT for 60 seconds, so a
	// block serves about its size in new connections a minute to one address
	// and port of a guest: with 4096 ports, about 68 a second. A block of 1024
	// had SYNs refused above about 17 a second.
	PortBlockSize = 4096
	// portBase is the first port of the first block.
	portBase = 1024
	// MaxMark is the highest mark of a leg, and so the most networks a node
	// reaches. Each mark owns a block of PortBlockSize source ports from
	// portBase up, and the blocks end below port 65536: 15 blocks end at 62463,
	// and a 16th would pass 65535.
	MaxMark = (65536 - portBase) / PortBlockSize
	// ManagedBridge is the bridge of the managed network, and its id.
	ManagedBridge = "pconet"
)

// ManagedLegMAC is the namespace end of the managed leg, the same on every
// node, so that a managed guest sees one gateway MAC wherever it runs.
var ManagedLegMAC = net.HardwareAddr{0x02, 0x50, 0x43, 0x4f, 0xff, 0x01}

// Leg is a network pco has a foot in, as the backend sees it.
type Leg struct {
	ID     string           // the network id
	Mark   int              // 1..MaxMark: the fwmark, the conntrack zone, table 100+Mark
	Bridge string           // the bridge the leg is enslaved to; ManagedBridge for the managed network
	VLAN   int              // the tag; 0 untagged
	Prefix netip.Prefix     // the guest network, with the leg's address as Addr()
	MAC    net.HardwareAddr // the namespace end; the bridge end is the same with fe as its first octet
	// Managed legs accept DHCP from their guests.
	Managed bool
	// Uplink legs are on a bridge with a physical port: the address is only
	// raised after duplicate-address detection found nobody holding it.
	Uplink bool
}

// Iface is the leg's interface: leg<mark> in both profiles.
func (l Leg) Iface() string { return LegIface(l.Mark) }

// LegIface is the interface of the leg with a mark.
func LegIface(mark int) string { return fmt.Sprintf("leg%d", mark) }

// PortBlock is the block of source ports the SNAT through the leg with a mark
// takes: legs into identical subnets on bridges where the host's conntrack
// sees their frames (bridge-nf-call-iptables, which pve-firewall turns on)
// then never present the same tuple there.
func PortBlock(mark int) (lo, hi uint16) {
	lo = uint16(portBase + (mark-1)*PortBlockSize)
	return lo, lo + PortBlockSize - 1
}

// Sticky is a static sticky forwarding entry for a managed guest's MAC on its
// own port of the bridge, so that no other port can take the MAC over.
type Sticky struct {
	MAC    net.HardwareAddr
	Bridge string // ManagedBridge
	VLAN   int
	// Port is tap<vmid>i<n>, fwpr<vmid>p<n> or veth<vmid>i<n>; "" while the
	// guest is stopped, which leaves the entry out.
	Port string
}

// LegState is what the backend reports about a leg.
type LegState struct {
	ID    string
	Up    bool   // the leg is enslaved, up and holds its address
	Iface string // leg<mark>
	// Problem says why the leg is not up, or what is wrong with it although
	// it is: "bridge vmbr0v20 is absent", "address in use by 52:54:00:12:34:56",
	// "no carrier, DAD not run"; empty when there is nothing to say.
	Problem string
	MTU     int
	DADAt   time.Time // when duplicate-address detection last found the address free
}

// Service is one DNAT: connectors dial Service, the leg sends to Addr.
type Service struct {
	Leg     string // the network id
	Service netip.Addr
	Addr    netip.Addr
	Ports   []uint16 // the published ports, sorted
}

// Pin is a verified neighbour on a leg.
type Pin struct {
	Leg  string // the network id
	Addr netip.Addr
	MAC  net.HardwareAddr
}

// Backend is what the engine drives. Every method is idempotent and may be
// called from one goroutine at a time.
type Backend interface {
	// Up brings the namespace (or the container's base) up: sysctls, transit,
	// base ruleset, host routes, the host table. It compares and changes only
	// what differs, so that a second Up under traffic changes nothing. It is
	// what pco netns up runs, and what the daemon's keeper runs when Verify
	// fails.
	Up(ctx context.Context) error
	// Sync makes the legs, the services, the pins and the sticky entries what
	// is given, in that order: legs are created and raised (after DAD on
	// uplink bridges), elements are added before they are removed, pins
	// replaced, sticky entries replaced on the ports that exist. It returns
	// the state of every leg; a leg that cannot be raised is reported, not an
	// error of the call.
	Sync(ctx context.Context, legs []Leg, services []Service, pins []Pin, sticky []Sticky) ([]LegState, error)
	// Verify compares the live network with what Up and Sync last applied.
	// A difference is a ChangedError, which is ErrChanged.
	Verify(ctx context.Context) error
	// Probe sends DAD-style ARP probes (sender 0.0.0.0) for addr on the bridge
	// and VLAN, through the leg there or through a temporary port when there
	// is none, and returns the MACs that answered.
	Probe(ctx context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error)
	// Detach removes one leg and its services.
	Detach(ctx context.Context, id string) error
	// Down removes everything Up and Sync made, the fallback route among
	// them; only pco uninstall calls it.
	Down(ctx context.Context) error
	// Netns returns the handle of the namespace the legs live in, for the
	// prober, the DHCP server and the neighbour watch; in the container
	// backend the appliance's own, which the process is in.
	Netns() netns.NsHandle
	// Dial opens a TCP connection to addr through the leg: from its address,
	// with its mark.
	Dial(ctx context.Context, leg Leg, addr netip.AddrPort) (net.Conn, error)
}

// Nft runs nft in one network namespace, fixed at construction.
type Nft interface {
	// Apply runs a script as nft -f -: one transaction.
	Apply(ctx context.Context, script string) error
	// List returns nft -j list table inet <table>, or ErrNotLoaded.
	List(ctx context.Context, table string) ([]byte, error)
}

var (
	// ErrNotLoaded says that a table is not there.
	ErrNotLoaded = errors.New("the table is not loaded")
	// ErrChanged says that the network is not as pco left it.
	ErrChanged = errors.New("the gateway is not as pco left it")
)

// ChangedError names what differs, the first few of it.
type ChangedError struct {
	Differences []string
}

// shownDifferences is how many differences a ChangedError names.
const shownDifferences = 8

func (e *ChangedError) Error() string {
	d := e.Differences
	if len(d) > shownDifferences {
		return fmt.Sprintf("%s; and %d more", strings.Join(d[:shownDifferences], "; "), len(d)-shownDifferences)
	}
	return strings.Join(d, "; ")
}

func (e *ChangedError) Unwrap() error { return ErrChanged }
