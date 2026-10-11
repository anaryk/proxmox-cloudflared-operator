//go:build linux

package egress_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appnet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// The appliance's three tables in the order the output path meets them:
// the gateway's DNAT at the NAT hook (-100), the service prefix's table at
// filter - 20, the egress filter at filter - 10, each as pco loads it. The
// test builds an appliance of its own in network namespaces, with a guest
// behind a leg and net0 to a LAN; nothing of the machine it runs on is
// touched.

const (
	orderUID   = 64125 // the connector user of the test; no account needs it
	orderProbe = "PCO_EGRESS_PROBE_UID"

	orderGuest   = "10.50.0.10"
	orderService = "198.18.1.1"
	orderNet0    = "192.168.78.240"
	orderLAN     = "192.168.78.1"
)

var orderSeq atomic.Int64

func TestLinuxTheThreeTablesOfTheApplianceInOrder(t *testing.T) {
	if os.Geteuid() != 0 {
		orderRequireLab(t, "needs root to create network namespaces and load nftables")
	}
	for _, bin := range []string{"/usr/sbin/nft", "/usr/sbin/ip"} {
		if _, err := os.Stat(bin); err != nil {
			orderRequireLab(t, "%s is not installed: %v", bin, err)
		}
	}
	id := fmt.Sprintf("%d-%d", os.Getpid()%100000, orderSeq.Add(1))
	app, lan, guest := orderNamespace(t, "pcotest-oapp-"+id), orderNamespace(t, "pcotest-olan-"+id), orderNamespace(t, "pcotest-og-"+id)
	check := exec.Command("/usr/sbin/ip", "netns", "exec", app.name, "/usr/sbin/nft", "-f", "-")
	check.Stdin = strings.NewReader("table inet pcotest_fib {\n\tchain o {\n\t\ttype filter hook output priority filter;\n" +
		"\t\tfib daddr type local accept\n\t}\n}\ndelete table inet pcotest_fib\n")
	if out, err := check.CombinedOutput(); err != nil {
		orderRequireLab(t, "the kernel has no fib expression for inet tables (nft_fib_inet), which the appliance's tables use: %s", out)
	}
	ip := func(args ...string) {
		t.Helper()
		out, err := exec.Command("/usr/sbin/ip", args...).CombinedOutput()
		require.NoError(t, err, "ip %s: %s", strings.Join(args, " "), out)
	}
	ip("-n", app.name, "link", "add", "net0", "type", "veth", "peer", "name", "eth0", "netns", lan.name)
	ip("-n", app.name, "addr", "add", orderNet0+"/24", "dev", "net0")
	ip("-n", app.name, "link", "set", "net0", "up")
	ip("-n", lan.name, "addr", "add", orderLAN+"/24", "dev", "eth0")
	ip("-n", lan.name, "link", "set", "eth0", "up")
	ip("-n", app.name, "route", "add", "default", "via", orderLAN)
	// The leg's card, as Proxmox adds it, straight to the guest.
	ip("-n", app.name, "link", "add", "leg1", "address", "02:50:43:4f:00:01", "type", "veth", "peer", "name", "eth0", "netns", guest.name)
	ip("-n", guest.name, "addr", "add", orderGuest+"/24", "dev", "eth0")
	ip("-n", guest.name, "link", "set", "eth0", "up")
	orderEcho(t, guest.h, "0.0.0.0:80")

	leg := gateway.Leg{ID: "vmbr1", Mark: 1, Bridge: "vmbr1", Prefix: netip.MustParsePrefix("10.50.0.250/24"),
		MAC: net.HardwareAddr{0x02, 0x50, 0x43, 0x4f, 0x00, 0x01}}
	svc := gateway.Service{Leg: "vmbr1", Service: netip.MustParseAddr(orderService), Addr: netip.MustParseAddr(orderGuest), Ports: []uint16{80}}
	netNft, egressNft := appnet.NewNft(), egress.NewNft()
	f := egress.New(egressNft, orderUID, func() ([]netip.Addr, error) { return nil, nil }, egress.NewOverrides(t.TempDir()))
	capture := &orderCapture{}
	var b gateway.Backend
	orderIn(t, app.h, func() error {
		// pco-net.service, then the gateway and the egress filter as pco net
		// load and the daemon load them.
		if _, err := appnet.Load(t.Context(), appnet.NewNetlink(), netNft); err != nil {
			return err
		}
		b = gateway.NewContainer(gateway.NewHostNft(), nil, model.GuestRef{Kind: model.KindLXC, VMID: 9250}, "pve1", nil, zerolog.Nop())
		if err := b.Up(t.Context()); err != nil {
			return err
		}
		states, err := b.Sync(t.Context(), []gateway.Leg{leg}, []gateway.Service{svc}, nil, nil)
		if err != nil {
			return err
		}
		if !states[0].Up {
			return fmt.Errorf("leg1 is not up: %s", states[0].Problem)
		}
		if _, err := egress.Load(t.Context(), egressNft, orderUID, nil, nil); err != nil {
			return err
		}
		return f.Set(t.Context(), []egress.Target{{Addr: svc.Service, Port: 80}})
	})
	require.Empty(t, gateway.Problems(b), "pco-net.service left nothing out")
	capture.start(t, lan.h)
	connector := orderStartProbe(t, app.h, orderUID)
	leaked := func() uint64 {
		var c egress.Counter
		orderIn(t, app.h, func() (err error) { c, err = appnet.Leaked(t.Context(), netNft); return err })
		return c.Packets
	}
	rejected := func() uint64 {
		var live egress.Live
		orderIn(t, app.h, func() (err error) { live, err = egress.ReadLive(t.Context(), egressNft, orderUID); return err })
		return live.Rejected.Packets
	}

	t.Run("a connector reaches the guest through its service address", func(t *testing.T) {
		l0, r0 := leaked(), rejected()
		require.Equal(t, "ok", connector.ask(t, "dial "+orderService+":80"))
		require.Equal(t, l0, leaked())
		require.Equal(t, r0, rejected())
	})

	t.Run("the guest's own address is refused by the egress filter at once", func(t *testing.T) {
		r0 := rejected()
		start := time.Now()
		require.Contains(t, connector.ask(t, "dial "+orderGuest+":80"), "connection refused")
		require.Less(t, time.Since(start), time.Second)
		require.Greater(t, rejected(), r0)
		require.Zero(t, capture.to(orderGuest), "nothing to the guest leaves through net0")
	})

	t.Run("a service address no DNAT maps is refused at once and counted as leaked", func(t *testing.T) {
		l0 := leaked()
		start := time.Now()
		require.Contains(t, connector.ask(t, "dial 198.18.1.9:80"), "connection refused")
		require.Less(t, time.Since(start), time.Second)
		require.Greater(t, leaked(), l0)
		require.Zero(t, capture.to("198.18.1.9"), "nothing to the prefix leaves through net0")
	})

	t.Run("the control: matched by the address after NAT, the service address is refused", func(t *testing.T) {
		script := orderScript(t)
		old := strings.Replace(script, "meta l4proto tcp ct original ip daddr . ct original proto-dst @targets4 accept",
			"ip daddr . tcp dport @targets4 accept", 1)
		require.NotEqual(t, script, old)
		orderIn(t, app.h, func() error { return egressNft.Apply(t.Context(), old) })
		r0 := rejected()
		require.NotEqual(t, "ok", connector.ask(t, "dial "+orderService+":80"))
		require.Greater(t, rejected(), r0)
		orderIn(t, app.h, func() error { return f.Reapply(t.Context()) })
		require.Equal(t, "ok", connector.ask(t, "dial "+orderService+":80"))
	})

	t.Run("the control: before the DNAT, the service prefix's table rejects every request to a leg", func(t *testing.T) {
		// dstnat - 10, written as the number: nft 1.0.6 takes the name in
		// prerouting only.
		before := strings.Replace(appnet.Script(), "priority filter - 20;", "priority -110;", 1)
		require.NotEqual(t, appnet.Script(), before)
		orderIn(t, app.h, func() error { return netNft.Apply(t.Context(), before) })
		l0 := leaked()
		require.Contains(t, connector.ask(t, "dial "+orderService+":80"), "connection refused")
		require.Greater(t, leaked(), l0)
		orderIn(t, app.h, func() error { return netNft.Apply(t.Context(), appnet.Script()) })
		require.Equal(t, "ok", connector.ask(t, "dial "+orderService+":80"))
	})
}

