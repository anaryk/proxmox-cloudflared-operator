package gateway

import (
	"cmp"
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTheGuestPortOfACard(t *testing.T) {
	for _, c := range []struct {
		name      string
		container bool
		firewall  bool
		want      string
	}{
		{"a virtual machine", false, false, "tap101i1"},
		{"a container", true, false, "veth101i1"},
		{"a virtual machine with the firewall", false, true, "fwpr101p1"},
		{"a container with the firewall", true, true, "fwpr101p1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, GuestPort(c.container, 101, 1, c.firewall))
		})
	}
}

func TestWhatIsTheNameOfAGuestPort(t *testing.T) {
	for name, want := range map[string]bool{
		"tap101i1":   true,
		"veth9305i0": true,
		"fwpr9302p0": true,
		"tap101":     false,
		"tap101i":    false,
		"tapi1":      false,
		"tap101i1x":  false,
		"veth101p1":  false,
		"fwpr101i1":  false,
		"fwbr101i1":  false,
		"vethpco3":   false,
		"vethpcop":   false,
		"leg3":       false,
		"pconet":     false,
		"":           false,
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, IsGuestPort(name))
		})
	}
}

// A port of a guest that appears between two cycles gets its entry from
// ApplySticky, which changes nothing else of the gateway.
func TestAPortThatAppearsLaterGetsItsEntry(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	guest := mac("06:50:43:4f:76:05")
	sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}}
	h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.NoError(t, h.b.Verify(t.Context()), "an absent port is no difference")

	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, carrier: true, master: ManagedBridge}}
	h.reset()
	require.NoError(t, ApplySticky(t.Context(), h.b, sticky))
	require.Equal(t, []string{"sticky tap101i1 06:50:43:4f:76:05 vlan 0", "fdb del vethpco3 06:50:43:4f:76:05 vlan 0"}, h.sys.changes(),
		"moved from the leg, where it waited")
	require.Empty(t, h.nsNft.scripts, "nothing of the namespace is touched")
	require.Empty(t, h.hostNft.scripts)
	require.NoError(t, h.b.Verify(t.Context()))

	h.reset()
	require.NoError(t, ApplySticky(t.Context(), h.b, sticky))
	require.Empty(t, h.sys.changes(), "an entry that is there is left alone")
}

func TestApplyStickyRemovesTheEntryOfALeaseThatWent(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, carrier: true, master: ManagedBridge}}
	guest := mac("06:50:43:4f:76:05")
	h.sync(t, []Leg{legM}, nil, nil, []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}})
	h.reset()

	require.NoError(t, ApplySticky(t.Context(), h.b, nil))
	require.Equal(t, []string{"fdb del tap101i1 06:50:43:4f:76:05 vlan 0"}, h.sys.changes())
	require.NoError(t, h.b.Verify(t.Context()), "what was applied last is what Verify compares with")
}

func TestVerifyNamesAMissingStickyEntry(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sys.host.links["veth102i1"] = &fakeLink{link: link{name: "veth102i1", kind: "veth", up: true, carrier: true, master: ManagedBridge}}
	guest := mac("06:50:43:4f:76:06")
	h.sync(t, []Leg{legM}, nil, nil, []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "veth102i1"}})
	require.NoError(t, h.b.Verify(t.Context()))

	h.sys.host.links["veth102i1"].fdb = nil
	err := h.b.Verify(t.Context())
	var changed *ChangedError
	require.True(t, errors.As(err, &changed), "%v", err)
	require.Contains(t, strings.Join(changed.Differences, "\n"), "the sticky entry of 0650434f7606 on veth102i1 is missing")

	h.reset()
	require.NoError(t, ApplySticky(t.Context(), h.b, []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "veth102i1"}}))
	require.NoError(t, h.b.Verify(t.Context()))
}

func TestApplyStickyEndsWithItsContext(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, ApplySticky(ctx, h.b, nil), ctx.Err())
	require.Empty(t, h.sys.changes())
}

