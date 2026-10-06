//go:build linux

package appnet

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// LinkAddrs are the global IPv4 addresses of the device, as the kernel lists
// them: the primary one first. A device that is not there has none.
func LinkAddrs(_ context.Context, dev string) ([]netip.Addr, error) {
	l, err := linkByName(dev)
	if err != nil || l == nil {
		return nil, err
	}
	addrs, err := netlink.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("listing the addresses of %s: %w", dev, err)
	}
	var out []netip.Addr
	for _, a := range addrs {
		if ip, ok := netip.AddrFromSlice(a.IP); ok && a.Scope == unix.RT_SCOPE_UNIVERSE {
			out = append(out, ip.Unmap())
		}
	}
	return out, nil
}

// WatchAddrs calls changed whenever an address of the container comes or
// goes, until ctx ends. When its subscription fails it subscribes again a
// second later and calls changed, so that no change goes unseen. It returns
// nil when ctx ends, and an error when it cannot subscribe at all.
func WatchAddrs(ctx context.Context, changed func()) error {
	for first := true; ; first = false {
		err := watchAddrsOnce(ctx, changed)
		switch {
		case ctx.Err() != nil:
			return nil
		case first && err != nil:
			return fmt.Errorf("watching the addresses: %w", err)
		}
		t := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
		changed()
	}
}

func watchAddrsOnce(ctx context.Context, changed func()) error {
	updates := make(chan netlink.AddrUpdate, 64)
	done := make(chan struct{})
	if err := netlink.AddrSubscribeWithOptions(updates, done, netlink.AddrSubscribeOptions{ReceiveBufferSize: 1 << 16}); err != nil {
		close(done)
		return err
	}
	defer func() {
		close(done)
		for range updates {
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-updates:
			if !ok {
				return nil
			}
			changed()
		}
	}
}
