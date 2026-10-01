package planner

import (
	"cmp"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const (
	notFoundService    = "http_status:404"
	reasonNoZone       = "no Cloudflare zone for this hostname in any credential"
	reasonNoAddr       = "no verified address yet"
	reasonSeveralCreds = "zone %s is visible through several credentials; pin it to one"
)

// Zone is a Cloudflare zone and the credential that can see it.
type Zone struct {
	ID           string
	Name         string
	AccountID    string
	CredentialID string
}

// ResolvedTarget is what address resolution knows about a hostname.
type ResolvedTarget struct {
	Addr      netip.Addr // last verified address; zero if never verified
	Reachable bool       // currently passing identity and probe
	Withdrawn bool       // identity check failed: serve nothing, keep DNS
	Reason    string
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
	AccountID    string
	CredentialID string
	Name         string
	Rules        []IngressRule
}

// RecordPlan is a DNS record that points a hostname at a tunnel.
type RecordPlan struct {
	ZoneID       string
	ZoneName     string
	AccountID    string
	CredentialID string
	Name         string
	TunnelName   string
}

// RouteState is how a route fares in the plan.
type RouteState string

const (
	StateActive      RouteState = "active"
	StateUnreachable RouteState = "unreachable"
	StateWithdrawn   RouteState = "withdrawn"
	StateConflict    RouteState = "conflict"
	StateNoZone      RouteState = "no-zone"
)

// RouteStatus is the plan's verdict on one route.
type RouteStatus struct {
	Hostname string
	Owner    string
	State    RouteState
	Reason   string
	Service  string // set when the route has an ingress rule
	Zone     string
	Warnings []string
}

// BuildInput is everything Build needs.
type BuildInput struct {
	Winners   []model.Route // at most one per hostname
	Conflicts []model.Route
	Holders   map[string]string         // hostname -> owner holding the claim
	Targets   map[string]ResolvedTarget // by hostname
	Zones     []Zone
	Writer    Writer
}

// Plan is the desired Cloudflare state.
type Plan struct {
	Tunnels []TunnelPlan  // sorted by account id
	Records []RecordPlan  // sorted by zone name, then name
	Routes  []RouteStatus // sorted by hostname, then owner
}

// Build turns the winning routes into the Cloudflare state that serves them:
// per account one tunnel with its ingress rules, the DNS records, and a status
// for every route including the ones that lost their hostname.
//
// Ingress is first-match, so each tunnel lists exact hostnames before
// wildcards. The output does not depend on the order of the input slices.
func Build(in BuildInput) Plan {
	b := builder{
		in:       in,
		zones:    newZoneIndex(in.Zones),
		accounts: make(map[string]*accountRules),
	}
	overlaps := overlapWarnings(in.Winners)
	for i, rt := range in.Winners {
		b.addWinner(rt, overlaps[i])
	}
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

// accountRules collects the ingress rules of one account's tunnel.
type accountRules struct {
	rules []IngressRule
}

type builder struct {
	in       BuildInput
	zones    zoneIndex
	accounts map[string]*accountRules // only accounts with a rule or a record
	records  []RecordPlan
	routes   []RouteStatus
}

func (b *builder) account(id string) *accountRules {
	if a, ok := b.accounts[id]; ok {
		return a
	}
	a := &accountRules{}
	b.accounts[id] = a
	return a
}

func (b *builder) addWinner(rt model.Route, warnings []string) {
	st := RouteStatus{Hostname: rt.Hostname, Owner: rt.Owner()}
	if zone, reason := b.zones.find(rt.Hostname); reason != "" {
		st.State, st.Reason = StateNoZone, reason
	} else {
		st.Zone = zone.Name
		if hostname.Depth(rt.Hostname, zone.Name) > 1 {
			warnings = append(warnings, fmt.Sprintf("more than one level below %s: needs an advanced certificate", zone.Name))
		}
		b.serve(&st, rt, zone)
	}
	st.Warnings = sortedUnique(warnings)
	b.routes = append(b.routes, st)
}

// serve plans the rule and the record of a route that has a zone and sets
// the outcome on st.
func (b *builder) serve(st *RouteStatus, rt model.Route, zone Zone) {
	target, known := b.in.Targets[rt.Hostname]
	if !known {
		// Without a verdict from resolution only the route's own address can
		// be served, and nothing speaks against it.
		target.Reachable = true
	}
	addr := rt.Target.Addr
	if !addr.IsValid() {
		addr = target.Addr
	}

	switch {
	case !addr.IsValid():
		st.State, st.Reason = StateUnreachable, cmp.Or(target.Reason, reasonNoAddr)
	case target.Withdrawn:
		st.State, st.Reason = StateWithdrawn, target.Reason
		b.addRecord(zone, rt.Hostname)
	default:
		rule := newRule(rt, addr)
		acc := b.account(zone.AccountID)
		acc.rules = append(acc.rules, rule)
		b.addRecord(zone, rt.Hostname)
		st.Service = rule.Service
		st.State = StateActive
		if !target.Reachable {
			st.State, st.Reason = StateUnreachable, target.Reason
		}
	}
}

func (b *builder) addRecord(zone Zone, name string) {
	b.account(zone.AccountID)
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
// serving owner is the winner when there is one; Holders covers a hostname
// that is reserved for an absent holder.
func (b *builder) addConflicts() {
	holders := make(map[string]string, len(b.in.Holders)+len(b.in.Winners))
	maps.Copy(holders, b.in.Holders)
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

// tunnels returns one plan per account that has something to serve.
func (b *builder) tunnels() []TunnelPlan {
	var out []TunnelPlan
	for _, id := range slices.Sorted(maps.Keys(b.accounts)) {
		rules := b.accounts[id].rules
		slices.SortStableFunc(rules, func(x, y IngressRule) int { return hostname.Compare(x.Hostname, y.Hostname) })
		rules = append(rules,
			IngressRule{Hostname: SentinelHostname(b.in.Writer), Service: notFoundService},
			IngressRule{Service: notFoundService},
		)
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
	if zs[0].CredentialID != zs[len(zs)-1].CredentialID {
		return Zone{}, fmt.Sprintf(reasonSeveralCreds, name)
	}
	return zs[0], ""
}
