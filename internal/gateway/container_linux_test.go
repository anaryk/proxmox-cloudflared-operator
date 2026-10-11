//go:build linux

package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// The tests build an appliance of their own: a network namespace with net0
// to a LAN namespace, and cards with the MACs of legs whose other ends are
// ports of bridges in a namespace that plays the node, with guests behind
// them. Nothing of the machine they run on is touched.

// netTableStandIn is the table of the service prefix as pco-net.service
// loads it; the egress package's root test loads the appliance's own.
const netTableStandIn = `add table inet pco_net
delete table inet pco_net
table inet pco_net {
	counter leaked {
	}
	chain output {
		type filter hook output priority filter - 20; policy accept;
		ip daddr 198.18.0.0/16 fib daddr type != local counter name leaked reject
	}
}
`

const (
	ctNet0Addr = "192.168.77.240"
	ctLANAddr  = "192.168.77.1"
)

var (
	ctLegA = Leg{ID: "br1", Mark: 1, Bridge: "br1", Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:01")}
	ctLegB = Leg{ID: "br2", Mark: 2, Bridge: "br2", Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:02")}
	ctLegM = Leg{ID: ManagedBridge, Mark: 3, Bridge: ManagedBridge, Prefix: prefix("10.203.118.1/24"), MAC: ManagedLegMAC, Managed: true}

	ctServices = []Service{
		{Leg: "br1", Service: addr("198.18.1.1"), Addr: addr("10.50.0.10"), Ports: []uint16{80, 7, 9}},
		{Leg: "br2", Service: addr("198.18.1.2"), Addr: addr("10.50.0.10"), Ports: []uint16{80, 7}},
		{Leg: ManagedBridge, Service: addr("198.18.1.3"), Addr: addr("10.203.118.5"), Ports: []uint16{80}},
	}
)

type ctLab struct {
	t                          *testing.T
	id                         string
	appName, hostName, lanName string
	app, host, lan             netns.NsHandle
	ga, gb, gm                 netns.NsHandle
	nft                        Nft
	b                          *containerBackend
}

func newCTLab(t *testing.T) *ctLab {
	t.Helper()
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create network namespaces")
	}
	for _, bin := range []string{nftPath, ipPath} {
		if _, err := os.Stat(bin); err != nil {
			requireLab(t, "%s is not installed: %v", bin, err)
		}
	}
	before := nftRuleset(t)
	t.Cleanup(func() { require.Equal(t, before, nftRuleset(t), "the ruleset of the machine is unchanged") })
	id := fmt.Sprintf("%d-%d", os.Getpid()%100000, labCount.Add(1))
	l := &ctLab{t: t, id: id, appName: "pcotest-app-" + id, hostName: "pcotest-node-" + id, lanName: "pcotest-lan-" + id}
	l.app, l.host, l.lan = l.namespace(l.appName), l.namespace(l.hostName), l.namespace(l.lanName)
	check := exec.Command(ipPath, "netns", "exec", l.appName, nftPath, "-f", "-")
	check.Stdin = strings.NewReader("table inet pcotest_fib {\n\tchain o {\n\t\ttype filter hook output priority filter;\n" +
		"\t\tfib daddr type local accept\n\t}\n}\ndelete table inet pcotest_fib\n")
	if out, err := check.CombinedOutput(); err != nil {
		requireLab(t, "the kernel has no fib expression for inet tables (nft_fib_inet), which the appliance's tables use: %s", out)
	}
	l.ip("-n", l.appName, "link", "add", "net0", "type", "veth", "peer", "name", "eth0", "netns", l.lanName)
	l.ip("-n", l.appName, "addr", "add", ctNet0Addr+"/24", "dev", "net0")
	l.ip("-n", l.appName, "link", "set", "net0", "up")
	l.ip("-n", l.lanName, "addr", "add", ctLANAddr+"/24", "dev", "eth0")
	l.ip("-n", l.lanName, "link", "set", "eth0", "up")
	l.ip("-n", l.appName, "route", "add", "default", "via", ctLANAddr)
	for _, br := range []string{"br1", "br2", ManagedBridge} {
		l.ip("-n", l.hostName, "link", "add", br, "type", "bridge")
		l.ip("-n", l.hostName, "link", "set", br, "up")
	}
	l.ga = l.guest("a", "br1", "10.50.0.10/24")
	l.gb = l.guest("b", "br2", "10.50.0.10/24")
	l.gm = l.guest("m", ManagedBridge, "10.203.118.5/24")
	for _, g := range []struct {
		ns   netns.NsHandle
		name string
	}{{l.ga, "a"}, {l.gb, "b"}, {l.gm, "m"}} {
		serve(t, g.ns, "0.0.0.0:80", g.name)
		echo(t, g.ns, "0.0.0.0:7")
	}
	// systemd-sysctl's rp_filter, as the appliance boots with it.
	sysctlIn(t, l.app, "net.ipv4.conf.all.rp_filter", "2")
	l.nft = NewNamespaceNft(l.appName)
	l.b = newContainer(newContainerSystem(l.app), l.nft, nil, model.GuestRef{Kind: model.KindLXC, VMID: 9250}, "pve1", nil,
		zerolog.New(zerolog.NewTestWriter(t)), time.Now, WithNetTable(func() string { return netTableStandIn }))
	return l
}

func (l *ctLab) ip(args ...string) {
	l.t.Helper()
	out, err := exec.Command(ipPath, args...).CombinedOutput()
	require.NoError(l.t, err, "ip %s: %s", strings.Join(args, " "), out)
}

func (l *ctLab) namespace(name string) netns.NsHandle {
	l.t.Helper()
	l.ip("netns", "add", name)
	l.t.Cleanup(func() { _ = exec.Command(ipPath, "netns", "del", name).Run() })
	h, err := netns.GetFromName(name)
	require.NoError(l.t, err)
	l.t.Cleanup(func() { _ = h.Close() })
	l.ip("-n", name, "link", "set", "lo", "up")
	return h
}

func (l *ctLab) guest(name, bridge, cidr string) netns.NsHandle {
	l.t.Helper()
	ns := "pcotest-" + name + "-" + l.id
	h := l.namespace(ns)
	port := "g" + name
	l.ip("-n", l.hostName, "link", "add", port, "type", "veth", "peer", "name", "eth0", "netns", ns)
	l.ip("-n", l.hostName, "link", "set", port, "master", bridge)
	l.ip("-n", l.hostName, "link", "set", port, "up")
	l.ip("-n", ns, "addr", "add", cidr, "dev", "eth0")
	l.ip("-n", ns, "link", "set", "eth0", "up")
	return h
}

// card puts a card with the MAC into the appliance, its other end a port of
// the bridge, as Proxmox does with a card of a container.
func (l *ctLab) card(name string, m net.HardwareAddr, bridge string) {
	l.t.Helper()
	port := "vp" + strings.ReplaceAll(m.String(), ":", "")[8:]
	l.ip("-n", l.hostName, "link", "add", port, "type", "veth", "peer", "name", name, "netns", l.appName)
	l.ip("-n", l.appName, "link", "set", name, "address", m.String())
	l.ip("-n", l.hostName, "link", "set", port, "master", bridge)
	l.ip("-n", l.hostName, "link", "set", port, "up")
}

// up brings the appliance up with the three legs and their services, their
// cards there as at boot.
func (l *ctLab) up() {
	l.t.Helper()
	l.card("leg1", ctLegA.MAC, "br1")
	l.card("leg2", ctLegB.MAC, "br2")
	l.card("leg3", ctLegM.MAC, ManagedBridge)
	require.NoError(l.t, l.b.Up(l.t.Context()))
	l.sync()
	require.NoError(l.t, l.b.Verify(l.t.Context()))
}

func (l *ctLab) sync() {
	l.t.Helper()
	states, err := l.b.Sync(l.t.Context(), []Leg{ctLegA, ctLegB, ctLegM}, ctServices, nil, nil)
	require.NoError(l.t, err)
	for _, s := range states {
		require.True(l.t, s.Up, "%s: %s", s.Iface, s.Problem)
		require.Empty(l.t, s.Problem, s.Iface)
	}
}

func (l *ctLab) apply(script string) {
	l.t.Helper()
	require.NoError(l.t, l.nft.Apply(l.t.Context(), script))
}

// capture keeps the IPv4 packets that reach the LAN through net0.
type capture struct {
	mu   sync.Mutex
	seen []string // "src > dst"
	f    *os.File
}

func (l *ctLab) captureLAN() *capture {
	l.t.Helper()
	f, err := Socket(l.lan, unix.AF_PACKET, unix.SOCK_RAW, HostToNet16(unix.ETH_P_IP), SocketOptions{Device: "eth0"})
	require.NoError(l.t, err)
	c := &capture{f: f}
	l.t.Cleanup(func() { _ = f.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, err := f.Read(buf)
			if err != nil {
				return
			}
			if n < 14+20 {
				continue
			}
			ip := buf[14:n]
			src, dst := netip.AddrFrom4([4]byte(ip[12:16])), netip.AddrFrom4([4]byte(ip[16:20]))
			c.mu.Lock()
			c.seen = append(c.seen, src.String()+" > "+dst.String())
			c.mu.Unlock()
		}
	}()
	return c
}

// of returns what was seen from or to any of the addresses.
func (c *capture) of(addrs ...string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(c.seen), func(s string) bool {
		return !slices.ContainsFunc(addrs, func(a string) bool { return strings.Contains(s, a+" ") || strings.HasSuffix(s, " "+a) })
	})
}

