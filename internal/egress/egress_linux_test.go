package egress

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// The test builds two network namespaces of its own, a node and a guest
// joined by a veth pair, and loads the table in the node's. Nothing of the
// host it runs on is touched, and both namespaces go with the test.
const (
	// probeEnv makes the test binary a probe: it drops to the uid in the
	// variable and dials what it is told on its input.
	probeEnv = "PCO_EGRESS_PROBE_UID"
	// labUID is the connector user of the test; no account needs it.
	labUID = 64123

	nodeIP     = "10.77.0.1"
	guestIP    = "10.77.0.2"
	nodeIP6    = "fd77::1"
	guestIP6   = "fd77::2"
	publicIP   = "203.0.113.7" // on the guest, and outside every range the edge rule excludes
	resolverIP = "127.0.0.53"

	targetPort     = 8080
	otherPort      = 8081
	managementPort = 8006
	edgePort       = 7844
)

func TestMain(m *testing.M) {
	if uid := os.Getenv(probeEnv); uid != "" {
		os.Exit(runProbe(uid))
	}
	os.Exit(m.Run())
}

func TestLinuxEgressFilter(t *testing.T) {
	lab := newEgressLab(t)
	nft := NewNft()
	f := New(nft, labUID, func() ([]netip.Addr, error) { return []netip.Addr{addr(resolverIP)}, nil }, NewOverrides(t.TempDir()))
	guest := netip.MustParseAddr(guestIP)
	allowed := []Target{{Addr: guest, Port: targetPort}}

	var loaded bool
	lab.inNode(t, func() (err error) {
		loaded, err = Load(t.Context(), nft, labUID, []netip.Addr{addr(resolverIP)}, nil)
		return err
	})
	require.True(t, loaded)
	connector := lab.probe(t, labUID)
	root := lab.probe(t, 0)

	t.Run("base reaches the resolvers and the edge and nothing else", func(t *testing.T) {
		connector.reaches(t, "udp", hostPort(resolverIP, 53))
		connector.refused(t, "tcp", hostPort(guestIP, targetPort))
		connector.reaches(t, "tcp", hostPort(publicIP, edgePort))
		connector.reaches(t, "udp", hostPort(publicIP, edgePort))
		connector.refused(t, "tcp", hostPort(guestIP, edgePort))
		connector.refused(t, "udp", hostPort(guestIP, edgePort))
		connector.refused(t, "tcp", hostPort(guestIP6, edgePort))
	})

	lab.inNode(t, func() error { return f.Set(t.Context(), allowed) })

	t.Run("a connector reaches its target and nothing beside it", func(t *testing.T) {
		connector.reaches(t, "tcp", hostPort(guestIP, targetPort))
		connector.refused(t, "tcp", hostPort(guestIP, otherPort))
		connector.refused(t, "tcp", hostPort(nodeIP, managementPort))
		connector.refused(t, "tcp", hostPort("127.0.0.1", managementPort))
		connector.refused(t, "tcp", hostPort("::1", managementPort))
		connector.refused(t, "tcp", hostPort(nodeIP6, managementPort))
		connector.refused(t, "tcp", hostPort(guestIP6, targetPort))
		connector.reaches(t, "udp", hostPort(resolverIP, 53))
	})

	t.Run("root is not confined", func(t *testing.T) {
		root.reaches(t, "tcp", hostPort(guestIP, otherPort))
		root.reaches(t, "tcp", hostPort("127.0.0.1", managementPort))
		root.reaches(t, "tcp", hostPort(nodeIP, managementPort))
		root.reaches(t, "tcp", hostPort(guestIP6, targetPort))
	})

	t.Run("the table is the one applied and counts what it rejected", func(t *testing.T) {
		lab.inNode(t, func() error { return f.Verify(t.Context()) })
		var live Live
		lab.inNode(t, func() (err error) { live, err = ReadLive(t.Context(), nft, labUID); return err })
		require.Equal(t, allowed, live.Targets)
		require.Equal(t, []netip.Addr{addr(resolverIP)}, live.Resolvers)
		require.NotZero(t, live.RejectedLocal.Packets)
		require.NotZero(t, live.Rejected.Packets)
	})

	t.Run("a removed target stops at once, also on a connection that is open", func(t *testing.T) {
		connector.hold(t, hostPort(guestIP, targetPort))
		connector.echoes(t)

		lab.inNode(t, func() error { return f.Remove(t.Context(), guest) })

		connector.stalls(t)
		connector.refused(t, "tcp", hostPort(guestIP, targetPort))
		lab.inNode(t, func() error { return f.Verify(t.Context()) })
	})

	lab.inNode(t, func() error { return f.Set(t.Context(), allowed) })
	connector.reaches(t, "tcp", hostPort(guestIP, targetPort))

	for _, tc := range []struct {
		name, script string
		gone         bool
	}{
		{name: "a flushed table", script: "flush table inet pco_egress\n"},
		{name: "an added rule", script: "insert rule inet pco_egress connector tcp dport 8081 accept\n"},
		{name: "a flushed ruleset", script: "flush ruleset\n", gone: true},
	} {
		t.Run("verify notices "+tc.name, func(t *testing.T) {
			lab.inNode(t, func() error { return nft.Apply(t.Context(), tc.script) })

			var err error
			lab.inNode(t, func() error { err = f.Verify(t.Context()); return nil })
			require.ErrorIs(t, err, ErrChanged)
			require.Equal(t, tc.gone, errors.Is(err, ErrNotLoaded), "%v", err)

			lab.inNode(t, func() error { return f.Set(t.Context(), allowed) })
			lab.inNode(t, func() error { return f.Verify(t.Context()) })
			connector.refused(t, "tcp", hostPort(guestIP, otherPort))
		})
	}
}

