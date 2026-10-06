package gateway

import (
	"context"
	"net/netip"
	"slices"

	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// prober is the node as the resolver sees it with the gateway's legs in it.
type prober struct {
	host resolve.Prober
	legs func() []Leg
	b    Backend
	// arp asks on a leg, run on a thread inside the leg's namespace.
	arp resolve.Prober
	// do runs a function on a thread inside a namespace, as Do does.
	do func(netns.NsHandle, func() error) error
	// route looks a route up in a namespace as a socket with a mark, bound
	// to an address, sends.
	route func(ns netns.NsHandle, mark int, src, dst netip.Addr) (legRoute, error)
}

// legRoute is the route a lookup in the namespace found: the interface it
// leaves through, whether it goes to the address directly, and the table that
// answered.
type legRoute struct {
	dev    string
	direct bool
	table  int
}

// NewProber returns a Prober that lists the legs beside the host's
// interfaces and runs ARP, Route and Dial on a leg inside the namespace.
// FDBPorts and everything about other interfaces go to host. An interface
// of host with a leg's name (the appliance's own leg<N> card) is replaced by
// the leg's entry, never duplicated.
//
// legs returns the legs that are up, as the last Sync reported them; it is
// called on every call, so that a leg that went away is no leg any more.
func NewProber(host resolve.Prober, legs func() []Leg, b Backend) resolve.Prober {
	return &prober{host: host, legs: legs, b: b, arp: resolve.NewHostProber(0, 0), do: Do, route: routeOnLeg}
}

// Interfaces lists the host's interfaces but those named like a leg, then
// one entry per leg: its address, the bridge and VLAN it is in, the
// forwarding table its frames are in, and the network it serves. The leg's
// address is thereby an address of the node, which no guest may claim.
func (p *prober) Interfaces(ctx context.Context) ([]resolve.HostIface, error) {
	host, err := p.host.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	var legs []Leg
	for _, l := range p.legs() {
		if !slices.ContainsFunc(legs, func(o Leg) bool { return o.Iface() == l.Iface() }) {
			legs = append(legs, l)
		}
	}
	isLeg := func(ifc resolve.HostIface) bool {
		return slices.ContainsFunc(legs, func(l Leg) bool { return l.Iface() == ifc.Name })
	}
	out := make([]resolve.HostIface, 0, len(host)+len(legs))
	for _, ifc := range host {
		if !isLeg(ifc) {
			out = append(out, ifc)
		}
	}
	for _, l := range legs {
		out = append(out, resolve.HostIface{
			Name:    l.Iface(),
			Addrs:   []netip.Prefix{l.Prefix},
			Segment: resolve.Segment{Bridge: l.Bridge, VLAN: l.VLAN},
			Table:   tableOfLeg(l, host),
			Via:     l.ID,
		})
	}
	return out, nil
}

// tableOfLeg is the forwarding table that holds a leg's frames: that of the
// bridge its port is enslaved to, in the leg's VLAN when that is the leg's
// own bridge, which then filters VLANs, and untagged on the bridge Proxmox
// makes for a VLAN of one that does not. A leg without a port on the host, as
// in the appliance, whose legs are its own cards on bridges that filter
// VLANs, is in its bridge and VLAN.
func tableOfLeg(l Leg, host []resolve.HostIface) resolve.FDBRef {
	i := slices.IndexFunc(host, func(ifc resolve.HostIface) bool { return ifc.Name == bridgePort(l.Mark) })
	if i < 0 || host[i].Master == "" || host[i].Master == l.Bridge {
		return resolve.FDBRef{Bridge: l.Bridge, VLAN: l.VLAN}
	}
	return resolve.FDBRef{Bridge: host[i].Master}
}

// leg returns the leg whose interface iface is.
func (p *prober) leg(iface string) (Leg, bool) {
	if iface == "" {
		return Leg{}, false
	}
	legs := p.legs()
	i := slices.IndexFunc(legs, func(l Leg) bool { return l.Iface() == iface })
	if i < 0 {
		return Leg{}, false
	}
	return legs[i], true
}

// Route looks the route up for a leg inside its namespace as the leg's dial
// sends: with its mark, from its address. Only the leg's own table puts the
// address on the leg's link; a route of another table, which the leg's rule
// did not select, is no route of the leg.
func (p *prober) Route(ctx context.Context, iface string, addr netip.Addr) (string, bool, error) {
	l, ok := p.leg(iface)
	if !ok {
		return p.host.Route(ctx, iface, addr)
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	r, err := p.route(p.b.Netns(), l.Mark, l.Prefix.Addr(), addr)
	if err != nil {
		return "", false, err
	}
	return r.dev, r.direct && r.table == tableBase+l.Mark, nil
}

// ARP on a leg runs the host's exchange from a thread inside the leg's
// namespace, where the leg's interface and its raw socket are.
func (p *prober) ARP(ctx context.Context, iface string, addr netip.Addr) ([]string, error) {
	if _, ok := p.leg(iface); !ok {
		return p.host.ARP(ctx, iface, addr)
	}
	var macs []string
	err := p.do(p.b.Netns(), func() (err error) {
		macs, err = p.arp.ARP(ctx, iface, addr)
		return err
	})
	return macs, err
}

// FDBPorts reads the host's bridges, where the legs' ports are too.
func (p *prober) FDBPorts(ctx context.Context, bridge string, vlan int, mac string) ([]string, error) {
	return p.host.FDBPorts(ctx, bridge, vlan, mac)
}

// Dial through a leg is the backend's: from the leg's address, with its mark.
func (p *prober) Dial(ctx context.Context, iface string, target netip.AddrPort) error {
	l, ok := p.leg(iface)
	if !ok {
		return p.host.Dial(ctx, iface, target)
	}
	conn, err := p.b.Dial(ctx, l, target)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
