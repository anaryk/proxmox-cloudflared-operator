package store

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// maxNICIndex is the highest netN a guest can have.
const maxNICIndex = 31

// Lease is one managed address, keyed by the MAC pco gave the NIC.
type Lease struct {
	MAC      string     `json:"mac"`
	Addr     netip.Addr `json:"addr"`
	Owner    string     `json:"owner"`
	Identity string     `json:"identity"`
	NIC      int        `json:"nic"`
	Since    time.Time  `json:"since"`
	LastDHCP *time.Time `json:"lastDHCP,omitempty"`
	Released *time.Time `json:"released,omitempty"` // detach grace running
	// Rev is the revision of the lease's file, for compare-and-swap: the
	// engine and the DHCP server each change a field of the same lease. The
	// envelope holds it, not the lease.
	Rev int64 `json:"rev,omitempty"`
}

// ManagedMAC returns the MAC of the card that holds addr on the managed
// network: 06:50:43:4f and the last two octets of the address, which name it
// alone in a subnet of at most a /16.
func ManagedMAC(addr netip.Addr) string {
	b := addr.As4()
	return fmt.Sprintf("06:50:43:4f:%02x:%02x", b[2], b[3])
}

// leaseID is the id a lease is stored under: its MAC with dashes, as a file
// name has no colon.
func leaseID(mac string) string { return strings.ReplaceAll(mac, ":", "-") }

// checkManagedAddr refuses an address a guest cannot have on the managed
// network: one outside its subnet, the subnet's own address, the first
// address, which is pco's leg, and the broadcast address.
func checkManagedAddr(subnet netip.Prefix, addr netip.Addr) error {
	switch {
	case !addr.Is4() || !subnet.Contains(addr):
		return fmt.Errorf("address %v is not in the managed subnet %s", addr, subnet)
	case addr == subnet.Addr():
		return fmt.Errorf("%s is the address of the managed subnet itself", addr)
	case addr == subnet.Addr().Next():
		return fmt.Errorf("%s is the address of pco's leg on the managed network", addr)
	case addr == lastAddr(subnet):
		return fmt.Errorf("%s is the broadcast address of the managed subnet", addr)
	}
	return nil
}

// Leases returns the leases by MAC, each with the revision of its file.
func (s *Store) Leases() (map[string]Lease, error) {
	out := make(map[string]Lease)
	err := eachRevision(s.cluster, kindIPAM, func(path, id string, rev int64, l Lease) error {
		if leaseID(l.MAC) != id {
			return fmt.Errorf("%s holds the lease of %q, which is stored as %q", path, l.MAC, id)
		}
		l.Rev = rev
		out[l.MAC] = l
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SaveLease stores a lease read at revision rev, 0 for a new one, under its
// MAC in normal form, while its file still has that revision; otherwise the
// error is ErrRevision and nothing is written, so that a writer that read
// before another wrote cannot undo that write, nor bring back a lease that was
// deleted. An address outside managed.subnet, the subnet's own, the leg's and
// the broadcast address are refused, and so is a MAC that is not the one
// ManagedMAC gives the address.
func (s *Store) SaveLease(l Lease, rev int64) error {
	mac, err := model.NormalizeMAC(l.MAC)
	if err != nil {
		return fmt.Errorf("lease: %w", err)
	}
	l.MAC = mac
	if _, err := model.ParseGuestRef(l.Owner); err != nil {
		return fmt.Errorf("lease %s: %w", mac, err)
	}
	if l.NIC < 0 || l.NIC > maxNICIndex {
		return fmt.Errorf("lease %s: net%d is not net0 to net%d", mac, l.NIC, maxNICIndex)
	}
	settings, err := s.Settings()
	if err != nil {
		return fmt.Errorf("lease %s: %w", mac, err)
	}
	if err := checkManagedAddr(settings.Managed.Subnet, l.Addr); err != nil {
		return fmt.Errorf("lease %s: %w", mac, err)
	}
	if want := ManagedMAC(l.Addr); mac != want {
		return fmt.Errorf("lease %s: the card of %s has the MAC %s", mac, l.Addr, want)
	}
	l.Rev = 0
	_, err = s.cluster.putIf(kindIPAM, leaseID(mac), l, rev)
	return err
}

// DeleteLease removes the lease of a MAC, whatever its revision: a write from
// a copy read before is refused afterwards. A missing one is not an error.
func (s *Store) DeleteLease(mac string) error {
	n, err := model.NormalizeMAC(mac)
	if err != nil {
		return fmt.Errorf("lease: %w", err)
	}
	return s.cluster.Delete(kindIPAM, leaseID(n))
}
