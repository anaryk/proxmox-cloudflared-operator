package engine

import (
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// gateNotReleased is why a target at an address whose service address was
// made for another owner gets none: that one could not be released.
const gateNotReleased = "service address %s of this address was made for another owner and could not be released"

// gateTaken is why a target gets none at an address where the guest its
// service address was made for is proven too: one of the two proofs is
// wrong, and neither guest is handed the other's.
const gateTaken = "service address %s of this address was made for %s, which is proven there too"

// serviceOwner is whom a service address made now for rt, whose result res
// has a binding, is made for: its owner, its guest and the identity the
// guest has in this cycle's snapshot.
func (c *cycleRun) serviceOwner(rt model.Route, res resolve.Result) store.ServiceOwner {
	o := store.ServiceOwner{Owner: rt.Owner(), Guest: res.Binding.Guest}
	if o.Guest == "" {
		return o
	}
	if c.guestIDs == nil {
		c.guestIDs = make(map[string]string, len(c.snap.Guests))
		for _, g := range c.snap.Guests {
			c.guestIDs[g.Ref.String()] = g.Identity
		}
	}
	o.Identity = c.guestIDs[o.Guest]
	return o
}

// madeFor reports whether the service address m was made for rt, whose
// result res has a binding: its owner, guest and the guest's identity are
// those it was made for. A mapping stored before owners were kept was made
// for the routes whose stored bindings were published under it, while those
// name one guest; otherwise for none, and it is made anew.
func (c *cycleRun) madeFor(m store.ServiceAddr, rt model.Route, res resolve.Result) bool {
	if res.Binding == nil {
		return false
	}
	if m.Known() {
		return m.ServiceOwner == c.serviceOwner(rt, res)
	}
	guest, owners := "", map[string]bool{}
	for _, b := range c.bindings {
		if b.Service != m.Service {
			continue
		}
		if len(owners) > 0 && b.Guest != guest {
			return false
		}
		guest, owners[b.Owner] = b.Guest, true
	}
	return owners[rt.Owner()] && res.Binding.Guest == guest
}

// sharedWith reports whether rt, whose result res has a binding, may be
// published under m, made for another owner: both reach the same guest, with
// the same identity, and a route m was made for is served at its address in
// this cycle, as a manual route to a guest beside the guest's own. The rules
// of either then reach only that guest. Once the routes m was made for are
// gone, rt gets a service address of its own.
func (c *cycleRun) sharedWith(m store.ServiceAddr, rt model.Route, res resolve.Result) bool {
	if res.Binding == nil || res.Binding.Guest == "" {
		return false
	}
	o := c.serviceOwner(rt, res)
	if m.Known() && (m.Guest != o.Guest || m.Identity != o.Identity) {
		return false
	}
	return c.usedByItsOwner(m, o.Guest)
}

// usedByItsOwner reports whether a route m was made for, to guest or, when
// guest is empty, to any, is served at the address m forwards to in this
// cycle.
func (c *cycleRun) usedByItsOwner(m store.ServiceAddr, guest string) bool {
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		if !ok || res.Binding == nil || !res.Target.Leg || !serves(rt, res) {
			continue
		}
		if (mappingKey{res.Binding.Via, res.Target.Addr}) == (mappingKey{m.Network, m.Addr}) && (guest == "" || res.Binding.Guest == guest) && c.madeFor(m, rt, res) {
			return true
		}
	}
	return false
}

// disown releases the service address m, made for another than rt, which is
// at its address now, and reports whether it is gone. A rule that still
// sends a connector to it, as one Cloudflare kept after a write that failed,
// reaches nothing: the pool never gives the address again.
func (c *cycleRun) disown(m store.ServiceAddr, rt model.Route, ms mappings) bool {
	c.freeze(m.Service)
	if err := c.e.d.Store.ReleaseServiceAddr(m.Network, m.Addr); err != nil {
		c.problem(problemServiceRelease, m.Service, m.Addr, m.Network, err)
		return false
	}
	ms.remove(m)
	c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindNetwork, Subject: m.Network, Route: rt.Hostname,
		Message: fmt.Sprintf("service address %s of %s on network %s released: it was made for %s, and %s is at the address now",
			m.Service, m.Addr, m.Network, madeForText(m), c.serviceOwner(rt, c.results[rt.Hostname]).Name())})
	return true
}

// madeForText names whom m was made for.
func madeForText(m store.ServiceAddr) string {
	if !m.Known() {
		return "a route of an earlier version"
	}
	return m.Name()
}
