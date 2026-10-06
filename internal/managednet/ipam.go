package managednet

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ErrExhausted is the error of an allocation in a managed subnet that has no
// free address left.
var ErrExhausted = errors.New("no free address on the managed network")

// ErrCardPresent is the error of a release while the card of the lease may
// still be in the guest's configuration.
var ErrCardPresent = errors.New("its card is still there")

// allocateTries is how often Allocate takes the next free address when
// another writer saved a lease under the one it chose first.
const allocateTries = 3

// LeaseStore is where the leases are kept; *store.Store is one.
type LeaseStore interface {
	Leases() (map[string]store.Lease, error)
	SaveLease(l store.Lease, rev int64) error
	DeleteLease(mac string) error
}

// IPAM gives the guests of the managed network their addresses. A lease is
// keyed by the MAC of its card and belongs to that card, not to the guest:
// a guest that migrates keeps it, and nothing about the guest's node is in
// it.
type IPAM struct {
	leases LeaseStore
	subnet netip.Prefix
	now    func() time.Time
}

// NewIPAM returns the address management of the managed subnet.
func NewIPAM(leases LeaseStore, subnet netip.Prefix, now func() time.Time) *IPAM {
	return &IPAM{leases: leases, subnet: subnet.Masked(), now: now}
}

// Allocate leases the lowest free address to the card net<nic> of owner, in
// identity, with the MAC the address gives. A lease this card holds already
// is returned as it is, so that a cycle that failed after the allocation
// gets the same address again. When another writer saved a lease under the
// chosen MAC meanwhile, the next free address is taken. A subnet without a
// free address is ErrExhausted.
func (p *IPAM) Allocate(owner, identity string, nic int) (store.Lease, error) {
	for try := 1; ; try++ {
		leases, err := p.leases.Leases()
		if err != nil {
			return store.Lease{}, fmt.Errorf("reading the leases: %w", err)
		}
		for _, l := range leases {
			if l.Owner == owner && l.Identity == identity && l.NIC == nic && p.subnet.Contains(l.Addr) {
				return l, nil
			}
		}
		addr, ok := LowestFree(p.subnet, leases)
		if !ok {
			return store.Lease{}, fmt.Errorf("%w: %s holds %d leases, from %s to %s",
				ErrExhausted, p.subnet, Available(p.subnet, nil), first(p.subnet), last(p.subnet))
		}
		l := store.Lease{MAC: store.ManagedMAC(addr), Addr: addr, Owner: owner, Identity: identity, NIC: nic, Since: p.now()}
		err = p.leases.SaveLease(l, 0)
		switch {
		case err == nil:
			// Read back for the revision a later change of the lease needs.
			saved, err := p.leases.Leases()
			if err != nil {
				return store.Lease{}, fmt.Errorf("reading the lease of %s back: %w", addr, err)
			}
			back, ok := saved[l.MAC]
			if !ok {
				return store.Lease{}, fmt.Errorf("the lease of %s for %s was gone as soon as it was saved", addr, owner)
			}
			return back, nil
		case errors.Is(err, store.ErrRevision) && try < allocateTries:
			continue
		}
		return store.Lease{}, fmt.Errorf("saving the lease of %s for %s: %w", addr, owner, err)
	}
}

// Release deletes the lease of an attachment's card once the card is gone:
// its removal was asked for (RemovedAt), and the configuration of the guest g
// lists neither a card with its MAC nor a card on the managed network at its
// index. g is nil for a guest that is gone. Before that the address stays
// leased, as the guest may still use it: a QEMU guest without network hotplug
// keeps the card until it restarts. A lease that is gone already is released;
// one that another guest holds under that MAC is kept, and so is an error.
func (p *IPAM) Release(a store.Attachment, g *model.Guest) error {
	if g != nil && g.Ref.String() != a.Owner {
		return fmt.Errorf("releasing the lease of %s against the configuration of %s", a.Owner, g.Ref)
	}
	if a.RemovedAt == nil {
		return fmt.Errorf("releasing the lease of %s: %w: its removal was not asked for yet", a.Owner, ErrCardPresent)
	}
	if g != nil {
		for _, nic := range g.NICs {
			if strings.EqualFold(nic.MAC, a.MAC) || nic.Index == a.NIC && nic.Bridge == gateway.ManagedBridge {
				return fmt.Errorf("releasing the lease of %s: %w: its configuration lists net%d", a.Owner, ErrCardPresent, nic.Index)
			}
		}
	}
	leases, err := p.leases.Leases()
	if err != nil {
		return fmt.Errorf("reading the leases: %w", err)
	}
	l, ok := leases[strings.ToLower(a.MAC)]
	switch {
	case !ok:
		return nil
	case l.Owner != a.Owner:
		return fmt.Errorf("the lease of %s belongs to %s, not to %s; it is kept", l.MAC, l.Owner, a.Owner)
	}
	if err := p.leases.DeleteLease(l.MAC); err != nil {
		return fmt.Errorf("releasing the lease of %s: %w", l.Addr, err)
	}
	return nil
}

// Free returns how many addresses of the subnet are not leased.
func (p *IPAM) Free() (int, error) {
	leases, err := p.leases.Leases()
	if err != nil {
		return 0, fmt.Errorf("reading the leases: %w", err)
	}
	return Available(p.subnet, leases), nil
}

// LowestFree returns the lowest address of the subnet that a guest may have
// and no lease holds: from the second host address, after pco's leg, to the
// last before the broadcast address.
func LowestFree(subnet netip.Prefix, leases map[string]store.Lease) (netip.Addr, bool) {
	subnet = subnet.Masked()
	taken := held(subnet, leases)
	end := last(subnet)
	for a := first(subnet); a.Compare(end) <= 0; a = a.Next() {
		if !taken[a] {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// Available returns how many addresses of the subnet a guest may have and no
// lease holds.
func Available(subnet netip.Prefix, leases map[string]store.Lease) int {
	subnet = subnet.Masked()
	size := 1<<(32-subnet.Bits()) - 3
	if size < 0 {
		return 0
	}
	return size - len(held(subnet, leases))
}

// held returns the addresses of the subnet that leases hold and a guest may
// have.
func held(subnet netip.Prefix, leases map[string]store.Lease) map[netip.Addr]bool {
	taken := map[netip.Addr]bool{}
	lo, hi := first(subnet), last(subnet)
	for _, l := range leases {
		if subnet.Contains(l.Addr) && l.Addr.Compare(lo) >= 0 && l.Addr.Compare(hi) <= 0 {
			taken[l.Addr] = true
		}
	}
	return taken
}

// first is the first address of the subnet a guest may have: .1 is pco's leg.
func first(subnet netip.Prefix) netip.Addr { return subnet.Addr().Next().Next() }

// last is the last address of the subnet a guest may have, the one before
// the broadcast address.
func last(subnet netip.Prefix) netip.Addr {
	b := subnet.Addr().As4()
	host := uint32(1)<<(32-subnet.Bits()) - 1
	n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) | host
	n--
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}
