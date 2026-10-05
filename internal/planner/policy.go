package planner

import (
	"fmt"

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
	names := make([]string, 0, len(zones))
	for _, z := range zones {
		names = append(names, z.Name)
	}
	var patterns []string
	for _, p := range allow {
		if pattern, err := hostname.NormalizePattern(p); err == nil {
			patterns = append(patterns, pattern)
		}
	}
	kept = make([]model.Route, 0, len(routes))
	for _, rt := range routes {
		zone, inZone := hostname.MatchZone(rt.Hostname, names)
		apex := inZone && rt.Hostname == zone
		if rt.Source != model.SourceAnnotation || !apex && !hostname.IsWildcard(rt.Hostname) || namedBy(patterns, rt.Hostname) {
			kept = append(kept, rt)
			continue
		}
		reason := fmt.Sprintf("a wildcard is published only when an allowHosts pattern names it: add %q to allowHosts", rt.Hostname)
		if apex {
			reason = fmt.Sprintf("the apex of zone %s is published only when allowHosts names it: add %q to allowHosts", zone, rt.Hostname)
		}
		refused = append(refused, RouteStatus{Hostname: rt.Hostname, Owner: rt.Owner(), State: StateRejected, Reason: reason, Zone: zone})
	}
	return kept, refused
}

func namedBy(patterns []string, host string) bool {
	for _, p := range patterns {
		if hostname.NamesExplicitly(p, host) {
			return true
		}
	}
	return false
}
