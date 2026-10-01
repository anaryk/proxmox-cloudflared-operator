package reconcile

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

const (
	tunnelDomain = ".cfargotunnel.com"

	heldNoTunnel  = "tunnel not created yet"
	heldInventory = "inventory incomplete"

	defaultGrace          = 60 * time.Second
	defaultMaxDeletes     = 5
	defaultMaxDeleteShare = 0.30
)

// DNSSettings tune the DNS reconciler. A setting that is zero or negative
// takes its default.
type DNSSettings struct {
	InstallID      string
	Grace          time.Duration // default 60s
	MaxDeletes     int           // default 5
	MaxDeleteShare float64       // default 0.30
}

// ZoneRef is a zone pco manages and the credential that reaches it.
type ZoneRef struct {
	ID           string
	Name         string
	CredentialID string
}

// DNSInput is what a DNS run brings in line.
type DNSInput struct {
	Records        []planner.RecordPlan
	Zones          []ZoneRef       // every zone pco manages, also those with no wanted records
	Tunnels        []TunnelState   // to translate tunnel names into ids
	InventoryOK    bool            // false: no deletes, no tombstone changes
	ConfirmDeletes bool            // lifts the mass-delete guard for this run
	Adopt          map[string]bool // record names the admin agreed to take over
}

// Conflict is a record that is not ours and holds a name pco wants.
type Conflict struct {
	Zone    string
	Name    string
	Type    string
	Content string
}

// DNSResult is what a DNS run found and did.
type DNSResult struct {
	Actions   []Action // by zone name, then record name
	Conflicts []Conflict
	Lost      []string // names that still point at our tunnel but lost the marker
	Problems  []string
}

// DNSReconciler keeps a proxied CNAME to the tunnel for every published
// hostname and removes the records of hostnames no longer published. It
// changes only records whose comment begins with the marker of its install.
// Calls to Run are serialised.
type DNSReconciler struct {
	clients Clients
	store   TombstoneStore
	s       DNSSettings
	now     func() time.Time
	log     zerolog.Logger

	mu sync.Mutex
}

// NewDNSReconciler returns a reconciler that reaches each zone through the
// client of its credential and keeps the start of each removal grace in store.
func NewDNSReconciler(clients Clients, store TombstoneStore, s DNSSettings, now func() time.Time, log zerolog.Logger) *DNSReconciler {
	// A negative setting would loosen a guard, so it gets the default as well.
	if s.Grace <= 0 {
		s.Grace = defaultGrace
	}
	if s.MaxDeletes <= 0 {
		s.MaxDeletes = defaultMaxDeletes
	}
	if s.MaxDeleteShare <= 0 {
		s.MaxDeleteShare = defaultMaxDeleteShare
	}
	return &DNSReconciler{clients: clients, store: store, s: s, now: now, log: log}
}

// Run brings the records of every zone in line with in.
//
// A wanted name gets a proxied CNAME to its tunnel. A record of this install
// whose name is no longer wanted is deleted once the grace that began when it
// was first seen unwanted has passed, the inventory is complete, a fresh read
// confirms it, and the run does not delete a large share of the records at
// once. Records of others are never changed, except one the admin asked to
// adopt. A zone whose records cannot be listed is left alone. In Observe mode
// Run only reads and does not touch the tombstones; it returns the actions it
// would take as held.
func (r *DNSReconciler) Run(ctx context.Context, in DNSInput, mode Mode) DNSResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	run := &dnsRun{
		r:       r,
		in:      in,
		mode:    mode,
		now:     r.now(),
		marker:  planner.DNSMarker(r.s.InstallID),
		tunnels: tunnelIDs(in.Tunnels),
		adopt:   lowerKeys(in.Adopt),
	}
	if r.s.InstallID == "" {
		run.problem("dns: no install id, so no record can be recognised as ours")
		return run.res
	}
	if mode == Enforce && !run.loadTombstones(ctx) {
		return run.res
	}
	zones := run.zones()
	for _, z := range zones {
		run.list(ctx, z)
	}
	run.decideGuard(zones)
	for _, z := range zones {
		run.reconcile(ctx, z)
	}
	run.saveTombstones(ctx, zones)
	run.finish()
	return run.res
}

