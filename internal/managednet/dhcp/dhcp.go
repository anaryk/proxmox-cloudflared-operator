// Package dhcp is the DHCP server of the managed network: it gives each
// virtual machine on pconet the address of its lease, and nothing else, to
// nobody else. The packets are read and written here by hand: the server
// needs a small part of DHCPv4, and every byte it reads comes from a guest.
package dhcp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// msgType is the DHCP message type, option 53.
type msgType byte

const (
	msgDiscover msgType = 1
	msgOffer    msgType = 2
	msgRequest  msgType = 3
	msgDecline  msgType = 4
	msgAck      msgType = 5
	msgNak      msgType = 6
	msgRelease  msgType = 7
	msgInform   msgType = 8
)

func (t msgType) String() string {
	names := [...]string{"", "DISCOVER", "OFFER", "REQUEST", "DECLINE", "ACK", "NAK", "RELEASE", "INFORM"}
	if int(t) < len(names) && t != 0 {
		return names[t]
	}
	return fmt.Sprintf("type %d", byte(t))
}

// The options the server reads and writes.
const (
	optPad         = 0
	optSubnetMask  = 1
	optRequestedIP = 50
	optLeaseTime   = 51
	optMsgType     = 53
	optServerID    = 54
	optRenewal     = 58
	optRebinding   = 59
	optEnd         = 255
)

const (
	opRequest = 1
	opReply   = 2
	// htypeEthernet and its address length are the only hardware the
	// managed network has.
	htypeEthernet = 1
	hlenEthernet  = 6
	// bootpFixed is the fixed part of a message, before the magic cookie;
	// minMessage that with the cookie, and minReply the size a reply is
	// padded to, which some old clients need.
	bootpFixed = 236
	minMessage = bootpFixed + 4
	minReply   = 300
	// flagBroadcast is the bit of the flags a client sets when it cannot
	// take a unicast reply before it has an address.
	flagBroadcast = 0x8000

	etherHeader  = 14
	etherTypeIP  = 0x0800
	ipv4MinHdr   = 20
	udpHeader    = 8
	protoUDP     = 17
	serverPort   = 67
	clientPort   = 68
	ipFragFields = 0x3fff // the more-fragments flag and the offset
)

var magicCookie = [4]byte{99, 130, 83, 99}

// errMalformed is a frame the server does not take: not a DHCP request it can
// read, whatever the reason, which the error names.
var errMalformed = errors.New("not a DHCP request")

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errMalformed, fmt.Sprintf(format, args...))
}

// request is what the server reads of a client's message.
type request struct {
	src    net.HardwareAddr // the Ethernet source of the frame
	xid    uint32
	flags  uint16
	ciaddr netip.Addr
	chaddr net.HardwareAddr
	typ    msgType
	// requested is option 50 and serverID option 54; invalid when absent.
	requested netip.Addr
	serverID  netip.Addr
}

