//go:build !linux

package gateway

import (
	"context"

	"github.com/vishvananda/netns"
)

// Run needs Linux.
func (SourceWatch) Run(context.Context, netns.NsHandle, string) error { return errNotLinux }
