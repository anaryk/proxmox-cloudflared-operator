package egress

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
)

// The test changes the ruleset of a network namespace of its own, and sees
// every change, a flush among them, within a second.
func TestLinuxWatchRuleset(t *testing.T) {
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create a network namespace and load nftables")
	}
	if _, err := os.Stat(nftPath); err != nil {
		requireLab(t, "nft is not installed: %v", err)
	}
	ns := newNamespace(t)
	changes := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- watchRulesetIn(ctx, ns, func() { changes <- struct{}{} }) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	nft := NewNft()
	apply := func(t *testing.T, script string) {
		t.Helper()
		var err error
		onThrowawayThread(func() {
			if err = netns.Set(ns); err == nil {
				err = nft.Apply(context.Background(), script)
			}
		})
		require.NoError(t, err)
	}
	seen := func(t *testing.T) {
		t.Helper()
		select {
		case <-changes:
		case <-time.After(time.Second):
			t.Fatal("no notification within a second")
		}
		for len(changes) > 0 {
			<-changes
		}
	}
	// Until the subscription is in place, a change may go unseen.
	require.Eventually(t, func() bool {
		apply(t, "add table inet pco_watch_probe\n")
		select {
		case <-changes:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}, 5*time.Second, time.Millisecond)
	for len(changes) > 0 {
		<-changes
	}

	t.Run("the egress table is loaded", func(t *testing.T) {
		apply(t, Base(labUID, nil, nil))
		seen(t)
	})
	t.Run("an element is deleted", func(t *testing.T) {
		apply(t, "add element inet pco_egress targets4 { 10.0.0.5 . 80 }\n")
		seen(t)
		apply(t, "delete element inet pco_egress targets4 { 10.0.0.5 . 80 }\n")
		seen(t)
	})
	t.Run("the ruleset is flushed", func(t *testing.T) {
		apply(t, "flush ruleset\n")
		seen(t)
	})
}