func TestLinuxTheApplianceReachesGuestsWithTheSameAddressBehindTwoLegs(t *testing.T) {
	l := newCTLab(t)
	l.up()
	require.Equal(t, []string{
		"pco-net.service left the dummy device pco0 out; added by the gateway",
		"pco-net.service left the address 198.18.0.1/32 on pco0 out; added by the gateway",
		"pco-net.service left the route 198.18.0.0/16 dev pco0 src 198.18.0.1 out; added by the gateway",
		"pco-net.service left the rule 1900: from 198.18.0.1 unreachable out; added by the gateway",
		"pco-net.service left the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main out; added by the gateway",
		"pco-net.service left the table inet pco_net out; added by the gateway",
	}, Problems(l.b))

	got, err := get(l.app, "198.18.1.1:80", nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "a 10.50.0.250:"), got)
	requireInBlock(t, got, ctLegA.Mark)
	got, err = get(l.app, "198.18.1.2:80", nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "b 10.50.0.250:"), got)
	requireInBlock(t, got, ctLegB.Mark)
	got, err = get(l.app, "198.18.1.3:80", nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "m 10.203.118.1:"), got)

	start := time.Now()
	_, err = get(l.app, "198.18.1.9:80", nil, 2*time.Second)
	require.ErrorIs(t, err, unix.ECONNREFUSED, "an address no DNAT maps is rejected by the table of the service prefix")
	require.Less(t, time.Since(start), time.Second)

	t.Run("a second Up and Sync change nothing", func(t *testing.T) {
		before := ctDump(t, l)
		require.NoError(t, l.b.Up(t.Context()))
		require.Empty(t, Problems(l.b))
		l.sync()
		require.Equal(t, before, ctDump(t, l))
	})
}

