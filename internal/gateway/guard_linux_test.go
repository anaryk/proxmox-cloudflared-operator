//go:build linux

package gateway

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
)

// legNS makes a namespace that plays the leg: eth0 on the bridge end
// vethpco<mark> of the bridge, with the address.
func (l *labNet) legNS(bridge, port, cidr string) netns.NsHandle {
	l.t.Helper()
	ns := "pcotest-leg-" + l.id
	l.ip("netns", "add", ns)
	l.t.Cleanup(func() { _ = exec.Command(ipPath, "netns", "del", ns).Run() })
	l.ip("-n", l.hostName, "link", "add", port, "type", "veth", "peer", "name", "eth0", "netns", ns)
	l.ip("-n", l.hostName, "link", "set", port, "master", bridge)
	l.ip("-n", l.hostName, "link", "set", port, "up")
	l.ip("-n", ns, "link", "set", "lo", "up")
	l.ip("-n", ns, "addr", "add", cidr, "dev", "eth0")
	l.ip("-n", ns, "link", "set", "eth0", "up")
	h, err := netns.GetFromName(ns)
	require.NoError(l.t, err)
	l.t.Cleanup(func() { _ = h.Close() })
	return h
}

// A guest that sends with another guest's MAC and address, from its own
// port, reaches nothing, nor with its own MAC and another's address, nor
// with ARP that claims another's address; each guest with its own card
// reaches the leg.
func TestLinuxTheGuardKeepsEachCardToItsPort(t *testing.T) {
	l := newLabNet(t)
	l.bridge(ManagedBridge, false)
	leg := l.legNS(ManagedBridge, "vethpco3", "10.203.118.1/24")
	serve(t, leg, "0.0.0.0:80", "leg")
	a := l.guest("a", ManagedBridge, 0, "10.203.118.2/24", "")
	b := l.guest("b", ManagedBridge, 0, "10.203.118.3/24", "")
	macA, err := macIn(a, "eth0")
	require.NoError(t, err)
	macB, err := macIn(b, "eth0")
	require.NoError(t, err)
	cards := []Sticky{
		{MAC: macA, Bridge: ManagedBridge, Port: "ga", Addr: addr("10.203.118.2")},
		{MAC: macB, Bridge: ManagedBridge, Port: "gb", Addr: addr("10.203.118.3")},
	}
	require.NoError(t, l.hostNft.Apply(t.Context(), renderGuard(guardElements(cards))))
	reaches := func(ns netns.NsHandle) bool {
		got, err := get(ns, "10.203.118.1:80", nil, time.Second)
		return err == nil && strings.HasPrefix(got, "leg ")
	}
	require.Eventually(t, func() bool { return reaches(a) }, 5*time.Second, 50*time.Millisecond, "a guest with its own card")
	require.True(t, reaches(b))
	spoofed := func() uint64 { return counter(t, l.hostNft, guardFamilyName, counterSpoofed) }

	// b takes a's address, then a's MAC as well.
	bName := "pcotest-b-" + l.id
	l.ip("-n", bName, "addr", "flush", "dev", "eth0")
	l.ip("-n", bName, "addr", "add", "10.203.118.2/24", "dev", "eth0")
	before := spoofed()
	require.False(t, reaches(b), "another's address")
	require.Greater(t, spoofed(), before)

	l.ip("-n", bName, "link", "set", "eth0", "address", macA.String())
	before = spoofed()
	require.False(t, reaches(b), "another's MAC and address")
	require.Greater(t, spoofed(), before)

	before = spoofed()
	f := arpProbeFrame(macA, addr("10.203.118.9"))
	copy(f[28:32], addr("10.203.118.2").AsSlice())
	sendFrame(t, b, f)
	require.Eventually(t, func() bool { return spoofed() > before }, time.Second, 10*time.Millisecond, "an ARP frame that claims a's address")

	l.ip("-n", bName, "link", "set", "eth0", "address", macB.String())
	require.Eventually(t, func() bool { return reaches(a) }, 5*time.Second, 50*time.Millisecond, "the guest whose card it is still does")
}

// While a guest's port is absent its entry waits on the leg's bridge end,
// where a forged frame does not move it, and it moves to the port when the
// port appears.
func TestLinuxAnEntryWaitsOnTheLegAndMovesToItsPort(t *testing.T) {
	l := newLabNet(t)
	l.bridge(ManagedBridge, false)
	spoofer := l.guest("s", ManagedBridge, 0, "10.203.118.9/24", "")
	require.NoError(t, l.b.Up(t.Context()))
	leg := Leg{ID: ManagedBridge, Mark: 3, Bridge: ManagedBridge, Prefix: prefix("10.203.118.1/24"), MAC: ManagedLegMAC, Managed: true}
	guest := mac("06:50:43:4f:76:05")
	sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap9301i1", Addr: addr("10.203.118.5")}}
	_, err := l.b.Sync(t.Context(), []Leg{leg}, nil, nil, sticky)
	require.NoError(t, err)
	ports := []string{"vethpco3", "gs"}
	p, isSticky := l.entryPort(t, ports, guest)
	require.Equal(t, "vethpco3", p, "parked on the leg")
	require.True(t, isSticky)

	for range 5 {
		sendFrom(t, spoofer, guest)
	}
	require.Never(t, func() bool { p, _ := l.entryPort(t, ports, guest); return p != "vethpco3" }, 300*time.Millisecond, 10*time.Millisecond,
		"a forged frame takes the parked MAC over")

	l.ip("-n", l.hostName, "link", "add", "tap9301i1", "type", "veth", "peer", "name", "p9301")
	l.ip("-n", l.hostName, "link", "set", "tap9301i1", "master", ManagedBridge)
	require.NoError(t, ApplySticky(t.Context(), l.b, sticky))
	p, isSticky = l.entryPort(t, append(ports, "tap9301i1"), guest)
	require.Equal(t, "tap9301i1", p, "moved to its port")
	require.True(t, isSticky)
	have, err := l.sys.fdb("vethpco3")
	require.NoError(t, err)
	for _, e := range have {
		require.False(t, macEqual(e.mac, guest), "left on the leg")
	}
	require.NoError(t, l.b.Verify(t.Context()))
}
