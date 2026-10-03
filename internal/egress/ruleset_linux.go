package egress

import (
	"context"
	"fmt"
	"time"

	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

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
	for first := true; ; first = false {
		s, err := nl.SubscribeAt(ns, netns.None(), unix.NETLINK_NETFILTER, unix.NFNLGRP_NFTABLES)
		if err != nil && first {
			return fmt.Errorf("watching the nftables ruleset: %w", err)
		}
		if err == nil {
			stop := context.AfterFunc(ctx, s.Close)
			for {
				if _, _, err := s.Receive(); err != nil {
					break
				}
				changed()
			}
			stop()
			s.Close()
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
