package dhcp

import (
	"encoding/binary"
	"flag"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

var (
	guestMAC = mustMAC("06:50:43:4f:76:05")
	guestIP  = netip.MustParseAddr("10.203.118.5")
	legIP    = netip.MustParseAddr("10.203.118.1")
	subnet   = netip.MustParsePrefix("10.203.118.0/24")
)

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}

// clientMsg is a client's message as a test writes it.
type clientMsg struct {
	typ    msgType
	xid    uint32
	flags  uint16
	ciaddr netip.Addr
	giaddr netip.Addr
	chaddr net.HardwareAddr
	opts   []byte // after option 53, before the end
}

// payload is the message as a client sends it: the options of a common
// client (client identifier, parameter request list, maximum size, host
// name) beside those the test gives.
func (m clientMsg) payload() []byte {
	b := make([]byte, minMessage)
	b[0], b[1], b[2] = opRequest, htypeEthernet, hlenEthernet
	binary.BigEndian.PutUint32(b[4:8], m.xid)
	binary.BigEndian.PutUint16(b[10:12], m.flags)
	put4(b[12:16], m.ciaddr)
	put4(b[24:28], m.giaddr)
	copy(b[28:44], m.chaddr)
	copy(b[236:240], magicCookie[:])
	b = append(b, optMsgType, 1, byte(m.typ))
	b = append(b, 61, 7, 1)
	b = append(b, m.chaddr...)
	b = append(b, 55, 7, 1, 3, 6, 15, 26, 28, 121)
	b = append(b, 57, 2, 0x05, 0xdc)
	b = append(b, 12, 4, 'v', 'm', '-', '5')
	b = append(b, m.opts...)
	return append(b, optEnd)
}

// frame wraps a payload as a frame from src to the server's port, from ipSrc
// to ipDst.
func frame(src net.HardwareAddr, ipSrc, ipDst netip.Addr, payload []byte) []byte {
	b := make([]byte, etherHeader+ipv4MinHdr+udpHeader, etherHeader+ipv4MinHdr+udpHeader+len(payload))
	copy(b[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(b[6:12], src)
	binary.BigEndian.PutUint16(b[12:14], etherTypeIP)
	ip := b[etherHeader:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipv4MinHdr+udpHeader+len(payload)))
	ip[8], ip[9] = 64, protoUDP
	put4(ip[12:16], ipSrc)
	put4(ip[16:20], ipDst)
	binary.BigEndian.PutUint16(ip[10:12], checksum(ip[:ipv4MinHdr]))
	udp := ip[ipv4MinHdr:]
	binary.BigEndian.PutUint16(udp[0:2], clientPort)
	binary.BigEndian.PutUint16(udp[2:4], serverPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpHeader+len(payload)))
	return append(b, payload...)
}

func opt4(code byte, a netip.Addr) []byte { return append([]byte{code, 4}, as4(a)...) }

var (
	zero      = netip.IPv4Unspecified()
	broadcast = netip.MustParseAddr("255.255.255.255")
)

// The frames of the goldens: what a client sends in each state.
var goldenFrames = map[string][]byte{
	"discover.bin": frame(guestMAC, zero, broadcast, clientMsg{typ: msgDiscover, xid: 0x3903f326, chaddr: guestMAC}.payload()),
	"request.bin": frame(guestMAC, zero, broadcast, clientMsg{typ: msgRequest, xid: 0x3903f326, chaddr: guestMAC,
		opts: append(opt4(optRequestedIP, guestIP), opt4(optServerID, legIP)...)}.payload()),
	"renew.bin":   frame(guestMAC, guestIP, legIP, clientMsg{typ: msgRequest, xid: 0x5a5a0001, ciaddr: guestIP, chaddr: guestMAC}.payload()),
	"rebind.bin":  frame(guestMAC, guestIP, broadcast, clientMsg{typ: msgRequest, xid: 0x5a5a0002, ciaddr: guestIP, chaddr: guestMAC}.payload()),
	"decline.bin": frame(guestMAC, zero, broadcast, clientMsg{typ: msgDecline, xid: 7, chaddr: guestMAC, opts: append(opt4(optRequestedIP, guestIP), opt4(optServerID, legIP)...)}.payload()),
}