// Each guard of the appliance's table, live: with what it guards against
// forced, the packet goes nowhere and its counter rises.
func TestLinuxTheGuardsOfTheAppliance(t *testing.T) {
	l := newCTLab(t)
	l.up()

	t.Run("a DNATed flow that would leave through the wrong leg is dropped", func(t *testing.T) {
		before := counter(t, l.nft, gwTable, counterOIF)
		l.ip("-n", l.appName, "route", "replace", "10.50.0.0/24", "dev", "leg2", "src", "10.50.0.250", "table", "101")
		_, err := get(l.app, "198.18.1.1:80", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.nft, gwTable, counterOIF), before)
		require.ErrorIs(t, l.b.Verify(t.Context()), ErrChanged)
		l.sync()
		require.NoError(t, l.b.Verify(t.Context()))
		got, err := get(l.app, "198.18.1.1:80", nil, 2*time.Second)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(got, "a "), got)
	})

	t.Run("a reply on a leg without the leg's zone lands in zone 0 and is dropped", func(t *testing.T) {
		l.apply("flush chain inet pco_gw pre_raw\n")
		_, err := get(l.app, "198.18.1.1:80", nil, time.Second)
		require.Error(t, err, "the reply is invalid in zone 0, where the flow is not")
		require.ErrorIs(t, l.b.Verify(t.Context()), ErrChanged)
		require.NoError(t, l.b.Up(t.Context()))
		_, err = get(l.app, "198.18.1.1:80", nil, 2*time.Second)
		require.NoError(t, err)
	})

	t.Run("a service address DNATed to an address of the appliance is dropped", func(t *testing.T) {
		l.ip("-n", l.appName, "addr", "add", "10.50.0.77/32", "dev", "lo")
		serve(t, l.app, "10.50.0.77:80", "appliance")
		services := append(slices.Clone(ctServices), Service{Leg: "br1", Service: addr("198.18.1.7"), Addr: addr("10.50.0.77"), Ports: []uint16{80}})
		_, err := l.b.Sync(t.Context(), []Leg{ctLegA, ctLegB, ctLegM}, services, nil, nil)
		require.NoError(t, err)
		before := counter(t, l.nft, gwTable, counterLocal)
		_, err = get(l.app, "198.18.1.7:80", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.nft, gwTable, counterLocal), before)
		l.sync()
	})

	t.Run("a guest reaches nothing of the appliance, and DHCP only on the managed leg", func(t *testing.T) {
		serve(t, l.app, "0.0.0.0:8643", "appliance")
		before := counter(t, l.nft, gwTable, counterLegIn)
		_, err := get(l.ga, "10.50.0.250:8643", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.nft, gwTable, counterLegIn), before)
		_, err = get(l.gm, "10.203.118.1:8643", nil, time.Second)
		require.Error(t, err)

		f, err := Socket(l.app, unix.AF_INET, unix.SOCK_DGRAM, 0, SocketOptions{ReuseAddr: true, Bind: netip.MustParseAddrPort("0.0.0.0:67")})
		require.NoError(t, err)
		dhcp, err := net.FilePacketConn(f)
		require.NoError(t, err)
		_ = f.Close()
		t.Cleanup(func() { _ = dhcp.Close() })
		counted := counter(t, l.nft, gwTable, counterLegIn)
		for _, g := range []struct {
			ns   netns.NsHandle
			to   string
			name string
		}{{l.ga, "10.50.0.250:67", "from leg1"}, {l.gm, "10.203.118.1:67", "from the managed leg"}} {
			require.NoError(t, Do(g.ns, func() error {
				c, err := net.DialUDP("udp4", &net.UDPAddr{Port: 68}, net.UDPAddrFromAddrPort(netip.MustParseAddrPort(g.to)))
				if err != nil {
					return err
				}
				defer func() { _ = c.Close() }()
				_, err = c.Write([]byte(g.name))
				return err
			}))
		}
		_ = dhcp.SetReadDeadline(time.Now().Add(time.Second))
		var got []string
		buf := make([]byte, 64)
		for {
			n, _, err := dhcp.ReadFrom(buf)
			if err != nil {
				break
			}
			got = append(got, string(buf[:n]))
		}
		require.Equal(t, []string{"from the managed leg"}, got)
		require.Equal(t, counted+1, counter(t, l.nft, gwTable, counterLegIn), "the datagram of leg1, not that of the managed leg")
	})
}

