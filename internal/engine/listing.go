package engine

import (
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// listing is what the last cycle saw of the guests, when it saw every one of
// them. The admin actions that depend on what a guest is now, or on what it
// claims, go by it: a cycle that did not list every guest leaves it empty,
// and they refuse rather than act on an older one.
type listing struct {
	complete bool // every guest was listed; guests holds them
	guests   map[model.GuestRef]listedGuest
	// claimed holds, by hostname, the owners that claim it with a route or
	// with a name their Notes hold. It is nil unless the routes were worked
	// out under a policy that could be read.
	claimed map[string]map[string]bool
}

// listedGuest is what the listing showed of a guest. The tags and the Notes
// are those of the snapshot, not copies.
type listedGuest struct {
	name, identity, node string
	running              bool
	tags                 []string
	notes                string
}

// listingOf is the listing of a complete snapshot, without the claims.
func listingOf(snap inventory.Snapshot) listing {
	l := listing{complete: true, guests: make(map[model.GuestRef]listedGuest, len(snap.Guests))}
	for _, g := range snap.Guests {
		l.guests[g.Ref] = listedGuest{
			name: strings.TrimSpace(g.Name), identity: g.Identity, node: g.Node,
			running: g.Running, tags: g.Tags, notes: g.Description,
		}
	}
	return l
}

// withClaims adds what the owners claim: every hostname of a route of theirs,
// and every one their Notes hold without a route.
func (l listing) withClaims(col planner.Collected) listing {
	l.claimed = make(map[string]map[string]bool)
	add := func(host, owner string) {
		if l.claimed[host] == nil {
			l.claimed[host] = make(map[string]bool)
		}
		l.claimed[host][owner] = true
	}
	for _, rt := range col.Routes {
		add(rt.Hostname, rt.Owner())
	}
	for _, h := range col.Held {
		add(h.Hostname, h.Owner)
	}
	return l
}

// claims reports whether owner claimed host in the listing.
func (l listing) claims(host, owner string) bool { return l.claimed[host][owner] }

// identity is the identity the listing showed for owner; empty for an owner
// that is no guest, and for a guest it does not have.
func (l listing) identity(owner string) string {
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return ""
	}
	return l.guests[ref].identity
}

// guestView names the guest of an owner, or is nil for an owner that is no
// guest. The name is known for a guest the listing has.
func (l listing) guestView(owner string) *GuestView {
	ref, err := model.ParseGuestRef(owner)
	if err != nil {
		return nil
	}
	return &GuestView{GuestRef: ref, Name: l.guests[ref].name}
}

// lastListing returns the listing of the last cycle.
func (e *Engine) lastListing() listing {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.listed
}
