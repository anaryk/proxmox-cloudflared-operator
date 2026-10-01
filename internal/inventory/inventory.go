// Package inventory builds the operator's view of Proxmox guests and nodes.
//
// An Inventory runs one Refresh at a time: it may be called from several
// goroutines, and the calls wait for each other. Every Snapshot it returns is
// an independent copy that shares no memory with the inventory or with any
// other snapshot.
package inventory

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

const (
	defaultGateTag        = "cf-tunnel"
	defaultFullSweepEvery = 5 * time.Minute
	defaultReportedTTL    = time.Minute
	defaultConcurrency    = 4
	defaultCallTimeout    = 5 * time.Second
)

// Source is the part of the Proxmox API the inventory reads. *pve.Client
// satisfies it.
type Source interface {
	Resources(ctx context.Context) ([]pve.Resource, error)
	GuestConfig(ctx context.Context, node string, ref model.GuestRef) (pve.GuestConfig, error)
	AgentInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error)
	LXCInterfaces(ctx context.Context, node string, vmid int) ([]pve.GuestIface, error)
	ClusterNodes(ctx context.Context) ([]pve.ClusterNode, error)
	NodeNetwork(ctx context.Context, node string) ([]pve.NodeIface, error)
}

var _ Source = (*pve.Client)(nil)

// Node is a cluster member with its network interfaces.
type Node struct {
	Name   string
	Addr   netip.Addr
	Online bool
	Local  bool
	Ifaces []pve.NodeIface
}

// Snapshot is the state of the cluster after one refresh. It shares no memory
// with the inventory, so a caller may change it freely.
type Snapshot struct {
	Guests []model.Guest // sorted by kind, then vmid
	Nodes  []Node        // sorted by name
	// Complete is false when absence from the snapshot proves nothing. Only
	// these clear it: the resource listing failed or listed a guest twice; a
	// guest config fetch failed or found no config; a guest is of an unknown
	// kind; the cluster members or the network of a node could not be read; a
	// watched guest on an offline node has never been cached; the refresh was
	// cancelled. Failing to read the addresses a guest agent or container
	// reports does not clear it, and neither does an unknown guest status.
	Complete bool
	// Problems says why Complete is false. It also holds notes that leave
	// Complete alone, such as an offline node, an unknown guest status or a
	// token without the guest-agent privilege.
	Problems []string
	TakenAt  time.Time // when the refresh ran, also for a failed one
	GoodAt   time.Time // when the last complete refresh ran; zero until one has
}

// NodeAddrs returns every IPv4 address configured on any node, sorted and
// without duplicates. The cluster address of a node counts, so that an offline
// node, whose interfaces are unknown, still contributes.
func (s Snapshot) NodeAddrs() []netip.Addr {
	var out []netip.Addr
	for _, n := range s.Nodes {
		if n.Addr.Is4() {
			out = append(out, n.Addr)
		}
		for _, ifc := range n.Ifaces {
			for _, p := range ifc.Addrs {
				if p.Addr().Is4() {
					out = append(out, p.Addr())
				}
			}
		}
	}
	slices.SortFunc(out, netip.Addr.Compare)
	return slices.Compact(out)
}

// Guest returns the guest with the given reference.
func (s Snapshot) Guest(ref model.GuestRef) (model.Guest, bool) {
	for _, g := range s.Guests {
		if g.Ref == ref {
			return g, true
		}
	}
	return model.Guest{}, false
}

// Options tunes the refresh. Zero values select the defaults.
type Options struct {
	GateTags       []string      // guests carrying one of these are "watched", default cf-tunnel
	FullSweepEvery time.Duration // config refresh for unwatched guests, default 5m
	ReportedTTL    time.Duration // agent / lxc interface cache, default 60s; quick answers without addresses are kept for 15s at most
	Concurrency    int           // parallel API calls, default 4
}

