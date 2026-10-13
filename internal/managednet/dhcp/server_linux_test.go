//go:build linux

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// The test builds a node of its own: a namespace that plays the host with
// the bridge pconet, the gateway namespace with the managed leg made by the
// host backend (its table, its rule, pco_gw with the SNAT port blocks), and a
// guest in a namespace of its own on a port of pconet with the MAC of its
// lease. Nothing of the machine it runs on is touched.

const (
	ipTool  = "/usr/sbin/ip"
	nftTool = "/usr/sbin/nft"
)

var labCount atomic.Int64

func requireLab(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

var managedLeg = gateway.Leg{
	ID: gateway.ManagedBridge, Mark: 3, Bridge: gateway.ManagedBridge, Prefix: netip.PrefixFrom(legIP, 24),
	MAC: gateway.ManagedLegMAC, Managed: true,
}

type dhcpLab struct {
	t     *testing.T
	b     gateway.Backend
	guest netns.NsHandle
	gName string
}

func ipCmd(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.Command(ipTool, args...).CombinedOutput()
	require.NoError(t, err, "ip %s: %s", strings.Join(args, " "), out)
}

func newDHCPLab(t *testing.T) *dhcpLab {
	t.Helper()
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create network namespaces")
	}
	for _, bin := range []string{ipTool, nftTool} {
		if _, err := os.Stat(bin); err != nil {
			requireLab(t, "%s is not installed: %v", bin, err)
		}
	}
	id := fmt.Sprintf("%d-%d", os.Getpid()%100000, labCount.Add(1))
	hostName, gwName, gName := "pcodhcp-host-"+id, "pcodhcp-gw-"+id, "pcodhcp-guest-"+id
	for _, ns := range []string{hostName, gName} {
		ipCmd(t, "netns", "add", ns)
		t.Cleanup(func() { _ = exec.Command(ipTool, "netns", "del", ns).Run() })
		ipCmd(t, "-n", ns, "link", "set", "lo", "up")
	}
	host, err := netns.GetFromName(hostName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = host.Close() })
	guest, err := netns.GetFromName(gName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = guest.Close() })

	ipCmd(t, "-n", hostName, "link", "add", gateway.ManagedBridge, "type", "bridge")
	ipCmd(t, "-n", hostName, "link", "set", gateway.ManagedBridge, "up")
	ipCmd(t, "-n", hostName, "link", "add", "tap9305i1", "type", "veth", "peer", "name", "eth0", "netns", gName)
	ipCmd(t, "-n", hostName, "link", "set", "tap9305i1", "master", gateway.ManagedBridge)
	ipCmd(t, "-n", hostName, "link", "set", "tap9305i1", "up")
	ipCmd(t, "-n", gName, "link", "set", "eth0", "address", guestMAC.String())
	ipCmd(t, "-n", gName, "link", "set", "eth0", "up")
	for _, key := range []string{"all", "default", "eth0"} {
		require.NoError(t, gateway.Do(guest, func() error {
			return os.WriteFile("/proc/sys/net/ipv4/conf/"+key+"/rp_filter", []byte("0"), 0o644)
		}))
	}

	b := gateway.NewHostAt(gwName, host, gateway.NewNamespaceNft(hostName), zerolog.New(zerolog.NewTestWriter(t)))
	t.Cleanup(func() { _ = b.Down(context.Background()) })
	require.NoError(t, b.Up(t.Context()))
	states, err := b.Sync(t.Context(), []gateway.Leg{managedLeg}, nil, nil, nil)
	require.NoError(t, err)
	require.Len(t, states, 1)
	require.True(t, states[0].Up, states[0].Problem)
	listing, err := gateway.NewNamespaceNft(gwName).List(t.Context(), "pco_gw")
	require.NoError(t, err)
	require.Contains(t, string(listing), "leg_snat", "pco_gw is loaded with the SNAT port blocks")
	return &dhcpLab{t: t, b: b, guest: guest, gName: gName}
}

// namespace holds the gateway namespace as the daemon does.
func (l *dhcpLab) namespace(f func(netns.NsHandle) error) error { return f(l.b.Netns()) }