// Without the gateway's table, a packet of a NATed flow that is open and the
// appliance's own answer to a guest behind a leg have no mark and a leg's
// address or the prefix's source: the source guards keep them off net0,
// where the main table's default route would take them. The rule to the
// prefix is what lets a request to it be made at all.
func TestLinuxTheSourceGuardsKeepTheLegsOffNet0(t *testing.T) {
	l := newCTLab(t)
	l.up()
	legAddrs := []string{"10.50.0.10", "10.50.0.250", "198.18.0.1"}
	ticker(t, l.ga, "0.0.0.0:9")

	t.Run("an open flow and the appliance's answer stay in the appliance", func(t *testing.T) {
		var c net.Conn
		require.NoError(t, Do(l.app, func() (err error) { c, err = net.DialTimeout("tcp4", "198.18.1.1:9", 2*time.Second); return err }))
		defer func() { _ = c.Close() }()
		lan := l.captureLAN()
		l.apply("delete table inet pco_gw\n")
		serve(t, l.app, "0.0.0.0:8643", "appliance")
		_, err := get(l.ga, "10.50.0.250:8643", nil, time.Second)
		require.Error(t, err)
		time.Sleep(500 * time.Millisecond)
		require.Empty(t, lan.of(legAddrs...), "nothing of a leg leaves through net0")
		require.NoError(t, l.b.Up(t.Context()))
	})

	t.Run("the controls: without the source guards both leave through net0", func(t *testing.T) {
		var c net.Conn
		require.NoError(t, Do(l.app, func() (err error) { c, err = net.DialTimeout("tcp4", "198.18.1.1:9", 2*time.Second); return err }))
		defer func() { _ = c.Close() }()
		lan := l.captureLAN()
		l.apply("delete table inet pco_gw\n")
		// Both legs into the identical subnet have the address, each with a
		// guard of its own.
		for _, pref := range []string{"1900", "1901", "1902"} {
			l.ip("-n", l.appName, "rule", "del", "pref", pref)
		}
		require.Eventually(t, func() bool { return len(lan.of("10.50.0.10")) > 0 }, 3*time.Second, 50*time.Millisecond,
			"the answer to a packet of the open flow takes the default route")
		serve(t, l.app, "0.0.0.0:8644", "appliance")
		before := len(lan.of("10.50.0.250"))
		_, _ = get(l.ga, "10.50.0.250:8644", nil, time.Second)
		require.Eventually(t, func() bool { return len(lan.of("10.50.0.250")) > before }, 3*time.Second, 50*time.Millisecond,
			"the answer to the guest takes the default route")
		require.NoError(t, l.b.Up(t.Context()))
		l.sync()
		require.NoError(t, l.b.Verify(t.Context()))
	})

	t.Run("with the unreachable rule alone every request fails, with the rule to the prefix it works", func(t *testing.T) {
		l.ip("-n", l.appName, "rule", "del", "pref", "1890")
		_, err := get(l.app, "198.18.1.1:80", nil, time.Second)
		require.Error(t, err)
		require.ErrorIs(t, l.b.Verify(t.Context()), ErrChanged)
		require.NoError(t, l.b.Up(t.Context()))
		require.Equal(t, []string{"pco-net.service left the rule 1890: from 198.18.0.1 to 198.18.0.0/16 lookup main out; added by the gateway"}, Problems(l.b))
		_, err = get(l.app, "198.18.1.1:80", nil, 2*time.Second)
		require.NoError(t, err)
	})
}

