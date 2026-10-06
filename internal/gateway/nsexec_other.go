//go:build !linux

package gateway

import (
	"context"
	"os"

	"github.com/vishvananda/netns"
)

// Do needs Linux.
func Do(netns.NsHandle, func() error) error { return errNotLinux }

// Socket needs Linux.
func Socket(netns.NsHandle, int, int, int, SocketOptions) (*os.File, error) { return nil, errNotLinux }

// WatchLinks needs Linux.
func WatchLinks(context.Context, func(name string)) error { return errNotLinux }
