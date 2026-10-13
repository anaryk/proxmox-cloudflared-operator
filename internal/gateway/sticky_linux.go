//go:build linux

package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// WatchGuestPorts calls joined with the name of every port of a guest's card
// (GuestPort) that is enslaved to the bridge in the network namespace ns, or
// in the process's own for netns.None(): those there when it starts, and
// each that joins or changes later, until ctx ends. A guest that starts
// makes its port anew, without the sticky entry its card had, so the entry
// is to be given again right then. It returns an error when it cannot
// subscribe, or when the subscription ends.
func WatchGuestPorts(ctx context.Context, ns netns.NsHandle, bridge string, joined func(port string)) error {
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return fmt.Errorf("opening a netlink handle: %w", err)
	}
	defer h.Close()
	ch := make(chan netlink.LinkUpdate, 256)
	done := make(chan struct{})
	defer close(done)
	failed := make(chan error, 1)
	opts := netlink.LinkSubscribeOptions{
		ListExisting: true,
		ErrorCallback: func(err error) {
			select {
			case failed <- err:
			default:
			}
		},
	}
	if ns.IsOpen() {
		opts.Namespace = &ns
	}
	if err := netlink.LinkSubscribeWithOptions(ch, done, opts); err != nil {
		return fmt.Errorf("watching the links: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failed:
			return fmt.Errorf("watching the links: %w", err)
		case u, ok := <-ch:
			if !ok {
				return errors.New("watching the links: the subscription ended")
			}
			if port, ok := joinedPort(h, u, bridge); ok {
				joined(port)
			}
		}
	}
}

// joinedPort returns the name of the link of an update when it is a guest's
// port enslaved to the bridge.
func joinedPort(h *netlink.Handle, u netlink.LinkUpdate, bridge string) (string, bool) {
	if u.Link == nil || u.Header.Type != unix.RTM_NEWLINK {
		return "", false
	}
	a := u.Attrs()
	if a.MasterIndex == 0 || !IsGuestPort(a.Name) {
		return "", false
	}
	br, err := h.LinkByName(bridge)
	if err != nil || br.Attrs().Index != a.MasterIndex {
		return "", false
	}
	return a.Name, true
}
