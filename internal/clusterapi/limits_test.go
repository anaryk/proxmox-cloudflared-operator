package clusterapi

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

func TestTheJoinRateIsTenPerMinutePerSource(t *testing.T) {
	c := &clock{t: t0}
	r := newRateLimiter(joinPerMinute, time.Minute)
	for i := range joinPerMinute {
		require.True(t, r.allow(addr2, c.now()), "attempt %d", i)
	}
	require.False(t, r.allow(addr2, c.now()))
	require.True(t, r.allow(addr3, c.now()), "another source has its own minute")

	c.advance(time.Minute - time.Second)
	require.False(t, r.allow(addr2, c.now()))
	c.advance(time.Second)
	for i := range joinPerMinute {
		require.True(t, r.allow(addr2, c.now()), "the attempts are a minute old: %d", i)
	}
	require.False(t, r.allow(addr2, c.now()))

	t.Run("sources that fell silent are forgotten", func(t *testing.T) {
		for i := range 2000 {
			r.allow(netip.AddrFrom4([4]byte{10, 1, byte(i / 256), byte(i % 256)}), c.now())
		}
		c.advance(2 * time.Minute)
		r.allow(addr2, c.now())
		require.Less(t, r.size(), 10)
	})
}

func TestTheResendIsCountedPerNodeAndEpoch(t *testing.T) {
	r := newResendCounter()
	epoch2 := agentproto.Epoch{InstallID: testInstall, Generation: 2, Nonce: "n2"}
	require.False(t, r.over(testNode, epoch1, 500, true))
	for range 19 {
		require.False(t, r.over(testNode, epoch1, 500, true))
	}
	require.True(t, r.over(testNode, epoch1, 1, true), "the 10001st event of the resend")
	require.False(t, r.over(testNode, epoch1, 500, false), "ordinary batches are not of the resend")
	require.True(t, r.over(testNode, epoch1, 1, true), "the count stands within the epoch")
	require.Equal(t, 10002, r.count(testNode))
	require.False(t, r.over("pve3", epoch1, 500, true), "each node its own")
	require.False(t, r.over(testNode, epoch2, 500, true), "each epoch its own")
	require.Equal(t, 500, r.count(testNode))
	require.False(t, r.over(testNode, epoch1, 1, true), "an epoch seen again starts over as well")
}
