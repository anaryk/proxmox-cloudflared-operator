package planner

import (
	"fmt"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// RefuseUnnamed takes out of routes the guest routes for the apex of one of
// zones, or for a wildcard, that no allowHosts pattern names, and returns
// them as statuses in the state rejected, with the reason and how to allow
// them. Whoever may edit the Notes of one tagged guest could otherwise take
// the apex of a zone, or every name of the zone that has no record of its
// own. A refused route takes no claim and holds none, so that naming a
// hostname before the admin allows it wins nothing, and Build gives it no rule
// and no record. A pattern that does not normalise names nothing. Manual
// routes are root's own and pass. The routes keep their order.
func RefuseUnnamed(routes []model.Route, zones []Zone, allow []string) (kept []model.Route, refused []RouteStatus) {
	p := newClaimRules(zones, allow, nil)
	kept = make([]model.Route, 0, len(routes))
	for _, rt := range routes {
		if st, ok := p.unnamed(rt.Hostname, rt.Owner()); ok && rt.Source == model.SourceAnnotation {
			refused = append(refused, st)
			continue
		}
		kept = append(kept, rt)
	}
	return kept, refused
}

// RefuseUnzoned takes out of routes the guest routes for a name in no zone
// of served, the names of the zones the install ever served, and returns them
// as statuses in the state no-zone, with the reason Build would give. Such a
// route takes no claim: naming a hostname before its zone is served wins
// nothing, and once it is, whoever names it then gets it by the rule for a
// hostname nobody holds. A route in a zone that was served keeps its claim as
// it is, also while the zone is in doubt, gone from its listing or let go.
// Manual routes pass. The routes keep their order.
func RefuseUnzoned(routes []model.Route, zones []Zone, served []string) (kept []model.Route, refused []RouteStatus) {
	p := newClaimRules(zones, nil, served)
	kept = make([]model.Route, 0, len(routes))
	for _, rt := range routes {
		if st, ok := p.unzoned(rt.Hostname, rt.Owner()); ok && rt.Source == model.SourceAnnotation {
			refused = append(refused, st)
			continue
		}
		kept = append(kept, rt)
	}
	return kept, refused
}

// RefuseHeldUnnamed takes out of held the names that RefuseUnnamed would take
// out as routes, and returns them as statuses: a broken entry keeps no claim
// on a name that a working one could not take.
func RefuseHeldUnnamed(held []HeldName, zones []Zone, allow []string) (kept []HeldName, refused []RouteStatus) {
	p := newClaimRules(zones, allow, nil)
	return refuseHeld(held, p.unnamed)
}

// RefuseHeldUnzoned takes out of held the names that RefuseUnzoned would take
// out as routes, and returns them as statuses, as RefuseHeldUnnamed does.
func RefuseHeldUnzoned(held []HeldName, zones []Zone, served []string) (kept []HeldName, refused []RouteStatus) {
	p := newClaimRules(zones, nil, served)
	return refuseHeld(held, p.unzoned)
}

// refuseHeld takes out of held the names that refuse says no claim may be
// taken on.
func refuseHeld(held []HeldName, refuse func(host, owner string) (RouteStatus, bool)) (kept []HeldName, refused []RouteStatus) {
	kept = make([]HeldName, 0, len(held))
	for _, h := range held {
		if st, ok := refuse(h.Hostname, h.Owner); ok {
			refused = append(refused, st)
			continue
		}
		kept = append(kept, h)
	}
	return kept, refused
}

// claimRules say what a guest may take a claim on: the zones of the plan, the
// allowHosts patterns and the zones the install served.
type claimRules struct {
	zones    zoneIndex
	names    []string // of the zones of the plan
	known    []string // of those and the zones served
	served   map[string]bool
	patterns []string
}

func newClaimRules(zones []Zone, allow, served []string) claimRules {
	p := claimRules{zones: newZoneIndex(zones), served: make(map[string]bool, len(served))}
	p.names = p.zones.names
	p.known = append(slices.Clone(p.names), served...)
	for _, name := range served {
		p.served[name] = true
	}
	for _, pattern := range allow {
		if pattern, err := hostname.NormalizePattern(pattern); err == nil {
			p.patterns = append(p.patterns, pattern)
		}
	}
	return p
}

// unnamed returns the status of host when it is the apex of a zone of the
// plan, or a wildcard, that no pattern names.
func (p claimRules) unnamed(host, owner string) (RouteStatus, bool) {
	zone, inZone := hostname.MatchZone(host, p.names)
	apex := inZone && host == zone
	if !apex && !hostname.IsWildcard(host) || namedBy(p.patterns, host) {
		return RouteStatus{}, false
	}
	reason := fmt.Sprintf("a wildcard is published only when an allowHosts pattern names it: add %q to allowHosts", host)
	if apex {
		reason = fmt.Sprintf("the apex of zone %s is published only when allowHosts names it: add %q to allowHosts", zone, host)
	}
	return RouteStatus{Hostname: host, Owner: owner, State: StateRejected, Reason: reason, Zone: zone}, true
}

// unzoned returns the status of host when its zone, the closest of the plan
// and of those served, is not one the install served.
func (p claimRules) unzoned(host, owner string) (RouteStatus, bool) {
	if zone, _ := hostname.MatchZone(host, p.known); p.served[zone] {
		return RouteStatus{}, false
	}
	zone, reason := p.zones.find(host)
	switch {
	case reserved(host):
		reason = reasonReserved
	case reason == "":
		reason = fmt.Sprintf(reasonUnserved, zone.Name)
	}
	return RouteStatus{Hostname: host, Owner: owner, State: StateNoZone, Reason: reason}, true
}

func namedBy(patterns []string, host string) bool {
	for _, p := range patterns {
		if hostname.NamesExplicitly(p, host) {
			return true
		}
	}
	return false
}