// golden returns the file of testdata, written from want first under
// -update.
func golden(t *testing.T, name string, want []byte) []byte {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, want, 0o644))
	}
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func TestTheFramesOfAClientAreRead(t *testing.T) {
	cases := []struct {
		file string
		want request
	}{
		{"discover.bin", request{xid: 0x3903f326, ciaddr: zero, typ: msgDiscover}},
		{"request.bin", request{xid: 0x3903f326, ciaddr: zero, typ: msgRequest, requested: guestIP, serverID: legIP}},
		{"renew.bin", request{xid: 0x5a5a0001, ciaddr: guestIP, typ: msgRequest}},
		{"rebind.bin", request{xid: 0x5a5a0002, ciaddr: guestIP, typ: msgRequest}},
		{"decline.bin", request{xid: 7, ciaddr: zero, typ: msgDecline, requested: guestIP, serverID: legIP}},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			got, err := parseFrame(golden(t, c.file, goldenFrames[c.file]))
			require.NoError(t, err)
			c.want.src, c.want.chaddr = guestMAC, guestMAC
			require.Equal(t, c.want, got)
		})
	}
}

// options reads the options of a reply by code.
func options(t *testing.T, b []byte) map[byte][]byte {
	t.Helper()
	require.GreaterOrEqual(t, len(b), minMessage)
	require.Equal(t, magicCookie[:], b[236:240])
	out := map[byte][]byte{}
	for i := minMessage; i < len(b); {
		code := b[i]
		if code == optPad {
			i++
			continue
		}
		if code == optEnd {
			return out
		}
		n := int(b[i+1])
		require.NotContains(t, out, code, "option %d twice", code)
		out[code] = b[i+2 : i+2+n]
		i += 2 + n
	}
	t.Fatal("no end option")
	return nil
}

func secs(d time.Duration) []byte { return binary.BigEndian.AppendUint32(nil, uint32(d/time.Second)) }

func TestARepliesCarriesTheLeaseAndNothingElse(t *testing.T) {
	base := reply{xid: 0x3903f326, chaddr: guestMAC, server: legIP, subnet: subnet, lease: DefaultLeaseTime}
	cases := []struct {
		file string
		r    reply
	}{
		{"offer.bin", func() reply { r := base; r.typ, r.yiaddr = msgOffer, guestIP; return r }()},
		{"ack.bin", func() reply { r := base; r.typ, r.yiaddr, r.flags = msgAck, guestIP, flagBroadcast; return r }()},
		{"ack-renew.bin", func() reply { r := base; r.typ, r.yiaddr, r.ciaddr = msgAck, guestIP, guestIP; return r }()},
		{"nak.bin", func() reply { r := base; r.typ = msgNak; return r }()},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			b := c.r.marshal()
			require.Equal(t, golden(t, c.file, b), b)
			require.Len(t, b, minReply)
			require.Equal(t, byte(opReply), b[0])
			require.Equal(t, binary.BigEndian.AppendUint32(nil, c.r.xid), b[4:8])
			require.Equal(t, []byte(guestMAC), b[28:34])
			require.Equal(t, make([]byte, 10), b[34:44], "the rest of chaddr is zero")
			require.Equal(t, as4(c.r.ciaddr), b[12:16])
			require.Equal(t, make([]byte, 4), b[20:24], "no next server")
			require.Equal(t, make([]byte, 4), b[24:28], "no relay")
			opts := options(t, b)
			require.Equal(t, []byte{byte(c.r.typ)}, opts[optMsgType])
			require.Equal(t, as4(legIP), opts[optServerID])
			if c.r.typ == msgNak {
				require.Equal(t, make([]byte, 4), b[16:20], "a NAK gives no address")
				require.Len(t, opts, 2)
				return
			}
			require.Equal(t, as4(guestIP), b[16:20])
			require.Equal(t, map[byte][]byte{
				optMsgType: {byte(c.r.typ)}, optServerID: as4(legIP), optSubnetMask: {255, 255, 255, 0},
				optLeaseTime: secs(604800 * time.Second), optRenewal: secs(302400 * time.Second), optRebinding: secs(529200 * time.Second),
			}, opts, "no router, no name server, no domain")
		})
	}
}