// The appliance's backend keeps no sticky entries: the bridge is the node's.
func TestApplyStickyIsNothingToABackendWithoutEntries(t *testing.T) {
	require.NoError(t, ApplySticky(t.Context(), noSticky{}, []Sticky{{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap101i1"}}))
}

type noSticky struct{ Backend }

// While a guest's port is absent its entry is parked on the bridge end of
// the managed leg, so that no other port takes the MAC over meanwhile: the
// frames to it end in the namespace, which is not their destination. The
// entry moves to the port when it appears, and goes with the lease.
func TestAnEntryWaitsOnTheLegWhileItsPortIsAbsent(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	guest := mac("06:50:43:4f:76:05")
	sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}}
	h.sync(t, []Leg{legM}, nil, nil, nil)
	h.reset()

	h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.Equal(t, []string{"sticky vethpco3 06:50:43:4f:76:05 vlan 0"}, h.sys.changes(), "parked on the leg")
	require.NoError(t, h.b.Verify(t.Context()))

	h.sys.host.links["vethpco3"].fdb = slices.DeleteFunc(h.sys.host.links["vethpco3"].fdb, func(e fdbEntry) bool { return macEqual(e.mac, guest) })
	require.ErrorIs(t, h.b.Verify(t.Context()), ErrChanged, "a parked entry that went is a difference")
	h.reset()
	require.NoError(t, ApplySticky(t.Context(), h.b, sticky))
	require.Equal(t, []string{"sticky vethpco3 06:50:43:4f:76:05 vlan 0"}, h.sys.changes())

	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, carrier: true, master: ManagedBridge}}
	h.reset()
	require.NoError(t, ApplySticky(t.Context(), h.b, sticky))
	require.Equal(t, []string{"sticky tap101i1 06:50:43:4f:76:05 vlan 0", "fdb del vethpco3 06:50:43:4f:76:05 vlan 0"}, h.sys.changes(),
		"moved to its port when it appears")
	require.NoError(t, h.b.Verify(t.Context()))

	delete(h.sys.host.links, "tap101i1")
	h.reset()
	require.NoError(t, ApplySticky(t.Context(), h.b, sticky))
	require.Equal(t, []string{"sticky vethpco3 06:50:43:4f:76:05 vlan 0"}, h.sys.changes(), "parked again when the guest stops")

	h.reset()
	require.NoError(t, ApplySticky(t.Context(), h.b, nil))
	require.Equal(t, []string{"fdb del vethpco3 06:50:43:4f:76:05 vlan 0"}, h.sys.changes(), "gone with the lease")
	require.NotContains(t, strings.Join(h.sys.changes(), "\n"), "02:50:43:4f:ff:01", "the leg's own entry stays")
}

// A guest that starts makes its port before the port joins the bridge; the
// kernel takes no entry for the bridge on it until then. Such a port is not
// there yet for the entry, which stays parked: no error, and no difference
// for Verify, which would have the keeper bring back what nobody changed.
// Nothing opens meanwhile: the MAC stays pinned to the leg, where frames to
// it end, and the guard admits from the port only the card's own pair.
func TestAnEntryWaitsWhileItsPortIsNotOnTheBridgeYet(t *testing.T) {
	for _, master := range []string{"", "fwbr101i1"} {
		t.Run("master "+cmp.Or(master, "none"), func(t *testing.T) {
			h := newHarness(t).node()
			h.up(t)
			guest := mac("06:50:43:4f:76:05")
			sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "veth101i1", Addr: addr("10.203.118.10")}}
			h.sync(t, []Leg{legM}, nil, nil, sticky)
			h.sys.host.links["veth101i1"] = &fakeLink{link: link{name: "veth101i1", kind: "veth", up: true, master: master}}
			h.reset()

			h.sync(t, []Leg{legM}, nil, nil, sticky)
			require.Empty(t, h.sys.changes(), "still parked on the leg")
			require.True(t, parked(h, guest), "the MAC stays pinned to the leg")
			require.Equal(t, []string{`"veth101i1" . 06:50:43:4f:76:05 . 10.203.118.10`}, guardElements(sticky)[setCards])
			require.NoError(t, h.b.Verify(t.Context()))
			require.NoError(t, ApplySticky(t.Context(), h.b, sticky))
			require.Empty(t, h.sys.changes())
			require.True(t, parked(h, guest))

			h.sys.host.links["veth101i1"].master = ManagedBridge
			require.ErrorIs(t, h.b.Verify(t.Context()), ErrChanged, "a port on the bridge without its entry is a difference")
			require.NoError(t, ApplySticky(t.Context(), h.b, sticky))
			require.Equal(t, []string{"sticky veth101i1 06:50:43:4f:76:05 vlan 0", "fdb del vethpco3 06:50:43:4f:76:05 vlan 0"}, h.sys.changes(),
				"moved to its port once the port is on the bridge")
			require.NoError(t, h.b.Verify(t.Context()))
		})
	}
}

