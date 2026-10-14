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

// gateUnknownIdentity is why a target of a guest whose identity the snapshot
// of this cycle does not hold gets no service address: whom it would be made
// for is not known.
const gateUnknownIdentity = "the identity of guest %s is not known in this cycle"

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

// identityUnknown reports whether rt, whose result res has a binding, goes
// to a guest whose identity this cycle's snapshot does not hold: it is
// proven to be neither the guest a service address was made for nor another.
func (c *cycleRun) identityUnknown(rt model.Route, res resolve.Result) bool {
	o := c.serviceOwner(rt, res)
	return o.Guest != "" && o.Identity == ""
}

// madeFor reports whether the service address m was made for rt, whose
// result res has a binding: its owner, guest and the guest's identity are
// those it was made for. A mapping stored without an owner was made for no
// one.
func (c *cycleRun) madeFor(m mapping, rt model.Route, res resolve.Result) bool {
	return res.Binding != nil && m.Known() && m.ServiceOwner == c.serviceOwner(rt, res)
}

// keeps reports whether rt, whose result res has a binding, is published
// under m: m was made for it, or is shared with it, or was made for its owner
// and guest while the guest's identity is not known in this cycle, which
// proves nothing either way.
func (c *cycleRun) keeps(m mapping, rt model.Route, res resolve.Result) bool {
	if c.madeFor(m, rt, res) || c.sharedWith(m, rt, res) {
		return true
	}
	o := c.serviceOwner(rt, res)
	return c.identityUnknown(rt, res) && m.Owner == o.Owner && m.Guest == o.Guest
}

// sharedWith reports whether rt, whose result res has a binding, may be
// published under m, made for another owner: both reach the same guest, with
// the same identity, and a route m was made for is served at its address in
// this cycle, as a manual route to a guest beside the guest's own. The rules
// of either then reach only that guest. Once the routes m was made for are
// gone, rt gets a service address of its own.
func (c *cycleRun) sharedWith(m mapping, rt model.Route, res resolve.Result) bool {
	if res.Binding == nil || res.Binding.Guest == "" || !m.Known() {
		return false
	}
	o := c.serviceOwner(rt, res)
	if m.Guest != o.Guest || m.Identity != o.Identity {
		return false
	}
	return c.usedByItsOwner(m, o.Guest)
}

// usedByItsOwner reports whether a route m was made for, to guest or, when
// guest is empty, to any, is served at the address m forwards to in this
// cycle.
func (c *cycleRun) usedByItsOwner(m mapping, guest string) bool {
	return c.servedAt(m, func(rt model.Route, res resolve.Result) bool {
		return (guest == "" || res.Binding.Guest == guest) && c.madeFor(m, rt, res)
	})
}

// servedFor reports whether a route published under m in this cycle is
// served at the address m forwards to: one m was made for, or one kept
// under it while its guest's identity is not known.
func (c *cycleRun) servedFor(m mapping) bool {
	return c.servedAt(m, func(rt model.Route, res resolve.Result) bool { return c.keeps(m, rt, res) })
}

// servedAt reports whether a winner that match accepts is served through a
// leg at the address m forwards to, here or on the node that resolves it.
func (c *cycleRun) servedAt(m mapping, match func(model.Route, resolve.Result) bool) bool {
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		if !ok || res.Binding == nil || !res.Target.Leg || !serves(rt, res) {
			continue
		}
		if c.localKey(res.Binding.Via, res.Target.Addr) == m.key() && match(rt, res) {
			return true
		}
	}
	for _, rl := range c.remoteLegs {
		if !rl.refused && rl.key == m.key() && match(rl.rt, rl.res) {
			return true
		}
	}
	return false
}

// disown releases the service address m, made for another than rt, which is
// at its address now, and reports whether it is gone. A rule that still
// sends a connector to it, as one Cloudflare kept after a write that failed,
// reaches nothing: the pool never gives the address again.
func (c *cycleRun) disown(m mapping, rt model.Route, res resolve.Result, ms mappings) bool {
	c.freeze(m.Service)
	if err := c.releaseMapping(m); err != nil {
		c.problem(problemServiceRelease, m.Service, m.Addr, m.onNetwork(), err)
		return false
	}
	ms.remove(m)
	c.events = append(c.events, Event{At: c.now, Level: levelInfo, Kind: kindNetwork, Subject: m.Network, Route: rt.Hostname,
		Message: fmt.Sprintf("service address %s of %s on network %s released: it was made for %s, and %s is at the address now",
			m.Service, m.Addr, m.onNetwork(), madeForText(m), c.serviceOwner(rt, res).Name())})
	return true
}

// madeForText names whom m was made for.
func madeForText(m mapping) string {
	if !m.Known() {
		return "a route of an earlier version"
	}
	return m.Name()
}
