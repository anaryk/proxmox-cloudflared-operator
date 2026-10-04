package planner

import (
	"cmp"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// BlockedService is the service of a rule that answers 503 for a hostname
// nobody serves, so that no other owner's wildcard serves it.
const BlockedService = "http_status:503"

const (
	notFoundService = "http_status:404"

	reasonNoZone       = "no Cloudflare zone for this hostname in any credential"
	reasonSeveralCreds = "zone %s is visible through several credentials; pin it to one"
	reasonNotServed    = "zone %s is served through no credential"
	reasonReserved     = "reserved hostname"
	reasonNoAddr       = "no verified address yet"
	reasonNotAnswering = "target is not answering"
	reasonWithdrawn    = "identity check failed"
	reasonRejected     = "address must never be served"
	reasonOtherOwner   = "address was verified for another owner"
)

// Zone is a Cloudflare zone and the credential that can see it. A zone with
// no credential is one that none serves: a hostname in it has no zone, and is
// not taken for one of a parent zone either.
type Zone struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AccountID    string `json:"accountId"`
	CredentialID string `json:"credentialId"`
}

// ResolvedTarget is what address resolution knows about a hostname. Its
// address is the only one Build ever serves; an address a route names itself
// counts only once resolution has verified it and reports it here.
//
// Owner is the owner the address was verified for. A target verified for
// another owner than the one that now serves the hostname, as after a
// transfer, counts as never verified. Empty means it is not known and the
// target is taken as it is.
//
// Level is the identity level the address was proven at, for the route status
// to show; it does not change what is served.
type ResolvedTarget struct {
	Addr      netip.Addr `json:"addr,omitzero"`       // last verified address; zero if never verified
	Reachable bool       `json:"reachable,omitempty"` // currently passing identity and probe
	Withdrawn bool       `json:"withdrawn,omitempty"` // identity check failed: serve nothing, keep DNS
	Rejected  bool       `json:"rejected,omitempty"`  // the address must never be served: no rule, no DNS
	Reason    string     `json:"reason,omitempty"`
	Owner     string     `json:"owner,omitempty"`
	Level     string     `json:"level,omitempty"`
}

// IngressRule is one entry of a tunnel's ingress configuration.
type IngressRule struct {
	Hostname         string `json:"hostname,omitempty"`
	Service          string `json:"service"`
	OriginServerName string `json:"originServerName,omitempty"`
	MatchSNIToHost   bool   `json:"matchSNItoHost,omitempty"`
	NoTLSVerify      bool   `json:"noTLSVerify,omitempty"`
	HTTPHostHeader   string `json:"httpHostHeader,omitempty"`
}

// TunnelPlan is the desired ingress of the tunnel in one account. Rules end
// with the sentinel and the catch-all.
type TunnelPlan struct {
	AccountID    string        `json:"accountId"`
	CredentialID string        `json:"credentialId"`
	Name         string        `json:"name"`
	Rules        []IngressRule `json:"rules"`
}

// RecordPlan is a DNS record that points a hostname at a tunnel.
type RecordPlan struct {
	ZoneID       string `json:"zoneId"`
	ZoneName     string `json:"zoneName"`
	AccountID    string `json:"accountId"`
	CredentialID string `json:"credentialId"`
	Name         string `json:"name"`
	TunnelName   string `json:"tunnelName"`
}

// RouteState is how a route fares in the plan.
type RouteState string

const (
	StateActive      RouteState = "active"
	StateUnreachable RouteState = "unreachable"
	StateWithdrawn   RouteState = "withdrawn"
	StateConflict    RouteState = "conflict"
	StateNoZone      RouteState = "no-zone"
	StateHeld        RouteState = "held"     // claimed, but nobody serves it: blocked until its holder routes it or loses it
	StateRejected    RouteState = "rejected" // a name the hostname policy publishes only when allowHosts names it: blocked
)

// RouteStatus is the plan's verdict on one route.
type RouteStatus struct {
	Hostname string     `json:"hostname"`
	Owner    string     `json:"owner"`
	State    RouteState `json:"state"`
	Level    string     `json:"level,omitempty"` // identity level of the winner's target, once it was proven
	Reason   string     `json:"reason,omitempty"`
	Service  string     `json:"service,omitempty"` // set when the route is served; empty when it is blocked
	Zone     string     `json:"zone,omitempty"`
	Warnings []string   `json:"warnings,omitempty"`
}

