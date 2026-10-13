//go:build linux

package dhcp

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// requestFilter passes IPv4 frames that are no fragment and carry UDP to port
// 67, so that the packet socket wakes for nothing else of the leg's traffic.
// The parser checks all of it again.
var requestFilter = []unix.SockFilter{
	{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: 12},                    // ether type
	{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 8, K: etherTypeIP},   // else drop
	{Code: unix.BPF_LD | unix.BPF_B | unix.BPF_ABS, K: etherHeader + 9},       // IP protocol
	{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 6, K: protoUDP},      // else drop
	{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_ABS, K: etherHeader + 6},       // flags and offset
	{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jt: 4, K: ipFragFields}, // a fragment: drop
	{Code: unix.BPF_LDX | unix.BPF_B | unix.BPF_MSH, K: etherHeader},          // X = IP header length
	{Code: unix.BPF_LD | unix.BPF_H | unix.BPF_IND, K: etherHeader + 2},       // UDP destination port
	{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: serverPort},    // else drop
	{Code: unix.BPF_RET | unix.BPF_K, K: frameBuf},
	{Code: unix.BPF_RET | unix.BPF_K, K: 0},
}

// socketOptions are the options of the server's sockets on a leg; a test
// leaves some out to show what they do.
type socketOptions struct {
	mark   int
	device bool // SO_BINDTODEVICE on the sending socket
}

// socketOpener opens the sockets inside the namespace ns holds.
func socketOpener(ns Namespace) opener {
	return func(cfg Config) (frameReader, replySender, error) {
		return openSockets(ns, cfg.Iface, socketOptions{mark: cfg.Mark, device: true})
	}
}

func openSockets(ns Namespace, iface string, o socketOptions) (frameReader, replySender, error) {
	var recv *os.File
	var send net.PacketConn
	err := ns(func(h netns.NsHandle) error {
		var err error
		if recv, err = openReceiver(h, iface, o.mark); err != nil {
			return fmt.Errorf("the packet socket: %w", err)
		}
		if send, err = openSender(h, iface, o); err != nil {
			_ = recv.Close()
			return fmt.Errorf("the UDP socket on port %d: %w", serverPort, err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	s := &udpSender{conn: send, done: make(chan struct{})}
	go s.drain()
	return fileReader{recv}, s, nil
}

// openReceiver opens a packet socket for IPv4 bound to the leg, with the
// filter, so that the Ethernet source of every request is read.
func openReceiver(ns netns.NsHandle, iface string, mark int) (*os.File, error) {
	f, err := gateway.Socket(ns, unix.AF_PACKET, unix.SOCK_RAW, gateway.HostToNet16(unix.ETH_P_IP),
		gateway.SocketOptions{Mark: mark, Device: iface})
	if err != nil {
		return nil, err
	}
	rc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	var serr error
	err = rc.Control(func(fd uintptr) {
		prog := unix.SockFprog{Len: uint16(len(requestFilter)), Filter: &requestFilter[0]}
		serr = unix.SetsockoptSockFprog(int(fd), unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &prog)
	})
	if err = errors.Join(err, serr); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("attaching the filter: %w", err)
	}
	return f, nil
}

// openSender opens the UDP socket the replies leave from: port 67 on the
// leg, broadcast allowed, with the leg's mark, so that a unicast reply to a
// renewing guest takes the leg's table.
func openSender(ns netns.NsHandle, iface string, o socketOptions) (net.PacketConn, error) {
	opts := gateway.SocketOptions{
		Mark: o.mark, Broadcast: true, ReuseAddr: true,
		Bind: netip.AddrPortFrom(netip.IPv4Unspecified(), serverPort),
	}
	if o.device {
		opts.Device = iface
	}
	f, err := gateway.Socket(ns, unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_UDP, opts)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return net.FilePacketConn(f)
}

type fileReader struct{ f *os.File }

func (r fileReader) ReadFrame(b []byte) (int, error) { return r.f.Read(b) }
func (r fileReader) Close() error                    { return r.f.Close() }

// udpSender sends the replies; what the socket receives, the same requests
// the packet socket reads, is read and dropped so that it does not fill.
type udpSender struct {
	conn net.PacketConn
	done chan struct{}
}

func (s *udpSender) Send(payload []byte, to netip.Addr) error {
	_, err := s.conn.WriteTo(payload, net.UDPAddrFromAddrPort(netip.AddrPortFrom(to, clientPort)))
	return err
}

func (s *udpSender) Close() error {
	err := s.conn.Close()
	<-s.done
	return err
}

func (s *udpSender) drain() {
	defer close(s.done)
	buf := make([]byte, 1500)
	for {
		if _, _, err := s.conn.ReadFrom(buf); err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				continue
			}
			return
		}
	}
}
