package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// fakeLeases is the engine as the server asks it.
type fakeLeases struct {
	mu       sync.Mutex
	addrs    map[string]netip.Addr
	seen     []string
	declined []string
	released []string
}

func newFakeLeases(pairs ...string) *fakeLeases {
	l := &fakeLeases{addrs: map[string]netip.Addr{}}
	for i := 0; i+1 < len(pairs); i += 2 {
		l.addrs[pairs[i]] = netip.MustParseAddr(pairs[i+1])
	}
	return l
}

func (l *fakeLeases) Lookup(mac net.HardwareAddr) (netip.Addr, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.addrs[mac.String()]
	return a, ok
}

func (l *fakeLeases) Seen(mac net.HardwareAddr, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, fmt.Sprintf("%s %s", mac, t.Format(time.RFC3339Nano)))
}

func (l *fakeLeases) Declined(mac net.HardwareAddr, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.declined = append(l.declined, fmt.Sprintf("%s %s", mac, t.Format(time.RFC3339)))
}

func (l *fakeLeases) Released(mac net.HardwareAddr, t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = append(l.released, fmt.Sprintf("%s %s", mac, t.Format(time.RFC3339)))
}

// sent is a reply as the fake socket took it.
type sent struct {
	to  netip.Addr
	msg []byte
}

// fakeSocket is both sockets of a server: frames go in through in, replies
// come out in out.
type fakeSocket struct {
	in      chan []byte
	readErr chan error
	mu      sync.Mutex
	out     []sent
	sendErr error
	closed  chan struct{}
	once    sync.Once
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{in: make(chan []byte), readErr: make(chan error, 1), closed: make(chan struct{})}
}

func (f *fakeSocket) ReadFrame(b []byte) (int, error) {
	select {
	case frame := <-f.in:
		return copy(b, frame), nil
	case err := <-f.readErr:
		return 0, err
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeSocket) Send(payload []byte, to netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.out = append(f.out, sent{to: to, msg: append([]byte(nil), payload...)})
	return nil
}

func (f *fakeSocket) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeSocket) replies() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.out...)
}

var testConfig = Config{Iface: "leg3", Mark: 3, Server: legIP, Subnet: subnet}

type testServer struct {
	*Server
	sock   *fakeSocket
	leases *fakeLeases
	now    time.Time
	opens  int
}

func newTestServer(t *testing.T, leases *fakeLeases) *testServer {
	t.Helper()
	ts := &testServer{sock: newFakeSocket(), leases: leases, now: t0}
	open := func(Config) (frameReader, replySender, error) { ts.opens++; return ts.sock, ts.sock, nil }
	ts.Server = newServer(testConfig, leases, open, func() time.Time { return ts.now }, zerolog.New(zerolog.NewTestWriter(t)))
	return ts
}

// in hands the server a frame as Run would.
func (ts *testServer) in(frame []byte) { ts.handle(frame, ts.sock) }

// reply is what a reply says, as a test compares it.
type replyView struct {
	to     netip.Addr
	typ    msgType
	xid    uint32
	flags  uint16
	ciaddr netip.Addr
	yiaddr netip.Addr
}

func view(t *testing.T, s sent) replyView {
	t.Helper()
	b := s.msg
	return replyView{
		to: s.to, typ: msgType(options(t, b)[optMsgType][0]), xid: uint32(b[4])<<24 | uint32(b[5])<<16 | uint32(b[6])<<8 | uint32(b[7]),
		flags: uint16(b[10])<<8 | uint16(b[11]), ciaddr: netip.AddrFrom4([4]byte(b[12:16])), yiaddr: netip.AddrFrom4([4]byte(b[16:20])),
	}
}

func (ts *testServer) views(t *testing.T) []replyView {
	t.Helper()
	var out []replyView
	for _, s := range ts.sock.replies() {
		out = append(out, view(t, s))
	}
	return out
}

func leased() *fakeLeases { return newFakeLeases(guestMAC.String(), guestIP.String()) }

func msgFrame(m clientMsg) []byte {
	ipSrc, ipDst := zero, broadcast
	if m.ciaddr.IsValid() && !m.ciaddr.IsUnspecified() {
		ipSrc = m.ciaddr
	}
	return frame(m.chaddr, ipSrc, ipDst, m.payload())
}

