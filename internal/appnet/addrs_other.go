//go:build !linux

package appnet

import (
	"context"
	"net/netip"
)

// LinkAddrs needs the netlink of Linux.
func LinkAddrs(context.Context, string) ([]netip.Addr, error) { return nil, errNotLinux }

// WatchAddrs needs the netlink notifications of Linux.
func WatchAddrs(context.Context, func()) error { return errNotLinux }