// BuildInput is everything Build needs.
type BuildInput struct {
	Winners   []model.Route // at most one per hostname
	Conflicts []model.Route
	Claims    map[string]Claim          // by hostname, as ResolveClaims left them
	Targets   map[string]ResolvedTarget // by hostname
	Zones     []Zone
	Writer    Writer
	// AllowHosts are the allow patterns of the settings: the apex of a zone
	// and a wildcard are published for a guest only when one names them.
	AllowHosts []string
}

// Plan is the desired Cloudflare state.
type Plan struct {
	Tunnels []TunnelPlan  `json:"tunnels"` // sorted by account id
	Records []RecordPlan  `json:"records"` // sorted by zone name, then name
	Routes  []RouteStatus `json:"routes"`  // sorted by hostname, then owner
}

// Build turns the winning routes into the Cloudflare state that serves them:
// per account one tunnel with its ingress rules, the DNS records, and a status
// for every route including the ones that lost their hostname.
//
// Only an address that resolution verified for the winner is served. A
// claimed hostname that is not served, whether its winner cannot be served,
// its holder is absent within the grace or the holder's entry is broken, gets
// a rule that answers 503, so that no other owner's wildcard serves it
// meanwhile. A claim without a winner also gets a status, held, that names the
// holder and says why. Names in the .invalid domain, where the sentinels live,
// never get a rule, a record or a held status.
//
// An account gets a tunnel only when it serves a rule or a record points at
// it. A tunnel of nothing but 503 rules would be reached by no DNS record.
//
// Ingress is first-match, so each tunnel lists exact hostnames before
// wildcards. The output does not depend on the order of the input slices.
func Build(in BuildInput) Plan {
	b := builder{
		in:      in,
		zones:   newZoneIndex(in.Zones),
		rules:   make(map[string][]IngressRule),
		live:    make(map[string]bool),
		records: []RecordPlan{},
		routes:  []RouteStatus{},
	}
	overlaps := overlapWarnings(in.Winners)
	for i, rt := range in.Winners {
		b.addWinner(rt, overlaps[i])
	}
	b.blockHeld()
	b.addConflicts()

	slices.SortStableFunc(b.routes, func(x, y RouteStatus) int {
		return cmp.Or(
			strings.Compare(x.Hostname, y.Hostname),
			model.CompareOwners(x.Owner, y.Owner),
		)
	})
	slices.SortStableFunc(b.records, func(x, y RecordPlan) int {
		return cmp.Or(strings.Compare(x.ZoneName, y.ZoneName), strings.Compare(x.Name, y.Name))
	})
	return Plan{Tunnels: b.tunnels(), Records: b.records, Routes: b.routes}
}

type builder struct {
	in      BuildInput
	zones   zoneIndex
	rules   map[string][]IngressRule // by account
	live    map[string]bool          // accounts that serve a rule or have a record
	records []RecordPlan
	routes  []RouteStatus
}

func (b *builder) addWinner(rt model.Route, warnings []string) {
	st := RouteStatus{Hostname: rt.Hostname, Owner: rt.Owner()}
	zone, reason := b.zones.find(rt.Hostname)
	switch {
	case reserved(rt.Hostname):
		st.State, st.Reason = StateNoZone, reasonReserved
	case reason != "":
		st.State, st.Reason = StateNoZone, reason
	default:
		st.Zone = zone.Name
		if why := b.unnamed(rt, zone.Name); why != "" {
			st.State, st.Reason = StateRejected, why
			b.block(zone.AccountID, rt.Hostname)
			break
		}
		if hostname.Depth(rt.Hostname, zone.Name) > 1 {
			warnings = append(warnings, fmt.Sprintf("more than one level below %s: needs an advanced certificate", zone.Name))
		}
		b.serve(&st, rt, zone)
	}
	st.Warnings = sortedUnique(warnings)
	b.routes = append(b.routes, st)
}

// unnamed says why the route of a guest for the apex of its zone, or for a
// wildcard, is not published: without an allowHosts pattern that names it,
// whoever may edit the Notes of one tagged guest would take the zone's apex
// or every name of the zone that has no record of its own. A pattern that
// does not normalise names nothing. Manual routes are root's own.
func (b *builder) unnamed(rt model.Route, zone string) string {
	apex := rt.Hostname == zone
	if rt.Source != model.SourceAnnotation || !apex && !hostname.IsWildcard(rt.Hostname) {
		return ""
	}
	for _, p := range b.in.AllowHosts {
		if pattern, err := hostname.NormalizePattern(p); err == nil && hostname.NamesExplicitly(pattern, rt.Hostname) {
			return ""
		}
	}
	if apex {
		return fmt.Sprintf("the apex of zone %s is published only when allowHosts names it: add %q to allowHosts", zone, rt.Hostname)
	}
	return fmt.Sprintf("a wildcard is published only when an allowHosts pattern names it: add %q to allowHosts", rt.Hostname)
}