// tunnelRef names the tunnel of an account.
type tunnelRef struct{ account, name string }

// tunnelIDs maps every tunnel known to exist to its id.
func tunnelIDs(tunnels []TunnelState) map[tunnelRef]string {
	ids := make(map[tunnelRef]string, len(tunnels))
	for _, t := range tunnels {
		if t.Exists && t.ID != "" {
			ids[tunnelRef{t.AccountID, t.Name}] = t.ID
		}
	}
	return ids
}

func lowerKeys(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		if v {
			out[strings.ToLower(k)] = true
		}
	}
	return out
}

// dnsRun is the state of one Run.
type dnsRun struct {
	r       *DNSReconciler
	in      DNSInput
	mode    Mode
	now     time.Time
	marker  string
	tunnels map[tunnelRef]string
	adopt   map[string]bool // lower-case names
	stones  *tombstones     // nil in Observe mode
	guard   string          // why deletes due in this run are held; empty when they may go
	res     DNSResult
}

// dnsZone is one zone during a run. Names are kept in lower case.
type dnsZone struct {
	ZoneRef
	api     cfapi.API
	listed  bool                          // the records of this install were read
	wanted  map[string]planner.RecordPlan // by name
	owned   map[string][]cfapi.Record     // records of this install by name, probes left out
	probes  []cfapi.Record
	actions []Action
}

func (z *dnsZone) about(name string) string { return name + " in zone " + z.Name }

func (z *dnsZone) isWanted(name string) bool {
	_, ok := z.wanted[strings.ToLower(name)]
	return ok
}

// zones returns the zones of the run sorted by name: every zone of the input,
// and the zone of any wanted record that the input does not list.
func (run *dnsRun) zones() []*dnsZone {
	byID := make(map[string]*dnsZone)
	add := func(ref ZoneRef) *dnsZone {
		z, ok := byID[ref.ID]
		if !ok {
			z = &dnsZone{ZoneRef: ref, api: run.r.clients[ref.CredentialID], wanted: make(map[string]planner.RecordPlan)}
			byID[ref.ID] = z
		}
		return z
	}
	for _, ref := range run.in.Zones {
		add(ref)
	}
	for _, rp := range run.in.Records {
		z := add(ZoneRef{ID: rp.ZoneID, Name: rp.ZoneName, CredentialID: rp.CredentialID})
		z.wanted[strings.ToLower(rp.Name)] = rp
	}
	zones := slices.Collect(maps.Values(byID))
	slices.SortFunc(zones, func(a, b *dnsZone) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
	})
	return zones
}

// list reads the records of this install in a zone. A zone that cannot be
// read stays unlisted and the run leaves it alone.
func (run *dnsRun) list(ctx context.Context, z *dnsZone) {
	if z.api == nil {
		run.problem(fmt.Sprintf("zone %s: no client for credential %s", z.Name, z.CredentialID))
		return
	}
	// The filter is a prefix match, so it also returns the records of an
	// install whose id begins with ours; owns sorts them out.
	records, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{CommentPrefix: run.marker})
	if err != nil {
		run.problem(fmt.Sprintf("zone %s: listing the records of this install: %v", z.Name, err))
		return
	}
	z.listed = true
	z.owned = make(map[string][]cfapi.Record)
	for _, rec := range records {
		switch {
		case !run.owns(rec):
		case isProbe(rec, run.marker):
			z.probes = append(z.probes, rec)
		default:
			name := strings.ToLower(rec.Name)
			z.owned[name] = append(z.owned[name], rec)
		}
	}
}

// owns reports whether a record belongs to this install: its comment begins
// with the marker as a whole word.
func (run *dnsRun) owns(rec cfapi.Record) bool {
	rest, ok := strings.CutPrefix(rec.Comment, run.marker)
	if !ok {
		return false
	}
	next, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || unicode.IsSpace(next)
}

func isAddress(rec cfapi.Record) bool {
	return isType(rec, "A") || isType(rec, "AAAA") || isType(rec, "CNAME")
}

