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

// ObserveForwarding starts from a full dump of the forwarding tables, every
// bridge's, then follows their changes, until ctx ends, and reports what they
// show of the expected MACs: Learned for one on its own guest's port wherever
// the snapshot says the guest runs, Moved for one that left it, Conflict for
// one on a port of another guest, and Gone when the port the last Learned or
// Conflict named no longer has it. Each subscription begins with Reset, the
// dump after it, and ends with one: what was reported before a Reset holds
// no more.
//
// It reads the notifications Watch reads, on a subscription of its own, and
// like Watch subscribes again, with a dump, after the subscription fails. A
// MAC an impostor took while no observer ran is reported the moment one runs
// again, for as long as the bridge keeps the entry.
//
// expected is asked at every entry, and report is called from the one
// goroutine that reads the notifications: neither may wait for anything slow,
// or the notifications queue up until the kernel drops some. It returns nil
// when ctx ends, and an error when it cannot subscribe at all.
func ObserveForwarding(ctx context.Context, expected func() map[string]Expected, report func(Observation)) error {
	return observeIn(ctx, netns.None(), expected, report)
}

// watchIn watches the tables of the network namespace ns.
func watchIn(ctx context.Context, ns netns.NsHandle, bound func() map[netip.Addr]Pin, onMove func(netip.Addr)) error {
	return follow(ctx, ns, "watching the network", resubscribeAfter, func(h *netlink.Handle) error {
		return readTables(ctx, ns, h, func(u netlink.NeighUpdate, name func(index int) string) {
			e, ok := entryOf(u, name)
			if !ok {
				return
			}
			for _, addr := range e.moved(bound()) {
				onMove(addr)
			}
		})
	})
}

// observeIn observes the forwarding tables of the network namespace ns.
func observeIn(ctx context.Context, ns netns.NsHandle, expected func() map[string]Expected, report func(Observation)) error {
	o := newObserver(expected, report, time.Now)
	return follow(ctx, ns, "watching the forwarding tables", resubscribeAfter, func(h *netlink.Handle) error {
		// A reset before the dump, and one the moment the subscription
		// ends: what was reported holds no more once the notifications
		// stop, whether or not a subscription is made again.
		o.reset()
		err := readTables(ctx, ns, h, func(u netlink.NeighUpdate, name func(index int) string) {
			if e, ok := forwardingOf(u, name); ok {
				o.see(e)
			}
		})
		o.reset()
		return err
	})
}

// follow runs once, which reads one subscription, and again after each time
// the subscription failed, after waiting for again, until ctx ends. A first
// subscription that cannot be made is an error that starts with what.
func follow(ctx context.Context, ns netns.NsHandle, what string, again time.Duration, once func(h *netlink.Handle) error) error {
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	defer h.Close()
	for first := true; ; first = false {
		err := once(h)
		switch {
		case ctx.Err() != nil:
			return nil
		case first && !errors.Is(err, errUnsubscribed):
			return fmt.Errorf("%s: %w", what, err)
		}
		t := time.NewTimer(again)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// subscription is how every watch subscribes to the notifications of the
// neighbour tables of ns: asking for what they hold first, the neighbour
// tables and then the forwarding tables of every bridge, before the changes
// that follow. A watch that did not would see nothing of what moved while it
// was not running.
func subscription(ns netns.NsHandle) netlink.NeighSubscribeOptions {
	return netlink.NeighSubscribeOptions{
		Namespace:         &ns,
		ListExisting:      true,
		ReceiveBufferSize: watchBuffer,
	}
}

// readTables reads one subscription, which begins with what the tables hold,
// and calls each with every update, with a way to name the devices it
// mentions, until ctx ends or the subscription does.
func readTables(ctx context.Context, ns netns.NsHandle, h *netlink.Handle, each func(u netlink.NeighUpdate, name func(index int) string)) error {
	updates := make(chan netlink.NeighUpdate, watchQueue)
	done := make(chan struct{})
	if err := netlink.NeighSubscribeWithOptions(updates, done, subscription(ns)); err != nil {
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
			each(u, name)
		}
	}
}

// entryOf reads an update that says where a MAC is now: an IPv4 neighbour on
// a device, or an entry a bridge learned on one of its ports. A port whose
// name cannot be found counts as another port, and a device whose name cannot
// be found as no leg.
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
		return entry{addr: addr.Unmap(), dev: name(u.LinkIndex), mac: u.HardwareAddr}, true
	case unix.AF_BRIDGE:
		return forwardingOf(u, name)
	}
	return entry{}, false
}

// forwardingOf reads an update of a bridge's forwarding table: a MAC the
// bridge learned on a port, or dropped. It says which bridge, port and VLAN
// as far as their names are known. An entry of a device that is no bridge's
// port, as a VXLAN device's own, has no master and is none of it.
func forwardingOf(u netlink.NeighUpdate, name func(index int) string) (entry, bool) {
	if u.Family != unix.AF_BRIDGE || len(u.HardwareAddr) == 0 || u.MasterIndex == 0 {
		return entry{}, false
	}
	if u.Type != unix.RTM_NEWNEIGH && u.Type != unix.RTM_DELNEIGH {
		return entry{}, false
	}
	return entry{
		mac: u.HardwareAddr, bridge: name(u.MasterIndex), port: name(u.LinkIndex),
		vlan: u.Vlan, gone: u.Type == unix.RTM_DELNEIGH,
	}, true
}
