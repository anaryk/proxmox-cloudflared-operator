package dhcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/vishvananda/netns"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Leases answers the server's questions; the engine implements it over IPAM.
type Leases interface {
	// Lookup returns the address of a MAC that has a lease.
	Lookup(mac net.HardwareAddr) (netip.Addr, bool)
	// Seen records a request from a MAC at t.
	Seen(mac net.HardwareAddr, t time.Time)
	// Declined says that the guest of a MAC found its address in use
	// (DECLINE), and Released that it gave the address up (RELEASE). The
	// lease stays either way: it belongs to the card, not to the client.
	Declined(mac net.HardwareAddr, t time.Time)
	Released(mac net.HardwareAddr, t time.Time)
}

// DefaultLeaseTime is the lease time the managed network gives: a week, so
// that a guest renews at 3.5 days and rebinds at 6.125.
const DefaultLeaseTime = 7 * 24 * time.Hour

// Config is the leg the server answers on.
type Config struct {
	Iface     string        // the managed leg
	Mark      int           // the leg's mark: SO_MARK on the sending socket, so that replies take the leg's table
	Server    netip.Addr    // the leg address, option 54
	Subnet    netip.Prefix  // option 1
	LeaseTime time.Duration // 7 days
}

func (c Config) check() error {
	switch {
	case c.Iface == "":
		return errors.New("the DHCP server has no leg")
	case c.Mark <= 0:
		return fmt.Errorf("the DHCP server on %s has no mark", c.Iface)
	case !c.Subnet.Addr().Is4() || c.Subnet.Bits() > 30 || c.Subnet != c.Subnet.Masked():
		return fmt.Errorf("the DHCP server on %s: %s is no IPv4 subnet of guests", c.Iface, c.Subnet)
	case !c.Subnet.Contains(c.Server):
		return fmt.Errorf("the DHCP server on %s: its address %s is not in %s", c.Iface, c.Server, c.Subnet)
	case c.LeaseTime < time.Minute:
		return fmt.Errorf("the DHCP server on %s: a lease time of %s", c.Iface, c.LeaseTime)
	}
	return nil
}

// Namespace runs f with the network namespace the leg is in, held open while
// f runs; netns.None() is the namespace of the process.
type Namespace func(f func(ns netns.NsHandle) error) error

// Stats counts what a server did since it was made.
type Stats struct {
	Answered   uint64 `json:"answered"`   // OFFER, ACK and NAK sent
	Unknown    uint64 `json:"unknown"`    // requests from a MAC without a lease
	Spoofed    uint64 `json:"spoofed"`    // requests whose Ethernet source is not the client's MAC
	Limited    uint64 `json:"limited"`    // requests over the rate limit
	Malformed  uint64 `json:"malformed"`  // frames that are no DHCP request the server takes
	SendFailed uint64 `json:"sendFailed"` // replies the socket refused
}

// frameReader reads whole Ethernet frames off the leg; Close ends a read
// that waits.
type frameReader interface {
	ReadFrame(b []byte) (int, error)
	Close() error
}

// replySender sends a reply to port 68 of an address, the limited broadcast
// among them, from port 67 on the leg.
type replySender interface {
	Send(payload []byte, to netip.Addr) error
	Close() error
}

// opener opens the two sockets of a server.
type opener func(cfg Config) (frameReader, replySender, error)

// limitedBroadcast is where a reply goes to a client without an address.
var limitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// frameBuf holds the largest frame the leg's MTU allows with room to spare;
// a longer one is cut and fails the length checks.
const frameBuf = 9216

// The rate limits: per MAC and second, one DISCOVER and a few of anything
// else (a REQUEST that was NAKed is followed by a DISCOVER and a REQUEST),
// and 50 messages a second in all.
const (
	discoversPerMAC = 1
	othersPerMAC    = 4
	totalPerSecond  = 50
)

