package store

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
)

// ErrPoolExhausted is the error of an allocation once every address of the
// pool was given out. None is ever given out twice.
var ErrPoolExhausted = errors.New("every service address was given out")

// transitBlock is the part of the service prefix below the pool. The transit
// link of the gateway is its first /30.
const transitBlock = 256

// ServiceAddr maps a real address on a network to the one address the
// connectors dial for it. A released mapping is deleted; the pool's next
// pointer alone keeps the address from being given to another target.
type ServiceAddr struct {
	Network     string     `json:"network"`
	Addr        netip.Addr `json:"addr"`
	Service     netip.Addr `json:"service"`
	AllocatedAt time.Time  `json:"allocatedAt"`
}

// servicePool is what meta/svcpool.json holds: the address the next
// allocation gets. It only moves forward.
type servicePool struct {
	Next netip.Addr `json:"next"`
}

// serviceAddrID is the id the mapping of a real address on a network is
// stored under.
func serviceAddrID(network string, addr netip.Addr) string { return network + "-" + addr.String() }

// poolBounds returns the first address of the pool, the address right after
// its last, and its size: the service prefix above its transit block.
func poolBounds() (first, end netip.Addr, size int) {
	p := gateway.ServicePrefix
	base := addrUint(p.Masked().Addr())
	span := uint32(1) << (32 - p.Bits())
	return uintAddr(base + transitBlock), uintAddr(base + span), int(span - transitBlock)
}

func addrUint(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func uintAddr(v uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}

// lastAddr is the highest address of an IPv4 prefix: its broadcast address.
func lastAddr(p netip.Prefix) netip.Addr {
	return uintAddr(addrUint(p.Masked().Addr()) | (uint32(1)<<(32-p.Bits()) - 1))
}

// checkServiceKey refuses what names no mapping.
func checkServiceKey(network string, addr netip.Addr) error {
	switch {
	case network == "":
		return errors.New("service address: the network is empty")
	case !addr.Is4():
		return fmt.Errorf("service address on %s: %v is not an IPv4 address", network, addr)
	}
	if _, err := FileName(serviceAddrID(network, addr)); err != nil {
		return fmt.Errorf("service address on %s: %w", network, err)
	}
	return nil
}

// checkMapping refuses a stored mapping whose service address is not in the
// pool: the connectors would be sent to it.
func checkMapping(where string, m ServiceAddr) error {
	first, end, _ := poolBounds()
	if !m.Service.Is4() || m.Service.Less(first) || !m.Service.Less(end) {
		return fmt.Errorf("%s: %v is not a service address", where, m.Service)
	}
	return nil
}

// ServiceAddrs returns the mappings, by network and then by address.
func (s *Store) ServiceAddrs() ([]ServiceAddr, error) {
	out := []ServiceAddr{}
	err := eachObject(s.cluster, kindSvcAddrs, func(path, id string, m ServiceAddr) error {
		if want := serviceAddrID(m.Network, m.Addr); want != id {
			return fmt.Errorf("%s holds the service address of %q, which is stored as %q", path, want, id)
		}
		if err := checkMapping(path, m); err != nil {
			return err
		}
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b ServiceAddr) int {
		return cmp.Or(cmp.Compare(a.Network, b.Network), a.Addr.Compare(b.Addr))
	})
	return out, nil
}

// AllocateServiceAddr returns the mapping of (network, addr), making it from
// the pool when there is none; the pool only moves forward, inside the
// service prefix, and ErrPoolExhausted says it reached the end.
func (s *Store) AllocateServiceAddr(network string, addr netip.Addr, now time.Time) (ServiceAddr, error) {
	if err := checkServiceKey(network, addr); err != nil {
		return ServiceAddr{}, err
	}
	id := serviceAddrID(network, addr)
	s.netMu.Lock()
	defer s.netMu.Unlock()

	var m ServiceAddr
	found, err := s.cluster.Get(kindSvcAddrs, id, &m)
	switch {
	case err != nil:
		return ServiceAddr{}, err
	case found && (m.Network != network || m.Addr != addr):
		return ServiceAddr{}, fmt.Errorf("service address %s holds the mapping of %v on %s", id, m.Addr, m.Network)
	case found:
		if err := checkMapping("service address "+id, m); err != nil {
			return ServiceAddr{}, err
		}
		return m, nil
	}
	service, err := s.takeFromPool()
	if err != nil {
		return ServiceAddr{}, err
	}
	m = ServiceAddr{Network: network, Addr: addr, Service: service, AllocatedAt: now}
	if err := s.cluster.put(kindSvcAddrs, id, m, true); err != nil {
		return ServiceAddr{}, err
	}
	return m, nil
}

// takeFromPool gives out the next address of the pool. The pool moves before
// the mapping is written: a write that fails in between loses an address, and
// never gives one out twice. The caller holds netMu.
func (s *Store) takeFromPool() (netip.Addr, error) {
	next, err := s.poolNext()
	if err != nil {
		return netip.Addr{}, err
	}
	first, end, _ := poolBounds()
	if next == end {
		return netip.Addr{}, fmt.Errorf("%w: every address from %s to %s was given out once", ErrPoolExhausted, first, end.Prev())
	}
	if err := s.cluster.put(kindMeta, idSvcPool, servicePool{Next: next.Next()}, true); err != nil {
		return netip.Addr{}, err
	}
	return next, nil
}

// ReleaseServiceAddr deletes the mapping; the pool does not move back. A
// mapping that is not there is no error.
func (s *Store) ReleaseServiceAddr(network string, addr netip.Addr) error {
	if err := checkServiceKey(network, addr); err != nil {
		return err
	}
	s.netMu.Lock()
	defer s.netMu.Unlock()
	return s.cluster.Delete(kindSvcAddrs, serviceAddrID(network, addr))
}

// ServicePool reports the address the next allocation gets, how many were
// given out, released ones included, and the size of the pool, for doctor's
// warning at 80 %.
func (s *Store) ServicePool() (next netip.Addr, used, size int, err error) {
	next, err = s.poolNext()
	if err != nil {
		return netip.Addr{}, 0, 0, err
	}
	first, _, size := poolBounds()
	return next, int(addrUint(next) - addrUint(first)), size, nil
}

// poolNext is the address the next allocation gets. Without a pool file it
// is the first of the pool or, when mappings exist, as after the file was
// lost, the one after the highest of them, of both kinds. A file whose next
// is outside the pool is an error: an allocation from it might give an
// address twice.
func (s *Store) poolNext() (netip.Addr, error) {
	first, end, _ := poolBounds()
	var p servicePool
	found, err := s.cluster.Get(kindMeta, idSvcPool, &p)
	switch {
	case err != nil:
		return netip.Addr{}, err
	case found && (!p.Next.Is4() || p.Next.Less(first) || end.Less(p.Next)):
		return netip.Addr{}, fmt.Errorf("the pool of service addresses says next is %v, outside %s to %s", p.Next, first, end)
	case found:
		return p.Next, nil
	}
	next := first
	after := func(path string, service netip.Addr) error {
		if err := checkMapping(path, ServiceAddr{Service: service}); err != nil {
			return err
		}
		if !service.Less(next) {
			next = service.Next()
		}
		return nil
	}
	err = eachObject(s.cluster, kindSvcAddrs, func(path, _ string, m ServiceAddr) error { return after(path, m.Service) })
	if err != nil {
		return netip.Addr{}, err
	}
	err = eachObject(s.cluster, kindNodeSvcAddrs, func(path, _ string, m NodeServiceAddr) error { return after(path, m.Service) })
	return next, err
}
