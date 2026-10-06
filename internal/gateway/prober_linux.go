//go:build linux

package gateway

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
)

// routeHere asks the kernel of the calling thread's namespace for the route a
// socket with the mark, bound to src, would send to dst on, through one route
// socket of its own. Having no route, or one that drops the traffic, is
// resolve.ErrNoRoute. The caller is on a thread locked inside the namespace.
func routeHere(mark int, src, dst netip.Addr) (legRoute, error) {
	if !dst.Is4() {
		return legRoute{}, fmt.Errorf("%s is not an IPv4 address", dst)
	}
	h, err := netlink.NewHandle(unix.NETLINK_ROUTE)
	if err != nil {
		return legRoute{}, fmt.Errorf("opening a route socket: %w", err)
	}
	defer h.Close()
	routes, err := h.RouteGetWithOptions(dst.AsSlice(), &netlink.RouteGetOptions{Mark: uint32(mark), SrcAddr: src.AsSlice()})
	switch {
	case noRoute(err), err == nil && len(routes) == 0:
		return legRoute{}, fmt.Errorf("route to %s with mark %d: %w", dst, mark, resolve.ErrNoRoute)
	case err != nil:
		return legRoute{}, fmt.Errorf("looking up the route to %s with mark %d: %w", dst, mark, err)
	}
	r := routes[0]
	link, err := h.LinkByIndex(r.LinkIndex)
	if err != nil {
		return legRoute{}, fmt.Errorf("looking up interface %d: %w", r.LinkIndex, err)
	}
	return legRoute{dev: link.Attrs().Name, direct: r.Gw == nil && r.Via == nil && len(r.MultiPath) == 0, table: r.Table}, nil
}

// noRoute reports whether the kernel answered a route lookup with having no
// route (ENETUNREACH) or one that drops the traffic: unreachable
// (EHOSTUNREACH), blackhole (EINVAL) or prohibit (EACCES). A source address
// the namespace does not have is EINVAL too, which leaves the leg no route.
func noRoute(err error) bool {
	return errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH) ||
		errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EACCES)
}

// nsID is a namespace as the kernel tells it apart: the device and inode of
// its file.
type nsID struct{ dev, ino uint64 }

func idOf(fd int) (nsID, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nsID{}, err
	}
	return nsID{dev: st.Dev, ino: st.Ino}, nil
}

// namespaceHolder returns what holds the namespace of the legs for a probe: a
// duplicate of the backend's handle, which the backend closing its own does
// not take away, after checking that it is a namespace and not the one of
// the process. A handle number the backend closed and something else reused
// would otherwise enter that, the host's namespace among others. The
// process's namespace is read now, from the calling thread: a thread that
// left it is locked to the function that took it there.
func namespaceHolder() func(netns.NsHandle) (netns.NsHandle, func(), error) {
	self, selfErr := processNamespace()
	return func(ns netns.NsHandle) (netns.NsHandle, func(), error) {
		if selfErr != nil {
			return netns.None(), nil, fmt.Errorf("reading the namespace of the process: %w", selfErr)
		}
		fd, err := unix.FcntlInt(uintptr(ns), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return netns.None(), nil, err
		}
		var fs unix.Statfs_t
		id, err := idOf(fd)
		switch {
		case err != nil:
		case unix.Fstatfs(fd, &fs) != nil || fs.Type != unix.NSFS_MAGIC:
			err = errors.New("the handle is no namespace")
		case id == self:
			err = errors.New("the handle is the namespace of the process")
		}
		if err != nil {
			_ = unix.Close(fd)
			return netns.None(), nil, err
		}
		return netns.NsHandle(fd), func() { _ = unix.Close(fd) }, nil
	}
}

func processNamespace() (nsID, error) {
	h, err := netns.Get()
	if err != nil {
		return nsID{}, err
	}
	defer func() { _ = h.Close() }()
	return idOf(int(h))
}