func TestTheServerAnswersAGuestWithItsLease(t *testing.T) {
	selecting := append(opt4(optRequestedIP, guestIP), opt4(optServerID, legIP)...)
	cases := []struct {
		name string
		in   []byte
		want []replyView
	}{
		{"a DISCOVER is offered the lease, to everyone", goldenFrames["discover.bin"],
			[]replyView{{to: broadcast, typ: msgOffer, xid: 0x3903f326, ciaddr: zero, yiaddr: guestIP}}},
		{"a REQUEST for the lease is acknowledged", goldenFrames["request.bin"],
			[]replyView{{to: broadcast, typ: msgAck, xid: 0x3903f326, ciaddr: zero, yiaddr: guestIP}}},
		{"the broadcast flag is kept", msgFrame(clientMsg{typ: msgDiscover, xid: 9, flags: flagBroadcast, chaddr: guestMAC}),
			[]replyView{{to: broadcast, typ: msgOffer, xid: 9, flags: flagBroadcast, ciaddr: zero, yiaddr: guestIP}}},
		{"a REQUEST for another address is refused", msgFrame(clientMsg{typ: msgRequest, xid: 2, chaddr: guestMAC,
			opts: append(opt4(optRequestedIP, netip.MustParseAddr("10.203.118.9")), opt4(optServerID, legIP)...)}),
			[]replyView{{to: broadcast, typ: msgNak, xid: 2, ciaddr: zero, yiaddr: zero}}},
		{"a REQUEST at a reboot for the lease is acknowledged", msgFrame(clientMsg{typ: msgRequest, xid: 3, chaddr: guestMAC,
			opts: opt4(optRequestedIP, guestIP)}),
			[]replyView{{to: broadcast, typ: msgAck, xid: 3, ciaddr: zero, yiaddr: guestIP}}},
		{"a REQUEST at a reboot for another address is refused", msgFrame(clientMsg{typ: msgRequest, xid: 4, chaddr: guestMAC,
			opts: opt4(optRequestedIP, netip.MustParseAddr("192.168.1.20"))}),
			[]replyView{{to: broadcast, typ: msgNak, xid: 4, ciaddr: zero, yiaddr: zero}}},
		{"a REQUEST naming no address is refused", msgFrame(clientMsg{typ: msgRequest, xid: 5, chaddr: guestMAC}),
			[]replyView{{to: broadcast, typ: msgNak, xid: 5, ciaddr: zero, yiaddr: zero}}},
		{"a client that chose another server hears nothing", msgFrame(clientMsg{typ: msgRequest, xid: 6, chaddr: guestMAC,
			opts: append(opt4(optRequestedIP, guestIP), opt4(optServerID, netip.MustParseAddr("10.203.118.77"))...)}), nil},
		{"a RENEW is answered to the guest's address", goldenFrames["renew.bin"],
			[]replyView{{to: guestIP, typ: msgAck, xid: 0x5a5a0001, ciaddr: guestIP, yiaddr: guestIP}}},
		{"a REBIND is answered to the guest's address", goldenFrames["rebind.bin"],
			[]replyView{{to: guestIP, typ: msgAck, xid: 0x5a5a0002, ciaddr: guestIP, yiaddr: guestIP}}},
		{"a RENEW of another address is refused, to everyone, never to that address", msgFrame(clientMsg{typ: msgRequest, xid: 7,
			ciaddr: netip.MustParseAddr("10.203.118.9"), chaddr: guestMAC}),
			[]replyView{{to: broadcast, typ: msgNak, xid: 7, ciaddr: zero, yiaddr: zero}}},
		{"a DISCOVER naming another address in ciaddr is offered the lease to everyone", msgFrame(clientMsg{typ: msgDiscover, xid: 8,
			ciaddr: netip.MustParseAddr("10.203.118.9"), chaddr: guestMAC}),
			[]replyView{{to: broadcast, typ: msgOffer, xid: 8, ciaddr: zero, yiaddr: guestIP}}},
		{"an INFORM is not answered", msgFrame(clientMsg{typ: msgInform, xid: 10, ciaddr: guestIP, chaddr: guestMAC}), nil},
		{"a DECLINE is not answered", goldenFrames["decline.bin"], nil},
		{"a RELEASE is not answered", msgFrame(clientMsg{typ: msgRelease, xid: 11, ciaddr: guestIP, chaddr: guestMAC}), nil},
		{"an OFFER of a rogue server on the vnet is not answered", msgFrame(clientMsg{typ: msgOffer, xid: 12, chaddr: guestMAC, opts: selecting}), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t, leased())
			ts.in(c.in)
			require.Equal(t, c.want, ts.views(t))
			require.Equal(t, uint64(len(c.want)), ts.Stats().Answered)
		})
	}
}

