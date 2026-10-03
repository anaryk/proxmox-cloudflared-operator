package egress

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const (
	// resubscribeAfter is how long the watch waits before it subscribes
	// again after its socket failed, as when a burst of notifications
	// overflowed it.
	resubscribeAfter = time.Second
	// watchBuffer is the receive buffer the watch asks for.
	watchBuffer = 1 << 20
	// watchQueue is how many notifications wait to be looked at.
	watchQueue = 256
)

// errUnsubscribed ends a subscription whose socket failed.
var errUnsubscribed = errors.New("the netlink subscription ended")

// Watch calls onMove for every bound address that moves, until ctx ends: the
// neighbour table of the node gives it a link-layer address its guest does not
// have, or the bridge of its pin learns the pinned MAC on another port. It
// subscribes to the notifications of both tables, neighbour updates and the
// forwarding tables of the bridges, and asks bound for the pins at every one.
// After the subscription fails, as when notifications came faster than they
// were read, it subscribes again and compares what both tables hold, so that
// no move goes unseen. It returns nil when ctx ends, and an error when it
// cannot subscribe at all.
func Watch(ctx context.Context, bound func() map[netip.Addr]Pin, onMove func(netip.Addr)) error {
	return watchIn(ctx, netns.None(), bound, onMove)
}

// watchIn watches the tables of the network namespace ns.
func watchIn(ctx context.Context, ns netns.NsHandle, bound func() map[netip.Addr]Pin, onMove func(netip.Addr)) error {
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return fmt.Errorf("watching the network: %w", err)
	}
	defer h.Close()
	for first := true; ; first = false {
		err := watchOnce(ctx, ns, h, bound, onMove)
		switch {
		case ctx.Err() != nil:
			return nil
		case first && !errors.Is(err, errUnsubscribed):
			return fmt.Errorf("watching the network: %w", err)
		}
		t := time.NewTimer(resubscribeAfter)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// watchOnce reads one subscription, which begins with what both tables hold,
// until ctx ends or the subscription does.
func watchOnce(ctx context.Context, ns netns.NsHandle, h *netlink.Handle, bound func() map[netip.Addr]Pin, onMove func(netip.Addr)) error {
	updates := make(chan netlink.NeighUpdate, watchQueue)
	done := make(chan struct{})
	err := netlink.NeighSubscribeWithOptions(updates, done, netlink.NeighSubscribeOptions{
		Namespace:         &ns,
		ListExisting:      true,
		ReceiveBufferSize: watchBuffer,
	})
	if err != nil {
		close(done)
		return err
	}
	defer func() {
		// The reader of the subscription ends once its socket is closed,
		// which it may not see while it waits to hand over an update.
		close(done)
		for range updates {
		}
	}()
	names := map[int]string{}
	name := func(index int) string {
		if n, ok := names[index]; ok {
			return n
		}
		link, err := h.LinkByIndex(index)
		if err != nil {
			return ""
		}
		names[index] = link.Attrs().Name
		return names[index]
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-updates:
			if !ok {
				return errUnsubscribed
			}
			e, ok := entryOf(u, name)
			if !ok {
				continue
			}
			for _, addr := range e.moved(bound()) {
				onMove(addr)
			}
		}
	}
}

// entryOf reads an update that says where a MAC is now: an IPv4 neighbour,
// or an entry a bridge learned on one of its ports. A port whose name cannot
// be found counts as another port.
func entryOf(u netlink.NeighUpdate, name func(index int) string) (entry, bool) {
	if u.Type != unix.RTM_NEWNEIGH || len(u.HardwareAddr) == 0 {
		return entry{}, false
	}
	switch u.Family {
	case unix.AF_INET:
		addr, ok := netip.AddrFromSlice(u.IP)
		if !ok {
			return entry{}, false
		}
		return entry{addr: addr.Unmap(), mac: u.HardwareAddr}, true
	case unix.AF_BRIDGE:
		if u.MasterIndex == 0 {
			return entry{}, false
		}
		return entry{mac: u.HardwareAddr, bridge: name(u.MasterIndex), port: name(u.LinkIndex)}, true
	}
	return entry{}, false
}