func isType(rec cfapi.Record, typ string) bool { return strings.EqualFold(rec.Type, typ) }

// reconcile brings one zone in line: wanted names first, then the records
// nobody wants any more.
func (run *dnsRun) reconcile(ctx context.Context, z *dnsZone) {
	if !z.listed {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(z.wanted)) {
		run.want(ctx, z, name)
	}
	for _, name := range slices.Sorted(maps.Keys(z.owned)) {
		if !z.isWanted(name) {
			run.retire(ctx, z, name)
		}
	}
	run.sweepProbes(ctx, z)
	if run.keepsTombstones() {
		run.forgetSettled(z)
	}
	slices.SortStableFunc(z.actions, func(a, b Action) int {
		return cmp.Compare(strings.ToLower(a.Target), strings.ToLower(b.Target))
	})
	run.res.Actions = append(run.res.Actions, z.actions...)
}

// want gives a wanted name its CNAME to the tunnel.
func (run *dnsRun) want(ctx context.Context, z *dnsZone, name string) {
	rp := z.wanted[name]
	ours := slices.DeleteFunc(slices.Clone(z.owned[name]), func(rec cfapi.Record) bool { return !isAddress(rec) })
	id, ok := run.tunnels[tunnelRef{rp.AccountID, rp.TunnelName}]
	if !ok {
		kind := CreateRecord
		if len(ours) > 0 {
			kind = UpdateRecord
		}
		z.add(Action{Kind: kind, Target: name, Detail: fmt.Sprintf("in zone %s: tunnel %s has no id", z.Name, rp.TunnelName)}, heldNoTunnel)
		return
	}
	target := id + tunnelDomain
	switch {
	case len(ours) == 0:
		run.claim(ctx, z, name, target)
	case len(ours) == 1 && isType(ours[0], "CNAME"):
		run.retarget(ctx, z, ours[0], target)
	default:
		// pco writes only CNAMEs, so address records under our marker were
		// made by hand; they are reported, not replaced.
		for _, rec := range ours {
			run.conflict(z, rec, target)
		}
	}
}

// retarget points a CNAME of this install at target. Its comment, which
// begins with the marker, is kept.
func (run *dnsRun) retarget(ctx context.Context, z *dnsZone, rec cfapi.Record, target string) {
	if strings.EqualFold(rec.Content, target) && rec.Proxied {
		return
	}
	upd := rec
	upd.Content, upd.Proxied = target, true
	run.write(z, Action{Kind: UpdateRecord, Target: rec.Name, Detail: pointDetail(z, target, rec)}, func() error {
		_, err := z.api.UpdateRecord(ctx, z.ID, upd)
		return err
	})
}

// claim creates the CNAME of a wanted name that has no record of this install,
// unless a record of someone else holds the name.
func (run *dnsRun) claim(ctx context.Context, z *dnsZone, name, target string) {
	found, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: name})
	if err != nil {
		run.problem(fmt.Sprintf("%s: looking up the records of that name: %v", z.about(name), err))
		return
	}
	holders := slices.DeleteFunc(found, func(rec cfapi.Record) bool { return !isAddress(rec) })
	switch {
	case slices.ContainsFunc(holders, run.owns):
		run.problem(fmt.Sprintf("%s: a record of this install appeared during the run; trying again on the next one", z.about(name)))
	case len(holders) == 0:
		run.create(ctx, z, Action{Kind: CreateRecord, Target: name, Detail: pointDetail(z, target)}, target)
	case run.adopt[name] && len(holders) == 1:
		run.adoptRecord(ctx, z, holders[0], target)
	default:
		for _, rec := range holders {
			run.conflict(z, rec, target)
		}
	}
}

func (run *dnsRun) create(ctx context.Context, z *dnsZone, a Action, target string) {
	rec := cfapi.Record{Type: "CNAME", Name: a.Target, Content: target, Proxied: true, Comment: run.marker}
	run.write(z, a, func() error {
		_, err := z.api.CreateRecord(ctx, z.ID, rec)
		return err
	})
}

