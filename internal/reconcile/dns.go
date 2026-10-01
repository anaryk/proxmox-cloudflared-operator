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

	heldNoTunnel      = "tunnel not created yet"
	heldTunnelUnknown = "tunnel state unknown"
	heldInventory     = "inventory incomplete"
	heldWriter        = "writer changed"
	heldUnreadable    = "writer unreadable"
	heldUnsaved       = "tombstones not saved"
	heldUnconfirmed   = "no inventory confirmation"
	heldAskFailed     = "inventory confirmation failed"

	defaultGrace          = 60 * time.Second
	defaultMaxGap         = 2 * time.Minute
	defaultMaxDeletes     = 5
	defaultMaxDeleteShare = 0.30
)

// DNSSettings tune the DNS reconciler. A setting that is zero or negative
// takes its default.
type DNSSettings struct {
	InstallID string        // must be the install id of the writer
	Grace     time.Duration // default 60s

	// MaxGap, default 2m, is the longest break in watching an unwanted name
	// that keeps its grace running. As the last sighting is saved at most
	// every MaxGap/4, the break between two runs that is tolerated lies
	// between 3/4 MaxGap and MaxGap.
	MaxGap time.Duration

	// The mass delete guard holds the deletes of unconfirmed removals while
	// more than MaxDeletes (default 5) and more than MaxDeleteShare (default
	// 0.30) of the records of this install are being removed.
	MaxDeletes     int
	MaxDeleteShare float64
}

// ZoneRef is a zone pco manages and the credential that reaches it.
type ZoneRef struct {
	ID           string
	Name         string
	CredentialID string
}

// DNSInput is what a DNS run brings in line.
type DNSInput struct {
	Records     []planner.RecordPlan
	Zones       []ZoneRef       // every zone pco manages, also those with no wanted records
	Tunnels     []TunnelState   // to translate tunnel names into ids
	InventoryOK bool            // false: no deletes, no tombstone started or confirmed
	Adopt       map[string]bool // record names the admin agreed to take over

	// ConfirmDeletes confirms every removal pending in a listed zone in this
	// run, in its grace or due: each then passes the mass delete guard once
	// its grace is over, also in later runs. A removal that becomes pending
	// later, or whose grace starts again, is not covered.
	ConfirmDeletes bool

	// StillUnwanted asks the inventory once more, right before a delete,
	// whether no guest publishes name. An error or false holds the delete.
	StillUnwanted func(ctx context.Context, name string) (bool, error)
}

// Conflict is a record at a wanted name that pco will not change: someone
// else's, or one of ours of an unexpected type.
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

	// Replaced lists records as they were before an adoption changed or
	// replaced them. A record is listed as the write goes out, so also when
	// Cloudflare refused it or its answer was lost.
	Replaced []cfapi.Record

	Problems []string
	Verdict  WriterVerdict // WriterStale when this process is not, or stopped being, the stored writer
}

// DNSReconciler keeps a proxied CNAME to the tunnel for every published
// hostname and removes the records of hostnames no longer published. It
// changes only records whose comment begins with the marker of its install.
// Calls to Run are serialised.
type DNSReconciler struct {
	clients Clients
	store   TombstoneStore
	writer  func() (us, stored planner.Writer, err error)
	s       DNSSettings
	now     func() time.Time
	log     zerolog.Logger

	mu sync.Mutex
	// wantedSinceSave holds the tombstone keys of the names runs saw wanted
	// since the tombstones were last saved: their stored grace is stale even
	// while the store, not yet saved, still holds it. It is bounded by the
	// number of distinct names planned, also in a process that only
	// observes, and emptied by the first enforcing run that saves the
	// tombstones or has nothing to save.
	wantedSinceSave map[string]bool
}