// parseFrame reads an Ethernet frame that carries a DHCP request to port 67:
// IPv4 without fragments, UDP from port 68. Every length is checked against
// what was read before it is used, so that no frame reads past its end.
func parseFrame(frame []byte) (request, error) {
	if len(frame) < etherHeader+ipv4MinHdr+udpHeader {
		return request{}, malformed("%d bytes", len(frame))
	}
	if t := binary.BigEndian.Uint16(frame[12:14]); t != etherTypeIP {
		return request{}, malformed("ether type %#04x", t)
	}
	src := net.HardwareAddr(append([]byte(nil), frame[6:12]...))
	ip := frame[etherHeader:]
	if ip[0]>>4 != 4 {
		return request{}, malformed("IP version %d", ip[0]>>4)
	}
	ihl := int(ip[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(ip[2:4]))
	switch {
	case ihl < ipv4MinHdr:
		return request{}, malformed("IP header of %d bytes", ihl)
	case total < ihl+udpHeader || total > len(ip):
		return request{}, malformed("IP length %d of %d bytes", total, len(ip))
	case binary.BigEndian.Uint16(ip[6:8])&ipFragFields != 0:
		return request{}, malformed("a fragment")
	case ip[9] != protoUDP:
		return request{}, malformed("IP protocol %d", ip[9])
	case checksum(ip[:ihl]) != 0:
		return request{}, malformed("IP header checksum")
	}
	udp := ip[ihl:total]
	sport, dport := binary.BigEndian.Uint16(udp[0:2]), binary.BigEndian.Uint16(udp[2:4])
	ulen := int(binary.BigEndian.Uint16(udp[4:6]))
	switch {
	case dport != serverPort || sport != clientPort:
		return request{}, malformed("UDP from port %d to %d", sport, dport)
	case ulen < udpHeader || ulen > len(udp):
		return request{}, malformed("UDP length %d of %d bytes", ulen, len(udp))
	}
	req, err := parseMessage(udp[udpHeader:ulen])
	if err != nil {
		return request{}, err
	}
	req.src = src
	return req, nil
}

// parseMessage reads a client's DHCP message: a BOOTREQUEST over Ethernet,
// not relayed, with a message type. Options other than 50, 53 and 54 are
// skipped, a repeated one counts once, and the client identifier is not read:
// the MAC is the key.
func parseMessage(b []byte) (request, error) {
	if len(b) < minMessage {
		return request{}, malformed("a message of %d bytes", len(b))
	}
	switch {
	case b[0] != opRequest:
		return request{}, malformed("op %d", b[0])
	case b[1] != htypeEthernet || b[2] != hlenEthernet:
		return request{}, malformed("hardware type %d of length %d", b[1], b[2])
	case [4]byte(b[236:240]) != magicCookie:
		return request{}, malformed("no magic cookie")
	}
	giaddr := netip.AddrFrom4([4]byte(b[24:28]))
	if !giaddr.IsUnspecified() || b[3] != 0 {
		return request{}, malformed("relayed by %s", giaddr)
	}
	req := request{
		xid:    binary.BigEndian.Uint32(b[4:8]),
		flags:  binary.BigEndian.Uint16(b[10:12]),
		ciaddr: netip.AddrFrom4([4]byte(b[12:16])),
		chaddr: net.HardwareAddr(append([]byte(nil), b[28:34]...)),
	}
	if req.chaddr[0]&1 != 0 {
		return request{}, malformed("a group address %s as the client's", req.chaddr)
	}
	if err := parseOptions(b[minMessage:], &req); err != nil {
		return request{}, err
	}
	if req.typ == 0 {
		return request{}, malformed("no message type")
	}
	return req, nil
}

// parseOptions reads the options the server needs into req.
func parseOptions(opts []byte, req *request) error {
	var seen [256]bool
	for i := 0; i < len(opts); {
		code := opts[i]
		switch code {
		case optPad:
			i++
			continue
		case optEnd:
			return nil
		}
		if i+1 >= len(opts) {
			return malformed("option %d without a length", code)
		}
		n := int(opts[i+1])
		if i+2+n > len(opts) {
			return malformed("option %d of %d bytes past the end", code, n)
		}
		v := opts[i+2 : i+2+n]
		i += 2 + n
		if seen[code] {
			continue
		}
		seen[code] = true
		switch code {
		case optMsgType:
			if n != 1 || v[0] == 0 {
				return malformed("message type of %d bytes", n)
			}
			req.typ = msgType(v[0])
		case optRequestedIP, optServerID:
			if n != 4 {
				return malformed("option %d of %d bytes", code, n)
			}
			a := netip.AddrFrom4([4]byte(v))
			if code == optRequestedIP {
				req.requested = a
			} else {
				req.serverID = a
			}
		}
	}
	return nil
}

// checksum is the Internet checksum of b; over a header that carries its
// own, it is 0 when the header is intact.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum > 0xffff {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

// reply is a message of the server: an OFFER or an ACK with the lease, or a
// NAK.
type reply struct {
	typ    msgType
	xid    uint32
	flags  uint16
	ciaddr netip.Addr // the client's, in an ACK
	yiaddr netip.Addr // the lease; unspecified in a NAK
	chaddr net.HardwareAddr
	server netip.Addr   // option 54
	subnet netip.Prefix // option 1
	lease  time.Duration
}

// marshal writes the UDP payload of the reply: options 53 and 54, and for an
// OFFER or an ACK 1, 51, 58 and 59 with T1 at half the lease and T2 at seven
// eighths of it. No router, no name server, nothing else.
func (r reply) marshal() []byte {
	b := make([]byte, minMessage, minReply)
	b[0], b[1], b[2] = opReply, htypeEthernet, hlenEthernet
	binary.BigEndian.PutUint32(b[4:8], r.xid)
	binary.BigEndian.PutUint16(b[10:12], r.flags&flagBroadcast)
	put4(b[12:16], r.ciaddr)
	put4(b[16:20], r.yiaddr)
	copy(b[28:44], r.chaddr)
	copy(b[236:240], magicCookie[:])
	b = append(b, optMsgType, 1, byte(r.typ))
	b = append(b, optServerID, 4)
	b = append(b, as4(r.server)...)
	if r.typ != msgNak {
		mask := net.CIDRMask(r.subnet.Bits(), 32)
		b = append(b, optSubnetMask, 4)
		b = append(b, mask...)
		b = appendSeconds(b, optLeaseTime, r.lease)
		b = appendSeconds(b, optRenewal, r.lease/2)
		b = appendSeconds(b, optRebinding, r.lease*7/8)
	}
	b = append(b, optEnd)
	for len(b) < minReply {
		b = append(b, optPad)
	}
	return b
}

// replyTTL is the time to live of a reply's frame; the client is on the
// link.
const replyTTL = 64

// frame wraps the reply as a frame from the leg to a client's card: a
// client without an address cannot be reached through the kernel's routes,
// as it answers no ARP for the address it is about to get, so the server
// writes the whole frame, and the client reads it off the wire, as every
// client does before it has an address. The headers carry both checksums:
// a client that checks them drops a frame without.
func (r reply) frame(src, dst net.HardwareAddr, from, to netip.Addr) []byte {
	payload := r.marshal()
	b := make([]byte, etherHeader+ipv4MinHdr+udpHeader+len(payload))
	copy(b[0:6], dst)
	copy(b[6:12], src)
	binary.BigEndian.PutUint16(b[12:14], etherTypeIP)
	ip := b[etherHeader:]
	ip[0] = 0x40 | ipv4MinHdr/4
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(ip)))
	ip[8], ip[9] = replyTTL, protoUDP
	put4(ip[12:16], from)
	put4(ip[16:20], to)
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip[:ipv4MinHdr]))
	udp := ip[ipv4MinHdr:]
	binary.BigEndian.PutUint16(udp[0:2], serverPort)
	binary.BigEndian.PutUint16(udp[2:4], clientPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[udpHeader:], payload)
	sum := udpChecksum(from, to, udp)
	if sum == 0 {
		sum = 0xffff // 0 would say that there is none
	}
	binary.BigEndian.PutUint16(udp[6:8], sum)
	return b
}

// udpChecksum is the checksum of a UDP datagram between two IPv4 addresses,
// over the pseudo header and the datagram; over one that carries its own,
// it is 0 when the datagram is intact.
func udpChecksum(src, dst netip.Addr, udp []byte) uint16 {
	pseudo := make([]byte, 0, 12+len(udp))
	pseudo = append(pseudo, as4(src)...)
	pseudo = append(pseudo, as4(dst)...)
	pseudo = append(pseudo, 0, protoUDP, byte(len(udp)>>8), byte(len(udp)))
	return checksum(append(pseudo, udp...))
}

func appendSeconds(b []byte, code byte, d time.Duration) []byte {
	s := uint32(min(d/time.Second, 1<<32-2))
	return binary.BigEndian.AppendUint32(append(b, code, 4), s)
}

func put4(b []byte, a netip.Addr) { copy(b, as4(a)) }

func as4(a netip.Addr) []byte {
	if !a.Is4() {
		return make([]byte, 4)
	}
	v := a.As4()
	return v[:]
}
