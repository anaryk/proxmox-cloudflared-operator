package setup

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// managedMade is the managed step on a node without an SDN of its own: the
// zone and the vnet made and applied under the lock.
func managedMade() []call {
	return slices.Concat([]call{sdnLocked()}, sdnRead(noZones, noVnets),
		[]call{zoneCreated(true), vnetCreated("1", true), sdnApplied(true), sdnUnlocked()})
}

func netnsEnabled() []call { return []call{{line: "systemctl enable --now pco-netns.service"}} }

func (e *testEnv) networks() map[string]store.Network {
	e.t.Helper()
	have, err := e.st.Networks()
	require.NoError(e.t, err)
	return have
}

// The managed network as setup stores it on node pve1 with mark.
func managedNetwork(mark int) store.Network {
	return store.Network{
		ID: "pconet", Kind: store.NetworkManaged, Bridge: "pconet",
		Prefix: netip.MustParsePrefix("10.203.118.0/24"),
		Legs:   map[string]netip.Addr{testNode: netip.MustParseAddr("10.203.118.1")},
		LegMAC: "02:50:43:4f:ff:01", Mark: mark, Scope: store.ScopeNode, CreatedAt: t0,
	}
}

func withoutRev(n store.Network) store.Network {
	n.Rev = 0
	return n
}

// pco setup --managed makes the managed network after the tags, notes the
// zone and the vnet in the manifest, stores the network, and brings up the
// gateway namespace, as it is the first network of the node.
func TestSetupManagedMakesTheManagedNetwork(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.installUnit(netnsUnit)
	steps := freshInstall()
	steps = slices.Insert(steps, 5, managedMade(), netnsEnabled())
	e.script(steps...)

	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode, Managed: true}))

	e.done()
	m := e.manifest()
	require.True(t, m.CreatedZone)
	require.True(t, m.CreatedVnet)
	require.Equal(t, map[string]store.Network{"pconet": managedNetwork(1)}, mapNetworks(e.networks()))
	e.requireShown("network pconet: stored, 10.203.118.0/24 with the leg 10.203.118.1 on node pve1")
	e.requireNoSecret()
}

func mapNetworks(have map[string]store.Network) map[string]store.Network {
	out := map[string]store.Network{}
	for id, n := range have {
		out[id] = withoutRev(n)
	}
	return out
}

func TestSetupManagedGoesWithItsFlags(t *testing.T) {
	e := newTestEnv(t)
	err := e.setup(Options{Yes: true, ConfirmSDNApply: true})
	require.EqualError(t, err, "--confirm-sdn-apply goes with --managed")
}

// installedHost is a node set up without the managed network, whose daemon
// runs.
func installedHost(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.installed(Manifest{CreatedRole: true, CreatedUser: true})
	e.installUnit(netnsUnit)
	return e
}

func (e *testEnv) enableManaged(o ManagedOptions) error {
	err := e.s.EnableManaged(context.Background(), o)
	if err != nil {
		e.errs = append(e.errs, err.Error())
	}
	return err
}

func TestEnableManagedOnTheHost(t *testing.T) {
	e := installedHost(t)
	e.script(managedMade(), netnsEnabled())

	require.NoError(t, e.enableManaged(ManagedOptions{Yes: true}))

	e.done()
	require.Equal(t, map[string]store.Network{"pconet": managedNetwork(1)}, mapNetworks(e.networks()))
	m := e.manifest()
	require.True(t, m.CreatedZone && m.CreatedVnet && m.CreatedRole, "the notes go into the manifest that is there")
}

// The gateway namespace is the daemon's from the first network on: a second
// network enables nothing, and takes the lowest mark that is free.
func TestEnableManagedBesideAnAttachedNetwork(t *testing.T) {
	e := installedHost(t)
	attached := store.Network{
		ID: "vmbr1", Kind: store.NetworkAttach, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.77.0.0/24"),
		Legs: map[string]netip.Addr{testNode: netip.MustParseAddr("10.77.0.250")}, LegMAC: store.LegMACFor(store.NetworkAttach, 1),
		Mark: 1, CreatedAt: t0,
	}
	require.NoError(t, e.st.SaveNetwork(attached, 0))
	e.script(managedMade())

	require.NoError(t, e.enableManaged(ManagedOptions{Yes: true}))

	e.done()
	require.Equal(t, managedNetwork(2), withoutRev(e.networks()["pconet"]))
}

// A second run finds everything there and changes nothing.
func TestEnableManagedTwice(t *testing.T) {
	e := installedHost(t)
	e.script(managedMade(), netnsEnabled())
	require.NoError(t, e.enableManaged(ManagedOptions{Yes: true}))
	e.done()

	e.script([]call{sdnLocked()}, sdnRead(list(zonePco), list(vnetPco), [2]string{"pconet", "[]"}), []call{sdnUnlocked()})
	require.NoError(t, e.enableManaged(ManagedOptions{Yes: true}))

	e.done()
	require.Len(t, e.networks(), 1)
	e.requireShown("network pconet: in the store, nothing needed")
}

// Without a mark that is free nothing is made, in the SDN or the store.
func TestEnableManagedRefusesBeyondTheLastMark(t *testing.T) {
	e := installedHost(t)
	for mark := 1; mark <= gateway.MaxMark; mark++ {
		bridge := "vmbr" + string(rune('a'+mark))
		require.NoError(t, e.st.SaveNetwork(store.Network{
			ID: bridge, Kind: store.NetworkAttach, Bridge: bridge, Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 77, byte(mark), 0}), 24),
			Legs:   map[string]netip.Addr{testNode: netip.AddrFrom4([4]byte{10, 77, byte(mark), 250})},
			LegMAC: store.LegMACFor(store.NetworkAttach, mark), Mark: mark, CreatedAt: t0.Add(time.Duration(mark)),
		}, 0))
	}

	err := e.enableManaged(ManagedOptions{Yes: true})

	require.ErrorContains(t, err, "network pconet: pco reaches at most 15 networks")
	e.done()
}

func TestEnableManagedNeedsASetUpHost(t *testing.T) {
	e := newTestEnv(t)
	require.EqualError(t, e.enableManaged(ManagedOptions{Yes: true}), "pco is not set up on this node; run pco setup --managed")

	e.s.host.euid = func() int { return 1000 }
	require.EqualError(t, e.enableManaged(ManagedOptions{Yes: true}), "pco network managed enable must run as root")

	e.s.host.profile = func() (string, error) { return store.ProfileAppliance, nil }
	require.ErrorIs(t, e.enableManaged(ManagedOptions{Yes: true}), errAppliance)
}

// Without the unit, as when pco runs from a build tree, the namespace is
// brought up by hand.
func TestEnableManagedWithoutTheUnit(t *testing.T) {
	e := newTestEnv(t)
	e.installed(Manifest{})
	e.script(managedMade())

	require.NoError(t, e.enableManaged(ManagedOptions{Yes: true}))

	e.done()
	e.requireShown("warn: pco-netns.service is not installed, as when pco runs from a build tree: run pco netns up as root before the daemon")
}

// A step of the SDN that is refused stores no network and enables nothing.
func TestEnableManagedRefusedByTheSDN(t *testing.T) {
	e := installedHost(t)
	e.script([]call{sdnLocked()}, sdnRead(list(`{"zone":"tenants","type":"vlan","state":"new","pending":{"bridge":"vmbr1"}}`), noVnets),
		[]call{sdnUnlocked()})

	err := e.enableManaged(ManagedOptions{Yes: true})

	require.ErrorContains(t, err, "zone tenants (new)")
	e.done()
	require.Empty(t, e.networks())
}
