//go:build linux

package gateway

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// routeOnLeg asks the kernel of the namespace ns for the route a socket with
// the mark, bound to src, would send to dst on. Having no route, or one that
// drops the traffic, is resolve.ErrNoRoute.
func routeOnLeg(ns netns.NsHandle, mark int, src, dst netip.Addr) (legRoute, error) {
	if !dst.Is4() {
		return legRoute{}, fmt.Errorf("%s is not an IPv4 address", dst)
	}
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return legRoute{}, fmt.Errorf("opening the namespace: %w", err)
	}
	defer h.Close()
	routes, err := h.RouteGetWithOptions(dst.AsSlice(), &netlink.RouteGetOptions{Mark: uint32(mark), SrcAddr: src.AsSlice()})
	switch {
	case noRoute(err), err == nil && len(routes) == 0:
		return legRoute{}, fmt.Errorf("route to %s with mark %d: %w", dst, mark, resolve.ErrNoRoute)
	case err != nil:
		return legRoute{}, fmt.Errorf("looking up the route to %s with mark %d: %w", dst, mark, err)
	}
	r := routes[0]
	link, err := h.LinkByIndex(r.LinkIndex)
	if err != nil {
		return legRoute{}, fmt.Errorf("looking up interface %d: %w", r.LinkIndex, err)
	}
	return legRoute{dev: link.Attrs().Name, direct: r.Gw == nil && r.Via == nil && len(r.MultiPath) == 0, table: r.Table}, nil
}

// noRoute reports whether the kernel answered a route lookup with having no
// route (ENETUNREACH) or one that drops the traffic: unreachable
// (EHOSTUNREACH), blackhole (EINVAL) or prohibit (EACCES). A source address
// the namespace does not have is EINVAL too, which leaves the leg no route.
func noRoute(err error) bool {
	return errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EACCES)
}