// adoptRecord takes over the one record that holds a name the admin asked to
// adopt: a CNAME is changed in place, an address record is replaced.
func (run *dnsRun) adoptRecord(ctx context.Context, z *dnsZone, rec cfapi.Record, target string) {
	if isType(rec, "CNAME") {
		upd := cfapi.Record{ID: rec.ID, Type: "CNAME", Name: rec.Name, Content: target, Proxied: true, Comment: run.marker}
		a := Action{Kind: UpdateRecord, Target: rec.Name, Detail: pointDetail(z, target, rec), Destructive: true}
		run.write(z, a, func() error {
			_, err := z.api.UpdateRecord(ctx, z.ID, upd)
			return err
		})
		return
	}

	del := Action{Kind: DeleteRecord, Target: rec.Name, Detail: pointDetail(z, target, rec), Destructive: true}
	add := Action{Kind: CreateRecord, Target: rec.Name, Detail: pointDetail(z, target, rec), Destructive: true}
	held := ""
	switch {
	case !run.in.InventoryOK:
		held = heldInventory
	case run.mode == Observe:
		held = heldObserve
	}
	if held != "" {
		z.add(del, held)
		z.add(add, held)
		return
	}
	if run.write(z, del, func() error { return deleteRecord(ctx, z, rec.ID) }) {
		run.create(ctx, z, add, target)
	}
}

// conflict reports a record of someone else that holds a wanted name.
func (run *dnsRun) conflict(z *dnsZone, rec cfapi.Record, target string) {
	run.res.Conflicts = append(run.res.Conflicts, Conflict{Zone: z.Name, Name: rec.Name, Type: rec.Type, Content: rec.Content})
	if isType(rec, "CNAME") && strings.EqualFold(rec.Content, target) && !run.owns(rec) {
		run.res.Lost = append(run.res.Lost, rec.Name)
	}
}

// pointDetail describes pointing a name at target, and the record that was
// there before, if any.
func pointDetail(z *dnsZone, target string, was ...cfapi.Record) string {
	d := fmt.Sprintf("in zone %s: CNAME %s", z.Name, target)
	for _, rec := range was {
		d += fmt.Sprintf(", was %s %s", rec.Type, rec.Content)
	}
	return d
}

// write makes one change, or in Observe mode only records it, and reports
// whether the change was made.
func (run *dnsRun) write(z *dnsZone, a Action, call func() error) bool {
	if run.mode == Observe {
		z.add(a, heldObserve)
		return false
	}
	if err := call(); err != nil {
		z.add(a, err.Error())
		run.problem(fmt.Sprintf("%s: %s the record: %v", z.about(a.Target), verb(a.Kind), err))
		return false
	}
	z.add(a, "")
	run.r.log.Info().Str("zone", z.Name).Str("record", a.Target).Str("action", string(a.Kind)).Str("detail", a.Detail).Msg("changed dns record")
	return true
}

// deleteRecord deletes a record; one that is gone already counts as deleted.
func deleteRecord(ctx context.Context, z *dnsZone, id string) error {
	if err := z.api.DeleteRecord(ctx, z.ID, id); err != nil && !cfapi.IsNotFound(err) {
		return err
	}
	return nil
}

func verb(k ActionKind) string {
	switch k {
	case CreateRecord:
		return "creating"
	case UpdateRecord:
		return "updating"
	}
	return "deleting"
}

// add records an action of the zone; it was applied when held is empty.
func (z *dnsZone) add(a Action, held string) {
	a.Credential = z.CredentialID
	a.Applied = held == ""
	a.Held = held
	z.actions = append(z.actions, a)
}

func (run *dnsRun) problem(msg string) {
	run.r.log.Warn().Msg(msg)
	run.res.Problems = append(run.res.Problems, msg)
}

// finish puts the conflicts and lost names in a fixed order.
func (run *dnsRun) finish() {
	slices.SortFunc(run.res.Conflicts, func(a, b Conflict) int {
		return cmp.Or(
			cmp.Compare(a.Zone, b.Zone),
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.Content, b.Content),
		)
	})
	slices.Sort(run.res.Lost)
	run.res.Lost = slices.Compact(run.res.Lost)
}