// Inventory polls Proxmox and keeps what it learned between refreshes, so
// that a refresh costs a few calls rather than one per guest.
type Inventory struct {
	src  Source
	opts Options
	now  func() time.Time
	log  zerolog.Logger

	// callTimeout bounds each interface call; a guest agent that does not
	// answer must not stall the refresh. Tests shorten it.
	callTimeout time.Duration

	mu     sync.Mutex // serialises Refresh
	cache  map[model.GuestRef]cacheEntry
	last   Snapshot
	goodAt time.Time // of the last complete refresh
}

// cacheEntry is what is remembered about one guest between refreshes.
type cacheEntry struct {
	cfg      pve.GuestConfig
	cfgAt    time.Time
	running  bool           // last running state read from Proxmox
	identity string         // of the guest running was read for; empty when none was
	reported *reportedEntry // nil when nothing is cached
}

// New returns an Inventory that reads from src. A nil now selects time.Now.
func New(src Source, opts Options, now func() time.Time, log zerolog.Logger) *Inventory {
	if opts.FullSweepEvery <= 0 {
		opts.FullSweepEvery = defaultFullSweepEvery
	}
	if opts.ReportedTTL <= 0 {
		opts.ReportedTTL = defaultReportedTTL
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency
	}
	opts.GateTags = slices.Clone(opts.GateTags)
	if len(opts.GateTags) == 0 {
		opts.GateTags = []string{defaultGateTag}
	}
	if now == nil {
		now = time.Now
	}
	return &Inventory{
		src:         src,
		opts:        opts,
		now:         now,
		log:         log,
		callTimeout: defaultCallTimeout,
		cache:       map[model.GuestRef]cacheEntry{},
	}
}

// run collects the outcome of one refresh.
type run struct {
	now      time.Time
	complete bool
	problems []string
}

// fail records a reason why the snapshot cannot be trusted to be complete.
func (r *run) fail(format string, args ...any) {
	r.complete = false
	r.note(format, args...)
}

// note records a problem that leaves Complete alone.
func (r *run) note(format string, args ...any) {
	r.problems = append(r.problems, fmt.Sprintf(format, args...))
}

// Refresh reads the cluster and returns its state. When the guest list
// cannot be read, or ctx ends first, it returns the previous guests and
// nodes with Complete false. Calls are serialised.
//
// The cluster members are read before the guests, so that no config is asked
// from a node that is known to be offline.
func (i *Inventory) Refresh(ctx context.Context) Snapshot {
	i.mu.Lock()
	defer i.mu.Unlock()

	r := &run{now: i.now(), complete: true}
	if err := ctx.Err(); err != nil {
		return i.previous(r, cancelled(err))
	}
	rows, err := i.src.Resources(ctx)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return i.previous(r, cancelled(cerr))
		}
		i.log.Debug().Err(err).Msg("listing resources failed")
		return i.previous(r, fmt.Sprintf("cluster resources not listed: %v", err))
	}

	nodes, listed := i.listNodes(ctx, r)
	if err := ctx.Err(); err != nil {
		return i.previous(r, cancelled(err))
	}
	var offline map[string]bool
	if listed {
		offline = offlineNames(nodes)
	}
	guests, cache := i.refreshGuests(ctx, r, r.uniqueRows(rows), offline)
	if err := ctx.Err(); err != nil {
		return i.previous(r, cancelled(err))
	}
	if listed {
		i.refreshNodeIfaces(ctx, r, nodes)
		if err := ctx.Err(); err != nil {
			return i.previous(r, cancelled(err))
		}
	}

	if r.complete {
		i.goodAt = r.now
	}
	snap := Snapshot{Guests: guests, Nodes: nodes, Complete: r.complete, Problems: r.problems, TakenAt: r.now, GoodAt: i.goodAt}
	i.cache = cache
	i.last = Snapshot{Guests: cloneGuests(guests), Nodes: cloneNodes(nodes)}
	i.log.Debug().
		Int("guests", len(guests)).
		Int("nodes", len(nodes)).
		Bool("complete", snap.Complete).
		Int("problems", len(snap.Problems)).
		Msg("inventory refreshed")
	return snap
}

