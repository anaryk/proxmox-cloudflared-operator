//go:build linux

package gateway

import (
	"context"
	"net/netip"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// hostIn is the host prober of a test's node: each call runs in the
// namespace that plays the host.
type hostIn struct {
	ns netns.NsHandle
	p  resolve.Prober
}

func (h hostIn) Interfaces(ctx context.Context) (out []resolve.HostIface, err error) {
	err = Do(h.ns, func() error { out, err = h.p.Interfaces(ctx); return err })
	return out, err
}

func (h hostIn) Route(ctx context.Context, iface string, addr netip.Addr) (out string, onLink bool, err error) {
	err = Do(h.ns, func() error { out, onLink, err = h.p.Route(ctx, iface, addr); return err })
	return out, onLink, err
}

func (h hostIn) ARP(ctx context.Context, iface string, addr netip.Addr) (out []string, err error) {
	err = Do(h.ns, func() error { out, err = h.p.ARP(ctx, iface, addr); return err })
	return out, err
}

func (h hostIn) FDBPorts(ctx context.Context, bridge string, vlan int, mac string) (out []string, err error) {
	err = Do(h.ns, func() error { out, err = h.p.FDBPorts(ctx, bridge, vlan, mac); return err })
	return out, err
}

func (h hostIn) Dial(ctx context.Context, iface string, target netip.AddrPort) error {
	return Do(h.ns, func() error { return h.p.Dial(ctx, iface, target) })
}

// tapGuest makes a guest with the vmid in a namespace of its own, on the
// port tap<vmid>i0 of the bridge, in the VLAN when one is given.
func (l *gatewayLab) tapGuest(vmid int, bridge string, vlan int, cidr string) netns.NsHandle {
	l.t.Helper()
	ns := "pcotest-" + strconv.Itoa(vmid) + "-" + l.id
	l.ip("netns", "add", ns)
	l.t.Cleanup(func() { _ = exec.Command(ipPath, "netns", "del", ns).Run() })
	port := "tap" + strconv.Itoa(vmid) + "i0"
	l.ip("-n", l.hostName, "link", "add", port, "type", "veth", "peer", "name", "eth0", "netns", ns)
	l.ip("-n", l.hostName, "link", "set", port, "master", bridge)
	if vlan > 0 {
		l.bridgeCmd("-n", l.hostName, "vlan", "add", "dev", port, "vid", strconv.Itoa(vlan), "pvid", "untagged")
		l.bridgeCmd("-n", l.hostName, "vlan", "del", "dev", port, "vid", "1")
	}
	l.ip("-n", l.hostName, "link", "set", port, "up")
	l.ip("-n", ns, "link", "set", "lo", "up")
	l.ip("-n", ns, "addr", "add", cidr, "dev", "eth0")
	l.ip("-n", ns, "link", "set", "eth0", "up")
	h, err := netns.GetFromName(ns)
	require.NoError(l.t, err)
	l.t.Cleanup(func() { _ = h.Close() })
	return h
}

// The prober of a node with the gateway: two guests with the same address
// behind two legs, one in a VLAN of a bridge that filters VLANs, each found,
// routed and dialled through its own leg from inside the namespace, and the
// forwarding tables read on the host.
func TestLinuxTheProberLooksThroughTheLegs(t *testing.T) {
	l := newGatewayLab(t)
	legs := []Leg{linuxLegA, linuxLegB, linuxLegM}
	p := NewProber(hostIn{ns: l.host, p: resolve.NewHostProber(0, 0)}, func() []Leg { return legs }, l.b)
	guest := addr("10.50.0.10")
	macA, macB := l.macOf(t, "pcotest-a-"+l.id), l.macOf(t, "pcotest-b-"+l.id)

	t.Run("the legs are listed beside the host's interfaces", func(t *testing.T) {
		ifaces, err := p.Interfaces(t.Context())
		require.NoError(t, err)
		byName := map[string]resolve.HostIface{}
		for _, ifc := range ifaces {
			require.NotContains(t, byName, ifc.Name, "listed twice")
			byName[ifc.Name] = ifc
		}
		require.Equal(t, resolve.HostIface{
			Name: "leg1", Addrs: []netip.Prefix{linuxLegA.Prefix}, Segment: resolve.Segment{Bridge: "br1"},
			Table: resolve.FDBRef{Bridge: "br1"}, Via: "br1",
		}, byName["leg1"])
		require.Equal(t, resolve.HostIface{
			Name: "leg2", Addrs: []netip.Prefix{linuxLegB.Prefix}, Segment: resolve.Segment{Bridge: "br2", VLAN: 20},
			Table: resolve.FDBRef{Bridge: "br2", VLAN: 20}, Via: "br2.20",
		}, byName["leg2"])
		require.Equal(t, resolve.FDBRef{Bridge: "br1"}, byName["br1"].Table, "the host's own interfaces are listed as the host prober lists them")
	})

	t.Run("ARP on a leg is asked inside the namespace", func(t *testing.T) {
		macs, err := p.ARP(t.Context(), "leg1", guest)
		require.NoError(t, err)
		require.Equal(t, []string{macA}, macs)
		macs, err = p.ARP(t.Context(), "leg2", guest)
		require.NoError(t, err)
		require.Equal(t, []string{macB}, macs)
	})

	t.Run("the route of a leg is that of its table, with its mark", func(t *testing.T) {
		for _, leg := range []string{"leg1", "leg2"} {
			out, onLink, err := p.Route(t.Context(), leg, guest)
			require.NoError(t, err)
			require.Equal(t, leg, out)
			require.True(t, onLink)
		}
		_, _, err := p.Route(t.Context(), "leg1", addr("10.51.0.10"))
		require.ErrorIs(t, err, resolve.ErrNoRoute, "the leg's table has nothing but its own network")
		_, _, err = p.Route(t.Context(), "", guest)
		require.ErrorIs(t, err, resolve.ErrNoRoute, "the host has no route into the networks of the legs")
	})

	t.Run("a dial on a leg reaches the guest behind it", func(t *testing.T) {
		serve(t, l.gb, "0.0.0.0:81", "b81")
		require.NoError(t, p.Dial(t.Context(), "leg1", netip.AddrPortFrom(guest, 80)))
		require.Error(t, p.Dial(t.Context(), "leg1", netip.AddrPortFrom(guest, 81)), "only the guest behind leg2 listens there")
		require.NoError(t, p.Dial(t.Context(), "leg2", netip.AddrPortFrom(guest, 81)))
	})

	t.Run("the forwarding tables are the host's", func(t *testing.T) {
		ports, err := p.FDBPorts(t.Context(), "br1", 0, macA)
		require.NoError(t, err)
		require.Equal(t, []string{"ga"}, ports)
		ports, err = p.FDBPorts(t.Context(), "br2", 20, macB)
		require.NoError(t, err)
		require.Equal(t, []string{"gb"}, ports)
	})

	deny, err := resolve.NewDenylist(nil, nil)
	require.NoError(t, err)
	r := resolve.NewResolver(p, resolve.Settings{}, nil)
	routeTo := func(ref model.GuestRef) model.Route {
		return model.Route{
			Hostname: ref.String() + ".example.com",
			Target:   model.Target{Scheme: model.SchemeHTTP, Port: 80},
			Source:   model.SourceAnnotation,
			Guest:    &ref,
		}
	}
	running := func(vmid int, nic model.NIC) model.Guest {
		return model.Guest{Ref: model.GuestRef{Kind: model.KindQEMU, VMID: vmid}, Name: "g" + strconv.Itoa(vmid), Running: true, NICs: []model.NIC{nic}}
	}

	t.Run("guests with one address behind two legs are proven at port, each through its own", func(t *testing.T) {
		g1 := l.tapGuest(951, "br1", 0, "10.50.0.11/24")
		l.tapGuest(952, "br2", 20, "10.50.0.11/24")
		serve(t, g1, "0.0.0.0:80", "951")
		mac1, mac2 := l.macOf(t, "pcotest-951-"+l.id), l.macOf(t, "pcotest-952-"+l.id)
		snap := inventory.Snapshot{Complete: true, Guests: []model.Guest{
			running(951, model.NIC{Index: 0, MAC: mac1, Bridge: "br1", Static: []netip.Addr{addr("10.50.0.11")}}),
			running(952, model.NIC{Index: 0, MAC: mac2, Bridge: "br2", VLAN: 20, Static: []netip.Addr{addr("10.50.0.11")}}),
		}}
		share := resolve.NewShared(nil)

		first := r.Resolve(t.Context(), routeTo(snap.Guests[0].Ref), snap, nil, deny, resolve.LevelPort, share)
		second := r.Resolve(t.Context(), routeTo(snap.Guests[1].Ref), snap, nil, deny, resolve.LevelPort, share)

		require.Equal(t, addr("10.50.0.11"), first.Target.Addr, first.Target.Reason)
		require.True(t, first.Target.Reachable, first.Target.Reason)
		require.Equal(t, resolve.LevelPort, first.Level)
		require.Equal(t, "br1", first.Binding.Via)
		require.Equal(t, "br1", first.Binding.Bridge)
		require.Equal(t, "tap951i0", first.Binding.Port)

		require.False(t, second.Target.Addr.IsValid(), "nothing listens on the second guest's port")
		require.Len(t, second.Candidates, 1)
		require.Equal(t, string(resolve.LevelPort), second.Candidates[0].Level, second.Candidates[0].Reason)
		require.Contains(t, second.Candidates[0].Reason, "port 80: ")
	})

	t.Run("a guest that claims the leg's address is refused", func(t *testing.T) {
		mac := l.macOf(t, "pcotest-a-"+l.id)
		snap := inventory.Snapshot{Complete: true, Guests: []model.Guest{
			running(953, model.NIC{Index: 0, MAC: mac, Bridge: "br1", Static: []netip.Addr{linuxLegA.Prefix.Addr()}}),
		}}

		res := r.Resolve(t.Context(), routeTo(snap.Guests[0].Ref), snap, nil, deny, resolve.LevelPort, nil)

		require.True(t, res.Target.Rejected)
		require.Equal(t, "address of this node", res.Target.Reason)
	})
}
