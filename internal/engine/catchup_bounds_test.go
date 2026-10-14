package engine

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// A report that names more service addresses than any gateway maps is not
// taken at all: it costs a cycle no work beyond its count.
func TestAReportOfTooManyServiceAddressesIsNotTaken(t *testing.T) {
	w := threeNodes(t)
	addrs := make([]netip.Addr, 0, maxReported+1)
	for a := netip.MustParseAddr("198.18.1.0"); len(addrs) <= maxReported; a = a.Next() {
		addrs = append(addrs, a)
	}
	w.acts["pve2"].edit = func(st *agentproto.Status) { st.ServiceAddrs = addrs }

	w.tick(time.Second)

	next, _, _, err := w.store.ServicePool()
	require.NoError(t, err)
	require.Equal(t, netip.MustParseAddr("198.18.1.0"), next)
	require.Contains(t, w.eventMessages(), fmt.Sprintf("node pve2 reports that its gateway maps %d service addresses, more than %d; none of them is taken",
		maxReported+1, maxReported))
}

// The leases a takeover saves again are bounded per cycle, and only for an
// address a guest's card may have: the subnet's own address, pco's leg and
// the broadcast address are no card's.
func TestATakeoverSavesALimitedNumberOfLeasesPerCycle(t *testing.T) {
	w, _ := legWorld(t)
	snap := w.inv.snapshot()
	for i := range maxLeasesAgain + 6 {
		addr := netip.AddrFrom4([4]byte{10, 203, 118, byte(30 + i)})
		g := carded(guest(200+i, fmt.Sprintf("g%d", i)), "pve3", fmt.Sprintf("bc:24:11:00:09:%02x", i))
		g.NICs = append(g.NICs, model.NIC{Index: 1, MAC: store.ManagedMAC(addr), Bridge: gateway.ManagedBridge})
		snap.Guests = append(snap.Guests, g)
	}
	leg := carded(guest(400, "leg"), "pve3", "bc:24:11:00:0a:01")
	leg.NICs = append(leg.NICs, model.NIC{Index: 1, MAC: store.ManagedMAC(netip.MustParseAddr("10.203.118.1")), Bridge: gateway.ManagedBridge})
	snap.Guests = append(snap.Guests, leg)
	w.inv.set(snap)
	before, err := w.store.Leases()
	require.NoError(t, err)

	w.tick(time.Second)

	leases, err := w.store.Leases()
	require.NoError(t, err)
	require.Len(t, leases, len(before)+maxLeasesAgain, "no more than the bound in one cycle")

	w.tick(time.Second)

	leases, err = w.store.Leases()
	require.NoError(t, err)
	require.Len(t, leases, len(before)+maxLeasesAgain+6, "the rest in the next")
	require.NotContains(t, leases, store.ManagedMAC(netip.MustParseAddr("10.203.118.1")), "the leg's address is no card's")
}
