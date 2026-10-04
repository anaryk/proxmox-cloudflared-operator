package engine

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// The kinds of what waits for a confirmation.
const (
	WaitingRemovals = "dns-removals"    // DNS removals behind the mass delete guard
	WaitingVanished = "vanished-guests" // guests Proxmox no longer lists, behind a vanish hold
	WaitingZone     = "stale-zone"      // a zone that left the listing of its credential, or whose DNS it can no longer read
	WaitingTunnel   = "unseen-tunnel"   // a tunnel no credential sees
)

// Waiting is one thing a confirmation would accept.
type Waiting struct {
	Kind    string   `json:"kind"`    // "dns-removals", "vanished-guests", "stale-zone", "unseen-tunnel"
	Subject string   `json:"subject"` // zone name, tunnel name; empty for the two summaries
	Detail  string   `json:"detail"`  // one sentence for the admin
	Items   []string `json:"items"`   // record names, or guests as "qemu/101 web-1"; sorted
}

// ApplyResult is what Apply did.
type ApplyResult struct {
	LeftObserveOnly bool      `json:"leftObserveOnly"`
	Accepted        []Waiting `json:"accepted"` // [] when nothing was confirmed
}

// waiting is what a state shows of what a confirmation would accept: one
// entry for the DNS removals behind the mass delete guard, in the guard's own
// words and with how many of them are still in their grace, one for the
// vanished guests, one per stale zone, one per zone whose DNS its credential
// can no longer read and one per tunnel no credential sees,
// sorted by kind and subject. The guests are named as the routes of the state
// last named them.
func (o confirmable) waiting(routes []RouteView) []Waiting {
	var out []Waiting
	if o.guard != "" {
		detail := o.guard
		if o.inGrace > 0 {
			detail += fmt.Sprintf("; %d of them are still in their grace and go when it ends", o.inGrace)
		}
		out = append(out, Waiting{Kind: WaitingRemovals, Detail: detail, Items: slices.Compact(slices.Sorted(slices.Values(o.removals)))})
	}
	if len(o.vanished) > 0 {
		names := make(map[model.GuestRef]string, len(routes))
		for _, r := range routes {
			if r.Guest != nil && r.Guest.Name != "" {
				names[r.Guest.GuestRef] = r.Guest.Name
			}
		}
		items := make([]string, 0, len(o.vanished))
		for _, ref := range o.vanished {
			items = append(items, strings.TrimSpace(ref.String()+" "+names[ref]))
		}
		out = append(out, Waiting{
			Kind: WaitingVanished,
			Detail: fmt.Sprintf("%d guests that hold a hostname are no longer listed by Proxmox; "+
				"a confirmation takes them as removed, and their hostnames are released after the grace period", len(o.vanished)),
			Items: slices.Compact(items),
		})
	}
	stale := make(map[string][]string)
	for _, sz := range o.stale {
		if !sz.refused {
			stale[sz.name] = append(stale[sz.name], sz.credential)
			continue
		}
		out = append(out, Waiting{
			Kind: WaitingZone, Subject: sz.name,
			Detail: fmt.Sprintf("credential %s can no longer read the DNS of zone %s; a confirmation lets the zone go: "+
				"its hostnames are taken off the tunnel, and its records are left as they are", sz.credential, sz.name),
			Items: []string{},
		})
	}
	for _, name := range slices.Sorted(maps.Keys(stale)) {
		out = append(out, Waiting{
			Kind: WaitingZone, Subject: name,
			Detail: fmt.Sprintf("zone %s is no longer listed by credential %s; a confirmation takes it as gone, "+
				"and its hostnames are taken off the tunnel", name, andList(stale[name])),
			Items: []string{},
		})
	}
	for _, t := range o.invisible {
		out = append(out, Waiting{
			Kind: WaitingTunnel, Subject: t.name,
			Detail: fmt.Sprintf("tunnel %s (%s) in account %s is not visible through any credential; "+
				"a confirmation takes it as gone and removes its connector", t.name, t.id, t.account),
			Items: []string{},
		})
	}
	slices.SortStableFunc(out, func(a, b Waiting) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Detail, b.Detail))
	})
	return out
}

// cloneWaiting copies what waits down to the items.
func cloneWaiting(w []Waiting) []Waiting {
	w = slices.Clone(w)
	for i := range w {
		w[i].Items = slices.Clone(w[i].Items)
	}
	return w
}

// offerOf names what waits: the first 16 hex digits of the SHA-256 of its
// JSON, so that the same in every cycle has the same name. Nothing waiting
// has none.
func offerOf(waiting []Waiting) string {
	if len(waiting) == 0 {
		return ""
	}
	// Strings and lists of them always encode.
	b, _ := json.Marshal(waiting)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