// The sysctls Up sets are what keeps a leg from answering ARP for net0's
// address and lets the replies on a leg past the source check.
func TestLinuxTheSysctlsOfTheAppliance(t *testing.T) {
	l := newCTLab(t)
	l.up()
	guestARP := newLinuxSystem("", l.ga)
	ask := func(t *testing.T) []net.HardwareAddr {
		t.Helper()
		macs, err := guestARP.arpProbeOn(t.Context(), onHost, "eth0", addr(ctNet0Addr), 300*time.Millisecond)
		require.NoError(t, err)
		return macs
	}

	t.Run("a leg answers ARP for net0's address only with arp_ignore=0", func(t *testing.T) {
		require.Empty(t, ask(t), "with the sysctls of Up")
		sysctlIn(t, l.app, "net.ipv4.conf.all.arp_ignore", "0")
		require.ErrorIs(t, l.b.Verify(t.Context()), ErrChanged)
		require.Contains(t, ask(t), ctLegA.MAC, "the control: leg1 answers for net0's address")
		require.NoError(t, l.b.Up(t.Context()))
		require.Empty(t, ask(t))
	})

	t.Run("a reply on a leg is dropped with rp_filter=2, as systemd sets it", func(t *testing.T) {
		sysctlIn(t, l.app, "net.ipv4.conf.all.rp_filter", "2")
		_, err := get(l.app, "198.18.1.1:80", nil, time.Second)
		require.Error(t, err)
		var changed *ChangedError
		require.ErrorAs(t, l.b.Verify(t.Context()), &changed)
		require.Contains(t, changed.Differences, "net.ipv4.conf.all.rp_filter is 2 in the appliance, want 0")
		require.NoError(t, l.b.Up(t.Context()))
		_, err = get(l.app, "198.18.1.1:80", nil, 2*time.Second)
		require.NoError(t, err)
		require.NoError(t, l.b.Verify(t.Context()))
	})
}

