//go:build linux

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"syscall"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// sourceSnap is how much of a frame the watch reads: the header is all it
// looks at.
const sourceSnap = 64

// sourceFilter lets through the frames a card shows itself with, and
// nothing of the traffic the leg carries for the connectors: a frame to a
// broadcast or multicast address (ARP requests, DHCP, IPv6 neighbour
// discovery) and any ARP frame. It reads the first octet of the destination
// and the type.
var sourceFilter = []unix.SockFilter{
	{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: 0},
	{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jt: 2, Jf: 0, K: 1},
	{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: 12},
	{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 0, Jf: 1, K: etherTypeARP},
	{Code: unix.BPF_RET | unix.BPF_K, K: sourceSnap},
	{Code: unix.BPF_RET | unix.BPF_K, K: 0},
}

// Run reads the frames that reach dev in the network namespace ns until ctx
// ends, and looks at each: those the leg sends itself are not read. The MAC
// dev has is the leg's own as well. It returns an error when it cannot
// listen, or when dev goes away, after which it is to be run again on the
// leg that is there then.
func (w SourceWatch) Run(ctx context.Context, ns netns.NsHandle, dev string) error {
	f, err := Socket(ns, unix.AF_PACKET, unix.SOCK_RAW, HostToNet16(unix.ETH_P_ALL), SocketOptions{Device: dev})
	if err != nil {
		return fmt.Errorf("listening on %s: %w", dev, err)
	}
	defer func() { _ = f.Close() }()
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER,
			&unix.SockFprog{Len: uint16(len(sourceFilter)), Filter: &sourceFilter[0]})
	}); err != nil {
		return err
	}
	if serr != nil {
		return fmt.Errorf("filtering the frames on %s: %w", dev, serr)
	}
	if own, err := macIn(ns, dev); err == nil {
		w.Own = append(slices.Clone(w.Own), own)
	}
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	buf := make([]byte, sourceSnap)
	for {
		n, outgoing, err := recvFrame(raw, buf)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			return fmt.Errorf("reading the frames on %s: %w", dev, err)
		case !outgoing:
			w.Frame(buf[:n])
		}
	}
}

// recvFrame reads one frame and says whether it is one the socket's own
// side sent.
func recvFrame(raw syscall.RawConn, buf []byte) (n int, outgoing bool, err error) {
	var from unix.Sockaddr
	var rerr error
	if err := raw.Read(func(fd uintptr) bool {
		n, from, rerr = unix.Recvfrom(int(fd), buf, 0)
		return !errors.Is(rerr, unix.EAGAIN)
	}); err != nil {
		return 0, false, err
	}
	if rerr != nil {
		return 0, false, rerr
	}
	ll, ok := from.(*unix.SockaddrLinklayer)
	return n, ok && ll.Pkttype == unix.PACKET_OUTGOING, nil
}

// macIn returns the MAC of an interface of a network namespace.
func macIn(ns netns.NsHandle, dev string) (net.HardwareAddr, error) {
	var mac net.HardwareAddr
	err := Do(ns, func() error {
		ifi, err := net.InterfaceByName(dev)
		if err != nil {
			return err
		}
		mac = ifi.HardwareAddr
		return nil
	})
	return mac, err
}