// serve runs a server on the managed leg with the sockets o opens until the
// test ends.
func (l *dhcpLab) serve(o socketOptions) *Server {
	l.t.Helper()
	cfg := Config{Iface: managedLeg.Iface(), Mark: managedLeg.Mark, Server: legIP, Subnet: subnet}
	open := func(cfg Config) (frameReader, replySender, error) { return openSockets(l.namespace, cfg.Iface, o) }
	s := newServer(cfg, leased(), open, time.Now, zerolog.New(zerolog.NewTestWriter(l.t)))
	ctx, cancel := context.WithCancel(l.t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	l.t.Cleanup(func() {
		cancel()
		require.NoError(l.t, <-done)
	})
	require.Eventually(l.t, s.Bound, 5*time.Second, 10*time.Millisecond)
	return s
}

// client is the guest's DHCP client: a packet socket on eth0 for what it
// sends without an address, and the UDP socket of port 68 for the rest.
type client struct {
	t    *testing.T
	raw  *os.File
	conn net.PacketConn
}

func (l *dhcpLab) client() *client {
	l.t.Helper()
	raw, err := gateway.Socket(l.guest, unix.AF_PACKET, unix.SOCK_RAW, gateway.HostToNet16(unix.ETH_P_IP), gateway.SocketOptions{Device: "eth0"})
	require.NoError(l.t, err)
	l.t.Cleanup(func() { _ = raw.Close() })
	f, err := gateway.Socket(l.guest, unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP, gateway.SocketOptions{
		Device: "eth0", Broadcast: true, ReuseAddr: true, Bind: netip.AddrPortFrom(netip.IPv4Unspecified(), clientPort),
	})
	require.NoError(l.t, err)
	conn, err := net.FilePacketConn(f)
	_ = f.Close()
	require.NoError(l.t, err)
	l.t.Cleanup(func() { _ = conn.Close() })
	return &client{t: l.t, raw: raw, conn: conn}
}

// broadcast sends a message from no address to everyone, as a client
// without a lease does.
func (c *client) broadcast(m clientMsg) {
	c.t.Helper()
	_, err := c.raw.Write(frame(m.chaddr, zero, broadcast, m.payload()))
	require.NoError(c.t, err)
}

// unicast sends a message to the server from the guest's address, as a
// renewing client does.
func (c *client) unicast(m clientMsg) {
	c.t.Helper()
	_, err := c.conn.WriteTo(m.payload(), net.UDPAddrFromAddrPort(netip.AddrPortFrom(legIP, serverPort)))
	require.NoError(c.t, err)
}

// receive returns the next reply of type typ to xid and where it came from;
// false when none came within the wait.
func (c *client) receive(xid uint32, wait time.Duration) (replyView, netip.AddrPort, bool) {
	c.t.Helper()
	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(wait)))
	buf := make([]byte, 1500)
	for {
		n, from, err := c.conn.ReadFrom(buf)
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return replyView{}, netip.AddrPort{}, false
		}
		require.NoError(c.t, err)
		v := view(c.t, sent{msg: buf[:n]})
		if v.xid == xid {
			return v, from.(*net.UDPAddr).AddrPort(), true
		}
	}
}

var serverAddr = netip.AddrPortFrom(legIP, serverPort)

