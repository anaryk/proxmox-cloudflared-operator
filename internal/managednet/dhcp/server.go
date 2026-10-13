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

// legSocket is the packet socket on the leg: whole Ethernet frames in, and
// the frames of replies to a client without an address out. Close ends a
// read that waits.
type legSocket interface {
	ReadFrame(b []byte) (int, error)
	WriteFrame(b []byte) error
	// MAC is the leg's, the source of the frames written.
	MAC() net.HardwareAddr
	Close() error
}

// replySender sends a reply to port 68 of an address, the limited broadcast
// among them, from port 67 on the leg, through the kernel's routes.
type replySender interface {
	Send(payload []byte, to netip.Addr) error
	Close() error
}

// opener opens the two sockets of a server.
type opener func(cfg Config) (legSocket, replySender, error)

// limitedBroadcast is where a reply goes to a client that asks for that.
var limitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// target is where a reply goes: through the routes to addr, or, with mac
// set, as a frame to that card with addr as its destination.
type target struct {
	addr netip.Addr
	mac  net.HardwareAddr
}

// frameBuf holds the largest frame the leg's MTU allows with room to spare;
// a longer one is cut and fails the length checks.
const frameBuf = 9216

// The rate limits, per client and second: one DISCOVER and a few of anything
// else (a REQUEST that was NAKed is followed by a DISCOVER and a REQUEST).
// They hold before the lease is looked up, so that a flood from a MAC with
// no lease costs one lookup a second, and the limiter counts at most
// limitKeys clients a second: past that, a flood of made-up MACs costs a
// counter each and nothing else, and holds off new clients for that second,
// which is what the guard at the leg's ingress is for. There is no limit in
// all: every guest with a lease is answered, however many boot at once.
const (
	discoversPerMAC = 1
	othersPerMAC    = 4
	limitKeys       = 4096
)

// logEvery is how often a line of the same kind is logged: a reply the
// socket refused, a request with a forged client MAC.
const logEvery = time.Minute

// Server is the DHCP server of the managed leg.
type Server struct {
	cfg    Config
	leases Leases
	open   opener
	now    func() time.Time
	log    zerolog.Logger

	bound                                                atomic.Bool
	answered, unknown, spoofed, limited, bad, sendFailed atomic.Uint64
	// run is held by Run; lim and the times a line was last logged belong
	// to the Run that holds it.
	run         sync.Mutex
	lim         limiter
	lastSendErr time.Time
	lastForged  time.Time
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
	leg, send, err := s.open(s.cfg)
	if err != nil {
		return fmt.Errorf("opening the sockets on %s: %w", s.cfg.Iface, err)
	}
	// The end of ctx closes both sockets, which ends the read; Run returns
	// once they are closed, port 67 among them, so that it can be run again.
	closeBoth := func() { _ = leg.Close(); _ = send.Close() }
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { closeBoth(); close(closed) })
	defer func() {
		if stop() {
			closeBoth()
			return
		}
		<-closed
	}()
	s.bound.Store(true)
	defer s.bound.Store(false)
	s.log.Info().Str("leg", s.cfg.Iface).Stringer("address", s.cfg.Server).Msg("the DHCP server of the managed network listens")
	buf := make([]byte, frameBuf)
	for {
		n, err := leg.ReadFrame(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading on %s: %w", s.cfg.Iface, err)
		}
		s.handle(buf[:n], leg, send)
	}
}

