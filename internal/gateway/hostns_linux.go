//go:build linux

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// pinDir is where named network namespaces are pinned, as ip netns does.
const pinDir = "/run/netns"

// NewHost returns the backend of the host profile, with the namespace pinned
// at /run/netns/pco. nsNft runs nft in that namespace, hostNft in the host's.
func NewHost(nsNft, hostNft Nft, log zerolog.Logger) Backend {
	return newHost(newLinuxSystem(Namespace, netns.None()), nsNft, hostNft, log, time.Now)
}

// linuxSystem is the system of a Linux node: the namespace pinned under a
// name, and the host's namespace, which is the process's own unless a test
// gives another.
type linuxSystem struct {
	name   string
	hostNS netns.NsHandle

	mu   sync.Mutex
	ns   netns.NsHandle
	gw   *netlink.Handle
	host *netlink.Handle
}

func newLinuxSystem(name string, hostNS netns.NsHandle) *linuxSystem {
	return &linuxSystem{name: name, hostNS: hostNS, ns: netns.None()}
}

func (s *linuxSystem) pin() string { return filepath.Join(pinDir, s.name) }

// pinned reports whether the pin is a mount of a network namespace: an
// empty file left by a pin that failed is none.
func (s *linuxSystem) pinned() (bool, error) {
	var st unix.Statfs_t
	err := unix.Statfs(s.pin(), &st)
	switch {
	case errors.Is(err, unix.ENOENT):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading %s: %w", s.pin(), err)
	}
	return st.Type == unix.NSFS_MAGIC, nil
}

// mayPin reports whether this process shares the mount namespace of PID 1:
// a pin made in a mount namespace of a service's own is seen by nobody else.
func (s *linuxSystem) mayPin() bool {
	self, err1 := os.Readlink("/proc/self/ns/mnt")
	first, err2 := os.Readlink("/proc/1/ns/mnt")
	return err1 == nil && err2 == nil && self == first
}

// createNamespace makes a network namespace and pins it, as ip netns add
// does: /run/netns is made a shared mount of its own first, so that a pin
// reaches the mount namespaces made later.
func (s *linuxSystem) createNamespace() error {
	if err := os.MkdirAll(pinDir, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("", pinDir, "none", unix.MS_SHARED|unix.MS_REC, ""); errors.Is(err, unix.EINVAL) {
		if err := unix.Mount(pinDir, pinDir, "none", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("making %s a mount: %w", pinDir, err)
		}
		if err := unix.Mount("", pinDir, "none", unix.MS_SHARED|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("sharing %s: %w", pinDir, err)
		}
	} else if err != nil {
		return fmt.Errorf("sharing %s: %w", pinDir, err)
	}
	if err := os.Remove(s.pin()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing what is left of a pin: %w", err)
	}
	f, err := os.OpenFile(s.pin(), os.O_CREATE|os.O_EXCL|os.O_RDONLY, 0o444)
	if err != nil {
		return err
	}
	_ = f.Close()
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			errc <- err
			return
		}
		defer func() { _ = orig.Close() }()
		if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("making a network namespace: %w", err)
			return
		}
		merr := unix.Mount(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()), s.pin(), "none", unix.MS_BIND, "")
		if err := netns.Set(orig); err != nil {
			errc <- errors.Join(merr, fmt.Errorf("leaving the new namespace: %w", err))
			return
		}
		runtime.UnlockOSThread()
		errc <- merr
	}()
	if err := <-errc; err != nil {
		_ = os.Remove(s.pin())
		return err
	}
	return nil
}

// openNamespace opens the pinned namespace, unless the one open is that.
func (s *linuxSystem) openNamespace() error {
	ns, err := netns.GetFromPath(s.pin())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ns.IsOpen() && s.ns.Equal(ns) && s.gw != nil {
		_ = ns.Close()
		return s.hostHandle()
	}
	gw, err := netlink.NewHandleAt(ns)
	if err != nil {
		_ = ns.Close()
		return err
	}
	s.closeNS()
	s.ns, s.gw = ns, gw
	return s.hostHandle()
}