// orderScript returns the egress script of the test's one target, as a fake
// nft takes it.
func orderScript(t *testing.T) string {
	t.Helper()
	c := &orderScripts{}
	g := egress.New(c, orderUID, func() ([]netip.Addr, error) { return nil, nil }, egress.NewOverrides(t.TempDir()))
	require.NoError(t, g.Set(t.Context(), []egress.Target{{Addr: netip.MustParseAddr(orderService), Port: 80}}))
	require.Len(t, c.scripts, 1)
	return c.scripts[0]
}

type orderScripts struct{ scripts []string }

func (c *orderScripts) Apply(_ context.Context, script string) error {
	c.scripts = append(c.scripts, script)
	return nil
}

func (c *orderScripts) List(context.Context) ([]byte, error) { return nil, egress.ErrNotLoaded }

func orderRequireLab(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PCO_REQUIRE_LINUX_TESTS") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

type orderNS struct {
	name string
	h    netns.NsHandle
}

func orderNamespace(t *testing.T, name string) orderNS {
	t.Helper()
	out, err := exec.Command("/usr/sbin/ip", "netns", "add", name).CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { _ = exec.Command("/usr/sbin/ip", "netns", "del", name).Run() })
	h, err := netns.GetFromName(name)
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.Close() })
	out, err = exec.Command("/usr/sbin/ip", "-n", name, "link", "set", "lo", "up").CombinedOutput()
	require.NoError(t, err, string(out))
	return orderNS{name: name, h: h}
}