func cancelled(err error) string {
	return fmt.Sprintf("refresh cancelled: %v", err)
}

// previous returns a copy of the last good guests and nodes marked
// incomplete. The caches stay as they were.
func (i *Inventory) previous(r *run, problem string) Snapshot {
	return Snapshot{
		Guests:   cloneGuests(i.last.Guests),
		Nodes:    cloneNodes(i.last.Nodes),
		Complete: false,
		Problems: []string{problem},
		TakenAt:  r.now,
		GoodAt:   i.goodAt,
	}
}

// cloneGuests copies guests down to the slices they hold, so that the copy and
// the original can be changed independently.
func cloneGuests(in []model.Guest) []model.Guest {
	out := slices.Clone(in)
	for j := range out {
		out[j].Tags = slices.Clone(out[j].Tags)
		out[j].NICs = slices.Clone(out[j].NICs)
		for k := range out[j].NICs {
			out[j].NICs[k].Static = slices.Clone(out[j].NICs[k].Static)
		}
		out[j].Reported = slices.Clone(out[j].Reported)
	}
	return out
}

// uniqueRows sorts the resource rows by kind, then vmid, and drops a row that
// repeats a guest, which would leave its true node in doubt.
func (r *run) uniqueRows(rows []pve.Resource) []pve.Resource {
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b pve.Resource) int {
		return cmp.Or(
			strings.Compare(string(a.Kind), string(b.Kind)),
			cmp.Compare(a.VMID, b.VMID),
			strings.Compare(a.Node, b.Node),
		)
	})
	out := rows[:0]
	for _, row := range rows {
		if n := len(out); n > 0 && refOf(out[n-1]) == refOf(row) {
			r.fail("guest %s is listed on both %s and %s", refOf(row), out[n-1].Node, row.Node)
			continue
		}
		out = append(out, row)
	}
	return out
}

func refOf(row pve.Resource) model.GuestRef {
	return model.GuestRef{Kind: row.Kind, VMID: row.VMID}
}

// label names a guest in a problem line.
func label(row pve.Resource) string {
	if row.Name == "" {
		return refOf(row).String()
	}
	return fmt.Sprintf("%s (%s)", refOf(row), row.Name)
}

// tracked is a guest of the current refresh with what is cached about it.
type tracked struct {
	row     pve.Resource
	watched bool
	offline bool // its node is offline, so nothing is asked about it
	entry   cacheEntry
	guest   model.Guest
}

// refreshGuests builds the guests for rows, which are sorted and unique, and
// the cache for the next refresh. Guests that are no longer listed are not
// carried over. Nothing is asked about guests on the offline nodes.
func (i *Inventory) refreshGuests(ctx context.Context, r *run, rows []pve.Resource, offline map[string]bool) ([]model.Guest, map[model.GuestRef]cacheEntry) {
	ts := i.refreshConfigs(ctx, r, rows, offline)
	if ctx.Err() != nil {
		return nil, nil
	}
	i.refreshReported(ctx, r, ts)
	if ctx.Err() != nil {
		return nil, nil
	}
	guests := make([]model.Guest, len(ts))
	cache := make(map[model.GuestRef]cacheEntry, len(ts))
	for j, t := range ts {
		guests[j] = t.guest
		cache[t.guest.Ref] = t.entry
	}
	return guests, cache
}

// isWatched reports whether the guest carries a gate tag. Tags are compared
// exactly.
func (i *Inventory) isWatched(row pve.Resource, cfg pve.GuestConfig) bool {
	return slices.ContainsFunc(guestTags(row, cfg.Values), func(tag string) bool {
		return slices.Contains(i.opts.GateTags, tag)
	})
}

// configResult is the outcome of one config fetch.
type configResult struct {
	attempted bool
	cfg       pve.GuestConfig
	err       error
}

