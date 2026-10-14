package engine

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestTheAgentAddressOfANodeComesFromTheRegistry(t *testing.T) {
	e, _ := onHost(t)
	addr := netip.MustParseAddr("10.9.0.2")
	require.NoError(t, e.store.SaveNode(store.NodeEntry{Name: "pve2", Since: t0, Kind: "host", AgentAddress: addr}))
	c := &cycleRun{e: e.eng}
	require.Equal(t, addr, c.agentAddress("pve2"))
	require.False(t, c.agentAddress("pve3").IsValid(), "a node without an entry has no address")
	require.False(t, c.agentAddress(testNode).IsValid(), "an entry without an address gives none")
}

func TestTheLeaseViewOfANodeThatDoesNotHoldIt(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 10, 0, 0, time.UTC)
	since := now.Add(-10 * time.Minute)
	addr := netip.MustParseAddr("10.9.0.2")
	addrOf := func(node string) netip.Addr {
		if node == "node2" {
			return addr
		}
		return netip.Addr{}
	}
	rec := store.LeaderRecord{Writer: planner.Writer{Generation: 7, Nonce: "abc"}, Node: "node2", AcquiredAt: since}

	t.Run("another node holds it", func(t *testing.T) {
		v := leaseView(lease.State{Waiting: "the lock is held", Record: rec}, now, addrOf)
		require.Equal(t, &LeaseView{Holder: "node2", Address: addr, Generation: 7, AcquiredAt: since, Fenced: "the lock is held"}, v,
			"the holder, its address and its generation, never its nonce or a validity this node cannot judge")
	})
	t.Run("the holder has no registered address", func(t *testing.T) {
		v := leaseView(lease.State{Waiting: "the lock is held", Record: rec}, now, func(string) netip.Addr { return netip.Addr{} })
		require.Equal(t, "node2", v.Holder)
		require.False(t, v.Address.IsValid())
	})
	t.Run("the lease was released", func(t *testing.T) {
		released := rec
		released.ReleasedAt = now.Add(-time.Minute)
		v := leaseView(lease.State{Waiting: "the lock is gone", Record: released}, now, addrOf)
		require.Equal(t, &LeaseView{Fenced: "the lock is gone"}, v)
	})
	t.Run("nothing read yet", func(t *testing.T) {
		v := leaseView(lease.State{Waiting: "the lease keeper has not tried yet"}, now, addrOf)
		require.Equal(t, &LeaseView{Fenced: "the lease keeper has not tried yet"}, v)
	})
	t.Run("the holder's own view carries its address too", func(t *testing.T) {
		own := rec
		own.Node = "node1"
		v := leaseView(lease.State{Holder: true, Valid: true, Writer: own.Writer, Record: own, LastRefresh: now.Add(-3 * time.Second)}, now,
			func(string) netip.Addr { return netip.MustParseAddr("10.9.0.1") })
		require.Equal(t, "node1", v.Holder)
		require.Equal(t, netip.MustParseAddr("10.9.0.1"), v.Address)
		require.Equal(t, "abc", v.Nonce)
		require.True(t, v.Valid)
		require.Equal(t, 3*time.Second, v.RefreshAge)
	})
}