// serve plans the rule and the record of a route that has a zone and sets
// the outcome on st. A route that cannot be served is blocked instead.
func (b *builder) serve(st *RouteStatus, rt model.Route, zone Zone) {
	target := b.in.Targets[rt.Hostname]
	st.Level = target.Level
	switch {
	case target.Owner != "" && target.Owner != rt.Owner():
		// Everything resolution knows is about another owner.
		st.State, st.Reason, st.Level = StateUnreachable, reasonOtherOwner, ""
	case target.Rejected:
		st.State, st.Reason = StateUnreachable, cmp.Or(target.Reason, reasonRejected)
	case !target.Addr.IsValid():
		st.State, st.Reason = StateUnreachable, cmp.Or(target.Reason, reasonNoAddr)
	case target.Withdrawn:
		st.State, st.Reason = StateWithdrawn, cmp.Or(target.Reason, reasonWithdrawn)
		b.addRecord(zone, rt.Hostname)
	default:
		rule := newRule(rt, target.Addr)
		b.addRule(zone.AccountID, rule)
		b.live[zone.AccountID] = true
		b.addRecord(zone, rt.Hostname)
		st.Service = rule.Service
		st.State = StateActive
		if !target.Reachable {
			st.State, st.Reason = StateUnreachable, cmp.Or(target.Reason, reasonNotAnswering)
		}
		return
	}
	b.block(zone.AccountID, rt.Hostname)
}

// blockHeld blocks every claimed hostname that has no winner, so that no other
// owner's wildcard serves it, and gives it a status that says why.
func (b *builder) blockHeld() {
	won := make(map[string]bool, len(b.in.Winners))
	for _, w := range b.in.Winners {
		won[w.Hostname] = true
	}
	for _, host := range slices.Sorted(maps.Keys(b.in.Claims)) {
		if won[host] || reserved(host) {
			continue
		}
		zone, reason := b.zones.find(host)
		if reason != "" {
			continue
		}
		claim := b.in.Claims[host]
		b.block(zone.AccountID, host)
		b.routes = append(b.routes, RouteStatus{
			Hostname: host,
			Owner:    claim.Owner,
			State:    StateHeld,
			Reason:   heldReason(claim),
			Zone:     zone.Name,
		})
	}
}

// heldReason explains a claim that nobody serves: its holder still names the
// hostname without a route for it, or no longer asks for it and is within the
// grace.
func heldReason(c Claim) string {
	if c.MissingSince == nil {
		return fmt.Sprintf("named in the Notes of %s but not routed; claim kept", c.Owner)
	}
	return fmt.Sprintf("no longer claimed by %s since %s; released after the grace period",
		c.Owner, c.MissingSince.UTC().Format(time.RFC3339))
}

// block plans a rule that answers 503 for host.
func (b *builder) block(account, host string) {
	b.addRule(account, IngressRule{Hostname: host, Service: BlockedService})
}

func (b *builder) addRule(account string, rule IngressRule) {
	b.rules[account] = append(b.rules[account], rule)
}

func (b *builder) addRecord(zone Zone, name string) {
	b.live[zone.AccountID] = true // the record points at this account's tunnel
	b.records = append(b.records, RecordPlan{
		ZoneID:       zone.ID,
		ZoneName:     zone.Name,
		AccountID:    zone.AccountID,
		CredentialID: zone.CredentialID,
		Name:         name,
		TunnelName:   TunnelName(b.in.Writer.InstallID),
	})
}

// addConflicts adds a status for each route that lost its hostname. The
// serving owner is the winner when there is one, and otherwise the holder of
// the claim.
func (b *builder) addConflicts() {
	holders := make(map[string]string, len(b.in.Claims)+len(b.in.Winners))
	for host, c := range b.in.Claims {
		holders[host] = c.Owner
	}
	for _, w := range b.in.Winners {
		holders[w.Hostname] = w.Owner()
	}
	for _, rt := range b.in.Conflicts {
		b.routes = append(b.routes, RouteStatus{
			Hostname: rt.Hostname,
			Owner:    rt.Owner(),
			State:    StateConflict,
			Reason:   fmt.Sprintf("hostname is held by %s", holders[rt.Hostname]),
		})
	}
}

