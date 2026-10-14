//go:build !linux

package gateway

import (
	"context"

	"github.com/vishvananda/netns"
)

// WatchGuestPorts needs Linux.
func WatchGuestPorts(context.Context, netns.NsHandle, string, func(string), func(error)) error {
	return errNotLinux
}
