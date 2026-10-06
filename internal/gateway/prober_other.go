//go:build !linux

package gateway

import (
	"net/netip"

	"github.com/vishvananda/netns"
)

// routeOnLeg needs Linux.
func routeOnLeg(netns.NsHandle, int, netip.Addr, netip.Addr) (legRoute, error) {
	return legRoute{}, errNotLinux
}