// NewDNSReconciler returns a reconciler that reaches each zone through the
// client of its credential and keeps the grace of each removal in store.
// writer is the callback the tunnel reconciler takes: it returns the identity
// this process writes as (us) and leader.json as stored now (stored).
func NewDNSReconciler(clients Clients, store TombstoneStore, writer func() (us, stored planner.Writer, err error), s DNSSettings, now func() time.Time, log zerolog.Logger) *DNSReconciler {
	// A negative setting would loosen a guard, so it gets the default as well.
	if s.Grace <= 0 {
		s.Grace = defaultGrace
	}
	if s.MaxGap <= 0 {
		s.MaxGap = defaultMaxGap
	}
	if s.MaxDeletes <= 0 {
		s.MaxDeletes = defaultMaxDeletes
	}
	if s.MaxDeleteShare <= 0 {
		s.MaxDeleteShare = defaultMaxDeleteShare
	}
	return &DNSReconciler{clients: clients, store: store, writer: writer, s: s, now: now, log: log, wantedSinceSave: make(map[string]bool)}
}

// Run brings the records of every zone in line with in.
//
// It works only while the writer callback names us as the writer stored in
// leader.json, asks again before every write to Cloudflare or to the store,
// and stops with WriterStale as soon as that changes. A wanted name gets a
// proxied CNAME to its tunnel; records of others are never changed, except
// the one the admin asked to adopt. A record of this install at an unwanted
// name is deleted only after a grace this writer watched without a break,
// with a complete inventory, the tombstones saved, a fresh read and an
// inventory check right before the call, and, while many removals are
// pending, the admin's confirmation (ConfirmDeletes). When in doubt a record
// stays. In Observe mode Run only reads and returns the actions it would take
// as held.
//
// A name seen wanted, by the plan of a run that did not find another writer
// stored or by the inventory right before a delete, starts a new grace when
// it is next unwanted, also when the drop of its tombstone could not be saved
// yet; that is remembered in memory, so a restart of the process between
// such a failed save and the next run loses it. A forward clock step larger
// than the grace but within MaxGap makes a tombstone due at once.
func (r *DNSReconciler) Run(ctx context.Context, in DNSInput, mode Mode) DNSResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	run := &dnsRun{
		r:      r,
		in:     in,
		mode:   mode,
		now:    r.now(),
		marker: planner.DNSMarker(r.s.InstallID),
		adopt:  lowerKeys(in.Adopt),
	}
	started := run.start()
	if run.res.Verdict != WriterStale {
		// What the plan of another writer wants says nothing about ours.
		for _, rp := range in.Records {
			r.wantedSinceSave[tombstoneKey(rp.ZoneID, rp.Name)] = true
		}
	}
	if !started {
		return run.res
	}
	if mode == Enforce && !run.loadTombstones(ctx) {
		return run.res
	}
	run.know(in.Tunnels)
	zones := run.zones()
	for _, z := range zones {
		run.list(ctx, z)
	}
	run.decide(zones)
	run.saveTombstones(ctx, true)
	for _, z := range zones {
		run.reconcile(ctx, z)
	}
	run.saveTombstones(ctx, false)
	run.finish()
	return run.res
}

// tunnelRef names the tunnel of an account.
type tunnelRef struct{ account, name string }

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
	us      planner.Writer // as the run started
	tunnels map[tunnelRef]TunnelState
	targets map[string]bool // lower-case CNAME targets of our tunnels
	adopt   map[string]bool // lower-case names
	twice   map[string]bool // lower-case names planned more than once
	stones  *tombstones     // nil in Observe mode

	// noDeletes says why no record is deleted in this run, when the
	// tombstones could not be saved before the deletes.
	noDeletes string
	// stopped says why the run stopped writing, once the writer changed or
	// could not be read: it makes no further call to Cloudflare or to the
	// store, and holds every action left for that reason.
	stopped string
	// askFailed is set once the inventory could not answer before a delete:
	// every delete left is held without further calls.
	askFailed bool

	res DNSResult
}