func (s *linuxSystem) hostHandle() error {
	if s.host != nil {
		return nil
	}
	h, err := netlink.NewHandleAt(s.hostNS)
	if err != nil {
		return err
	}
	s.host = h
	return nil
}

func (s *linuxSystem) closeNS() {
	if s.gw != nil {
		s.gw.Close()
		s.gw = nil
	}
	if s.ns.IsOpen() {
		_ = s.ns.Close()
	}
	s.ns = netns.None()
}

func (s *linuxSystem) removeNamespace() error {
	s.mu.Lock()
	s.closeNS()
	s.mu.Unlock()
	if err := unix.Unmount(s.pin(), unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := os.Remove(s.pin()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *linuxSystem) handle() netns.NsHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ns
}

// handles returns the netlink handle and the namespace of a side.
func (s *linuxSystem) handles(sd side) (*netlink.Handle, netns.NsHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sd == onHost {
		if err := s.hostHandle(); err != nil {
			return nil, netns.None(), err
		}
		return s.host, s.hostNS, nil
	}
	if s.gw == nil {
		return nil, netns.None(), errors.New("the namespace is not open")
	}
	return s.gw, s.ns, nil
}

func (s *linuxSystem) nsOf(sd side) (netns.NsHandle, error) {
	_, ns, err := s.handles(sd)
	return ns, err
}

func sysctlPath(key string) string {
	return filepath.Join("/proc/sys", strings.ReplaceAll(key, ".", "/"))
}

func (s *linuxSystem) sysctl(sd side, key string) (string, error) {
	ns, err := s.nsOf(sd)
	if err != nil {
		return "", err
	}
	var v string
	err = Do(ns, func() error {
		b, err := os.ReadFile(sysctlPath(key))
		v = strings.TrimSpace(string(b))
		return err
	})
	return v, err
}

func (s *linuxSystem) setSysctl(sd side, key, value string) error {
	ns, err := s.nsOf(sd)
	if err != nil {
		return err
	}
	return Do(ns, func() error { return os.WriteFile(sysctlPath(key), []byte(value), 0o644) })
}

func (s *linuxSystem) addrs(sd side, dev string) ([]address, error) {
	h, _, err := s.handles(sd)
	if err != nil {
		return nil, err
	}
	l, err := h.LinkByName(dev)
	if err != nil {
		return nil, err
	}
	list, err := h.AddrList(l, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	out := make([]address, 0, len(list))
	for _, a := range list {
		p, ok := prefixOf(a.IPNet)
		if !ok {
			continue
		}
		out = append(out, address{prefix: p, noPrefixRoute: a.Flags&unix.IFA_F_NOPREFIXROUTE != 0})
	}
	return out, nil
}

func (s *linuxSystem) addAddr(sd side, dev string, a address) error {
	h, _, err := s.handles(sd)
	if err != nil {
		return err
	}
	l, err := h.LinkByName(dev)
	if err != nil {
		return err
	}
	na := &netlink.Addr{IPNet: ipNet(a.prefix)}
	if a.noPrefixRoute {
		na.Flags = unix.IFA_F_NOPREFIXROUTE
	}
	return h.AddrAdd(l, na)
}

func (s *linuxSystem) delAddr(sd side, dev string, p netip.Prefix) error {
	h, _, err := s.handles(sd)
	if err != nil {
		return err
	}
	l, err := h.LinkByName(dev)
	if err != nil {
		return err
	}
	return h.AddrDel(l, &netlink.Addr{IPNet: ipNet(p)})
}

func (s *linuxSystem) routes(sd side, table int) ([]route, error) {
	h, _, err := s.handles(sd)
	if err != nil {
		return nil, err
	}
	names, err := linkNames(h)
	if err != nil {
		return nil, err
	}
	var list []netlink.Route
	for range 3 {
		list, err = h.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
		if !errors.Is(err, netlink.ErrDumpInterrupted) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]route, 0, len(list))
	for _, r := range list {
		if r.Type != unix.RTN_UNICAST && r.Type != unix.RTN_UNREACHABLE {
			continue
		}
		out = append(out, routeOf(r, names))
	}
	return out, nil
}

func routeOf(r netlink.Route, names map[int]string) route {
	out := route{table: r.Table, dev: names[r.LinkIndex], metric: r.Priority, unreachable: r.Type == unix.RTN_UNREACHABLE}
	if p, ok := prefixOf(r.Dst); ok && p.Bits() > 0 {
		out.dst = p
	}
	if a, ok := netip.AddrFromSlice(r.Src); ok {
		out.src = a.Unmap()
	}
	if a, ok := netip.AddrFromSlice(r.Gw); ok {
		out.gw = a.Unmap()
	}
	return out
}

func (s *linuxSystem) netlinkRoute(h *netlink.Handle, r route) (*netlink.Route, error) {
	dst := r.dst
	if !dst.IsValid() {
		dst = netip.MustParsePrefix("0.0.0.0/0")
	}
	nr := &netlink.Route{Table: r.table, Dst: ipNet(dst), Priority: r.metric}
	if r.unreachable {
		nr.Type = unix.RTN_UNREACHABLE
		return nr, nil
	}
	l, err := h.LinkByName(r.dev)
	if err != nil {
		return nil, err
	}
	nr.LinkIndex = l.Attrs().Index
	if r.src.IsValid() {
		nr.Src = r.src.AsSlice()
	}
	if r.gw.IsValid() {
		nr.Gw = r.gw.AsSlice()
	} else {
		nr.Scope = netlink.SCOPE_LINK
	}
	return nr, nil
}

func (s *linuxSystem) addRoute(sd side, r route) error {
	h, _, err := s.handles(sd)
	if err != nil {
		return err
	}
	nr, err := s.netlinkRoute(h, r)
	if err != nil {
		return err
	}
	return h.RouteReplace(nr)
}

func (s *linuxSystem) delRoute(sd side, r route) error {
	h, _, err := s.handles(sd)
	if err != nil {
		return err
	}
	nr, err := s.netlinkRoute(h, r)
	if err != nil {
		return err
	}
	return h.RouteDel(nr)
}

func (s *linuxSystem) rules(sd side) ([]rule, error) {
	h, _, err := s.handles(sd)
	if err != nil {
		return nil, err
	}
	list, err := h.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	out := make([]rule, 0, len(list))
	for _, r := range list {
		mark := int(r.Mark)
		// A rule that matches on more than its mark is no rule of a leg. The
		// listing leaves out the action; a rule that looks a table up is the
		// one with a table.
		if r.Src != nil || r.Dst != nil || r.IifName != "" || r.OifName != "" || r.Invert ||
			(r.Mask != nil && *r.Mask != 0xffffffff) {
			mark = -1
		}
		out = append(out, rule{pref: r.Priority, mark: mark, table: r.Table})
	}
	return out, nil
}

func (s *linuxSystem) addRule(sd side, r rule) error {
	h, _, err := s.handles(sd)
	if err != nil {
		return err
	}
	nr := netlink.NewRule()
	nr.Family = unix.AF_INET
	nr.Priority = r.pref
	nr.Mark = uint32(r.mark)
	nr.Table = r.table
	return h.RuleAdd(nr)
}

func (s *linuxSystem) delRule(sd side, r rule) error {
	h, _, err := s.handles(sd)
	if err != nil {
		return err
	}
	nr := netlink.NewRule()
	nr.Family = unix.AF_INET
	nr.Priority = r.pref
	return h.RuleDel(nr)
}

func (s *linuxSystem) dial(ctx context.Context, mark int, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	ns, err := s.nsOf(inNS)
	if err != nil {
		return nil, err
	}
	return dialIn(ctx, ns, mark, src, dst)
}

// linkNames returns the names of the links by index.
func linkNames(h *netlink.Handle) (map[int]string, error) {
	links, err := h.LinkList()
	if err != nil {
		return nil, err
	}
	names := make(map[int]string, len(links))
	for _, l := range links {
		names[l.Attrs().Index] = l.Attrs().Name
	}
	return names, nil
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	if n == nil {
		return netip.Prefix{}, false
	}
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(a.Unmap(), ones), true
}