// Only the entry waits for a port that is not on the bridge yet: the guard
// admits from the port its card's pair alone in the same Sync, before the
// port can carry a frame, and a guard that cannot be loaded fails the Sync
// as ever. The entry stays in what Verify compares with, so the port that
// joins without it is a difference.
func TestAPendingPortWaitsForNothingButItsEntry(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	guest := mac("06:50:43:4f:76:05")
	sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "veth101i1", Addr: addr("10.203.118.10")}}
	h.sync(t, []Leg{legM}, nil, nil, nil)
	h.sys.host.links["veth101i1"] = &fakeLink{link: link{name: "veth101i1", kind: "veth", up: true}}

	h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.Equal(t, guardElements(sticky)[setCards], h.hostNft.tables[guardFamilyName].elems[setCards], "the card is in the guard already")
	require.True(t, parked(h, guest))

	h.hostNft.fail = errors.New("nft: no memory")
	_, err := h.b.Sync(t.Context(), []Leg{legM}, nil, nil, append(sticky, Sticky{MAC: mac("06:50:43:4f:76:06"), Bridge: ManagedBridge, Port: "veth102i1", Addr: addr("10.203.118.11")}))
	require.ErrorContains(t, err, "loading table bridge pco_guard")
	h.hostNft.fail = nil
	h.sync(t, []Leg{legM}, nil, nil, sticky)

	h.sys.host.links["veth101i1"].master = ManagedBridge
	err = h.b.Verify(t.Context())
	var changed *ChangedError
	require.True(t, errors.As(err, &changed), "%v", err)
	require.Contains(t, strings.Join(changed.Differences, "\n"), "the sticky entry of 0650434f7605 on veth101i1 is missing")
}

// A port that stays off its bridge for longer than a guest takes to start,
// as a card that runs behind its firewall bridge, is said on the managed
// leg; it is no difference that bringing the gateway back could mend.
func TestAPortPendingForLongIsSaidOnTheManagedLeg(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	sticky := []Sticky{{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap101i1"}}
	h.sync(t, []Leg{legM}, nil, nil, sticky)
	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, master: "fwbr101i1"}}

	st := h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.Empty(t, st[0].Problem, "a moment is no matter")
	h.now = h.now.Add(2 * time.Minute)
	st = h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.Equal(t, "the port tap101i1 of the card 06:50:43:4f:76:05 is not a port of pconet since 2026-10-06T12:00:00Z; its sticky entry waits on the leg", st[0].Problem)
	h.now = h.now.Add(time.Minute)
	require.Equal(t, st, h.sync(t, []Leg{legM}, nil, nil, sticky), "the same words a cycle later")
	require.NoError(t, h.b.Verify(t.Context()))

	h.sys.host.links["tap101i1"].master = ManagedBridge
	st = h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.Empty(t, st[0].Problem)

	delete(h.sys.host.links, "tap101i1")
	h.now = h.now.Add(10 * time.Minute)
	st = h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.Empty(t, st[0].Problem, "a stopped guest has no port, and that is no matter")
}

// parked reports whether the guest's MAC has its sticky entry on the bridge
// end of the managed leg.
func parked(h *harness, guest net.HardwareAddr) bool {
	return slices.ContainsFunc(h.sys.host.links["vethpco3"].fdb, func(e fdbEntry) bool { return stickyOf(e, guest, 0) })
}

// A port that leaves the bridge between its read and the entry's add, as
// when its guest stops or starts again right then, is pending too: the Sync
// goes on with the entry parked, and the next one or the port's event moves
// it.
func TestAPortThatLeavesTheBridgeWhileItsEntryIsAddedIsPending(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	guest := mac("06:50:43:4f:76:05")
	sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}}
	h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.True(t, parked(h, guest))
	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, master: ManagedBridge}}
	h.sys.before = func(line string) {
		if strings.HasPrefix(line, "sticky tap101i1") {
			h.sys.host.links["tap101i1"].master = ""
		}
	}
	h.reset()

	h.sync(t, []Leg{legM}, nil, nil, sticky)
	require.NotContains(t, strings.Join(h.sys.changes(), "\n"), "sticky tap101i1")
	require.True(t, parked(h, guest), "still pinned to the leg")
	require.NoError(t, h.b.Verify(t.Context()))
}

