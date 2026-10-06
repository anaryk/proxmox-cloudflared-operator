package appnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

// The test builds two network namespaces of its own: the appliance, whose
// default route leads through a veth pair to the outside, and the outside,
// which holds 198.18.0.5 and listens on it, as a network on net0 that used
// the prefix would. Nothing of the host it runs on is touched.
const (
	applianceIP = "10.78.0.1"
	outsideIP   = "10.78.0.2"
	inPrefix    = "198.18.0.5"
	udpPort     = 9999
	tcpPort     = 80
	// nftPath is where the runner NewNft returns finds nft.
	nftPath = "/usr/sbin/nft"
)

func TestLinuxTheServicePrefixNeverLeaves(t *testing.T) {
	lab := newNetLab(t)
	nl, nft := NewNetlink(), NewNft()
	ctx := t.Context()

	// Without either guard the prefix leaves: the observer would see a leak.
	lab.leaks(t)

	var changed []string
	lab.in(t, func() (err error) { changed, err = Load(ctx, nl, nft); return err })
	require.Equal(t, []string{nameDevice, nameAddr, nameRoute, nameUnreach, nameRule, nameTable}, changed)
	lab.in(t, func() (err error) { changed, err = Load(ctx, nl, nft); return err })
	require.Empty(t, changed, "a second load changes nothing")
	lab.in(t, func() error { return Verify(ctx, nl, nft) })

	t.Run("the rules and the source of the route", func(t *testing.T) {
		var rules []byte
		lab.in(t, func() (err error) { rules, err = exec.Command("ip", "-4", "rule", "show").Output(); return err })
		require.Contains(t, string(rules), "1890:\tfrom 198.18.0.1 to 198.18.0.0/16 lookup main\n")
		require.Contains(t, string(rules), "1900:\tfrom 198.18.0.1 unreachable\n")

		var route []byte
		lab.in(t, func() (err error) { route, err = exec.Command("ip", "route", "get", inPrefix).Output(); return err })
		require.Contains(t, string(route), "dev pco0 src 198.18.0.1")

		var dialed error
		lab.in(t, func() error {
			_, dialed = (&net.Dialer{LocalAddr: &net.TCPAddr{IP: ServiceSource.AsSlice()}, Timeout: time.Second}).
				DialContext(ctx, "tcp", net.JoinHostPort(outsideIP, fmt.Sprint(tcpPort)))
			return nil
		})
		require.ErrorContains(t, dialed, "unreachable", "the source goes nowhere but to the prefix")
	})

	t.Run("with the route", func(t *testing.T) { lab.rejected(t, nft) })

	t.Run("without the route", func(t *testing.T) {
		lab.in(t, func() error { return exec.Command("ip", "route", "del", ServicePrefix.String(), "dev", Device).Run() })
		var err error
		lab.in(t, func() error { err = Verify(ctx, nl, nft); return nil })
		require.ErrorIs(t, err, egress.ErrChanged)
		require.EqualError(t, err, "the route 198.18.0.0/16 dev pco0 src 198.18.0.1 is missing")

		lab.rejected(t, nft)

		lab.in(t, func() (err error) { changed, err = Load(ctx, nl, nft); return err })
		require.Equal(t, []string{nameRoute}, changed)
	})

	t.Run("after the ruleset was flushed", func(t *testing.T) {
		lab.in(t, func() error { return exec.Command(nftPath, "flush", "ruleset").Run() })
		lab.in(t, func() (err error) { changed, err = Load(ctx, nl, nft); return err })
		require.Equal(t, []string{nameTable}, changed)
		lab.in(t, func() error { return Verify(ctx, nl, nft) })
	})
}

type netLab struct {
	appliance, outside netns.NsHandle
	udp                *recorder
	tokens             int
	mu                 sync.Mutex
}

func newNetLab(t *testing.T) *netLab {
	t.Helper()
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create network namespaces and load nftables")
	}
	if _, err := os.Stat(nftPath); err != nil {
		requireLab(t, "nft is not installed: %v", err)
	}
	if _, err := exec.LookPath("ip"); err != nil {
		requireLab(t, "ip is not installed: %v", err)
	}
	lab := &netLab{appliance: newNamespace(t), outside: newNamespace(t)}
	app, out := linkHandle(t, lab.appliance), linkHandle(t, lab.outside)
	require.NoError(t, app.LinkAdd(&netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: "net0"},
		PeerName:      "up0",
		PeerNamespace: netlink.NsFd(lab.outside),
	}))
	setUp(t, app, "net0", applianceIP+"/24")
	setUp(t, out, "up0", outsideIP+"/24", inPrefix+"/32")
	net0, err := app.LinkByName("net0")
	require.NoError(t, err)
	require.NoError(t, app.RouteAdd(&netlink.Route{LinkIndex: net0.Attrs().Index, Gw: net.ParseIP(outsideIP)}))

	lab.udp = record(t, lab.outside, "0.0.0.0:"+fmt.Sprint(udpPort))
	listenTCP(t, lab.outside, "0.0.0.0:"+fmt.Sprint(tcpPort))
	return lab
}

func requireLab(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// in runs fn in the appliance's namespace: the sockets it opens and the
// processes it starts, nft among them, are the appliance's.
func (l *netLab) in(t *testing.T, fn func() error) {
	t.Helper()
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(l.appliance); err == nil {
			err = fn()
		}
	})
	require.NoError(t, err)
}