// refreshConfigs fetches the configs that are due and combines them with the
// cached ones. Watched guests are always due; the others on first sight and
// then once per FullSweepEvery. The current resource row always wins over what
// the cached config says about name, node, status and tags, except that a
// status other than running or stopped keeps the last known state, or marks
// the guest StatusUnknown when there is none.
func (i *Inventory) refreshConfigs(ctx context.Context, r *run, rows []pve.Resource, offline map[string]bool) []tracked {
	var due []int
	for j, row := range rows {
		if offline[row.Node] {
			continue
		}
		old, cached := i.cache[refOf(row)]
		if !cached || i.isWatched(row, old.cfg) || r.now.Sub(old.cfgAt) >= i.opts.FullSweepEvery {
			due = append(due, j)
		}
	}
	results := make([]configResult, len(rows))
	i.forEach(ctx, len(due), func(k int) {
		row := rows[due[k]]
		cfg, err := i.src.GuestConfig(ctx, row.Node, refOf(row))
		results[due[k]] = configResult{attempted: true, cfg: cfg, err: err}
	})
	if ctx.Err() != nil {
		return nil
	}

	ts := make([]tracked, 0, len(rows))
	held := map[string]bool{}
	unread := 0
	for j, row := range rows {
		isOffline := offline[row.Node]
		if isOffline {
			held[row.Node] = true
		}
		entry, cached := i.configEntry(r, row, results[j], isOffline)
		if !cached {
			continue
		}
		guest, err := BuildGuest(row, entry.cfg)
		if err != nil {
			r.fail("guest %s: %v", label(row), err)
			continue
		}
		if applyStatus(&guest, row, &entry) {
			unread++
		}
		ts = append(ts, tracked{row: row, watched: i.isWatched(row, entry.cfg), offline: isOffline, entry: entry, guest: guest})
	}
	for _, node := range slices.Sorted(maps.Keys(held)) {
		r.note("node %s is offline; using cached data for its guests", node)
	}
	if unread > 0 {
		r.note("status of %d guests is unknown; using last known state", unread)
	}
	i.log.Debug().Int("guests", len(rows)).Int("configCalls", len(due)).Msg("configs refreshed")
	return ts
}

// configEntry returns the config to build a guest from, and false when there
// is none. A config that could not be read leaves the cached one in place and
// the snapshot incomplete: not-found is not taken as a deletion, because a
// guest that has just moved to another node is answered that way too. A
// deletion shows as the guest missing from the next listing.
func (i *Inventory) configEntry(r *run, row pve.Resource, res configResult, offline bool) (cacheEntry, bool) {
	ref := refOf(row)
	entry, cached := i.cache[ref]
	switch {
	case offline:
		// A guest that is not cached cannot be told apart from one that is
		// gone. Only a watched one has routes that must not be dropped.
		if !cached && i.isWatched(row, pve.GuestConfig{}) {
			r.fail("guest %s: node %s is offline and the guest is not cached", label(row), row.Node)
		}
	case !res.attempted:
	case res.err == nil:
		entry.cfg, entry.cfgAt, cached = res.cfg, r.now, true
	case pve.IsNotFound(res.err):
		i.log.Debug().Stringer("guest", ref).Str("node", row.Node).Msg("config not found; the guest may have moved")
		r.fail("config for %s not found on %s; will re-check", label(row), row.Node)
	default:
		i.log.Debug().Stringer("guest", ref).Err(res.err).Msg("reading config failed")
		r.fail("guest %s: config not refreshed: %v", label(row), res.err)
	}
	return entry, cached
}

// forEach calls fn for 0 to n-1 on at most Concurrency goroutines and waits
// for them. It stops handing out work once ctx is done. fn must write only to
// the result slot it owns.
func (i *Inventory) forEach(ctx context.Context, n int, fn func(k int)) {
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(i.opts.Concurrency, n) {
		wg.Go(func() {
			for k := range jobs {
				fn(k)
			}
		})
	}
	for k := range n {
		if ctx.Err() != nil {
			break
		}
		jobs <- k
	}
	close(jobs)
	wg.Wait()
}