// tunnels returns one plan per account that serves a rule or has a record.
func (b *builder) tunnels() []TunnelPlan {
	out := []TunnelPlan{}
	for _, id := range slices.Sorted(maps.Keys(b.live)) {
		rules := b.rules[id]
		slices.SortStableFunc(rules, func(x, y IngressRule) int { return hostname.Compare(x.Hostname, y.Hostname) })
		rules = append(rules, SentinelRule(b.in.Writer), CatchAllRule())
		out = append(out, TunnelPlan{
			AccountID:    id,
			CredentialID: b.zones.credential[id],
			Name:         TunnelName(b.in.Writer.InstallID),
			Rules:        rules,
		})
	}
	return out
}

// newRule builds the ingress rule that sends hostname traffic to addr.
func newRule(rt model.Route, addr netip.Addr) IngressRule {
	rule := IngressRule{
		Hostname:       rt.Hostname,
		Service:        fmt.Sprintf("%s://%s", rt.Target.Scheme, netip.AddrPortFrom(addr, rt.Target.Port)),
		HTTPHostHeader: rt.Options.HostHeader,
	}
	if rt.Target.Scheme != model.SchemeHTTPS {
		return rule
	}
	rule.NoTLSVerify = rt.Options.NoTLSVerify
	switch {
	case rt.Options.SNI != "":
		rule.OriginServerName = rt.Options.SNI
	case rt.Options.NoTLSVerify:
		// Nothing to verify, so there is no name to present either.
	case hostname.IsWildcard(rt.Hostname):
		rule.MatchSNIToHost = true
	default:
		rule.OriginServerName = rt.Hostname
	}
	return rule
}

// overlapWarnings returns, per winner, the warnings about another owner's
// hostnames that its wildcard covers or that cover it.
func overlapWarnings(winners []model.Route) [][]string {
	out := make([][]string, len(winners))
	for i, wild := range winners {
		if !hostname.IsWildcard(wild.Hostname) {
			continue
		}
		for j, other := range winners {
			if wild.Owner() == other.Owner() || !hostname.Covers(wild.Hostname, other.Hostname) {
				continue
			}
			out[i] = append(out[i], fmt.Sprintf("%s overlaps %s owned by %s", wild.Hostname, other.Hostname, other.Owner()))
			out[j] = append(out[j], fmt.Sprintf("%s overlaps %s owned by %s", other.Hostname, wild.Hostname, wild.Owner()))
		}
	}
	return out
}

func sortedUnique(ss []string) []string {
	return slices.Compact(slices.Sorted(slices.Values(ss)))
}

// zoneIndex finds the zone of a hostname.
type zoneIndex struct {
	names      []string
	byName     map[string][]Zone // each sorted by credential, so the first is the pick
	credential map[string]string // account id -> credential that manages its tunnel
}

func newZoneIndex(zones []Zone) zoneIndex {
	idx := zoneIndex{byName: make(map[string][]Zone), credential: make(map[string]string)}
	for _, z := range zones {
		idx.byName[z.Name] = append(idx.byName[z.Name], z)
		if z.CredentialID == "" {
			continue
		}
		if cur, ok := idx.credential[z.AccountID]; !ok || z.CredentialID < cur {
			idx.credential[z.AccountID] = z.CredentialID
		}
	}
	for _, zs := range idx.byName {
		slices.SortFunc(zs, func(a, b Zone) int {
			return cmp.Or(
				strings.Compare(a.CredentialID, b.CredentialID),
				strings.Compare(a.AccountID, b.AccountID),
				strings.Compare(a.ID, b.ID),
			)
		})
	}
	idx.names = slices.Sorted(maps.Keys(idx.byName))
	return idx
}

// find returns the zone for host. When none can be used, reason says why.
func (x zoneIndex) find(host string) (zone Zone, reason string) {
	name, ok := hostname.MatchZone(host, x.names)
	if !ok {
		return Zone{}, reasonNoZone
	}
	zs := x.byName[name]
	switch {
	case zs[0].CredentialID == "":
		// Sorted by credential, a zone without one comes first.
		return Zone{}, fmt.Sprintf(reasonNotServed, name)
	case zs[0].CredentialID != zs[len(zs)-1].CredentialID:
		return Zone{}, fmt.Sprintf(reasonSeveralCreds, name)
	}
	return zs[0], ""
}