// A guest's port on a bridge other than pconet is no port that is late: Sync
// fails on it and Verify names it.
func TestAPortOnAnotherBridgeIsADifference(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	guest := mac("06:50:43:4f:76:05")
	sticky := []Sticky{{MAC: guest, Bridge: ManagedBridge, Port: "tap101i1"}}
	h.sync(t, []Leg{legM}, nil, nil, sticky)
	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, master: "vmbr0"}}

	err := h.b.Verify(t.Context())
	var changed *ChangedError
	require.True(t, errors.As(err, &changed), "%v", err)
	require.Contains(t, strings.Join(changed.Differences, "\n"), "tap101i1 is a port of vmbr0, not of pconet")
	_, err = h.b.Sync(t.Context(), []Leg{legM}, nil, nil, sticky)
	require.ErrorContains(t, err, "tap101i1 is a port of vmbr0, not of pconet")
	require.True(t, parked(h, guest), "the MAC stays pinned to the leg")
}

// One card whose entry cannot be placed costs the others nothing: they get
// their entries, the guard is loaded with every card, and the error of the
// one is returned after.
func TestOneCardThatFailsLeavesTheOthersTheirEntriesAndTheGuard(t *testing.T) {
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "Sync", true: "ApplySticky"}[apply], func(t *testing.T) {
			h := newHarness(t).node()
			h.up(t)
			h.sync(t, []Leg{legM}, nil, nil, nil)
			h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, master: "vmbr0"}}
			h.sys.host.links["veth102i1"] = &fakeLink{link: link{name: "veth102i1", kind: "veth", up: true, master: ManagedBridge}}
			h.sys.host.links["veth104i1"] = &fakeLink{link: link{name: "veth104i1", kind: "veth", up: true, master: ManagedBridge}}
			h.sys.fail["sticky veth104i1"] = syscall.ENOBUFS
			sticky := []Sticky{
				{MAC: mac("06:50:43:4f:76:02"), Bridge: ManagedBridge, Port: "tap101i1", Addr: addr("10.203.118.2")},
				{MAC: mac("06:50:43:4f:76:03"), Bridge: ManagedBridge, Port: "veth102i1", Addr: addr("10.203.118.3")},
				{MAC: mac("06:50:43:4f:76:04"), Bridge: ManagedBridge, Port: "tap103i1", Addr: addr("10.203.118.4")},
				{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "veth104i1", Addr: addr("10.203.118.5")},
			}
			// The leg's end already holds the parked entry of veth104i1's card,
			// which its failure must not take away.
			require.NoError(t, h.sys.addSticky("vethpco3", mac("06:50:43:4f:76:05"), 0))
			delete(h.hostNft.tables, guardFamilyName)

			var err error
			if apply {
				err = ApplySticky(t.Context(), h.b, sticky)
			} else {
				_, err = h.b.Sync(t.Context(), []Leg{legM}, nil, nil, sticky)
			}

			require.ErrorContains(t, err, "tap101i1 is a port of vmbr0, not of pconet")
			require.ErrorIs(t, err, syscall.ENOBUFS)
			require.True(t, slices.ContainsFunc(h.sys.host.links["veth102i1"].fdb, func(e fdbEntry) bool { return stickyOf(e, mac("06:50:43:4f:76:03"), 0) }))
			require.True(t, parked(h, mac("06:50:43:4f:76:04")), "an absent port waits on the leg")
			require.True(t, parked(h, mac("06:50:43:4f:76:02")), "a port on another bridge leaves its MAC pinned to the leg")
			require.True(t, parked(h, mac("06:50:43:4f:76:05")), "an entry that failed keeps the one it had")
			require.NotNil(t, h.hostNft.tables[guardFamilyName], "the guard is loaded")
			require.Equal(t, guardElements(sticky)[setCards], h.hostNft.tables[guardFamilyName].elems[setCards])
		})
	}
}

// An entry that cannot be added on a port of the bridge is an error still.
func TestAnEntryThatCannotBeAddedOnAPortOfTheBridgeFails(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legM}, nil, nil, nil)
	h.sys.host.links["tap101i1"] = &fakeLink{link: link{name: "tap101i1", kind: "tun", up: true, master: ManagedBridge}}
	h.sys.fail["sticky tap101i1"] = syscall.ENOBUFS

	_, err := h.b.Sync(t.Context(), []Leg{legM}, nil, nil, []Sticky{{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap101i1"}})
	require.ErrorIs(t, err, syscall.ENOBUFS)
}

// Without a managed leg there is nowhere to park: the entry waits for its
// port.
func TestAnEntryWithoutAManagedLegWaitsForItsPort(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legA}, nil, nil, []Sticky{{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap101i1"}})
	require.NotContains(t, strings.Join(h.sys.changes(), "\n"), "06:50:43:4f:76:05")
}
