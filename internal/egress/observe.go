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
)

// Observation is one thing a bridge's forwarding table showed of an expected
// MAC.
type Observation struct {
	Kind   ObservationKind
	MAC    string
	Bridge string
	VLAN   int // 0 on a bridge without VLANs
	Port   string
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
// show of the expected MACs. It remembers where each MAC was last on its own
// guest's port, which is all that tells a MAC that left it from one that never
// was there, as on every node but the one that runs the guest.
type observer struct {
	expected func() map[string]Expected
	report   func(Observation)
	now      func() time.Time
	last     map[fdbKey]string
}

// fdbKey is an entry of a forwarding table: a MAC in a VLAN of a bridge.
type fdbKey struct {
	bridge string
	vlan   int
	mac    string
}

func newObserver(expected func() map[string]Expected, report func(Observation), now func() time.Time) *observer {
	return &observer{expected: expected, report: report, now: now, last: map[fdbKey]string{}}
}

// see takes in one entry of a forwarding table. An entry of anything else,
// one whose bridge has no name, and one the bridge dropped report nothing; a
// dropped one forgets where its MAC was, since a guest that stopped on this
// node takes its port with it and is no move when it starts on another.
func (o *observer) see(e entry) {
	if e.bridge == "" || len(e.mac) == 0 {
		return
	}
	key := fdbKey{bridge: e.bridge, vlan: e.vlan, mac: e.mac.String()}
	expected := o.expected()
	want, ok := expected[key.mac]
	if !ok || e.gone {
		delete(o.last, key)
		return
	}
	o.sees(key, want, e.port, expected)
}

func (o *observer) sees(key fdbKey, want Expected, port string, expected map[string]Expected) {
	obs := Observation{MAC: key.mac, Bridge: key.bridge, VLAN: key.vlan, Port: port}
	on, isGuest := parseGuestPort(port)
	switch {
	case isGuest && on.vmid == want.Guest.VMID && on.nic == want.NIC:
		o.last[key] = port
		obs.Kind = Learned
	case isGuest && on.vmid != want.Guest.VMID:
		obs.Kind, obs.From = Conflict, o.last[key]
		owner := on.owner(expected)
		obs.Owner = &owner
		delete(o.last, key)
	default:
		from, was := o.last[key]
		if !was {
			return
		}
		obs.Kind, obs.From = Moved, from
		delete(o.last, key)
	}
	obs.At = o.now()
	o.report(obs)
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
