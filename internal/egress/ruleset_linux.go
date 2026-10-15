package egress

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// rulesetBuffer holds the largest notification nf_tables sends at once.
const rulesetBuffer = 1 << 16

// WatchRuleset calls changed whenever the nftables ruleset of the node
// changes, through the notifications nf_tables sends, until ctx ends. After
// the subscription failed, as when notifications came faster than they were
// read, it calls changed once more, since one may have been lost, and
// subscribes again. It returns nil when ctx ends, and an error when it cannot
// subscribe at all.
func WatchRuleset(ctx context.Context, changed func()) error {
	return watchRulesetIn(ctx, netns.None(), changed)
}

func watchRulesetIn(ctx context.Context, ns netns.NsHandle, changed func()) error {
	buf := make([]byte, rulesetBuffer)
	for first := true; ; first = false {
		s, err := subscribeRuleset(ns)
		if err != nil && first {
			return fmt.Errorf("watching the nftables ruleset: %w", err)
		}
		if err == nil {
			stop := context.AfterFunc(ctx, func() { _ = s.Close() })
			for {
				if _, err := s.Read(buf); err != nil {
					break
				}
				changed()
			}
			stop()
			_ = s.Close()
		}
		if ctx.Err() != nil {
			return nil
		}
		changed()
		t := time.NewTimer(resubscribeAfter)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// subscribeRuleset opens a netlink socket in ns that receives the
// notifications of nf_tables. The socket becomes a file only once it is bound:
// nl.Subscribe wraps it in one first and closes it by its number when the bind
// fails, as it does for a user without CAP_NET_ADMIN, and the file left behind
// closes that number again when it is collected, whatever it is by then.
func subscribeRuleset(ns netns.NsHandle) (*os.File, error) {
	var fd int
	err := gateway.Do(ns, func() error {
		var err error
		fd, err = unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_NETFILTER)
		if err != nil {
			return fmt.Errorf("opening a netlink socket: %w", err)
		}
		sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1 << (unix.NFNLGRP_NFTABLES - 1)}
		if err := unix.Bind(fd, sa); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("subscribing to the notifications of nf_tables: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "nftables notifications"), nil
}
