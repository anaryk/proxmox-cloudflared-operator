//go:build linux

package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The tests build a node of their own: a network namespace that plays the
// host, with bridges and guests in namespaces of their own behind them, and
// the gateway namespace pinned under a name of the test's. Nothing of the
// machine they run on is touched; everything goes with the test.

var labCount atomic.Int64

// labNet is a node of a test.
type labNet struct {
	t                *testing.T
	id               string
	hostName, gwName string
	host             netns.NsHandle
	sys              *linuxSystem
	b                *hostBackend
	nsNft, hostNft   Nft
}

func newLabNet(t *testing.T) *labNet {
	t.Helper()
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create network namespaces")
	}
	for _, bin := range []string{nftPath, ipPath} {
		if _, err := os.Stat(bin); err != nil {
			requireLab(t, "%s is not installed: %v", bin, err)
		}
	}
	id := fmt.Sprintf("%d-%d", os.Getpid()%100000, labCount.Add(1))
	l := &labNet{t: t, id: id, hostName: "pcotest-host-" + id, gwName: "pcotest-gw-" + id}
	l.ip("netns", "add", l.hostName)
	t.Cleanup(func() { _ = exec.Command(ipPath, "netns", "del", l.hostName).Run() })
	host, err := netns.GetFromName(l.hostName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close() })
	l.host = host
	l.ip("-n", l.hostName, "link", "set", "lo", "up")
	l.sys = newLinuxSystem(l.gwName, host)
	t.Cleanup(func() { _ = l.sys.removeNamespace() })
	l.nsNft, l.hostNft = NewNamespaceNft(l.gwName), NewNamespaceNft(l.hostName)
	l.b = newHost(l.sys, l.nsNft, l.hostNft, zerolog.New(zerolog.NewTestWriter(t)), time.Now)
	return l
}

func (l *labNet) ip(args ...string) {
	l.t.Helper()
	out, err := exec.Command(ipPath, args...).CombinedOutput()
	require.NoError(l.t, err, "ip %s: %s", strings.Join(args, " "), out)
}

// bridgeCmd runs the bridge tool of iproute2 on the host.
func (l *labNet) bridgeCmd(args ...string) {
	l.t.Helper()
	out, err := exec.Command("bridge", args...).CombinedOutput()
	require.NoError(l.t, err, "bridge %s: %s", strings.Join(args, " "), out)
}

// bridge makes a bridge on the host, up.
func (l *labNet) bridge(name string, vlanFiltering bool) {
	l.t.Helper()
	args := []string{"-n", l.hostName, "link", "add", name, "type", "bridge"}
	if vlanFiltering {
		args = append(args, "vlan_filtering", "1")
	}
	l.ip(args...)
	l.ip("-n", l.hostName, "link", "set", name, "up")
}

// guest makes a namespace with eth0 on a port of the bridge, in the VLAN
// when one is given, with the address and, when given, a default route.
func (l *labNet) guest(name, bridge string, vlan int, cidr, gateway string) netns.NsHandle {
	l.t.Helper()
	ns := "pcotest-" + name + "-" + l.id
	l.ip("netns", "add", ns)
	l.t.Cleanup(func() { _ = exec.Command(ipPath, "netns", "del", ns).Run() })
	port := "g" + name
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
	if gateway != "" {
		l.ip("-n", ns, "route", "add", "default", "via", gateway)
	}
	h, err := netns.GetFromName(ns)
	require.NoError(l.t, err)
	l.t.Cleanup(func() { _ = h.Close() })
	return h
}

func sysctlIn(t *testing.T, ns netns.NsHandle, key, value string) {
	t.Helper()
	require.NoError(t, Do(ns, func() error { return os.WriteFile(sysctlPath(key), []byte(value), 0o644) }))
}

// serve answers each connection with name and the address it came from,
// then closes it.
func serve(t *testing.T, ns netns.NsHandle, addr, name string) {
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
			_, _ = fmt.Fprintf(c, "%s %s\n", name, c.RemoteAddr())
			_ = c.Close()
		}
	}()
}

// echo answers each line with itself, for long-lived connections.
func echo(t *testing.T, ns netns.NsHandle, addr string) {
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
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
}