func TestEveryRequestOfALeaseIsSeenAtItsTime(t *testing.T) {
	leases := leased()
	ts := newTestServer(t, leases)
	ts.in(goldenFrames["discover.bin"])
	ts.now = t0.Add(1500 * time.Millisecond)
	ts.in(goldenFrames["request.bin"])

	require.Equal(t, []string{
		"06:50:43:4f:76:05 2026-10-13T12:00:00Z", "06:50:43:4f:76:05 2026-10-13T12:00:01.5Z",
	}, leases.seen)
}

func TestADeclineAndAReleaseAreTold(t *testing.T) {
	leases := leased()
	ts := newTestServer(t, leases)
	ts.in(goldenFrames["decline.bin"])
	ts.in(msgFrame(clientMsg{typ: msgRelease, xid: 11, ciaddr: guestIP, chaddr: guestMAC}))

	require.Equal(t, []string{"06:50:43:4f:76:05 2026-10-13T12:00:00Z"}, leases.declined)
	require.Equal(t, []string{"06:50:43:4f:76:05 2026-10-13T12:00:00Z"}, leases.released)
	r, ok := leases.Lookup(guestMAC)
	require.True(t, ok, "the lease stays")
	require.Equal(t, guestIP, r)
}

func TestWhatHasNoLeaseGetsNothing(t *testing.T) {
	stranger := mustMAC("52:54:00:12:34:56")
	cases := []struct {
		name   string
		leases *fakeLeases
		mac    net.HardwareAddr
	}{
		{"a MAC without a lease", leased(), stranger},
		{"a lease outside the subnet", newFakeLeases("06:50:43:4f:01:05", "10.9.1.5"), mustMAC("06:50:43:4f:01:05")},
		{"a lease of the leg's address", newFakeLeases("06:50:43:4f:76:01", "10.203.118.1"), mustMAC("06:50:43:4f:76:01")},
		{"a lease of the subnet's own address", newFakeLeases("06:50:43:4f:76:00", "10.203.118.0"), mustMAC("06:50:43:4f:76:00")},
		{"a lease of the broadcast address", newFakeLeases("06:50:43:4f:76:ff", "10.203.118.255"), mustMAC("06:50:43:4f:76:ff")},
		{"a lease whose MAC is not the one of its address", newFakeLeases(stranger.String(), "10.203.118.5"), stranger},
		{"an IPv6 lease", newFakeLeases(guestMAC.String(), "fd00::5"), guestMAC},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts := newTestServer(t, c.leases)
			for _, m := range []clientMsg{
				{typ: msgDiscover, xid: 1, chaddr: c.mac},
				{typ: msgRequest, xid: 2, chaddr: c.mac, opts: opt4(optRequestedIP, guestIP)},
				{typ: msgRequest, xid: 3, chaddr: c.mac, ciaddr: guestIP},
				{typ: msgDecline, xid: 4, chaddr: c.mac},
			} {
				ts.in(msgFrame(m))
			}
			require.Empty(t, ts.sock.replies())
			require.Equal(t, Stats{Unknown: 4}, ts.Stats())
			require.Empty(t, c.leases.seen)
			require.Empty(t, c.leases.declined)
		})
	}
}

// A guest that puts another lease's MAC in chaddr gets nothing: the frame
// still carries its own.
func TestARequestWhoseEthernetSourceIsNotTheClientIsDropped(t *testing.T) {
	leases := leased()
	ts := newTestServer(t, leases)
	forged := frame(mustMAC("06:50:43:4f:76:09"), zero, broadcast, clientMsg{typ: msgDiscover, xid: 1, chaddr: guestMAC}.payload())

	ts.in(forged)

	require.Empty(t, ts.sock.replies())
	require.Equal(t, Stats{Spoofed: 1}, ts.Stats())
	require.Empty(t, leases.seen)
}

func TestWhatIsNoRequestIsCounted(t *testing.T) {
	ts := newTestServer(t, leased())
	ts.in([]byte{1, 2, 3})
	ts.in(goldenFrames["discover.bin"][:100])

	require.Empty(t, ts.sock.replies())
	require.Equal(t, Stats{Malformed: 2}, ts.Stats())
}

