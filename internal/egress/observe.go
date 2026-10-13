package egress

import (
	"strconv"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// Expected is a published guest's MAC as the controller's snapshot binds it:
// the guest and the index of the NIC that has the MAC, which tell the port
// the bridge must learn it on.
type Expected struct {
	MAC   string
	Guest model.GuestRef
	NIC   int
}

// ObservationKind says what a forwarding table shows of an expected MAC.
type ObservationKind string

const (
	// Learned: the MAC is on its own guest's port, which is what an
	// attestation rests on.
	Learned ObservationKind = "learned"
	// Moved: the MAC left its own guest's port for one that is no other
	// guest's, the uplink's for one.
	Moved ObservationKind = "moved"
	// Conflict: the MAC is on a port of another guest.
	Conflict ObservationKind = "conflict"
	// Gone: the MAC is no longer on the port the last Learned or Conflict
	// of it named, and on none the observer reports: the bridge dropped
	// the entry, or learned the MAC of a conflict on a port that is no
	// guest's. What the earlier observation said no longer holds.
	Gone ObservationKind = "gone"
	// Reset: the observer subscribed again and what follows is a dump of the
	// tables as they are now. Nothing reported before it still holds: an
	// entry that went while no subscription was read gave no Gone, and the
	// dump does not say what it does not show. It carries only At.
	Reset ObservationKind = "reset"
)

// Observation is one thing a bridge's forwarding table showed of an expected
// MAC. A Learned and a Conflict hold until the next observation of the same
// MAC in the same VLAN of the same bridge.
type Observation struct {
	Kind   ObservationKind
	MAC    string
	Bridge string
	VLAN   int // 0 on a bridge without VLANs
	// Port is the port the MAC is on, and for Gone the one it was on.
	Port string
	// From is the port of the guest the MAC was on before, for a move and for
	// a conflict that follows one. It is empty when the observer never saw the
	// MAC on its own port.
	From string
	// Owner is the guest whose port the MAC is on, for a conflict. Its kind is
	// empty when only the port's name tells whose it is and the name does not
	// say the kind: the firewall bridge port of a guest that is not expected.
	Owner *model.GuestRef
	At    time.Time
}

// observer reads the entries of the forwarding tables, and reports what they
// show of the expected MACs. It holds the last Learned or Conflict of each
// entry, which tells a MAC that left its own port from one that never was
// there, as on every node but the one that runs the guest, and which port an
// entry was on when the bridge drops it.
type observer struct {
	expected func() map[string]Expected
	report   func(Observation)
	now      func() time.Time
	held     map[fdbKey]sighting
}

// fdbKey is an entry of a forwarding table: a MAC in a VLAN of a bridge.
type fdbKey struct {
	bridge string
	vlan   int
	mac    string
}

// sighting is the Learned or Conflict of an entry that still holds: the port
// it put the MAC on and which of the two it was.
type sighting struct {
	port string
	kind ObservationKind
}

// own is the port the MAC was last on as its guest's own, if it still is.
func (s sighting) own() string {
	if s.kind == Learned {
		return s.port
	}
	return ""
}

func newObserver(expected func() map[string]Expected, report func(Observation), now func() time.Time) *observer {
	return &observer{expected: expected, report: report, now: now, held: map[fdbKey]sighting{}}
}

// see takes in one entry of a forwarding table. An entry of anything else
// and one whose bridge has no name report nothing. One the bridge dropped
// ends what was held of its MAC with Gone: the kernel says the same of an
// entry that aged out and of one that went with its port, so a guest that
// is quiet for the ageing time is gone until it speaks again, and one that
// stopped on this node is no move when its MAC shows on the uplink once it
// runs on another. A MAC that reappears on the uplink after it aged out is
// the pin watch's to catch.
func (o *observer) see(e entry) {
	if e.bridge == "" || len(e.mac) == 0 {
		return
	}
	key := fdbKey{bridge: e.bridge, vlan: e.vlan, mac: e.mac.String()}
	expected := o.expected()
	want, ok := expected[key.mac]
	switch {
	case !ok:
		delete(o.held, key)
	case e.gone:
		o.ends(key)
	default:
		o.sees(key, want, e.port, expected)
	}
}

func (o *observer) sees(key fdbKey, want Expected, port string, expected map[string]Expected) {
	obs := Observation{MAC: key.mac, Bridge: key.bridge, VLAN: key.vlan, Port: port}
	held := o.held[key]
	on, isGuest := parseGuestPort(port)
	switch {
	case isGuest && on.vmid == want.Guest.VMID && on.nic == want.NIC:
		obs.Kind = Learned
		o.held[key] = sighting{port: port, kind: Learned}
	case isGuest && on.vmid != want.Guest.VMID:
		obs.Kind, obs.From = Conflict, held.own()
		owner := on.owner(expected)
		obs.Owner = &owner
		o.held[key] = sighting{port: port, kind: Conflict}
	case held.kind == Learned:
		obs.Kind, obs.From = Moved, held.port
		delete(o.held, key)
	default:
		o.ends(key)
		return
	}
	obs.At = o.now()
	o.report(obs)
}

// reset says that a subscription begins, with a dump, and forgets what was
// held: a MAC the dump shows on the uplink is no move from the port it was
// on before the gap, since what happened meanwhile was not seen.
func (o *observer) reset() {
	clear(o.held)
	o.report(Observation{Kind: Reset, At: o.now()})
}

// ends reports Gone of what is held of key, if anything is, and forgets it.
func (o *observer) ends(key fdbKey) {
	held, ok := o.held[key]
	if !ok {
		return
	}
	delete(o.held, key)
	o.report(Observation{Kind: Gone, MAC: key.mac, Bridge: key.bridge, VLAN: key.vlan, Port: held.port, At: o.now()})
}

// guestPort is what the name of a bridge port says of the guest it belongs to.
type guestPort struct {
	vmid int
	nic  int
	kind model.GuestKind // empty when the name does not say
}

// parseGuestPort reads the name of a bridge port Proxmox creates for the NIC
// of a guest: tap<vmid>i<n> of a virtual machine, veth<vmid>i<n> of a
// container, and fwpr<vmid>p<n> of either behind its firewall. Any other
// name, the uplink's, a leg's, a VLAN interface's, is no guest's.
func parseGuestPort(name string) (guestPort, bool) {
	for _, form := range []struct {
		prefix, between string
		kind            model.GuestKind
	}{
		{"tap", "i", model.KindQEMU},
		{"veth", "i", model.KindLXC},
		{"fwpr", "p", ""},
	} {
		rest, ok := strings.CutPrefix(name, form.prefix)
		if !ok {
			continue
		}
		id, index, ok := strings.Cut(rest, form.between)
		vmid, vmidOK := number(id)
		nic, nicOK := number(index)
		if ok && vmidOK && nicOK {
			return guestPort{vmid: vmid, nic: nic, kind: form.kind}, true
		}
	}
	return guestPort{}, false
}

// number reads a decimal number the way Proxmox writes one into a name, with
// no sign and no leading zero.
func number(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0 && strconv.Itoa(n) == s
}

// owner is the guest the port belongs to. A firewall bridge port does not
// say whether it is a virtual machine's or a container's: the expected guests
// may.
func (p guestPort) owner(expected map[string]Expected) model.GuestRef {
	if p.kind == "" {
		for _, x := range expected {
			if x.Guest.VMID == p.vmid {
				return x.Guest
			}
		}
	}
	return model.GuestRef{Kind: p.kind, VMID: p.vmid}
}