// get connects from ns to addr, from local when given, and returns what the
// other end said.
func get(ns netns.NsHandle, addr string, local *net.TCPAddr, timeout time.Duration) (string, error) {
	var c net.Conn
	err := Do(ns, func() (err error) {
		d := net.Dialer{Timeout: timeout, LocalAddr: local}
		if local != nil && local.Port != 0 {
			d.Control = func(_, _ string, rc syscall.RawConn) error {
				var serr error
				err := rc.Control(func(fd uintptr) {
					serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
				})
				return errors.Join(err, serr)
			}
		}
		c, err = d.Dial("tcp4", addr)
		return err
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(timeout))
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

// counter returns the packets a named counter of a table counted.
func counter(t *testing.T, n Nft, table, name string) uint64 {
	t.Helper()
	raw, err := n.List(t.Context(), table)
	require.NoError(t, err)
	var doc struct {
		Nftables []struct {
			Counter *struct {
				Name    string `json:"name"`
				Packets uint64 `json:"packets"`
			} `json:"counter"`
		} `json:"nftables"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	for _, e := range doc.Nftables {
		if e.Counter != nil && e.Counter.Name == name {
			return e.Counter.Packets
		}
	}
	t.Fatalf("table %s has no counter %s", table, name)
	return 0
}

// portOf returns the port of the address a guest's answer names.
func portOf(t *testing.T, answer string) int {
	t.Helper()
	f := strings.Fields(answer)
	require.Len(t, f, 2, answer)
	ap, err := netip.ParseAddrPort(f[1])
	require.NoError(t, err, answer)
	return int(ap.Port())
}

// The two guests with the same address behind two legs, one of them in a
// VLAN of a bridge that filters VLANs, and the managed leg.
var (
	linuxLegA = Leg{ID: "br1", Mark: 1, Bridge: "br1", Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:01")}
	linuxLegB = Leg{ID: "br2.20", Mark: 2, Bridge: "br2", VLAN: 20, Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:02")}
	linuxLegM = Leg{ID: ManagedBridge, Mark: 3, Bridge: ManagedBridge, Prefix: prefix("10.203.118.1/24"), MAC: ManagedLegMAC, Managed: true}

	linuxServices = []Service{
		{Leg: "br1", Service: addr("198.18.1.1"), Addr: addr("10.50.0.10"), Ports: []uint16{80, 7}},
		{Leg: "br2.20", Service: addr("198.18.1.2"), Addr: addr("10.50.0.10"), Ports: []uint16{80, 7}},
		{Leg: ManagedBridge, Service: addr("198.18.1.3"), Addr: addr("10.203.118.5"), Ports: []uint16{80}},
	}
)

// gatewayLab is a node with the gateway up and the guests behind it.
type gatewayLab struct {
	*labNet
	ga, gb, gm, gm2, glan netns.NsHandle
}

func newGatewayLab(t *testing.T) *gatewayLab {
	t.Helper()
	l := &gatewayLab{labNet: newLabNet(t)}
	before := nftRuleset(t)
	t.Cleanup(func() { require.Equal(t, before, nftRuleset(t), "the ruleset of the machine is unchanged") })
	l.bridge("br1", false)
	l.bridge("br2", true)
	l.bridge(ManagedBridge, false)
	l.bridge("vmbr0", false)
	l.ga = l.guest("a", "br1", 0, "10.50.0.10/24", "")
	l.gb = l.guest("b", "br2", 20, "10.50.0.10/24", "")
	l.gm = l.guest("m", ManagedBridge, 0, "10.203.118.5/24", "10.203.118.254")
	l.gm2 = l.guest("m2", ManagedBridge, 0, "10.203.118.6/24", "")
	l.glan = l.guest("lan", "vmbr0", 0, "192.168.77.50/24", "192.168.77.1")
	// The node: an address on the managed bridge, as one an admin gave it,
	// and on its LAN, forwarding as a router would.
	l.ip("-n", l.hostName, "addr", "add", "10.203.118.254/24", "dev", ManagedBridge)
	l.ip("-n", l.hostName, "addr", "add", "192.168.77.1/24", "dev", "vmbr0")
	sysctlIn(t, l.host, "net.ipv4.ip_forward", "1")
	for _, g := range []struct {
		ns   netns.NsHandle
		name string
	}{{l.ga, "a"}, {l.gb, "b"}, {l.gm, "m"}, {l.gm2, "m2"}, {l.glan, "lan"}} {
		serve(t, g.ns, "0.0.0.0:80", g.name)
		echo(t, g.ns, "0.0.0.0:7")
	}
	require.NoError(t, l.b.Up(t.Context()))
	states, err := l.b.Sync(t.Context(), []Leg{linuxLegA, linuxLegB, linuxLegM}, linuxServices, nil, nil)
	require.NoError(t, err)
	for _, s := range states {
		require.True(t, s.Up, "%s: %s", s.Iface, s.Problem)
		require.Empty(t, s.Problem, s.Iface)
	}
	require.NoError(t, l.b.Verify(t.Context()))
	return l
}

func TestLinuxTheGatewayServesGuestsWithTheSameAddressBehindTwoLegs(t *testing.T) {
	l := newGatewayLab(t)
	// Frames bridged on the host pass its conntrack, which sees both legs'
	// flows in one zone, and its firewall drops what conntrack finds
	// invalid, as pve-firewall does.
	if err := Do(l.host, func() error {
		return os.WriteFile("/proc/sys/net/bridge/bridge-nf-call-iptables", []byte("1"), 0o644)
	}); err != nil {
		requireLab(t, "bridge-nf-call-iptables cannot be set in a namespace: %v", err)
	}
	require.NoError(t, l.hostNft.Apply(t.Context(),
		"table inet pcotest {\n\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n"+
			"\t\tct state invalid counter drop\n\t}\n}\n"))

	t.Run("each service address reaches its own guest, from the leg's block of ports", func(t *testing.T) {
		got, err := get(l.host, "198.18.1.1:80", nil, 2*time.Second)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(got, "a 10.50.0.250:"), got)
		lo, hi := PortBlock(1)
		require.True(t, portOf(t, got) >= int(lo) && portOf(t, got) <= int(hi), got)
		got, err = get(l.host, "198.18.1.2:80", nil, 2*time.Second)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(got, "b 10.50.0.250:"), got)
		lo, hi = PortBlock(2)
		require.True(t, portOf(t, got) >= int(lo) && portOf(t, got) <= int(hi), got)
		got, err = get(l.host, "198.18.1.3:80", nil, 2*time.Second)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(got, "m 10.203.118.1:"), got)
	})

	t.Run("pairs that share a source port never cross and never collide", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 100)
		for i := range 50 {
			local := &net.TCPAddr{IP: TransitHost.AsSlice(), Port: 41000 + i}
			for _, target := range []struct{ addr, name string }{{"198.18.1.1:80", "a "}, {"198.18.1.2:80", "b "}} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					got, err := get(l.host, target.addr, local, 3*time.Second)
					if err == nil && !strings.HasPrefix(got, target.name) {
						err = fmt.Errorf("%s answered %q", target.addr, got)
					}
					if err != nil {
						errs <- fmt.Errorf("from port %d: %w", local.Port, err)
					}
				}()
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		require.Zero(t, counter(t, l.nsNft, gwTable, counterOIF))
		require.Zero(t, counter(t, l.nsNft, gwTable, counterForward))
	})

	t.Run("a port that is not published is not reached", func(t *testing.T) {
		serve(t, l.ga, "0.0.0.0:8080", "a8080")
		_, err := get(l.host, "198.18.1.1:8080", nil, time.Second)
		require.Error(t, err, "without a DNAT the service address is routed nowhere")
	})
}

// Each guard of the table, live: with what it guards against forced, its
// counter rises and the packet goes nowhere.
func TestLinuxTheGuardsOfTheNamespace(t *testing.T) {
	l := newGatewayLab(t)

	t.Run("a DNATed flow that would leave through the wrong leg is dropped", func(t *testing.T) {
		before := counter(t, l.nsNft, gwTable, counterOIF)
		require.NoError(t, l.sys.addRoute(inNS, route{table: tableBase + 1, dst: prefix("10.50.0.0/24"), dev: "leg2", src: addr("10.50.0.250")}))
		_, err := get(l.host, "198.18.1.1:80", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.nsNft, gwTable, counterOIF), before)
		require.ErrorIs(t, l.b.Verify(t.Context()), ErrChanged)
		_, err = l.b.Sync(t.Context(), []Leg{linuxLegA, linuxLegB, linuxLegM}, linuxServices, nil, nil)
		require.NoError(t, err)
		require.NoError(t, l.b.Verify(t.Context()))
		got, err := get(l.host, "198.18.1.1:80", nil, 2*time.Second)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(got, "a "), got)
	})

	t.Run("a leg's ingress without its zone does not reach the flow", func(t *testing.T) {
		require.NoError(t, l.nsNft.Apply(t.Context(), `delete element inet pco_gw leg_zone { "leg1" }`+"\n"))
		_, err := get(l.host, "198.18.1.1:80", nil, time.Second)
		require.Error(t, err, "the reply lands in zone 0, where the flow is not")
		_, err = l.b.Sync(t.Context(), []Leg{linuxLegA, linuxLegB, linuxLegM}, linuxServices, nil, nil)
		require.NoError(t, err)
		_, err = get(l.host, "198.18.1.1:80", nil, 2*time.Second)
		require.NoError(t, err)
	})

	t.Run("the transit link takes nothing but the host's address", func(t *testing.T) {
		l.ip("-n", l.hostName, "addr", "add", "198.18.0.5/32", "dev", hostTransit)
		defer l.ip("-n", l.hostName, "addr", "del", "198.18.0.5/32", "dev", hostTransit)
		before := counter(t, l.nsNft, gwTable, counterTransit)
		_, err := get(l.host, "198.18.1.1:80", &net.TCPAddr{IP: net.ParseIP("198.18.0.5")}, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.nsNft, gwTable, counterTransit), before)
	})

	t.Run("a guest reaches nothing of the namespace, and DHCP only on the managed leg", func(t *testing.T) {
		ns := l.sys.handle()
		serve(t, ns, "0.0.0.0:8643", "namespace")
		before := counter(t, l.nsNft, gwTable, counterLegIn)
		_, err := get(l.ga, "10.50.0.250:8643", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.nsNft, gwTable, counterLegIn), before)
		_, err = get(l.gm, "10.203.118.1:8643", nil, time.Second)
		require.Error(t, err)
		// The namespace has no route to a service address but from the host.
		l.ip("-n", "pcotest-a-"+l.id, "route", "add", ServicePrefix.String(), "via", "10.50.0.250")
		_, err = get(l.ga, "198.18.1.2:80", nil, time.Second)
		require.Error(t, err, "a guest reaches no service address")

		f, err := Socket(ns, unix.AF_INET, unix.SOCK_DGRAM, 0, SocketOptions{ReuseAddr: true, Bind: netip.MustParseAddrPort("0.0.0.0:67")})
		require.NoError(t, err)
		dhcp, err := net.FilePacketConn(f)
		require.NoError(t, err)
		_ = f.Close()
		t.Cleanup(func() { _ = dhcp.Close() })
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
	})
}

// The host's table, live: the node takes nothing from the managed bridge
// and no new flow from the transit link, routes nothing a managed guest sends
// it, bridges what managed guests send each other, and forwards nothing into
// the transit link, masqueraded or not.
func TestLinuxTheHostTable(t *testing.T) {
	l := newGatewayLab(t)
	serve(t, l.host, "0.0.0.0:8006", "node")

	t.Run("the node's services are out of reach of the managed bridge", func(t *testing.T) {
		before := counter(t, l.hostNft, hostTable, "from_pconet")
		_, err := get(l.gm, "10.203.118.254:8006", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.hostNft, hostTable, "from_pconet"), before)
		v, err := l.sys.sysctl(onHost, "net.ipv6.conf.pconet.disable_ipv6")
		require.NoError(t, err)
		require.Equal(t, "1", v, "no link-local address on the managed bridge")
	})

	t.Run("no new flow comes in from the transit link", func(t *testing.T) {
		before := counter(t, l.hostNft, hostTable, "from_transit")
		_, err := get(l.sys.handle(), "198.18.0.1:8006", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.hostNft, hostTable, "from_transit"), before)
	})

	t.Run("a managed guest cannot use the node as its router", func(t *testing.T) {
		before := counter(t, l.hostNft, hostTable, "routed_from_pconet")
		_, err := get(l.gm, "192.168.77.50:80", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.hostNft, hostTable, "routed_from_pconet"), before)
	})

	t.Run("managed guests reach each other through the bridge", func(t *testing.T) {
		if err := Do(l.host, func() error {
			return os.WriteFile("/proc/sys/net/bridge/bridge-nf-call-iptables", []byte("1"), 0o644)
		}); err != nil {
			requireLab(t, "bridge-nf-call-iptables cannot be set in a namespace: %v", err)
		}
		before := counter(t, l.hostNft, hostTable, "routed_from_pconet")
		got, err := get(l.gm, "10.203.118.6:80", nil, 2*time.Second)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(got, "m2 "), got)
		require.Equal(t, before, counter(t, l.hostNft, hostTable, "routed_from_pconet"))
	})

	t.Run("a node that forwards or masquerades passes nothing into the transit link", func(t *testing.T) {
		require.NoError(t, l.hostNft.Apply(t.Context(),
			"table ip pcotmasq {\n\tchain postrouting {\n\t\ttype nat hook postrouting priority srcnat; policy accept;\n"+
				"\t\toifname \"pcotr0\" masquerade\n\t}\n}\n"))
		before := counter(t, l.hostNft, hostTable, "to_transit")
		_, err := get(l.glan, "198.18.1.1:80", nil, time.Second)
		require.Error(t, err)
		require.Greater(t, counter(t, l.hostNft, hostTable, "to_transit"), before)
	})
}

// Up and Sync once more change nothing: no link, address, route, rule or
// table is touched, and connections that are open stay open.
func TestLinuxASecondUpUnderTrafficChangesNothing(t *testing.T) {
	l := newGatewayLab(t)
	conns := openEchoes(t, l, 10)
	before := dump(t, l)
	require.NoError(t, l.b.Up(t.Context()))
	_, err := l.b.Sync(t.Context(), []Leg{linuxLegA, linuxLegB, linuxLegM}, linuxServices, nil, nil)
	require.NoError(t, err)
	require.Equal(t, before, dump(t, l))
	conns.check(t)

	t.Run("a restore from the cache with stale pins changes nothing either", func(t *testing.T) {
		fresh := newHost(l.sys, l.nsNft, l.hostNft, zerolog.Nop(), time.Now)
		c := Cache{Legs: []Leg{linuxLegA, linuxLegB, linuxLegM}, Services: linuxServices}
		_, err := Restore(t.Context(), fresh, c, time.Now())
		require.NoError(t, err)
		require.Equal(t, before, dump(t, l))
		require.NoError(t, fresh.Verify(t.Context()))
		conns.check(t)
	})
}

// A leg and services added under traffic interrupt nothing on the other legs.
func TestLinuxALegAddedUnderTrafficInterruptsNoOther(t *testing.T) {
	l := newGatewayLab(t)
	conns := openEchoes(t, l, 10)
	l.bridge("br3", false)
	c := l.guest("c", "br3", 0, "10.60.0.10/24", "")
	serve(t, c, "0.0.0.0:80", "c")
	legC := Leg{ID: "br3", Mark: 4, Bridge: "br3", Prefix: prefix("10.60.0.1/24"), MAC: mac("02:50:43:4f:00:04")}
	services := append([]Service{}, linuxServices...)
	for i := range 10 {
		services = append(services, Service{Leg: "br3", Service: addr(fmt.Sprintf("198.18.2.%d", i)), Addr: addr("10.60.0.10"), Ports: []uint16{80}})
	}
	_, err := l.b.Sync(t.Context(), []Leg{linuxLegA, linuxLegB, linuxLegM, legC}, services, nil, nil)
	require.NoError(t, err)
	got, err := get(l.host, "198.18.2.9:80", nil, time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "c "), got)
	conns.check(t)
}

// A mark that passes to another bridge in one Sync, with a guest at the same
// address behind its new bridge: the namespace has learned the MAC of the
// guest of the old bridge for that address, and keeps sending to it unless
// the leg starts without what it learned there.
func TestLinuxAMarkThatMovesToAnotherBridgeReachesTheGuestThere(t *testing.T) {
	l := newLabNet(t)
	l.bridge("br1", false)
	l.bridge("br3", false)
	a := l.guest("a", "br1", 0, "10.50.0.10/24", "")
	c := l.guest("c", "br3", 0, "10.50.0.10/24", "")
	serve(t, a, "0.0.0.0:80", "a")
	serve(t, c, "0.0.0.0:80", "c")
	require.NoError(t, l.b.Up(t.Context()))
	macA, macC := l.macOf(t, "pcotest-a-"+l.id), l.macOf(t, "pcotest-c-"+l.id)
	neighbour := func() string {
		out, err := exec.Command(ipPath, "-n", l.gwName, "neigh", "show", "dev", "leg1").CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}
	services := func(leg string) []Service {
		return []Service{{Leg: leg, Service: addr("198.18.1.1"), Addr: addr("10.50.0.10"), Ports: []uint16{80}}}
	}
	leg := func(bridge string) Leg {
		return Leg{ID: bridge, Mark: 1, Bridge: bridge, Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:01")}
	}

	states, err := l.b.Sync(t.Context(), []Leg{leg("br1")}, services("br1"), nil, nil)
	require.NoError(t, err)
	require.True(t, states[0].Up, states[0].Problem)
	got, err := get(l.host, "198.18.1.1:80", nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "a "), got)
	require.Contains(t, neighbour(), "10.50.0.10 lladdr "+macA, "the namespace learned the guest of br1")

	states, err = l.b.Sync(t.Context(), []Leg{leg("br3")}, services("br3"), nil, nil)
	require.NoError(t, err)
	require.True(t, states[0].Up, states[0].Problem)
	require.NotContains(t, neighbour(), macA, "nothing of br1 is kept")
	got, err = get(l.host, "198.18.1.1:80", nil, 2*time.Second)
	require.NoError(t, err, "the guest of br3 answers, not the MAC learned in br1")
	require.True(t, strings.HasPrefix(got, "c "), got)
	require.Contains(t, neighbour(), "10.50.0.10 lladdr "+macC)
	require.NoError(t, l.b.Verify(t.Context()))
}

// ifindexOf is the index of an interface of the namespace ns: it changes when
// the interface is made anew.
func (l *labNet) ifindexOf(t *testing.T, ns, name string) int {
	t.Helper()
	out, err := exec.Command(ipPath, "-n", ns, "-j", "link", "show", name).Output()
	require.NoError(t, err)
	var links []struct {
		Ifindex int `json:"ifindex"`
	}
	require.NoError(t, json.Unmarshal(out, &links))
	require.Len(t, links, 1)
	return links[0].Ifindex
}

// portVLANsOf is what `bridge vlan show` says of a port of the host.
func (l *labNet) portVLANsOf(t *testing.T, port string) string {
	t.Helper()
	out, err := exec.Command("bridge", "-n", l.hostName, "vlan", "show", "dev", port).CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.Join(strings.Fields(string(out)), " ")
}

// A mark that passes from a VLAN of a bridge that filters VLANs to the
// untagged network of the same bridge, with the same prefix: the port is put
// in the bridge's default VLAN, not left in the tag, and the guests of the
// untagged network answer, those of the VLAN do not.
func TestLinuxAMarkThatMovesFromAVLANToTheUntaggedNetworkReachesTheGuestThere(t *testing.T) {
	l := newLabNet(t)
	l.bridge("br2", true)
	tagged := l.guest("b", "br2", 20, "10.50.0.10/24", "")
	untagged := l.guest("u", "br2", 0, "10.50.0.10/24", "")
	serve(t, tagged, "0.0.0.0:80", "b")
	serve(t, untagged, "0.0.0.0:80", "u")
	require.NoError(t, l.b.Up(t.Context()))
	leg := func(id string, vlan int) Leg {
		return Leg{ID: id, Mark: 1, Bridge: "br2", VLAN: vlan, Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:01")}
	}
	services := func(leg string) []Service {
		return []Service{{Leg: leg, Service: addr("198.18.1.1"), Addr: addr("10.50.0.10"), Ports: []uint16{80}}}
	}

	states, err := l.b.Sync(t.Context(), []Leg{leg("br2.20", 20)}, services("br2.20"), nil, nil)
	require.NoError(t, err)
	require.True(t, states[0].Up, states[0].Problem)
	got, err := get(l.host, "198.18.1.1:80", nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "b "), got)
	require.Contains(t, l.portVLANsOf(t, "vethpco1"), "20 PVID Egress Untagged")

	states, err = l.b.Sync(t.Context(), []Leg{leg("br2", 0)}, services("br2"), nil, nil)
	require.NoError(t, err)
	require.True(t, states[0].Up, states[0].Problem)
	vlans := l.portVLANsOf(t, "vethpco1")
	require.Contains(t, vlans, "1 PVID Egress Untagged")
	require.NotContains(t, vlans, "20", "the tag of the old network is gone")
	got, err = get(l.host, "198.18.1.1:80", nil, 2*time.Second)
	require.NoError(t, err, "the untagged guest answers")
	require.True(t, strings.HasPrefix(got, "u "), got)
	require.NoError(t, l.b.Verify(t.Context()))
}

// A bridge whose default VLAN is not 1 is no reason to make a leg anew: its
// ports get that VLAN, and every Sync finds the leg where it is.
func TestLinuxABridgeWhoseDefaultVLANIsNotOneLeavesTheLegAlone(t *testing.T) {
	l := newLabNet(t)
	l.bridge("br2", true)
	l.ip("-n", l.hostName, "link", "set", "br2", "type", "bridge", "vlan_default_pvid", "5")
	untagged := l.guest("u", "br2", 0, "10.50.0.10/24", "")
	serve(t, untagged, "0.0.0.0:80", "u")
	require.NoError(t, l.b.Up(t.Context()))
	leg := Leg{ID: "br2", Mark: 1, Bridge: "br2", Prefix: prefix("10.50.0.250/24"), MAC: mac("02:50:43:4f:00:01")}
	services := []Service{{Leg: "br2", Service: addr("198.18.1.1"), Addr: addr("10.50.0.10"), Ports: []uint16{80}}}

	_, err := l.b.Sync(t.Context(), []Leg{leg}, services, nil, nil)
	require.NoError(t, err)
	require.Contains(t, l.portVLANsOf(t, "vethpco1"), "5 PVID Egress Untagged")
	index := l.ifindexOf(t, l.gwName, "leg1")
	got, err := get(l.host, "198.18.1.1:80", nil, 2*time.Second)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, "u "), got)

	for range 3 {
		states, err := l.b.Sync(t.Context(), []Leg{leg}, services, nil, nil)
		require.NoError(t, err)
		require.True(t, states[0].Up, states[0].Problem)
	}
	require.Equal(t, index, l.ifindexOf(t, l.gwName, "leg1"), "the pair is the one that was made")
	require.NoError(t, l.b.Verify(t.Context()))
}

// Duplicate-address detection through a leg on an uplink bridge: a guest
// that holds the address keeps it off; a free one is raised; a bridge whose
// other ports have no carrier cannot be probed, and the address is raised
// unprobed.
func TestLinuxDuplicateAddressDetection(t *testing.T) {
	l := newLabNet(t)
	l.bridge("br1", false)
	l.guest("a", "br1", 0, "10.77.0.5/24", "")
	require.NoError(t, l.b.Up(t.Context()))
	guestMAC := l.macOf(t, "pcotest-a-"+l.id)

	taken := Leg{ID: "br1", Mark: 1, Bridge: "br1", Prefix: prefix("10.77.0.5/24"), MAC: mac("02:50:43:4f:00:01"), Uplink: true}
	states, err := l.b.Sync(t.Context(), []Leg{taken}, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "address in use by "+guestMAC, states[0].Problem)
	require.False(t, states[0].Up)
	have, err := l.sys.addrs(inNS, "leg1")
	require.NoError(t, err)
	require.Empty(t, have)

	free := taken
	free.Prefix = prefix("10.77.0.6/24")
	states, err = l.b.Sync(t.Context(), []Leg{free}, nil, nil, nil)
	require.NoError(t, err)
	require.True(t, states[0].Up, states[0].Problem)
	require.Empty(t, states[0].Problem)
	require.False(t, states[0].DADAt.IsZero())

	l.bridge("br3", false)
	l.ip("-n", l.hostName, "link", "add", "gdown", "type", "dummy")
	l.ip("-n", l.hostName, "link", "set", "gdown", "master", "br3")
	alone := Leg{ID: "br3", Mark: 3, Bridge: "br3", Prefix: prefix("10.78.0.6/24"), MAC: mac("02:50:43:4f:00:03"), Uplink: true}
	states, err = l.b.Sync(t.Context(), []Leg{free, alone}, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "no carrier, DAD not run", states[1].Problem)
	require.True(t, states[1].Up)
}

// A probe asks for the address with the sender 0.0.0.0, as a host does before
// it takes an address, and counts as a claim what answers, announces or
// probes for the address from another MAC.
func TestLinuxAProbeIsSentFromNoAddress(t *testing.T) {
	own, other := mac("02:50:43:4f:00:01"), mac("52:54:00:12:34:56")
	probe := arpProbeFrame(own, addr("10.77.0.5"))
	require.Len(t, probe, arpFrameLen)
	require.Equal(t, mac("ff:ff:ff:ff:ff:ff"), net.HardwareAddr(probe[0:6]))
	require.Equal(t, own, net.HardwareAddr(probe[22:28]), "the sender's MAC")
	require.Equal(t, []byte{0, 0, 0, 0}, probe[28:32], "the sender's address")
	require.Equal(t, []byte{10, 77, 0, 5}, probe[38:42], "the target")

	frame := func(op byte, sha net.HardwareAddr, spa, tpa netip.Addr) []byte {
		f := arpProbeFrame(sha, tpa)
		f[21] = op
		s := spa.As4()
		copy(f[28:32], s[:])
		return f
	}
	for _, tt := range []struct {
		name  string
		frame []byte
		want  bool
	}{
		{"an answer", frame(arpReply, other, addr("10.77.0.5"), addr("0.0.0.0")), true},
		{"an announcement", frame(arpRequest, other, addr("10.77.0.5"), addr("10.77.0.5")), true},
		{"a probe of another host", frame(arpRequest, other, addr("0.0.0.0"), addr("10.77.0.5")), true},
		{"its own probe", probe, false},
		{"a question for another address", frame(arpRequest, other, addr("10.77.0.9"), addr("10.77.0.1")), false},
		{"a short frame", probe[:30], false},
	} {
		got, ok := claimant(tt.frame, own, addr("10.77.0.5"))
		require.Equal(t, tt.want, ok, tt.name)
		if ok {
			require.Equal(t, other, got, tt.name)
		}
	}
}

// The temporary probe port sees a guest on a bridge without a leg, and is
// gone after.
func TestLinuxAProbeWithoutALegGoesThroughATemporaryPort(t *testing.T) {
	l := newLabNet(t)
	l.bridge("br1", false)
	l.guest("a", "br1", 0, "10.77.0.5/24", "")
	require.NoError(t, l.b.Up(t.Context()))
	macs, err := l.b.Probe(t.Context(), "br1", 0, addr("10.77.0.5"))
	require.NoError(t, err)
	require.Len(t, macs, 1)
	require.Equal(t, l.macOf(t, "pcotest-a-"+l.id), macs[0].String())
	_, ok, err := l.sys.link(onHost, probePort)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestLinuxDownLeavesNothing(t *testing.T) {
	l := newGatewayLab(t)
	require.NoError(t, l.b.Down(t.Context()))
	pinned, err := l.sys.pinned()
	require.NoError(t, err)
	require.False(t, pinned)
	links, err := l.sys.links(onHost)
	require.NoError(t, err)
	for _, k := range links {
		require.False(t, strings.HasPrefix(k.name, bridgePortPrefix) || k.name == hostTransit, k.name)
	}
	_, err = l.hostNft.List(t.Context(), hostTable)
	require.ErrorIs(t, err, ErrNotLoaded)
	routes, err := l.sys.routes(onHost, mainTable)
	require.NoError(t, err)
	for _, r := range routes {
		require.NotEqual(t, ServicePrefix, r.dst, r.String())
	}
}

// An empty file where the pin would be, as a pin that failed leaves, is no
// namespace; Down takes it away.
func TestLinuxDownRemovesWhatAFailedPinLeft(t *testing.T) {
	l := newLabNet(t)
	pin := filepath.Join(pinDir, l.gwName)
	require.NoError(t, os.MkdirAll(pinDir, 0o755))
	require.NoError(t, os.WriteFile(pin, nil, 0o444))
	pinned, err := l.sys.pinned()
	require.NoError(t, err)
	require.False(t, pinned)
	require.NoError(t, l.b.Down(t.Context()))
	require.NoFileExists(t, pin)
}

// macOf is the MAC address of eth0 in the namespace ns.
func (l *labNet) macOf(t *testing.T, ns string) string {
	t.Helper()
	out, err := exec.Command(ipPath, "-n", ns, "-j", "link", "show", "eth0").Output()
	require.NoError(t, err)
	var links []struct {
		Address string `json:"address"`
	}
	require.NoError(t, json.Unmarshal(out, &links))
	require.Len(t, links, 1)
	return links[0].Address
}

// dump is what the node and the namespace hold, without counters and
// handles, which a change of nothing leaves as they are.
func dump(t *testing.T, l *gatewayLab) string {
	t.Helper()
	var b strings.Builder
	for _, ns := range []string{l.hostName, l.gwName} {
		for _, args := range [][]string{
			{"-n", ns, "-o", "link", "show"}, {"-n", ns, "addr", "show"}, {"-n", ns, "route", "show", "table", "all"},
			{"-n", ns, "rule", "show"}, {"-n", ns, "neigh", "show", "nud", "permanent"},
		} {
			out, err := exec.Command(ipPath, args...).CombinedOutput()
			require.NoError(t, err, string(out))
			b.WriteString(stripVolatile(string(out)))
		}
		out, err := exec.Command(ipPath, "netns", "exec", ns, nftPath, "list", "ruleset").CombinedOutput()
		require.NoError(t, err, string(out))
		b.WriteString(stripVolatile(string(out)))
	}
	out, err := exec.Command("bridge", "-n", l.hostName, "vlan", "show").CombinedOutput()
	require.NoError(t, err, string(out))
	b.WriteString(string(out))
	out, err = exec.Command("bridge", "-n", l.hostName, "fdb", "show").CombinedOutput()
	require.NoError(t, err, string(out))
	for _, line := range strings.Split(string(out), "\n") {
		// What the bridges learn from the traffic comes and goes.
		if strings.Contains(line, " static") || strings.Contains(line, " permanent") {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// stripVolatile removes what moves by itself: counters, address lifetimes,
// link statistics.
func stripVolatile(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) > 0 && (f[0] == "valid_lft" || f[0] == "packets"):
			continue
		case strings.Contains(line, "counter packets"):
			line, _, _ = strings.Cut(line, "counter packets")
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// echoes are long-lived connections through the legs, each sending a line
// every few milliseconds and reading it back.
type echoes struct {
	stop   chan struct{}
	done   sync.WaitGroup
	errs   chan error
	maxGap atomic.Int64
}

func openEchoes(t *testing.T, l *gatewayLab, n int) *echoes {
	t.Helper()
	e := &echoes{stop: make(chan struct{}), errs: make(chan error, n)}
	for i := range n {
		target := []string{"198.18.1.1:7", "198.18.1.2:7"}[i%2]
		var c net.Conn
		require.NoError(t, Do(l.host, func() (err error) { c, err = net.DialTimeout("tcp4", target, 2*time.Second); return err }))
		e.done.Add(1)
		go func() {
			defer e.done.Done()
			defer func() { _ = c.Close() }()
			r := bufio.NewReader(c)
			last := time.Now()
			for k := 0; ; k++ {
				select {
				case <-e.stop:
					return
				default:
				}
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				line := fmt.Sprintf("%d\n", k)
				if _, err := io.WriteString(c, line); err != nil {
					e.errs <- err
					return
				}
				got, err := r.ReadString('\n')
				if err != nil || got != line {
					e.errs <- fmt.Errorf("%s: %q: %w", target, got, err)
					return
				}
				now := time.Now()
				if gap := now.Sub(last); gap > time.Duration(e.maxGap.Load()) {
					e.maxGap.Store(int64(gap))
				}
				last = now
				time.Sleep(5 * time.Millisecond)
			}
		}()
	}
	t.Cleanup(func() {
		select {
		case <-e.stop:
		default:
			close(e.stop)
		}
		e.done.Wait()
	})
	return e
}

// check stops the connections and fails on any that broke or stalled for
// more than 300 ms.
func (e *echoes) check(t *testing.T) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-e.errs:
		t.Fatalf("a connection through a leg broke: %v", err)
	default:
	}
	require.Less(t, time.Duration(e.maxGap.Load()), 300*time.Millisecond, "the longest stall of a connection")
}
