package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/atomicfile"
)

// PinsFresh is how long cached pins are taken as verified: older ones may
// name a MAC a guest no longer has.
const PinsFresh = 5 * time.Minute

// CacheFile is where the node keeps its cache, below its local state.
const CacheFile = "netns/gateway.json"

// Cache is what the gateway served, as the node keeps it so that pco netns up
// brings it back at boot before the daemon runs, and before the guests start.
type Cache struct {
	Legs     []Leg
	Services []Service
	Sticky   []Sticky
	Pins     []Pin
	// PinsAt is when the pins were verified.
	PinsAt time.Time
}

// Fresh reports whether the pins are fresh at now.
func (c Cache) Fresh(now time.Time) bool {
	return !c.PinsAt.IsZero() && !now.Before(c.PinsAt) && now.Sub(c.PinsAt) < PinsFresh
}

type cacheJSON struct {
	Legs     []legJSON     `json:"legs"`
	Services []serviceJSON `json:"services"`
	Sticky   []stickyJSON  `json:"sticky,omitempty"`
	Pins     []pinJSON     `json:"pins,omitempty"`
	PinsAt   time.Time     `json:"pinsAt,omitzero"`
}

type legJSON struct {
	ID      string       `json:"id"`
	Mark    int          `json:"mark"`
	Bridge  string       `json:"bridge"`
	VLAN    int          `json:"vlan,omitempty"`
	Prefix  netip.Prefix `json:"prefix"`
	MAC     string       `json:"mac"`
	Managed bool         `json:"managed,omitempty"`
	Uplink  bool         `json:"uplink,omitempty"`
}

type serviceJSON struct {
	Leg     string     `json:"leg"`
	Service netip.Addr `json:"service"`
	Addr    netip.Addr `json:"addr"`
	Ports   []uint16   `json:"ports"`
}

type stickyJSON struct {
	MAC    string     `json:"mac"`
	Bridge string     `json:"bridge"`
	VLAN   int        `json:"vlan,omitempty"`
	Port   string     `json:"port,omitempty"`
	Addr   netip.Addr `json:"addr,omitzero"`
}

type pinJSON struct {
	Leg  string     `json:"leg"`
	Addr netip.Addr `json:"addr"`
	MAC  string     `json:"mac"`
}

// MarshalJSON writes the MACs as text.
func (c Cache) MarshalJSON() ([]byte, error) {
	w := cacheJSON{PinsAt: c.PinsAt, Legs: []legJSON{}, Services: []serviceJSON{}}
	for _, l := range c.Legs {
		w.Legs = append(w.Legs, legJSON{
			ID: l.ID, Mark: l.Mark, Bridge: l.Bridge, VLAN: l.VLAN, Prefix: l.Prefix,
			MAC: l.MAC.String(), Managed: l.Managed, Uplink: l.Uplink,
		})
	}
	for _, s := range c.Services {
		w.Services = append(w.Services, serviceJSON(s))
	}
	for _, s := range c.Sticky {
		w.Sticky = append(w.Sticky, stickyJSON{MAC: s.MAC.String(), Bridge: s.Bridge, VLAN: s.VLAN, Port: s.Port, Addr: s.Addr})
	}
	for _, p := range c.Pins {
		w.Pins = append(w.Pins, pinJSON{Leg: p.Leg, Addr: p.Addr, MAC: p.MAC.String()})
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads what MarshalJSON writes, and refuses a field it does
// not know.
func (c *Cache) UnmarshalJSON(data []byte) error {
	var w cacheJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return err
	}
	out := Cache{PinsAt: w.PinsAt}
	for _, l := range w.Legs {
		mac, err := parseMAC("leg "+l.ID, l.MAC)
		if err != nil {
			return err
		}
		out.Legs = append(out.Legs, Leg{
			ID: l.ID, Mark: l.Mark, Bridge: l.Bridge, VLAN: l.VLAN, Prefix: l.Prefix,
			MAC: mac, Managed: l.Managed, Uplink: l.Uplink,
		})
	}
	for _, s := range w.Services {
		out.Services = append(out.Services, Service(s))
	}
	for _, s := range w.Sticky {
		mac, err := parseMAC("sticky entry on "+s.Port, s.MAC)
		if err != nil {
			return err
		}
		out.Sticky = append(out.Sticky, Sticky{MAC: mac, Bridge: s.Bridge, VLAN: s.VLAN, Port: s.Port, Addr: s.Addr})
	}
	for _, p := range w.Pins {
		mac, err := parseMAC("pin of "+p.Addr.String(), p.MAC)
		if err != nil {
			return err
		}
		out.Pins = append(out.Pins, Pin{Leg: p.Leg, Addr: p.Addr, MAC: mac})
	}
	*c = out
	return nil
}

func parseMAC(of, s string) (net.HardwareAddr, error) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		return nil, fmt.Errorf("%s: %q is no MAC", of, s)
	}
	return mac, nil
}

// ReadCache reads the cache below the local root; without one it returns
// false.
func ReadCache(local string) (Cache, bool, error) {
	data, err := os.ReadFile(filepath.Join(local, CacheFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Cache{}, false, nil
	case err != nil:
		return Cache{}, false, err
	}
	var c Cache
	if err := json.Unmarshal(data, &c); err != nil {
		return Cache{}, false, fmt.Errorf("reading %s: %w", filepath.Join(local, CacheFile), err)
	}
	return c, true, nil
}

// WriteCache replaces the cache below the local root.
func WriteCache(local string, c Cache) error {
	path := filepath.Join(local, CacheFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, append(data, '\n'), atomicfile.Options{Mode: 0o600})
}

// restorer is a backend that takes a cache before Up, so that Up loads the
// table with the cached elements, and that can sync without the pins.
type restorer interface {
	seed(legs []Leg, services []Service, sticky []Sticky)
	expect(legs []Leg, services []Service, sticky []Sticky)
	syncKeepingPins(ctx context.Context, legs []Leg, services []Service, sticky []Sticky) ([]LegState, error)
}

// Expect makes a cache what Verify compares the network with, in a process
// that did not apply it itself, as pco netns status: every leg is expected up
// with its address.
func Expect(b Backend, c Cache) {
	if r, ok := b.(restorer); ok {
		r.expect(c.Legs, c.Services, c.Sticky)
	}
}

// Restore brings the gateway back from a cache: Up with the cached legs and
// services, so that a table loaded anew holds their elements in the one
// transaction that loads it, then Sync. Pins go with it while fresh; stale
// ones leave the pins of the namespace as they are, for the daemon to replace.
func Restore(ctx context.Context, b Backend, c Cache, now time.Time) ([]LegState, error) {
	r, ok := b.(restorer)
	if ok {
		r.seed(c.Legs, c.Services, c.Sticky)
	}
	if err := b.Up(ctx); err != nil {
		return nil, err
	}
	if c.Fresh(now) || !ok {
		var pins []Pin
		if c.Fresh(now) {
			pins = c.Pins
		}
		return b.Sync(ctx, c.Legs, c.Services, pins, c.Sticky)
	}
	return r.syncKeepingPins(ctx, c.Legs, c.Services, c.Sticky)
}

func (b *hostBackend) syncKeepingPins(ctx context.Context, legs []Leg, services []Service, sticky []Sticky) ([]LegState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sync(ctx, legs, services, nil, sticky, false)
}
