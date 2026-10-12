package clusterapi

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type sourceFeed struct {
	mu    sync.Mutex
	src   Sources
	err   error
	calls int
}

func (f *sourceFeed) refresh(context.Context) (Sources, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.src, f.err
}

func (f *sourceFeed) set(s Sources) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.src = s
}

func (f *sourceFeed) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestTheAllowlistRefreshesOnAMissAtMostEveryTenSeconds(t *testing.T) {
	c := &clock{t: t0}
	feed := &sourceFeed{src: Sources{Nodes: map[string][]netip.Addr{testNode: {addr2}}}}
	l := NewAllowlist(feed.refresh, c.now, zerolog.Nop())
	require.True(t, l.Allowed(addr2, false), "a miss reads the sources at once")
	require.Equal(t, 1, feed.count())
	require.True(t, l.Allowed(addr2, false))
	require.False(t, l.Allowed(addr2, true), "a node is no join source")
	require.Equal(t, 1, feed.count(), "a hit does not")

	feed.set(Sources{Nodes: map[string][]netip.Addr{testNode: {addr2}, "pve3": {addr3}}, Join: []netip.Addr{loopback}})
	require.False(t, l.Allowed(addr3, false), "a miss within ten seconds of the last is a miss")
	require.Equal(t, 1, feed.count())
	c.advance(missGap - time.Millisecond)
	require.False(t, l.Allowed(addr3, false))
	c.advance(time.Millisecond)
	require.True(t, l.Allowed(addr3, false))
	require.Equal(t, 2, feed.count())
	require.True(t, l.Allowed(loopback, true))
	require.False(t, l.Allowed(loopback, false), "a join source is no node")

	t.Run("every cycle refreshes", func(t *testing.T) {
		feed.set(Sources{Nodes: map[string][]netip.Addr{"pve3": {addr3}}})
		require.NoError(t, l.Refresh(t.Context()))
		require.False(t, l.Allowed(addr2, false), "a node gone from the sources is dropped")
		require.True(t, l.Allowed(addr3, false))
		require.Equal(t, []netip.Addr{addr3}, l.Addrs("pve3"))
		require.Nil(t, l.Addrs(testNode))
	})
	t.Run("a refresh that fails keeps what was known", func(t *testing.T) {
		feed.mu.Lock()
		feed.err = errors.New("the members file could not be read")
		feed.mu.Unlock()
		require.Error(t, l.Refresh(t.Context()))
		require.True(t, l.Allowed(addr3, false))
		c.advance(missGap)
		require.False(t, l.Allowed(addr2, false))
		require.True(t, l.Allowed(addr3, false))
	})
}

func TestTheAllowlistIsSafeForConcurrentUse(t *testing.T) {
	c := &clock{t: t0}
	feed := &sourceFeed{src: Sources{Nodes: map[string][]netip.Addr{testNode: {addr2}}}}
	l := NewAllowlist(feed.refresh, c.now, zerolog.Nop())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				l.Allowed(addr2, false)
				l.Allowed(addr3, true)
				_ = l.Refresh(context.Background())
				l.Addrs(testNode)
				c.advance(time.Second)
			}
		})
	}
	wg.Wait()
}
