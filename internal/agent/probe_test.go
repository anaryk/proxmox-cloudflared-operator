package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// nodeIfaces are the interfaces of a node with an uplinked vmbr1, a guest on
// it, a leg of vmbr2 and the node's own address on vmbr0.
func nodeIfaces() []resolve.HostIface {
	return []resolve.HostIface{
		{Name: "vmbr0", Segment: resolve.Segment{Bridge: "vmbr0"}, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.4.12/24"), netip.MustParsePrefix("fe80::1/64")}},
		{Name: "eno1", Master: "vmbr0"},
		{Name: "vmbr1", Segment: resolve.Segment{Bridge: "vmbr1"}},
		{Name: "eno2", Master: "vmbr1"},
		{Name: "tap101i0", Master: "vmbr1", Segment: resolve.Segment{Bridge: "vmbr1"}},
		{Name: "fwpr101p0", Master: "vmbr1"},
		{Name: "leg2", Master: "vmbr2", Segment: resolve.Segment{Bridge: "vmbr2"}, Via: "vmbr2", Addrs: []netip.Prefix{netip.MustParsePrefix("10.2.0.250/24")}},
		{Name: "lo", Addrs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
	}
}

// A probe action is answered with the interfaces of the node, less the
// ports of the guests, and the MACs that answered for the address.
func TestAProbeActionIsAnsweredWithTheNodeAndWhoAnswered(t *testing.T) {
	e := newEnv(t)
	var asked []string
	e.agent.d.Interfaces = func(context.Context) ([]resolve.HostIface, error) { return nodeIfaces(), nil }
	e.agent.d.ProbeAddr = func(_ context.Context, bridge string, vlan int, addr netip.Addr) ([]net.HardwareAddr, error) {
		asked = append(asked, fmt.Sprintf("%s %d %s", bridge, vlan, addr))
		mac, _ := net.ParseMAC("bc:24:11:00:00:09")
		return []net.HardwareAddr{mac}, nil
	}
	s := withAction(e.snapshot(), "p1", agentproto.ActionProbe, `{"bridge":"vmbr1","vlan":20,"addr":"10.77.0.250"}`)

	st := e.apply(s)

	require.Equal(t, []string{"vmbr1 20 10.77.0.250"}, asked)
	require.Len(t, st.Actions, 1)
	require.True(t, st.Actions[0].OK, st.Actions[0].Error)
	got, err := agentproto.DecodeProbeAnswer(st.Actions[0].Data)
	require.NoError(t, err)
	require.Equal(t, agentproto.ProbeAnswer{
		Interfaces: []agentproto.ProbeIface{
			{Name: "vmbr0", Bridge: "vmbr0", Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.4.12/24")}},
			{Name: "eno1", Master: "vmbr0"},
			{Name: "vmbr1", Bridge: "vmbr1"},
			{Name: "eno2", Master: "vmbr1"},
			{Name: "leg2", Master: "vmbr2", Bridge: "vmbr2", Via: "vmbr2", Addrs: []netip.Prefix{netip.MustParsePrefix("10.2.0.250/24")}},
			{Name: "lo", Addrs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
		},
		AnsweredBy: []string{"bc:24:11:00:00:09"},
	}, got)

	e.apply(s)
	require.Len(t, asked, 1, "a probe runs once")
}

// A probe without an address only looks at the node.
func TestAProbeWithoutAnAddressProbesNothing(t *testing.T) {
	e := newEnv(t)
	e.agent.d.Interfaces = func(context.Context) ([]resolve.HostIface, error) { return nodeIfaces(), nil }
	e.agent.d.ProbeAddr = func(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error) {
		t.Fatal("nothing is probed")
		return nil, nil
	}

	st := e.apply(withAction(e.snapshot(), "p1", agentproto.ActionProbe, `{"bridge":"pconet"}`))

	require.True(t, st.Actions[0].OK, st.Actions[0].Error)
	got, err := agentproto.DecodeProbeAnswer(st.Actions[0].Data)
	require.NoError(t, err)
	require.Empty(t, got.AnsweredBy)
	require.NotEmpty(t, got.Interfaces)
}

func TestAProbeActionIsRefused(t *testing.T) {
	ifaces := func(context.Context) ([]resolve.HostIface, error) { return nodeIfaces(), nil }
	probe := func(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error) { return nil, nil }
	addrs := func(n int) []netip.Prefix {
		out := make([]netip.Prefix, n)
		for j := range out {
			out[j] = netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(j >> 8), byte(j), 1}), 24)
		}
		return out
	}
	crowded := func(context.Context) ([]resolve.HostIface, error) {
		return []resolve.HostIface{{Name: "vmbr0", Segment: resolve.Segment{Bridge: "vmbr0"}, Addrs: addrs(agentproto.MaxProbeAddrs + 1)}}, nil
	}
	large := func(context.Context) ([]resolve.HostIface, error) {
		out := make([]resolve.HostIface, agentproto.MaxProbeIfaces)
		for i := range out {
			out[i] = resolve.HostIface{Name: fmt.Sprintf("vmbr%d", i), Segment: resolve.Segment{Bridge: fmt.Sprintf("vmbr%d", i)}, Addrs: addrs(agentproto.MaxProbeAddrs)}
		}
		return out, nil
	}
	many := func(context.Context) ([]resolve.HostIface, error) {
		out := make([]resolve.HostIface, agentproto.MaxProbeIfaces+1)
		for i := range out {
			out[i] = resolve.HostIface{Name: fmt.Sprintf("eno%d", i), Master: "vmbr1"}
		}
		return out, nil
	}
	for _, tc := range []struct {
		name   string
		kind   pki.Kind
		args   string
		ifaces func(context.Context) ([]resolve.HostIface, error)
		probe  func(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error)
		err    string
	}{
		{name: "by an appliance agent", kind: pki.KindAppliance, args: `{"bridge":"vmbr1"}`, ifaces: ifaces, probe: probe, err: "appliance"},
		{name: "without a prober", kind: pki.KindHost, args: `{"bridge":"vmbr1"}`, err: "probes nothing"},
		{name: "with a bridge of other characters", kind: pki.KindHost, args: `{"bridge":"vmbr1 up"}`, ifaces: ifaces, probe: probe, err: "bridge"},
		{name: "with an IPv6 address", kind: pki.KindHost, args: `{"bridge":"vmbr1","addr":"fd00::1"}`, ifaces: ifaces, probe: probe, err: "IPv4"},
		{name: "with fields it does not know", kind: pki.KindHost, args: `{"bridge":"vmbr1","run":"reboot"}`, ifaces: ifaces, probe: probe, err: "unknown field"},
		{name: "when the interfaces cannot be read", kind: pki.KindHost, args: `{"bridge":"vmbr1"}`, probe: probe,
			ifaces: func(context.Context) ([]resolve.HostIface, error) { return nil, errors.New("netlink is gone") }, err: "netlink is gone"},
		{name: "when the node has too many interfaces to say", kind: pki.KindHost, args: `{"bridge":"vmbr1"}`, ifaces: many, probe: probe, err: "interfaces"},
		{name: "when an interface has too many addresses to say", kind: pki.KindHost, args: `{"bridge":"vmbr1"}`, ifaces: crowded, probe: probe, err: "addresses"},
		{name: "when the answer is larger than the controller reads", kind: pki.KindHost, args: `{"bridge":"vmbr1"}`, ifaces: large, probe: probe, err: "bytes"},
		{name: "when the probe fails", kind: pki.KindHost, args: `{"bridge":"vmbr1","addr":"10.77.0.250"}`, ifaces: ifaces,
			probe: func(context.Context, string, int, netip.Addr) ([]net.HardwareAddr, error) {
				return nil, errors.New("no namespace")
			}, err: "no namespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.kind = tc.kind
			e.agent = e.newAgent()
			e.agent.d.Interfaces, e.agent.d.ProbeAddr = tc.ifaces, tc.probe

			st := e.apply(withAction(e.snapshot(), "p1", agentproto.ActionProbe, tc.args))

			require.Len(t, st.Actions, 1)
			require.False(t, st.Actions[0].OK)
			require.Empty(t, st.Actions[0].Data)
			require.Contains(t, st.Actions[0].Error, tc.err)
		})
	}
}