func TestDiscoversAreLimitedPerGuestAndInAll(t *testing.T) {
	t.Run("one DISCOVER of a guest a second", func(t *testing.T) {
		ts := newTestServer(t, leased())
		for range 3 {
			ts.in(goldenFrames["discover.bin"])
		}
		require.Len(t, ts.sock.replies(), 1)
		ts.in(goldenFrames["request.bin"])
		require.Len(t, ts.sock.replies(), 2, "the REQUEST that follows is answered")
		ts.now = t0.Add(time.Second)
		ts.in(goldenFrames["discover.bin"])
		require.Len(t, ts.sock.replies(), 3, "a second later the guest is answered again")
		require.Equal(t, uint64(2), ts.Stats().Limited)
	})

	t.Run("a few REQUESTs of a guest a second", func(t *testing.T) {
		ts := newTestServer(t, leased())
		for range 10 {
			ts.in(goldenFrames["renew.bin"])
		}
		require.Len(t, ts.sock.replies(), othersPerMAC)
		require.Equal(t, uint64(10-othersPerMAC), ts.Stats().Limited)
	})

	t.Run("fifty a second in all", func(t *testing.T) {
		leases := newFakeLeases()
		var macs []net.HardwareAddr
		for i := range 60 {
			addr := netip.AddrFrom4([4]byte{10, 203, 118, byte(10 + i)})
			mac := mustMAC(fmt.Sprintf("06:50:43:4f:76:%02x", 10+i))
			leases.addrs[mac.String()] = addr
			macs = append(macs, mac)
		}
		ts := newTestServer(t, leases)
		for _, mac := range macs {
			ts.in(msgFrame(clientMsg{typ: msgDiscover, xid: 1, chaddr: mac}))
		}
		require.Len(t, ts.sock.replies(), totalPerSecond)
		require.Equal(t, uint64(60-totalPerSecond), ts.Stats().Limited)
		ts.now = t0.Add(1100 * time.Millisecond)
		ts.in(msgFrame(clientMsg{typ: msgDiscover, xid: 1, chaddr: macs[59]}))
		require.Len(t, ts.sock.replies(), totalPerSecond+1)
	})

	t.Run("a clock that goes back starts the counts again", func(t *testing.T) {
		ts := newTestServer(t, leased())
		ts.in(goldenFrames["discover.bin"])
		ts.now = t0.Add(-time.Hour)
		ts.in(goldenFrames["discover.bin"])
		require.Len(t, ts.sock.replies(), 2)
	})
}

func TestAReplyTheSocketRefusesIsCounted(t *testing.T) {
	ts := newTestServer(t, leased())
	ts.sock.sendErr = errors.New("network is unreachable")
	ts.in(goldenFrames["discover.bin"])
	require.Equal(t, Stats{SendFailed: 1}, ts.Stats())
}

func TestRunIsBoundWhileItAnswersAndStopsWithTheContext(t *testing.T) {
	ts := newTestServer(t, leased())
	require.False(t, ts.Bound(), "not bound before the sockets are open")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ts.Run(ctx) }()

	ts.sock.in <- goldenFrames["discover.bin"] // taken once Run reads
	require.True(t, ts.Bound())
	ts.sock.in <- goldenFrames["request.bin"]
	cancel()
	require.NoError(t, <-done)
	require.False(t, ts.Bound(), "not bound once it stopped")
	require.Len(t, ts.sock.replies(), 2)
}

func TestRunEndsWhenTheLegGoes(t *testing.T) {
	ts := newTestServer(t, leased())
	done := make(chan error, 1)
	go func() { done <- ts.Run(t.Context()) }()
	ts.sock.readErr <- errors.New("network is down")
	err := <-done
	require.ErrorContains(t, err, "reading on leg3: network is down")
	require.False(t, ts.Bound())
}

func TestRunFailsWhenTheSocketsCannotBeOpened(t *testing.T) {
	s := newServer(testConfig, leased(), func(Config) (frameReader, replySender, error) {
		return nil, nil, errors.New("no such device")
	}, time.Now, zerolog.Nop())
	require.ErrorContains(t, s.Run(t.Context()), "opening the sockets on leg3: no such device")
	require.False(t, s.Bound())
}

func TestAConfigWithoutALegIsRefused(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no leg":                    {Mark: 3, Server: legIP, Subnet: subnet},
		"no mark":                   {Iface: "leg3", Server: legIP, Subnet: subnet},
		"the server off the subnet": {Iface: "leg3", Mark: 3, Server: netip.MustParseAddr("10.0.0.1"), Subnet: subnet},
		"an IPv6 subnet":            {Iface: "leg3", Mark: 3, Server: netip.MustParseAddr("fd00::1"), Subnet: netip.MustParsePrefix("fd00::/64")},
		"a subnet with host bits":   {Iface: "leg3", Mark: 3, Server: legIP, Subnet: netip.MustParsePrefix("10.203.118.1/24")},
		"a lease of a second":       {Iface: "leg3", Mark: 3, Server: legIP, Subnet: subnet, LeaseTime: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			ts := newTestServer(t, leased())
			ts.cfg = cfg
			require.Error(t, ts.Run(t.Context()))
			require.Zero(t, ts.opens)
		})
	}
}
