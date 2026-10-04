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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// labUID is the connector user of the test, and otherUID another user;
	// no account needs them.
	labUID   = 64123
	otherUID = 64124

	nodeIP      = "10.77.0.1"
	guestIP     = "10.77.0.2"
	nodeIP6     = "fd77::1"
	guestIP6    = "fd77::2"
	nodePublic  = "198.41.192.1"
	publicIP    = "198.41.192.7" // on the guest: an address of the edge, outside every range the edge rule excludes
	broadcastIP = "198.41.192.255"
	resolverIP  = "127.0.0.53"

	targetPort      = 8080
	otherPort       = 8081
	managementPort  = 8006
	edgePort        = 7844
	metricsPort     = 20300
	nodeServicePort = 8082  // of the node, a service a manual route with allowNode publishes
	seedPort        = 5353  // of the guest, which seeds a flow towards the node
	closedPort      = 40000 // of the node, where nothing listens
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
	ov := NewOverrides(t.TempDir())
	f := New(nft, labUID, func() ([]netip.Addr, error) { return []netip.Addr{addr(resolverIP)}, nil }, ov)
	guest := netip.MustParseAddr(guestIP)
	allowed := []Target{{Addr: guest, Port: targetPort}}
	set := func(t *testing.T) { lab.inNode(t, func() error { return f.Set(t.Context(), allowed) }) }
	verify := func(t *testing.T) error {
		var err error
		lab.inNode(t, func() error { err = f.Verify(t.Context()); return nil })
		return err
	}

	var loaded bool
	lab.inNode(t, func() (err error) {
		loaded, err = Load(t.Context(), nft, labUID, []netip.Addr{addr(resolverIP)}, nil)
		return err
	})
	require.True(t, loaded)
	connector := lab.probe(t, labUID)
	root := lab.probe(t, 0)
	other := lab.probe(t, otherUID)

	t.Run("base reaches the resolvers and the edge and nothing else", func(t *testing.T) {
		lab.arrives(t, connector, lab.resolver, hostPort(resolverIP, 53))
		connector.refused(t, hostPort(guestIP, targetPort))
		connector.reaches(t, hostPort(publicIP, edgePort))
		lab.arrives(t, connector, lab.edge4, hostPort(publicIP, edgePort))
		connector.refused(t, hostPort(guestIP, edgePort))
		connector.refused(t, hostPort(guestIP6, edgePort))
	})

	set(t)

	t.Run("a connector reaches its target and nothing beside it", func(t *testing.T) {
		connector.reaches(t, hostPort(guestIP, targetPort))
		connector.refused(t, hostPort(guestIP, otherPort))
		connector.refused(t, hostPort(nodeIP, managementPort))
		connector.refused(t, hostPort("127.0.0.1", managementPort))
		connector.refused(t, hostPort("::1", managementPort))
		connector.refused(t, hostPort(nodeIP6, managementPort))
		connector.refused(t, hostPort(guestIP6, targetPort))
	})

	t.Run("a resolver is reached on port 53 only", func(t *testing.T) {
		lab.arrives(t, connector, lab.resolver, hostPort(resolverIP, 53))
		lab.stopped(t, connector, root, lab.resolverOther, hostPort(resolverIP, 54))
	})

	t.Run("the edge is unicast to a public address", func(t *testing.T) {
		lab.arrives(t, connector, lab.edge4, hostPort(publicIP, edgePort))
		lab.stopped(t, connector, root, lab.edge4, hostPort(guestIP, edgePort))
		lab.stopped(t, connector, root, lab.edge4, hostPort(broadcastIP, edgePort))
		lab.stopped(t, connector, root, lab.edge6, net.JoinHostPort("ff02::1%eg0", strconv.Itoa(edgePort)))
	})

	t.Run("answers pass for the metrics scrape and for nothing a datagram seeded", func(t *testing.T) {
		connector.listen(t, hostPort("127.0.0.1", metricsPort))
		root.reaches(t, hostPort("127.0.0.1", metricsPort))
		other.reaches(t, hostPort("127.0.0.1", metricsPort))

		// The guest's datagram to a closed port of the node leaves a flow
		// whose reply direction leads from that port back to the guest.
		lab.seed.sendTo(t, hostPort(nodeIP, closedPort))
		lab.stoppedFrom(t, connector, root, closedPort, lab.seed, hostPort(guestIP, seedPort))
	})

	t.Run("root is not confined", func(t *testing.T) {
		root.reaches(t, hostPort(guestIP, otherPort))
		root.reaches(t, hostPort("127.0.0.1", managementPort))
		root.reaches(t, hostPort(nodeIP, managementPort))
		root.reaches(t, hostPort(guestIP6, targetPort))
	})

	// A verified target that becomes an address of the node, as a virtual
	// address that fails over to it, is refused; a target of a manual route
	// with allowNode is reached.
	t.Run("an address of the node is refused as a target and reached as one of allowNode", func(t *testing.T) {
		root.listen(t, hostPort(nodeIP, nodeServicePort))
		node := netip.MustParseAddr(nodeIP)
		apply := func(extra Target) {
			lab.inNode(t, func() error { return f.Set(t.Context(), append(slices.Clone(allowed), extra)) })
		}

		apply(Target{Addr: node, Port: nodeServicePort})
		connector.refused(t, hostPort(nodeIP, nodeServicePort))
		connector.reaches(t, hostPort(guestIP, targetPort))

		apply(Target{Addr: node, Port: nodeServicePort, AllowNode: true})
		connector.reaches(t, hostPort(nodeIP, nodeServicePort))
		connector.refused(t, hostPort(nodeIP, managementPort))
		require.NoError(t, verify(t))

		set(t)
		connector.refused(t, hostPort(nodeIP, nodeServicePort))
	})

	t.Run("the table is the one applied and counts what it rejected", func(t *testing.T) {
		require.NoError(t, verify(t))
		var live Live
		lab.inNode(t, func() (err error) { live, err = ReadLive(t.Context(), nft, labUID); return err })
		require.Equal(t, allowed, live.Targets)
		require.Equal(t, []netip.Addr{addr(resolverIP)}, live.Resolvers)
		require.Empty(t, live.Differences)
		require.NotZero(t, live.RejectedLocal.Packets)
		require.NotZero(t, live.Rejected.Packets)
	})

	t.Run("a block is absolute", func(t *testing.T) {
		public := netip.MustParseAddr(publicIP)
		_, err := ov.Block(public)
		require.NoError(t, err)
		set(t)
		connector.refused(t, hostPort(publicIP, edgePort))
		lab.stopped(t, connector, root, lab.edge4, hostPort(publicIP, edgePort))

		// What pco egress block does to a target between two cycles.
		_, err = ov.Block(guest)
		require.NoError(t, err)
		lab.inNode(t, func() error { _, err := BlockLive(t.Context(), nft, guest); return err })
		connector.refused(t, hostPort(guestIP, targetPort))
		require.NoError(t, verify(t))

		for _, a := range []netip.Addr{public, guest} {
			_, err = ov.Unblock(a)
			require.NoError(t, err)
		}
		set(t)
		connector.reaches(t, hostPort(publicIP, edgePort))
		connector.reaches(t, hostPort(guestIP, targetPort))
	})

	t.Run("a removed target stops at once, also on a connection that is open", func(t *testing.T) {
		connector.hold(t, hostPort(guestIP, targetPort))
		connector.echoes(t)

		lab.inNode(t, func() error { return f.Remove(t.Context(), guest) })

		connector.cut(t)
		connector.refused(t, hostPort(guestIP, targetPort))
		require.NoError(t, verify(t))
	})

	set(t)
	connector.reaches(t, hostPort(guestIP, targetPort))

	for _, tc := range []struct {
		name, script string
		gone         bool
	}{
		{name: "a flushed table", script: "flush table inet pco_egress\n"},
		{name: "an added rule", script: "insert rule inet pco_egress connector tcp dport 8081 accept\n"},
		{name: "a dormant table", script: "add table inet pco_egress { flags dormant; }\n"},
		{name: "a flushed ruleset", script: "flush ruleset\n", gone: true},
	} {
		t.Run("verify notices "+tc.name, func(t *testing.T) {
			lab.inNode(t, func() error { return nft.Apply(t.Context(), tc.script) })

			err := verify(t)
			require.ErrorIs(t, err, ErrChanged)
			require.Equal(t, tc.gone, errors.Is(err, ErrNotLoaded), "%v", err)

			set(t)
			require.NoError(t, verify(t))
			connector.refused(t, hostPort(guestIP, otherPort))
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

type egressLab struct {
	node, guest netns.NsHandle

	// Where datagrams are looked for: the guest's port of the edge, for
	// IPv4 and IPv6, its port that seeded a flow, and two ports of the
	// node's resolver.
	edge4, edge6, seed, resolver, resolverOther *recorder

	tokens atomic.Int64
}

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
	// The node has an address in the guest's public network as well, so that
	// the network has a broadcast address for the node.
	setUp(t, node, "eg0", nodeIP+"/24", nodePublic+"/24", nodeIP6+"/64")
	setUp(t, guest, "eg1", guestIP+"/24", publicIP+"/24", guestIP6+"/64")
	// Until the link-local addresses are past duplicate address detection, a
	// multicast datagram between the two can be lost.
	settled(t, node, "eg0")
	settled(t, guest, "eg1")

	for _, port := range []int{targetPort, otherPort, edgePort} {
		echoTCP(t, lab.guest, hostPort("::", port))
	}
	echoTCP(t, lab.node, hostPort("::", managementPort))
	lab.edge4 = record(t, lab.guest, "udp4", hostPort("0.0.0.0", edgePort))
	lab.edge6 = record(t, lab.guest, "udp6", hostPort("::", edgePort))
	lab.seed = record(t, lab.guest, "udp4", hostPort(guestIP, seedPort))
	lab.resolver = record(t, lab.node, "udp4", hostPort(resolverIP, 53))
	lab.resolverOther = record(t, lab.node, "udp4", hostPort(resolverIP, 54))
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

// settled waits until no IPv6 address of a link is tentative.
func settled(t *testing.T, h *netlink.Handle, name string) {
	t.Helper()
	link, err := h.LinkByName(name)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		addrs, err := h.AddrList(link, netlink.FAMILY_V6)
		if err != nil || len(addrs) == 0 {
			return false
		}
		for _, a := range addrs {
			if a.Flags&unix.IFA_F_TENTATIVE != 0 {
				return false
			}
		}
		return true
	}, 10*time.Second, 50*time.Millisecond, "the IPv6 addresses of %s stay tentative", name)
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
	go serveEcho(ln)
}

func serveEcho(ln net.Listener) {
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
}

// recorder keeps the datagrams that reach a socket.
type recorder struct {
	pc      net.PacketConn
	mu      sync.Mutex
	got     map[string]bool
	arrived chan struct{} // closed and replaced whenever a datagram arrives
}

func record(t *testing.T, ns netns.NsHandle, network, address string) *recorder {
	t.Helper()
	var pc net.PacketConn
	var err error
	onThrowawayThread(func() {
		if err = netns.Set(ns); err == nil {
			pc, err = net.ListenPacket(network, address)
		}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = pc.Close() })
	r := &recorder{pc: pc, got: map[string]bool{}, arrived: make(chan struct{})}
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

// waitFor reports whether the datagram arrives within d.
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

// sendTo sends a datagram from the recorder's socket.
func (r *recorder) sendTo(t *testing.T, address string) {
	t.Helper()
	to, err := net.ResolveUDPAddr("udp", address)
	require.NoError(t, err)
	_, err = r.pc.WriteTo([]byte("seed"), to)
	require.NoError(t, err)
}

func (l *egressLab) token(t *testing.T) string {
	return fmt.Sprintf("%s#%d", t.Name(), l.tokens.Add(1))
}

// arrives expects a datagram from p to address to reach rec.
func (l *egressLab) arrives(t *testing.T, p *probe, rec *recorder, address string) {
	t.Helper()
	token := l.token(t)
	require.Equal(t, "ok", p.ask(t, "send "+address+" "+token), address)
	require.True(t, rec.waitFor(token, 5*time.Second), "%s did not arrive", address)
}

// stopped expects a datagram from p to address not to reach rec. The same
// from root, sent after it, has to arrive, so the path itself is known to
// work; what p sent is looked for once root's is there, and a moment longer.
func (l *egressLab) stopped(t *testing.T, p, root *probe, rec *recorder, address string) {
	t.Helper()
	l.stoppedFrom(t, p, root, 0, rec, address)
}

// stoppedFrom is stopped with both datagrams sent from port of the node, or
// from any port for 0.
func (l *egressLab) stoppedFrom(t *testing.T, p, root *probe, port int, rec *recorder, address string) {
	t.Helper()
	blocked, control := l.token(t), l.token(t)
	p.ask(t, fmt.Sprintf("send %s %s %d", address, blocked, port))
	require.Equal(t, "ok", root.ask(t, fmt.Sprintf("send %s %s %d", address, control, port)), "root to %s", address)
	require.True(t, rec.waitFor(control, 5*time.Second), "root's datagram to %s did not arrive", address)
	require.False(t, rec.waitFor(blocked, 200*time.Millisecond), "the connector's datagram to %s arrived", address)
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

// reaches expects a TCP connection to address to carry a byte both ways.
func (p *probe) reaches(t *testing.T, address string) {
	t.Helper()
	require.Equal(t, "ok", p.ask(t, "dial "+address), address)
}

// refused expects a TCP connection to address to be refused at once, as the
// reset of a reject makes it, IPv6 included: not to run into the timeout of
// the probe, as a drop would.
func (p *probe) refused(t *testing.T, address string) {
	t.Helper()
	got := p.ask(t, "dial "+address)
	require.Contains(t, got, "connection refused", address)
}

func (p *probe) listen(t *testing.T, address string) {
	t.Helper()
	require.Equal(t, "ok", p.ask(t, "listen "+address))
}

func (p *probe) hold(t *testing.T, address string) {
	t.Helper()
	require.Equal(t, "ok", p.ask(t, "hold "+address))
}

func (p *probe) echoes(t *testing.T) {
	t.Helper()
	require.Equal(t, "ok", p.ask(t, "echo"))
}

// cut expects the held connection to be reset.
func (p *probe) cut(t *testing.T) {
	t.Helper()
	require.Contains(t, p.ask(t, "echo"), "reset")
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
		case len(f) == 2 && f[0] == "dial":
			err = dialEcho(f[1])
		case len(f) >= 3 && f[0] == "send":
			err = send(f[1], f[2], f[3:])
		case len(f) == 2 && f[0] == "listen":
			var ln net.Listener
			if ln, err = net.Listen("tcp", f[1]); err == nil {
				go serveEcho(ln)
			}
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

func dialEcho(address string) error {
	c, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return echo(c)
}

// send sends one datagram with the token, from the port given after it when
// one is.
func send(address, token string, from []string) error {
	var local *net.UDPAddr
	if len(from) == 1 && from[0] != "0" {
		port, err := strconv.Atoi(from[0])
		if err != nil {
			return err
		}
		local = &net.UDPAddr{Port: port}
	}
	to, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return err
	}
	c, err := net.DialUDP("udp", local, to)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	_, err = c.Write([]byte(token))
	return err
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
