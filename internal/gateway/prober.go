package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// errNamespaceNotOpen is a probe of a leg while the backend has no handle on
// the gateway namespace: it never runs in the host's namespace instead.
var errNamespaceNotOpen = errors.New("the gateway namespace is not open")

// prober is the node as the resolver sees it with the gateway's legs in it.
type prober struct {
	host resolve.Prober
	legs func() []Leg
	b    Backend
	// ownNamespace says that the legs live in a network namespace of their
	// own, the host backend's; in the appliance they are cards of the
	// process's own namespace.
	ownNamespace bool
	// arp asks on a leg, run on a thread where the leg is.
	arp resolve.Prober
	// do runs a function on a thread inside a namespace, as Do does.
	do func(netns.NsHandle, func() error) error
	// hold returns a handle of its own on the namespace of the legs for one
	// probe, and what lets it go, or refuses one that is not that namespace.
	hold func(netns.NsHandle) (netns.NsHandle, func(), error)
	// lookup asks the namespace of the calling thread for the route a socket
	// with a mark, bound to an address, sends on.
	lookup func(mark int, src, dst netip.Addr) (legRoute, error)
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
// legs returns the legs that are up, as the last Sync reported them. It is
// called on every call, from the resolver's goroutines at once, so it must be
// safe for that; a name of a leg that is no leg any more is an error of the
// probe, never a probe of the host.
func NewProber(host resolve.Prober, legs func() []Leg, b Backend) resolve.Prober {
	_, own := b.(*hostBackend)
	return &prober{
		host: host, legs: legs, b: b, ownNamespace: own,
		arp: resolve.NewHostProber(0, 0), do: Do, hold: namespaceHolder(), lookup: routeHere,
	}
}

// Interfaces lists the host's interfaces but those named like a leg, then
// one entry per leg: its address, the bridge and VLAN it is in, the
// forwarding table its frames are in, and the network it serves. The leg's
// address is thereby an address of the node, which no guest may claim. A
// name of the leg's form is the gateway's: an interface of the host with it
// that is no leg now is left out, not taken for one of the host's own.
func (p *prober) Interfaces(ctx context.Context) ([]resolve.HostIface, error) {
	host, err := p.host.Interfaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]resolve.HostIface, 0, len(host))
	for _, ifc := range host {
		if _, isLeg := markOfIface(ifc.Name); !isLeg {
			out = append(out, ifc)
		}
	}
	var listed []string
	for _, l := range p.legs() {
		table, ok := p.tableOfLeg(l, host)
		if !ok || slices.Contains(listed, l.Iface()) {
			continue
		}
		listed = append(listed, l.Iface())
		out = append(out, resolve.HostIface{
			Name:    l.Iface(),
			Addrs:   []netip.Prefix{l.Prefix},
			Segment: resolve.Segment{Bridge: l.Bridge, VLAN: l.VLAN},
			Table:   table,
			Via:     l.ID,
		})
	}
	return out, nil
}

// tableOfLeg is the forwarding table that holds a leg's frames: that of the
// bridge its port is enslaved to, in the leg's VLAN when that is the leg's
// own bridge, which then filters VLANs, and untagged on the bridge Proxmox
// makes for a VLAN of one that does not. In the appliance the legs are its
// own cards on bridges that filter VLANs, in their bridge and VLAN. On the
// host a leg whose port is missing or on no bridge is not up, and false.
func (p *prober) tableOfLeg(l Leg, host []resolve.HostIface) (resolve.FDBRef, bool) {
	if !p.ownNamespace {
		return resolve.FDBRef{Bridge: l.Bridge, VLAN: l.VLAN}, true
	}
	i := slices.IndexFunc(host, func(ifc resolve.HostIface) bool { return ifc.Name == bridgePort(l.Mark) })
	switch {
	case i < 0 || host[i].Master == "":
		return resolve.FDBRef{}, false
	case host[i].Master == l.Bridge:
		return resolve.FDBRef{Bridge: l.Bridge, VLAN: l.VLAN}, true
	}
	return resolve.FDBRef{Bridge: host[i].Master}, true
}

// leg resolves iface once for a probe: a leg, or an interface of the host. A
// name of the leg's form that is no leg now, as when the leg was taken away
// after the interfaces were listed, is an error.
func (p *prober) leg(iface string) (Leg, bool, error) {
	if _, isLeg := markOfIface(iface); !isLeg {
		return Leg{}, false, nil
	}
	legs := p.legs()
	i := slices.IndexFunc(legs, func(l Leg) bool { return l.Iface() == iface })
	if i < 0 {
		return Leg{}, false, fmt.Errorf("%s is no leg of the gateway now", iface)
	}
	return legs[i], true, nil
}

// enter runs f where the legs are: on the host on a thread inside their
// namespace, through a handle held for the call, so that the backend closing
// its own does not move the probe elsewhere; in the appliance on a thread of
// the process. A namespace that is not open is an error before anything runs.
func (p *prober) enter(f func() error) error {
	if !p.ownNamespace {
		return p.do(netns.None(), f)
	}
	ns := p.b.Netns()
	if ns == netns.None() {
		return errNamespaceNotOpen
	}
	held, release, err := p.hold(ns)
	if err != nil {
		return fmt.Errorf("holding the gateway namespace: %w", err)
	}
	defer release()
	return p.do(held, f)
}

// Route looks the route up for a leg inside its namespace as the leg's dial
// sends: with its mark, from its address. Only the leg's own table puts the
// address on the leg's link; a route of another table, which the leg's rule
// did not select, is no route of the leg.
func (p *prober) Route(ctx context.Context, iface string, addr netip.Addr) (string, bool, error) {
	l, isLeg, err := p.leg(iface)
	switch {
	case err != nil:
		return "", false, err
	case !isLeg:
		return p.host.Route(ctx, iface, addr)
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	var r legRoute
	if err := p.enter(func() (err error) { r, err = p.lookup(l.Mark, l.Prefix.Addr(), addr); return err }); err != nil {
		return "", false, err
	}
	return r.dev, r.direct && r.table == tableBase+l.Mark, nil
}

// ARP on a leg runs the host's exchange from a thread where the leg is, so
// that its interface is found and its raw socket opened there.
func (p *prober) ARP(ctx context.Context, iface string, addr netip.Addr) ([]string, error) {
	l, isLeg, err := p.leg(iface)
	switch {
	case err != nil:
		return nil, err
	case !isLeg:
		return p.host.ARP(ctx, iface, addr)
	}
	var macs []string
	err = p.enter(func() (err error) {
		macs, err = p.arp.ARP(ctx, l.Iface(), addr)
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
	l, isLeg, err := p.leg(iface)
	switch {
	case err != nil:
		return err
	case !isLeg:
		return p.host.Dial(ctx, iface, target)
	}
	conn, err := p.b.Dial(ctx, l, target)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}