// sendErrorEvery is how often a reply the socket refused is logged.
const sendErrorEvery = time.Minute

// Server is the DHCP server of the managed leg.
type Server struct {
	cfg    Config
	leases Leases
	open   opener
	now    func() time.Time
	log    zerolog.Logger

	bound                                                atomic.Bool
	answered, unknown, spoofed, limited, bad, sendFailed atomic.Uint64
	// run is held by Run; lim and lastSendErr belong to the Run that holds it.
	run         sync.Mutex
	lim         limiter
	lastSendErr time.Time
}

// New returns a server that receives on a packet socket bound to the leg
// and sends from a UDP socket bound to 0.0.0.0:67 on the leg, both opened
// from a thread inside the namespace ns holds, with SO_BINDTODEVICE and
// SO_MARK.
func New(ns Namespace, cfg Config, leases Leases, log zerolog.Logger) *Server {
	return newServer(cfg, leases, socketOpener(ns), time.Now, log)
}

func newServer(cfg Config, leases Leases, open opener, now func() time.Time, log zerolog.Logger) *Server {
	if cfg.LeaseTime == 0 {
		cfg.LeaseTime = DefaultLeaseTime
	}
	return &Server{cfg: cfg, leases: leases, open: open, now: now, log: log}
}

// Bound reports whether both sockets are open (the daemon's readiness).
func (s *Server) Bound() bool { return s.bound.Load() }

// Stats returns the counts so far.
func (s *Server) Stats() Stats {
	return Stats{
		Answered: s.answered.Load(), Unknown: s.unknown.Load(), Spoofed: s.spoofed.Load(),
		Limited: s.limited.Load(), Malformed: s.bad.Load(), SendFailed: s.sendFailed.Load(),
	}
}

// Run opens the sockets and answers until ctx ends, which returns nil, or a
// socket fails, which returns why: a leg that goes away or down ends it, and
// the caller runs it again once the leg is back. One Run at a time.
func (s *Server) Run(ctx context.Context) error {
	if err := s.cfg.check(); err != nil {
		return err
	}
	s.run.Lock()
	defer s.run.Unlock()
	recv, send, err := s.open(s.cfg)
	if err != nil {
		return fmt.Errorf("opening the sockets on %s: %w", s.cfg.Iface, err)
	}
	closeBoth := func() { _ = recv.Close(); _ = send.Close() }
	stop := context.AfterFunc(ctx, closeBoth)
	defer func() {
		if stop() {
			closeBoth()
		}
	}()
	s.bound.Store(true)
	defer s.bound.Store(false)
	s.log.Info().Str("leg", s.cfg.Iface).Stringer("address", s.cfg.Server).Msg("the DHCP server of the managed network listens")
	buf := make([]byte, frameBuf)
	for {
		n, err := recv.ReadFrame(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading on %s: %w", s.cfg.Iface, err)
		}
		s.handle(buf[:n], send)
	}
}

// handle answers one frame. The checks run in the order of their cost to
// a guest that floods: what is not a request, what is forged and what has
// no lease cost a counter each and nothing else.
func (s *Server) handle(frame []byte, send replySender) {
	now := s.now()
	req, err := parseFrame(frame)
	if err != nil {
		s.bad.Add(1)
		return
	}
	if !bytes.Equal(req.src, req.chaddr) {
		s.spoofed.Add(1)
		return
	}
	addr, ok := s.lease(req.chaddr)
	if !ok {
		s.unknown.Add(1)
		return
	}
	if !s.lim.allow(req.chaddr, req.typ, now) {
		s.limited.Add(1)
		return
	}
	s.leases.Seen(req.chaddr, now)
	switch req.typ {
	case msgDiscover:
		s.send(send, s.replyTo(req, msgOffer, addr), dest(req, addr))
	case msgRequest:
		s.request(send, req, addr)
	case msgDecline:
		s.leases.Declined(req.chaddr, now)
	case msgRelease:
		s.leases.Released(req.chaddr, now)
	}
}

