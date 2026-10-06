package store

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// PinsMaxAge is how long saved pins stand. A neighbour verified longer ago
// may have moved, and a pin to its old MAC would cut it off.
const PinsMaxAge = 5 * time.Minute

// NetCache is the node-local copy pco netns up restores from.
type NetCache struct {
	Networks     []Network     `json:"networks"`
	ServiceAddrs []ServiceAddr `json:"serviceAddrs"`
	Leases       []Lease       `json:"leases"`
	WrittenAt    time.Time     `json:"writtenAt"`
}

// Pin is a verified neighbour on a leg: the address and the MAC that
// answered for it there.
type Pin struct {
	Network string     `json:"network"`
	Addr    netip.Addr `json:"addr"`
	MAC     string     `json:"mac"`
}

// pinsFile is what netns/pins.json holds.
type pinsFile struct {
	Pins []Pin     `json:"pins"`
	At   time.Time `json:"at"`
}

// NetCache returns the cached state of the gateway, and false when none was
// saved.
func (s *Store) NetCache() (NetCache, bool, error) {
	return getOne[NetCache](s.local, kindNetns, idNetState)
}

// SaveNetCache stores the cached state of the gateway on the node-local root.
func (s *Store) SaveNetCache(c NetCache) error {
	return s.local.put(kindNetns, idNetState, c, true)
}

// Pins returns the saved pins and the time they were verified at; none and
// the zero time when none were saved. FreshPins says whether they still stand.
func (s *Store) Pins() ([]Pin, time.Time, error) {
	f, _, err := getOne[pinsFile](s.local, kindNetns, idPins)
	if err != nil {
		return nil, time.Time{}, err
	}
	if f.Pins == nil {
		f.Pins = []Pin{}
	}
	return f.Pins, f.At, nil
}

// SavePins stores the pins verified at at, with their MACs in normal form.
func (s *Store) SavePins(pins []Pin, at time.Time) error {
	out := make([]Pin, len(pins))
	for i, p := range pins {
		mac, err := model.NormalizeMAC(p.MAC)
		switch {
		case err != nil:
			return fmt.Errorf("pin of %v on %s: %w", p.Addr, p.Network, err)
		case p.Network == "":
			return fmt.Errorf("pin of %v has no network", p.Addr)
		case !p.Addr.IsValid():
			return fmt.Errorf("pin on %s: the address is not valid", p.Network)
		}
		p.MAC = mac
		out[i] = p
	}
	return s.local.put(kindNetns, idPins, pinsFile{Pins: out, At: at}, true)
}

// FreshPins reports whether pins verified at at still stand at now. Pins from
// a time ahead of now, as after the clock was set back, do not.
func FreshPins(at, now time.Time) bool {
	age := now.Sub(at)
	return !at.IsZero() && age >= 0 && age < PinsMaxAge
}