// ticker sends a line every 50 ms on each connection, until it breaks.
func ticker(t *testing.T, ns netns.NsHandle, addr string) {
	t.Helper()
	var ln net.Listener
	require.NoError(t, Do(ns, func() (err error) { ln, err = net.Listen("tcp4", addr); return err }))
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				for {
					if _, err := c.Write([]byte("tick\n")); err != nil {
						return
					}
					time.Sleep(50 * time.Millisecond)
				}
			}()
		}
	}()
}

// liveNICs is the Proxmox API of a lab's appliance: a card it adds appears in
// the container a moment later, as hot-plug makes it, and one it removes
// goes.
type liveNICs struct {
	l     *ctLab
	mu    sync.Mutex
	nics  map[int]string
	calls []string
}

func (a *liveNICs) NICs(context.Context, string, model.GuestRef) (map[int]string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[int]string{0: "name=net0,bridge=vmbr0,hwaddr=bc:24:11:92:50:00"}
	for k, v := range a.nics {
		out[k] = v
	}
	return out, fmt.Sprintf("d%d", len(a.calls)), nil
}

func (a *liveNICs) VLANAware(context.Context, string, string) (bool, error) { return false, nil }

func (a *liveNICs) AddNIC(_ context.Context, _ string, _ model.GuestRef, index int, spec, _ string) error {
	a.mu.Lock()
	a.calls = append(a.calls, fmt.Sprintf("add net%d %s", index, spec))
	a.nics[index] = spec
	a.mu.Unlock()
	var name, bridge string
	var m net.HardwareAddr
	for part := range strings.SplitSeq(spec, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "name":
			name = v
		case "bridge":
			bridge = v
		case "hwaddr":
			m = mac(v)
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		a.l.card(name, m, bridge)
	}()
	return nil
}

func (a *liveNICs) RemoveNIC(_ context.Context, _ string, _ model.GuestRef, index int, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, fmt.Sprintf("remove net%d", index))
	spec := a.nics[index]
	delete(a.nics, index)
	_, rest, _ := strings.Cut(spec, "name=")
	name, _, _ := strings.Cut(rest, ",")
	return exec.Command(ipPath, "-n", a.l.appName, "link", "del", name).Run()
}