func unmapped(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// A guest's client gets its lease in the namespace as the product runs it:
// the broadcast OFFER and ACK and, once the guest has the address, the
// unicast ACK of a renewal, each from the leg's address and port 67. The
// SNAT port block of the leg is TCP only and leaves them alone.
func TestLinuxAGuestGetsAndRenewsItsLease(t *testing.T) {
	l := newDHCPLab(t)
	l.serve(socketOptions{mark: managedLeg.Mark, device: true})
	c := l.client()

	c.broadcast(clientMsg{typ: msgDiscover, xid: 1, chaddr: guestMAC})
	offer, from, ok := c.receive(1, 3*time.Second)
	require.True(t, ok, "an OFFER")
	require.Equal(t, msgOffer, offer.typ)
	require.Equal(t, guestIP, offer.yiaddr)
	require.Equal(t, serverAddr, unmapped(from))

	c.broadcast(clientMsg{typ: msgRequest, xid: 2, chaddr: guestMAC, opts: append(opt4(optRequestedIP, guestIP), opt4(optServerID, legIP)...)})
	ack, from, ok := c.receive(2, 3*time.Second)
	require.True(t, ok, "an ACK")
	require.Equal(t, msgAck, ack.typ)
	require.Equal(t, serverAddr, unmapped(from))

	ipCmd(t, "-n", l.gName, "addr", "add", netip.PrefixFrom(guestIP, 24).String(), "dev", "eth0")
	c.unicast(clientMsg{typ: msgRequest, xid: 3, ciaddr: guestIP, chaddr: guestMAC})
	renewed, from, ok := c.receive(3, 3*time.Second)
	require.True(t, ok, "the ACK of the renewal")
	require.Equal(t, msgAck, renewed.typ)
	require.Equal(t, guestIP, renewed.ciaddr)
	require.Equal(t, serverAddr, unmapped(from))
}

// Which option carries the unicast reply of a renewal to the guest: with
// neither the mark nor the device the namespace has no route to the guest
// and nothing arrives (the control); the mark alone takes the leg's table.
// Device alone is logged: the kernel takes the address of a socket bound to
// a device for one on the link when no route matches.
func TestLinuxTheRenewalIsAnsweredThroughTheLegsTable(t *testing.T) {
	cases := []struct {
		name   string
		o      socketOptions
		arrive *bool
	}{
		{"neither the mark nor the device", socketOptions{}, new(false)},
		{"the mark", socketOptions{mark: managedLeg.Mark}, new(true)},
		{"the device", socketOptions{device: true}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newDHCPLab(t)
			l.serve(c.o)
			cl := l.client()
			ipCmd(t, "-n", l.gName, "addr", "add", netip.PrefixFrom(guestIP, 24).String(), "dev", "eth0")
			cl.unicast(clientMsg{typ: msgRequest, xid: 7, ciaddr: guestIP, chaddr: guestMAC})
			_, from, ok := cl.receive(7, 2*time.Second)
			if c.arrive == nil {
				t.Logf("with %s the renewal's ACK arrived: %v (from %s)", c.name, ok, from)
				return
			}
			require.Equal(t, *c.arrive, ok)
			if ok {
				require.Equal(t, serverAddr, unmapped(from))
			}
		})
	}
}

// Both sockets are bound to the leg and carry its mark.
func TestLinuxTheSocketsCarryTheLegAndTheMark(t *testing.T) {
	l := newDHCPLab(t)
	recv, send, err := openSockets(l.namespace, managedLeg.Iface(), socketOptions{mark: managedLeg.Mark, device: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = recv.Close(); _ = send.Close() })

	var legIndex int
	require.NoError(t, l.namespace(func(ns netns.NsHandle) error {
		return gateway.Do(ns, func() error {
			ifi, err := net.InterfaceByName(managedLeg.Iface())
			if err == nil {
				legIndex = ifi.Index
			}
			return err
		})
	}))

	raw, err := recv.(fileReader).f.SyscallConn()
	require.NoError(t, err)
	require.NoError(t, raw.Control(func(fd uintptr) {
		mark, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
		require.NoError(t, err)
		require.Equal(t, managedLeg.Mark, mark)
		sa, err := unix.Getsockname(int(fd))
		require.NoError(t, err)
		ll, ok := sa.(*unix.SockaddrLinklayer)
		require.True(t, ok, "%T", sa)
		require.Equal(t, legIndex, ll.Ifindex, "the packet socket is bound to the leg")
	}))

	udp, err := send.(*udpSender).conn.(*net.UDPConn).SyscallConn()
	require.NoError(t, err)
	require.NoError(t, udp.Control(func(fd uintptr) {
		mark, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK)
		require.NoError(t, err)
		require.Equal(t, managedLeg.Mark, mark)
		dev, err := unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		require.NoError(t, err)
		require.Equal(t, managedLeg.Iface(), dev)
		sa, err := unix.Getsockname(int(fd))
		require.NoError(t, err)
		require.Equal(t, &unix.SockaddrInet4{Port: serverPort}, sa)
	}))
}

// A guest that forges another lease's MAC in chaddr gets nothing on the wire
// either.
func TestLinuxAForgedClientGetsNothing(t *testing.T) {
	l := newDHCPLab(t)
	s := l.serve(socketOptions{mark: managedLeg.Mark, device: true})
	c := l.client()
	other := mustMAC("06:50:43:4f:76:09")
	_, err := c.raw.Write(frame(guestMAC, zero, broadcast, clientMsg{typ: msgDiscover, xid: 5, chaddr: other}.payload()))
	require.NoError(t, err)

	_, _, ok := c.receive(5, time.Second)
	require.False(t, ok)
	require.Eventually(t, func() bool { return s.Stats().Spoofed == 1 }, time.Second, 10*time.Millisecond)
}
