package gateway

import (
	"encoding/binary"
	"net/netip"
)

// SocketOptions are what Socket sets on a socket before it returns it.
type SocketOptions struct {
	Mark      int    // SO_MARK; 0 sets none
	Device    string // SO_BINDTODEVICE; a packet socket is bound to the device instead
	Broadcast bool   // SO_BROADCAST
	ReuseAddr bool   // SO_REUSEADDR
	// Bind binds an IPv4 socket to the address and port.
	Bind netip.AddrPort
}

// HostToNet16 returns v in network order, as socket(2) takes the protocol of
// a packet socket.
func HostToNet16(v uint16) int {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return int(binary.NativeEndian.Uint16(b))
}