// A card added through the API appears in the container after the call, is
// waited for, configured and serves; Detach takes it away.
func TestLinuxACardAddedThroughTheAPIServes(t *testing.T) {
	l := newCTLab(t)
	api := &liveNICs{l: l, nics: map[int]string{}}
	var announced []string
	l.b.api = api
	l.b.announce = func(m net.HardwareAddr) { announced = append(announced, m.String()) }
	require.NoError(t, l.b.Up(t.Context()))
	states, err := l.b.Sync(t.Context(), []Leg{ctLegA}, ctServices[:1], nil, nil)
	require.NoError(t, err)
	require.True(t, states[0].Up, states[0].Problem)
	require.Equal(t, []string{"add net1 name=leg1,bridge=br1,hwaddr=02:50:43:4f:00:01,firewall=0"}, api.calls)
	require.Equal(t, []string{"02:50:43:4f:00:01"}, announced)
	got, err := get(l.app, "198.18.1.1:80", nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "a 10.50.0.250:"), got)
	require.NoError(t, l.b.Verify(t.Context()))

	require.NoError(t, l.b.Detach(t.Context(), ctLegA.ID))
	require.Equal(t, "remove net1", api.calls[len(api.calls)-1])
	_, err = get(l.app, "198.18.1.1:80", nil, time.Second)
	require.Error(t, err)
	require.NoError(t, l.b.Verify(t.Context()))
}

// The rules are listed with their action, as the backend writes them.
func TestLinuxTheRulesOfTheApplianceAreListedWithTheirAction(t *testing.T) {
	l := newCTLab(t)
	sys := newContainerSystem(l.app)
	want := []rule{
		{pref: 1001, mark: 1, table: 101},
		{pref: 1890, src: TransitHost, dst: ServicePrefix, table: mainTable},
		{pref: 1900, src: TransitHost, unreachable: true},
		{pref: 1901, src: addr("10.50.0.250"), unreachable: true},
	}
	for _, r := range want {
		require.NoError(t, sys.addRule(r), r.String())
	}
	l.ip("-n", l.appName, "rule", "add", "pref", "1950", "from", "10.0.0.0/8", "iif", "net0", "lookup", "main")
	got, err := sys.rules()
	require.NoError(t, err)
	for _, r := range want {
		require.Contains(t, got, r)
	}
	i := slices.IndexFunc(got, func(r rule) bool { return r.pref == 1950 })
	require.GreaterOrEqual(t, i, 0)
	require.Equal(t, -1, got[i].mark, "a rule that matches on more is no rule of pco's")
	require.NoError(t, sys.delRule(want[3]))
	got, err = sys.rules()
	require.NoError(t, err)
	require.NotContains(t, got, want[3])
}

// requireInBlock requires the port of a guest's answer to be of the block of
// the leg with the mark.
func requireInBlock(t *testing.T, answer string, mark int) {
	t.Helper()
	lo, hi := PortBlock(mark)
	port := portOf(t, answer)
	require.True(t, port >= int(lo) && port <= int(hi), "%s: not in %d-%d", answer, lo, hi)
}

// ctDump is what the appliance holds, without counters and lifetimes.
func ctDump(t *testing.T, l *ctLab) string {
	t.Helper()
	var b strings.Builder
	for _, args := range [][]string{
		{"-n", l.appName, "-o", "link", "show"}, {"-n", l.appName, "addr", "show"}, {"-n", l.appName, "route", "show", "table", "all"},
		{"-n", l.appName, "rule", "show"}, {"-n", l.appName, "neigh", "show", "nud", "permanent"},
	} {
		out, err := exec.Command(ipPath, args...).CombinedOutput()
		require.NoError(t, err, string(out))
		b.WriteString(stripVolatile(string(out)))
	}
	out, err := exec.Command(ipPath, "netns", "exec", l.appName, nftPath, "list", "ruleset").CombinedOutput()
	require.NoError(t, err, string(out))
	b.WriteString(stripVolatile(string(out)))
	return b.String()
}
