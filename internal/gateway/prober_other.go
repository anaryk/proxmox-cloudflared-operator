//go:build !linux

package gateway

import (
	"net/netip"

	"github.com/vishvananda/netns"
)

// routeHere needs Linux.
func routeHere(int, netip.Addr, netip.Addr) (legRoute, error) {
	return legRoute{}, errNotLinux
}

// namespaceHolder needs Linux.
func namespaceHolder() func(netns.NsHandle) (netns.NsHandle, func(), error) {
	return func(netns.NsHandle) (netns.NsHandle, func(), error) { return netns.None(), nil, errNotLinux }
}