// handle answers one frame. The checks run in the order of their cost to
// a guest that floods: what is not a request, what is forged and what is
// over its rate cost a counter each and nothing else, and only what is left
// is looked up. The forgery caught here is the sloppy kind, another card's
// MAC as the client behind the guest's own source: a guest that forges the
// source too passes, and is for the guard at the leg's ingress, which lets
// through only the MAC and address pairs of the leases, and for the MAC
// filter of the guest firewall in the appliance.
func (s *Server) handle(frame []byte, leg legSocket, send replySender) {
	now := s.now()
	req, err := parseFrame(frame)
	if err != nil {
		s.bad.Add(1)
		return
	}
	if !bytes.Equal(req.src, req.chaddr) {
		s.spoofed.Add(1)
		if logAgain(&s.lastForged, now) {
			s.log.Warn().Str("leg", s.cfg.Iface).Stringer("source", req.src).Stringer("client", req.chaddr).Str("type", req.typ.String()).
				Msg("a DHCP request with a forged client MAC was dropped: the frame came from another card")
		}
		return
	}
	if !s.lim.allow(req.chaddr, req.typ, now) {
		s.limited.Add(1)
		return
	}
	addr, ok := s.lease(req.chaddr)
	if !ok {
		s.unknown.Add(1)
		return
	}
	s.leases.Seen(req.chaddr, now)
	switch req.typ {
	case msgDiscover:
		s.send(leg, send, s.replyTo(req, msgOffer, addr), where(req, addr))
	case msgRequest:
		s.request(leg, send, req, addr)
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
func (s *Server) request(leg legSocket, send replySender, req request, addr netip.Addr) {
	if req.serverID.IsValid() && req.serverID != s.cfg.Server {
		return
	}
	want := req.requested
	if !want.IsValid() {
		want = req.ciaddr
	}
	if want != addr {
		s.send(leg, send, s.replyTo(req, msgNak, netip.Addr{}), target{addr: limitedBroadcast})
		return
	}
	s.send(leg, send, s.replyTo(req, msgAck, addr), where(req, addr))
}

// where is where an OFFER or an ACK goes, as RFC 2131 section 4.1 has it:
// to the client's own address when it has it, which is how a renewal is
// answered, through the routes; to everyone on the leg when the client asks
// for that with the broadcast bit; and otherwise as a frame to the client's
// card with the lease as its destination, which the client reads off the
// wire before it has the address. A NAK goes to everyone, and a ciaddr
// other than the lease is never sent to.
func where(req request, addr netip.Addr) target {
	switch {
	case req.ciaddr == addr:
		return target{addr: addr}
	case req.flags&flagBroadcast != 0:
		return target{addr: limitedBroadcast}
	}
	return target{addr: addr, mac: req.chaddr}
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

func (s *Server) send(leg legSocket, send replySender, r reply, to target) {
	var err error
	if to.mac != nil {
		err = leg.WriteFrame(r.frame(leg.MAC(), to.mac, s.cfg.Server, to.addr))
	} else {
		err = send.Send(r.marshal(), to.addr)
	}
	if err != nil {
		s.sendFailed.Add(1)
		if logAgain(&s.lastSendErr, s.now()) {
			s.log.Warn().Err(err).Str("leg", s.cfg.Iface).Stringer("to", to.addr).Str("reply", r.typ.String()).
				Msg("the DHCP server could not send a reply")
		}
		return
	}
	s.answered.Add(1)
}

// logAgain says whether a line last logged at *last is due again at now,
// logEvery later or after the clock went back, and takes now as its time
// when it is.
func logAgain(last *time.Time, now time.Time) bool {
	if now.Sub(*last) < logEvery && !now.Before(*last) {
		return false
	}
	*last = now
	return true
}

// lastOf is the broadcast address of an IPv4 subnet.
func lastOf(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// limiter counts the messages of each client in windows of one second.
type limiter struct {
	start time.Time
	byMAC map[[6]byte]*clientCount
}

type clientCount struct{ discovers, others int }

// allow counts a message of a MAC at now and says whether it is within the
// limits. The counts start again with every second, and hold at most
// limitKeys clients within one: a client past that is not counted and not
// allowed.
func (l *limiter) allow(mac net.HardwareAddr, typ msgType, now time.Time) bool {
	if l.byMAC == nil || now.Before(l.start) || now.Sub(l.start) >= time.Second {
		l.start, l.byMAC = now, map[[6]byte]*clientCount{}
	}
	c, ok := l.byMAC[[6]byte(mac)]
	if !ok {
		if len(l.byMAC) >= limitKeys {
			return false
		}
		c = &clientCount{}
		l.byMAC[[6]byte(mac)] = c
	}
	n, limit := &c.others, othersPerMAC
	if typ == msgDiscover {
		n, limit = &c.discovers, discoversPerMAC
	}
	if *n >= limit {
		return false
	}
	*n++
	return true
}