func (l *netLab) token(t *testing.T) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens++
	return fmt.Sprintf("%s#%d", t.Name(), l.tokens)
}

// send sends a datagram from the appliance. A packet the output hook rejects
// fails the send with EPERM, which is no failure of the test.
func (l *netLab) send(t *testing.T, to, token string) {
	t.Helper()
	l.in(t, func() error {
		c, err := net.Dial("udp4", net.JoinHostPort(to, fmt.Sprint(udpPort)))
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Write([]byte(token)); err != nil && !errors.Is(err, unix.EPERM) {
			return err
		}
		return nil
	})
}

// leaks expects a datagram to the prefix to reach the outside.
func (l *netLab) leaks(t *testing.T) {
	t.Helper()
	token := l.token(t)
	l.send(t, inPrefix, token)
	require.True(t, l.udp.waitFor(token, 5*time.Second), "the datagram to %s did not leave: the lab cannot see a leak", inPrefix)
}

// rejected expects a datagram to the prefix to be counted and never to reach
// the outside, and a connect to the prefix to fail at once. The datagram
// that the outside's own address is sent after it has to arrive, so the way
// out is known to work; the one to the prefix is looked for once that is
// there, and a moment longer. With the route, a request has the source
// 198.18.0.1: the connect fails at once only when the answer of the reject
// reaches it there.
func (l *netLab) rejected(t *testing.T, nft egress.Nft) {
	t.Helper()
	ctx := t.Context()
	var before, after egress.Counter
	l.in(t, func() (err error) { before, err = Leaked(ctx, nft); return err })

	blocked, control := l.token(t), l.token(t)
	l.send(t, inPrefix, blocked)
	l.send(t, outsideIP, control)
	require.True(t, l.udp.waitFor(control, 5*time.Second), "the datagram to %s did not arrive", outsideIP)
	require.False(t, l.udp.waitFor(blocked, 200*time.Millisecond), "the datagram to %s left the appliance", inPrefix)

	l.in(t, func() (err error) { after, err = Leaked(ctx, nft); return err })
	require.Greater(t, after.Packets, before.Packets, "the counter leaked counts it")

	var dialed error
	var took time.Duration
	l.in(t, func() error {
		start := time.Now()
		c, err := net.DialTimeout("tcp4", net.JoinHostPort(inPrefix, fmt.Sprint(tcpPort)), 5*time.Second)
		took, dialed = time.Since(start), err
		if err == nil {
			_ = c.Close()
		}
		return nil
	})
	require.Error(t, dialed, "a connect to the prefix")
	require.Less(t, took, 100*time.Millisecond, "a connect to the prefix fails at once: %v", dialed)
}

func newNamespace(t *testing.T) netns.NsHandle {
	t.Helper()
	var ns netns.NsHandle
	var err error
	onThrowawayThread(func() { ns, err = netns.New() })
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := ns.Close(); err != nil {
			t.Errorf("closing a namespace: %v", err)
		}
	})
	return ns
}

func linkHandle(t *testing.T, ns netns.NsHandle) *netlink.Handle {
	t.Helper()
	h, err := netlink.NewHandleAt(ns)
	require.NoError(t, err)
	t.Cleanup(h.Close)
	lo, err := h.LinkByName("lo")
	require.NoError(t, err)
	require.NoError(t, h.LinkSetUp(lo))
	return h
}

func setUp(t *testing.T, h *netlink.Handle, name string, prefixes ...string) {
	t.Helper()
	link, err := h.LinkByName(name)
	require.NoError(t, err)
	for _, p := range prefixes {
		require.NoError(t, h.AddrAdd(link, &netlink.Addr{IPNet: ipNet(netip.MustParsePrefix(p))}), p)
	}
	require.NoError(t, h.LinkSetUp(link))
}

// onThrowawayThread runs fn on an OS thread that ends with it, so that the
// namespace fn switches to never reaches another goroutine.
func onThrowawayThread(fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		fn()
	}()
	<-done
}

func listenTCP(t *testing.T, ns netns.NsHandle, address string) {
	t.Helper()
	var ln net.Listener
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err == nil {
			ln, err = net.Listen("tcp4", address)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

// recorder keeps the datagrams that arrive at a socket.
type recorder struct {
	mu      sync.Mutex
	got     map[string]bool
	arrived chan struct{} // closed and replaced whenever a datagram arrives
}

func record(t *testing.T, ns netns.NsHandle, address string) *recorder {
	t.Helper()
	var pc net.PacketConn
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err == nil {
			pc, err = net.ListenPacket("udp4", address)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	r := &recorder{got: map[string]bool{}, arrived: make(chan struct{})}
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			r.mu.Lock()
			r.got[string(buf[:n])] = true
			close(r.arrived)
			r.arrived = make(chan struct{})
			r.mu.Unlock()
		}
	}()
	return r
}

// waitFor reports whether the token arrived within d.
func (r *recorder) waitFor(token string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		r.mu.Lock()
		got, arrived := r.got[token], r.arrived
		r.mu.Unlock()
		if got {
			return true
		}
		select {
		case <-arrived:
		case <-deadline:
			return false
		}
	}
}