// dnsZone is one zone during a run. Names are kept in lower case.
type dnsZone struct {
	ZoneRef
	api     cfapi.API
	listed  bool                          // the records of this install were read
	wanted  map[string]planner.RecordPlan // by name
	owned   map[string][]cfapi.Record     // records of this install by name, probes left out
	probes  []cfapi.Record
	holds   map[string]string // unwanted names of owned records: why their delete is held, empty when it is due
	actions []Action
}

func (z *dnsZone) about(name string) string { return name + " in zone " + z.Name }

func (z *dnsZone) isWanted(name string) bool {
	_, ok := z.wanted[strings.ToLower(name)]
	return ok
}

// know indexes the tunnels of the input.
func (run *dnsRun) know(tunnels []TunnelState) {
	run.tunnels = make(map[tunnelRef]TunnelState, len(tunnels))
	run.targets = make(map[string]bool, len(tunnels))
	for _, t := range tunnels {
		run.tunnels[tunnelRef{t.AccountID, t.Name}] = t
		if t.ID != "" {
			run.targets[strings.ToLower(t.ID+tunnelDomain)] = true
		}
	}
}

// pointsAtOurs reports whether a record is a CNAME to one of our tunnels.
func (run *dnsRun) pointsAtOurs(rec cfapi.Record) bool {
	return isType(rec, "CNAME") && run.targets[strings.ToLower(rec.Content)]
}

// zones returns the zones of the run sorted by name: every zone of the input,
// and the zone of any wanted record that the input does not list. A name
// planned more than once is a problem and is not written.
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
	plans := make(map[string][]planner.RecordPlan)
	for _, rp := range run.in.Records {
		name := strings.ToLower(rp.Name)
		add(ZoneRef{ID: rp.ZoneID, Name: rp.ZoneName, CredentialID: rp.CredentialID}).wanted[name] = rp
		plans[name] = append(plans[name], rp)
	}
	run.twice = make(map[string]bool)
	for _, name := range slices.Sorted(maps.Keys(plans)) {
		if ps := plans[name]; len(ps) > 1 {
			run.twice[name] = true
			run.problem(fmt.Sprintf("%s: planned %d times (%s); not writing it", name, len(ps), describePlans(ps)))
		}
	}
	zones := slices.Collect(maps.Values(byID))
	slices.SortFunc(zones, func(a, b *dnsZone) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.ID, b.ID))
	})
	return zones
}

func describePlans(ps []planner.RecordPlan) string {
	out := make([]string, len(ps))
	for i, rp := range ps {
		out[i] = fmt.Sprintf("zone %s, tunnel %s in account %s", rp.ZoneName, rp.TunnelName, rp.AccountID)
	}
	return strings.Join(out, "; ")
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
		case run.isProbe(rec):
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

// recheck reads rec again by its id right before a write. found is false when
// it is gone or now has another name; ours tells whether it is still of the
// same type and carries the marker of this install.
func (run *dnsRun) recheck(ctx context.Context, z *dnsZone, rec cfapi.Record) (fresh cfapi.Record, found, ours bool, err error) {
	got, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: strings.ToLower(rec.Name)})
	if err != nil {
		return cfapi.Record{}, false, false, err
	}
	i := slices.IndexFunc(got, func(f cfapi.Record) bool { return f.ID == rec.ID })
	if i < 0 || !strings.EqualFold(got[i].Name, rec.Name) {
		return cfapi.Record{}, false, false, nil
	}
	fresh = got[i]
	return fresh, true, isType(fresh, rec.Type) && run.owns(fresh), nil
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
	for _, name := range slices.Sorted(maps.Keys(z.holds)) {
		run.retire(ctx, z, name)
	}
	run.sweepProbes(ctx, z)
	slices.SortStableFunc(z.actions, func(a, b Action) int {
		return cmp.Compare(strings.ToLower(a.Target), strings.ToLower(b.Target))
	})
	run.res.Actions = append(run.res.Actions, z.actions...)
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
