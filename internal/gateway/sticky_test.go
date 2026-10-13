package gateway

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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

// Without a managed leg there is nowhere to park: the entry waits for its
// port.
func TestAnEntryWithoutAManagedLegWaitsForItsPort(t *testing.T) {
	h := newHarness(t).node()
	h.up(t)
	h.sync(t, []Leg{legA}, nil, nil, []Sticky{{MAC: mac("06:50:43:4f:76:05"), Bridge: ManagedBridge, Port: "tap101i1"}})
	require.NotContains(t, strings.Join(h.sys.changes(), "\n"), "06:50:43:4f:76:05")
}