func hostPort(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// requireLab skips the test, or fails it when PCO_REQUIRE_LINUX_TESTS=1 says
// this environment exists to run it.
func requireLab(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

type egressLab struct{ node, guest netns.NsHandle }

func newEgressLab(t *testing.T) *egressLab {
	t.Helper()
	if os.Geteuid() != 0 {
		requireLab(t, "needs root to create network namespaces and load nftables")
	}
	if _, err := os.Stat(nftPath); err != nil {
		requireLab(t, "nft is not installed: %v", err)
	}
	lab := &egressLab{node: newNamespace(t), guest: newNamespace(t)}
	node, guest := linkHandle(t, lab.node), linkHandle(t, lab.guest)
	require.NoError(t, node.LinkAdd(&netlink.Veth{
		LinkAttrs:     netlink.LinkAttrs{Name: "eg0"},
		PeerName:      "eg1",
		PeerNamespace: netlink.NsFd(lab.guest),
	}))
	setUp(t, node, "eg0", nodeIP+"/24", nodeIP6+"/64")
	setUp(t, guest, "eg1", guestIP+"/24", publicIP+"/32", guestIP6+"/64")
	eg0, err := node.LinkByName("eg0")
	require.NoError(t, err)
	require.NoError(t, node.RouteAdd(&netlink.Route{
		LinkIndex: eg0.Attrs().Index, Scope: netlink.SCOPE_LINK, Dst: mustPrefix(t, "203.0.113.0/24"),
	}))

	for _, port := range []int{targetPort, otherPort, edgePort} {
		echoTCP(t, lab.guest, hostPort("::", port))
	}
	// A socket bound to every address would answer from the guest's first
	// one, which the probe's connected socket does not take for an answer.
	echoUDP(t, lab.guest, hostPort(publicIP, edgePort))
	echoTCP(t, lab.node, hostPort("::", managementPort))
	echoUDP(t, lab.node, hostPort(resolverIP, 53))
	return lab
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

// setUp gives a link its addresses, without duplicate address detection, so
// that the IPv6 ones work at once, and brings it up.
func setUp(t *testing.T, h *netlink.Handle, name string, prefixes ...string) {
	t.Helper()
	link, err := h.LinkByName(name)
	require.NoError(t, err)
	for _, p := range prefixes {
		require.NoError(t, h.AddrAdd(link, &netlink.Addr{IPNet: mustPrefix(t, p), Flags: unix.IFA_F_NODAD}), p)
	}
	require.NoError(t, h.LinkSetUp(link))
}

func mustPrefix(t *testing.T, s string) *net.IPNet {
	t.Helper()
	p := netip.MustParsePrefix(s)
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

// inNode runs fn in the node's namespace: the sockets it opens and the
// processes it starts, nft among them, are the node's.
func (l *egressLab) inNode(t *testing.T, fn func() error) {
	t.Helper()
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(l.node); err == nil {
			err = fn()
		}
	})
	require.NoError(t, err)
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

func echoTCP(t *testing.T, ns netns.NsHandle, address string) {
	t.Helper()
	var ln net.Listener
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err == nil {
			ln, err = net.Listen("tcp", address)
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
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
}

func echoUDP(t *testing.T, ns netns.NsHandle, address string) {
	t.Helper()
	var pc net.PacketConn
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err == nil {
			pc, err = net.ListenPacket("udp", address)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
}

// probe is a process in the node's namespace, running as one uid, that dials
// what it is told.
type probe struct {
	in    io.Writer
	lines chan string
}

func (l *egressLab) probe(t *testing.T, uid int) *probe {
	t.Helper()
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), probeEnv+"="+strconv.Itoa(uid))
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	l.inNode(t, cmd.Start)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	p := &probe{in: in, lines: make(chan string)}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	return p
}

func (p *probe) ask(t *testing.T, command string) string {
	t.Helper()
	_, err := fmt.Fprintln(p.in, command)
	require.NoError(t, err)
	select {
	case line, ok := <-p.lines:
		require.True(t, ok, "the probe ended")
		return line
	case <-time.After(20 * time.Second):
		t.Fatalf("the probe did not answer %q", command)
		return ""
	}
}

func (p *probe) reaches(t *testing.T, network, address string) {
	t.Helper()
	require.Equal(t, "ok", p.ask(t, "dial "+network+" "+address), "%s %s", network, address)
}

// refused expects the dial to fail with an error, as a reject makes it, not
// to run into the timeout of the probe, as a drop would.
func (p *probe) refused(t *testing.T, network, address string) {
	t.Helper()
	got := p.ask(t, "dial "+network+" "+address)
	require.NotEqual(t, "ok", got, "%s %s", network, address)
	require.NotContains(t, got, "timeout", "%s %s", network, address)
}

func (p *probe) hold(t *testing.T, address string) {
	t.Helper()
	require.Equal(t, "ok", p.ask(t, "hold "+address))
}

func (p *probe) echoes(t *testing.T) {
	t.Helper()
	require.Equal(t, "ok", p.ask(t, "echo"))
}

// stalls expects nothing to come back on the held connection.
func (p *probe) stalls(t *testing.T) {
	t.Helper()
	require.Contains(t, p.ask(t, "echo"), "timeout")
}

// runProbe is the probe process: it drops to uid, and answers each line of
// its input with "ok" or the error.
func runProbe(uidText string) int {
	uid, err := strconv.Atoi(uidText)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if uid != 0 {
		// Go sets the ids of every thread of the process.
		for _, step := range []func() error{
			func() error { return syscall.Setgroups(nil) },
			func() error { return syscall.Setgid(uid) },
			func() error { return syscall.Setuid(uid) },
		} {
			if err := step(); err != nil {
				fmt.Fprintln(os.Stderr, "dropping to uid", uid, err)
				return 2
			}
		}
	}
	var held net.Conn
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		var err error
		switch {
		case len(f) == 3 && f[0] == "dial":
			err = dialEcho(f[1], f[2])
		case len(f) == 2 && f[0] == "hold":
			held, err = net.DialTimeout("tcp", f[1], 3*time.Second)
		case len(f) == 1 && f[0] == "echo" && held != nil:
			err = echo(held)
		default:
			err = fmt.Errorf("unknown command %q", sc.Text())
		}
		if err != nil {
			fmt.Println("error:", err)
		} else {
			fmt.Println("ok")
		}
	}
	return 0
}

func dialEcho(network, address string) error {
	c, err := net.DialTimeout(network, address, 3*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return echo(c)
}

// echo sends a byte and waits two seconds for it to come back.
func echo(c net.Conn) error {
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	if _, err := c.Write([]byte{'x'}); err != nil {
		return err
	}
	b := make([]byte, 1)
	_, err := io.ReadFull(c, b)
	return err
}
