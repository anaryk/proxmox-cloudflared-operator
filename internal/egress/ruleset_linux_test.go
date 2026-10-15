package egress

import (
	"context"
	"os"
	"runtime"
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

// A user that may not subscribe, as one without CAP_NET_ADMIN, is told so,
// and what the attempt opened is not closed once more when it is collected:
// by then its number may be another file's, such as a connection to the API.
func TestLinuxAWatchThatCannotSubscribeClosesNothingLater(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := WatchRuleset(ctx, func() {}); err == nil {
		t.Skip("this user may subscribe to the notifications of nf_tables")
	}
	var files []*os.File
	for range 8 {
		f, err := os.Open(os.DevNull)
		require.NoError(t, err)
		t.Cleanup(func() { _ = f.Close() })
		files = append(files, f)
	}
	// Two rounds, so that what the first collection found is finalized
	// before the second one's mark is.
	for range 2 {
		collected := make(chan struct{})
		runtime.SetFinalizer(new([64]byte), func(*[64]byte) { close(collected) })
		runtime.GC()
		<-collected
	}
	for _, f := range files {
		_, err := f.Stat()
		require.NoError(t, err, "a file the test opened after the attempt was closed")
	}
}
