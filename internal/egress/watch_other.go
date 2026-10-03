//go:build !linux

package egress

import (
	"context"
	"errors"
	"net/netip"
)

// Watch needs the netlink notifications of Linux.
func Watch(context.Context, func() map[netip.Addr]Pin, func(netip.Addr)) error {
	return errors.New("watching the network for addresses that move needs Linux")
}

// WatchRuleset needs the netlink notifications of Linux.
func WatchRuleset(context.Context, func()) error {
	return errors.New("watching the nftables ruleset needs Linux")
}
