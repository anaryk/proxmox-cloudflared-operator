//go:build linux

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// Do runs f on an OS thread of its own that is in the network namespace ns
// for the duration, and moves the thread back to where it was before any
// other goroutine runs on it. A socket f opens stays in ns, whichever thread
// uses it later. When the move back fails, the thread ends with f's
// goroutine instead of serving another one in the wrong namespace. For
// netns.None() f runs in the namespace of the process.
func Do(ns netns.NsHandle, f func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if !ns.IsOpen() {
			defer runtime.UnlockOSThread()
			errc <- f()
			return
		}
		orig, err := netns.Get()
		if err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("reading the namespace of the thread: %w", err)
			return
		}
		defer func() { _ = orig.Close() }()
		if err := netns.Set(ns); err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("entering the network namespace: %w", err)
			return
		}
		ferr := f()
		if err := netns.Set(orig); err != nil {
			errc <- errors.Join(ferr, fmt.Errorf("leaving the network namespace: %w", err))
			return
		}
		runtime.UnlockOSThread()
		errc <- ferr
	}()
	return <-errc
}

// Socket opens a socket in the network namespace ns, non-blocking, with the
// options, and returns it as a file a net.FileConn or net.FilePacketConn
// takes, or that is read and written with deadlines as it is. proto is that
// of socket(2): for a packet socket the protocol in network order, as
// HostToNet16 returns it.
func Socket(ns netns.NsHandle, domain, typ, proto int, o SocketOptions) (*os.File, error) {
	var fd int
	err := Do(ns, func() error {
		var err error
		fd, err = unix.Socket(domain, typ|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, proto)
		if err != nil {
			return fmt.Errorf("opening a socket: %w", err)
		}
		if err := o.apply(fd, domain, proto); err != nil {
			_ = unix.Close(fd)
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "socket"), nil
}

func (o SocketOptions) apply(fd, domain, proto int) error {
	set := func(name string, opt, v int) error {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, opt, v); err != nil {
			return fmt.Errorf("setting %s: %w", name, err)
		}
		return nil
	}
	if o.ReuseAddr {
		if err := set("SO_REUSEADDR", unix.SO_REUSEADDR, 1); err != nil {
			return err
		}
	}
	if o.Broadcast {
		if err := set("SO_BROADCAST", unix.SO_BROADCAST, 1); err != nil {
			return err
		}
	}
	if o.Mark != 0 {
		if err := set("SO_MARK", unix.SO_MARK, o.Mark); err != nil {
			return err
		}
	}
	if o.Device != "" {
		if domain == unix.AF_PACKET {
			ifi, err := net.InterfaceByName(o.Device)
			if err != nil {
				return err
			}
			if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: uint16(proto), Ifindex: ifi.Index}); err != nil {
				return fmt.Errorf("binding to %s: %w", o.Device, err)
			}
		} else if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, o.Device); err != nil {
			return fmt.Errorf("binding to %s: %w", o.Device, err)
		}
	}
	if o.Bind.IsValid() {
		if !o.Bind.Addr().Is4() {
			return fmt.Errorf("%s is no IPv4 address", o.Bind)
		}
		if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: o.Bind.Addr().As4(), Port: int(o.Bind.Port())}); err != nil {
			return fmt.Errorf("binding to %s: %w", o.Bind, err)
		}
	}
	return nil
}

// dialIn connects over TCP from a socket opened in the namespace, with the
// mark, from src.
func dialIn(ctx context.Context, ns netns.NsHandle, mark int, src netip.Addr, dst netip.AddrPort) (net.Conn, error) {
	var conn net.Conn
	err := Do(ns, func() error {
		d := net.Dialer{
			LocalAddr: &net.TCPAddr{IP: src.AsSlice()},
			Control: func(_, _ string, c syscall.RawConn) error {
				var serr error
				if err := c.Control(func(fd uintptr) {
					serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, mark)
				}); err != nil {
					return err
				}
				return serr
			},
		}
		var err error
		conn, err = d.DialContext(ctx, "tcp4", dst.String())
		return err
	})
	return conn, err
}