func TestWhatIsNoRequestIsRefused(t *testing.T) {
	good := goldenFrames["discover.bin"]
	edit := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	ipOff, udpOff, msgOff := etherHeader, etherHeader+ipv4MinHdr, etherHeader+ipv4MinHdr+udpHeader
	reIPSum := func(b []byte) []byte {
		ip := b[ipOff : ipOff+ipv4MinHdr]
		ip[10], ip[11] = 0, 0
		binary.BigEndian.PutUint16(ip[10:12], checksum(ip))
		return b
	}
	withMsg := func(m clientMsg) []byte { return frame(guestMAC, zero, broadcast, m.payload()) }
	discover := clientMsg{typ: msgDiscover, xid: 1, chaddr: guestMAC}
	cases := map[string][]byte{
		"empty":              nil,
		"cut in the headers": good[:etherHeader+10],
		"cut in the message": good[:msgOff+100],
		"a VLAN tag":         edit(func(b []byte) []byte { b[12], b[13] = 0x81, 0x00; return b }),
		"IPv6":               edit(func(b []byte) []byte { b[ipOff] = 0x65; return reIPSum(b) }),
		"a short IP header":  edit(func(b []byte) []byte { b[ipOff] = 0x44; return reIPSum(b) }),
		"an IP length past the frame": edit(func(b []byte) []byte {
			binary.BigEndian.PutUint16(b[ipOff+2:], uint16(len(b)))
			return reIPSum(b)
		}),
		"a fragment":         edit(func(b []byte) []byte { b[ipOff+6] = 0x20; return reIPSum(b) }),
		"a later fragment":   edit(func(b []byte) []byte { b[ipOff+7] = 0x10; return reIPSum(b) }),
		"TCP":                edit(func(b []byte) []byte { b[ipOff+9] = 6; return reIPSum(b) }),
		"a bad IP checksum":  edit(func(b []byte) []byte { b[ipOff+10]++; return b }),
		"to another port":    edit(func(b []byte) []byte { b[udpOff+3] = 68; return b }),
		"from another port":  edit(func(b []byte) []byte { b[udpOff+1] = 67; return b }),
		"a UDP length past":  edit(func(b []byte) []byte { binary.BigEndian.PutUint16(b[udpOff+4:], 0xffff); return b }),
		"a UDP length short": edit(func(b []byte) []byte { binary.BigEndian.PutUint16(b[udpOff+4:], 4); return b }),
		"a reply":            edit(func(b []byte) []byte { b[msgOff] = opReply; return b }),
		"token ring":         edit(func(b []byte) []byte { b[msgOff+1] = 6; return b }),
		"a long address":     edit(func(b []byte) []byte { b[msgOff+2] = 16; return b }),
		"relayed":            withMsg(clientMsg{typ: msgDiscover, xid: 1, chaddr: guestMAC, giaddr: netip.MustParseAddr("10.0.0.1")}),
		"hops":               edit(func(b []byte) []byte { b[msgOff+3] = 1; return b }),
		"no cookie":          edit(func(b []byte) []byte { b[msgOff+236] = 0; return b }),
		"a group address":    withMsg(clientMsg{typ: msgDiscover, xid: 1, chaddr: mustMAC("01:00:5e:00:00:01")}),
		"no message type": func() []byte {
			p := discover.payload()
			p[minMessage+2] = 0 // option 53 says type 0
			return frame(guestMAC, zero, broadcast, p)
		}(),
		"an option past the end": func() []byte {
			p := discover.payload()
			p = append(p[:len(p)-1], 12, 200, 'x')
			return frame(guestMAC, zero, broadcast, p)
		}(),
		"an option without a length": func() []byte {
			p := discover.payload()
			return frame(guestMAC, zero, broadcast, append(p[:len(p)-1], 12))
		}(),
		"a requested address of 3 bytes": withMsg(clientMsg{typ: msgRequest, xid: 1, chaddr: guestMAC, opts: []byte{optRequestedIP, 3, 1, 2, 3}}),
	}
	twoByteType := discover.payload()
	twoByteType[minMessage+1] = 2
	cases["a message type of 2 bytes"] = frame(guestMAC, zero, broadcast, twoByteType)
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseFrame(b)
			require.ErrorIs(t, err, errMalformed)
		})
	}

	t.Run("padding after the datagram is left alone", func(t *testing.T) {
		got, err := parseFrame(append(append([]byte(nil), good...), make([]byte, 30)...))
		require.NoError(t, err)
		require.Equal(t, msgDiscover, got.typ)
	})
	t.Run("a repeated option counts once", func(t *testing.T) {
		got, err := parseFrame(withMsg(clientMsg{typ: msgRequest, xid: 1, chaddr: guestMAC,
			opts: append(opt4(optRequestedIP, guestIP), opt4(optRequestedIP, legIP)...)}))
		require.NoError(t, err)
		require.Equal(t, guestIP, got.requested)
	})
}

// No frame makes the parser read past its end or panic, and what it takes
// is a request with a type and an Ethernet client.
func FuzzParseFrame(f *testing.F) {
	for _, b := range goldenFrames {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		req, err := parseFrame(b)
		if err != nil {
			require.ErrorIs(t, err, errMalformed)
			return
		}
		require.NotZero(t, req.typ)
		require.Len(t, req.chaddr, 6)
		require.Len(t, req.src, 6)
		require.True(t, req.ciaddr.Is4())
	})
}