// orderIn runs fn on an OS thread inside ns that ends with it: the sockets it
// opens and the processes it starts, nft among them, are of ns.
func orderIn(t *testing.T, ns netns.NsHandle, fn func() error) {
	t.Helper()
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		if err = netns.Set(ns); err == nil {
			err = fn()
		}
	}()
	<-done
	require.NoError(t, err)
}

func orderEcho(t *testing.T, ns netns.NsHandle, address string) {
	t.Helper()
	var ln net.Listener
	orderIn(t, ns, func() (err error) { ln, err = net.Listen("tcp4", address); return err })
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

// orderCapture counts the IPv4 packets that reach the LAN through net0, by
// their destination.
type orderCapture struct {
	mu  sync.Mutex
	dst map[string]int
}

func (c *orderCapture) start(t *testing.T, ns netns.NsHandle) {
	t.Helper()
	c.dst = map[string]int{}
	f, err := gateway.Socket(ns, unix.AF_PACKET, unix.SOCK_RAW, gateway.HostToNet16(unix.ETH_P_IP), gateway.SocketOptions{Device: "eth0"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
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
			c.mu.Lock()
			c.dst[netip.AddrFrom4([4]byte(buf[14+16:14+20])).String()]++
			c.mu.Unlock()
		}
	}()
}

func (c *orderCapture) to(addr string) int {
	time.Sleep(200 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dst[addr]
}

// orderProbeProc is the egress package's probe: this test binary run as the
// connector user in the appliance, dialling what it is told.
type orderProbeProc struct {
	in    io.Writer
	lines chan string
}

func orderStartProbe(t *testing.T, ns netns.NsHandle, uid int) *orderProbeProc {
	t.Helper()
	cmd := exec.Command("/proc/self/exe")
	cmd.Env = append(os.Environ(), orderProbe+"="+strconv.Itoa(uid))
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	orderIn(t, ns, cmd.Start)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	p := &orderProbeProc{in: in, lines: make(chan string)}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	return p
}

func (p *orderProbeProc) ask(t *testing.T, command string) string {
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
