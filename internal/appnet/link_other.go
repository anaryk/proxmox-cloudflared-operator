//go:build !linux

package appnet

import (
	"context"
	"errors"
	"net/netip"
)

var errNotLinux = errors.New("the network of the appliance needs Linux")

// NewNetlink returns a Netlink that fails every call: the appliance is a
// Linux container.
func NewNetlink() Netlink { return otherNetlink{} }

type otherNetlink struct{}

func (otherNetlink) EnsureDummy(context.Context, string) error { return errNotLinux }

func (otherNetlink) HasDummy(context.Context, string) (bool, error) { return false, errNotLinux }

func (otherNetlink) EnsureAddr(context.Context, string, netip.Prefix) error { return errNotLinux }

func (otherNetlink) HasAddr(context.Context, string, netip.Prefix) (bool, error) {
	return false, errNotLinux
}

func (otherNetlink) EnsureRoute(context.Context, netip.Prefix, string, netip.Addr) error {
	return errNotLinux
}

func (otherNetlink) HasRoute(context.Context, netip.Prefix, string, netip.Addr) (bool, error) {
	return false, errNotLinux
}

func (otherNetlink) EnsureRule(context.Context, int, netip.Addr, netip.Prefix, bool) error {
	return errNotLinux
}

func (otherNetlink) HasRule(context.Context, int, netip.Addr, netip.Prefix, bool) (bool, error) {
	return false, errNotLinux
}