// lease is the address of a MAC's lease, when it is one the server may give:
// the MAC is the one pco makes for the address, and the address a guest's in
// the subnet, not the leg's, the subnet's own or its broadcast.
func (s *Server) lease(mac net.HardwareAddr) (netip.Addr, bool) {
	addr, ok := s.leases.Lookup(mac)
	switch {
	case !ok, !addr.Is4(), !s.cfg.Subnet.Contains(addr):
		return netip.Addr{}, false
	case addr == s.cfg.Server, addr == s.cfg.Subnet.Addr(), addr == lastOf(s.cfg.Subnet):
		return netip.Addr{}, false
	case mac.String() != store.ManagedMAC(addr):
		return netip.Addr{}, false
	}
	return addr, true
}

// request answers a REQUEST: an ACK when it asks for the lease, nothing when
// the client chose another server, a NAK otherwise. A client that renews or
// rebinds names its address in ciaddr and asks without option 50.
func (s *Server) request(send replySender, req request, addr netip.Addr) {
	if req.serverID.IsValid() && req.serverID != s.cfg.Server {
		return
	}
	want := req.requested
	if !want.IsValid() {
		want = req.ciaddr
	}
	if want != addr {
		s.send(send, s.replyTo(req, msgNak, netip.Addr{}), limitedBroadcast)
		return
	}
	s.send(send, s.replyTo(req, msgAck, addr), dest(req, addr))
}

// dest is where a reply goes: to the client's own address when it has it,
// which is how a renewal is answered, and to everyone on the leg otherwise.
// A ciaddr other than the lease is never sent to.
func dest(req request, addr netip.Addr) netip.Addr {
	if req.ciaddr == addr {
		return addr
	}
	return limitedBroadcast
}

func (s *Server) replyTo(req request, typ msgType, addr netip.Addr) reply {
	r := reply{
		typ: typ, xid: req.xid, flags: req.flags, yiaddr: addr, chaddr: req.chaddr,
		server: s.cfg.Server, subnet: s.cfg.Subnet, lease: s.cfg.LeaseTime,
	}
	if typ == msgAck && req.ciaddr == addr {
		r.ciaddr = addr
	}
	return r
}

func (s *Server) send(send replySender, r reply, to netip.Addr) {
	if err := send.Send(r.marshal(), to); err != nil {
		s.sendFailed.Add(1)
		if now := s.now(); now.Sub(s.lastSendErr) >= sendErrorEvery || now.Before(s.lastSendErr) {
			s.lastSendErr = now
			s.log.Warn().Err(err).Str("leg", s.cfg.Iface).Stringer("to", to).Str("reply", r.typ.String()).
				Msg("the DHCP server could not send a reply")
		}
		return
	}
	s.answered.Add(1)
}

// lastOf is the broadcast address of an IPv4 subnet.
func lastOf(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// limiter counts messages in windows of one second.
type limiter struct {
	start time.Time
	total int
	byMAC map[limitKey]int
}

type limitKey struct {
	mac      [6]byte
	discover bool
}

// allow counts a message of a MAC at now and says whether it is within the
// limits. The counts start again with every second, so they hold no more
// keys than MACs with a lease sent something within it.
func (l *limiter) allow(mac net.HardwareAddr, typ msgType, now time.Time) bool {
	if l.byMAC == nil || now.Before(l.start) || now.Sub(l.start) >= time.Second {
		l.start, l.total, l.byMAC = now, 0, map[limitKey]int{}
	}
	key := limitKey{mac: [6]byte(mac), discover: typ == msgDiscover}
	limit := othersPerMAC
	if key.discover {
		limit = discoversPerMAC
	}
	if l.total >= totalPerSecond || l.byMAC[key] >= limit {
		return false
	}
	l.total++
	l.byMAC[key]++
	return true
}
