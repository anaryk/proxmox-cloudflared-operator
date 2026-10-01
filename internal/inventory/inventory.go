package inventory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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

// Snapshot is the state of the cluster after one refresh. It must be treated
// as read-only; the inventory hands out the same data again when a later
// refresh fails.
type Snapshot struct {
	Guests []model.Guest // sorted by kind, then vmid
	Nodes  []Node        // sorted by name
	// Complete is false when any call needed to learn the true state failed,
	// so that absence from the snapshot proves nothing. A guest that merely
	// has no agent does not clear it.
	Complete bool
	// Problems says why Complete is false. It may also hold notes that leave
	// Complete alone, such as a token without the guest-agent privilege.
	Problems []string
	TakenAt  time.Time // when the refresh ran, also for a failed one
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
	GateTags       []string      // guests carrying one of these are "watched"
	FullSweepEvery time.Duration // config refresh for unwatched guests, default 5m
	ReportedTTL    time.Duration // agent / lxc interface cache, default 60s
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

	mu    sync.Mutex // serialises Refresh
	cache map[model.GuestRef]cacheEntry
	last  Snapshot
}

// cacheEntry is what is remembered about one guest between refreshes.
type cacheEntry struct {
	cfg      pve.GuestConfig
	cfgAt    time.Time
	reported *reportedEntry // nil when nothing is cached
}

// reportedEntry is the outcome of one interface lookup. An agent that could
// not answer is cached like any other result, as an entry without addresses,
// so that a guest whose agent hangs is not retried on every refresh.
type reportedEntry struct {
	identity  string // the guest it was read from
	at        time.Time
	addrs     []model.ReportedAddr
	forbidden bool // the token may not ask
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

	guests, cache := i.refreshGuests(ctx, r, r.uniqueRows(rows))
	if err := ctx.Err(); err != nil {
		return i.previous(r, cancelled(err))
	}
	nodes := i.refreshNodes(ctx, r)
	if err := ctx.Err(); err != nil {
		return i.previous(r, cancelled(err))
	}

	snap := Snapshot{Guests: guests, Nodes: nodes, Complete: r.complete, Problems: r.problems, TakenAt: r.now}
	i.cache, i.last = cache, snap
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

// previous returns the last good guests and nodes marked incomplete. The
// caches stay as they were.
func (i *Inventory) previous(r *run, problem string) Snapshot {
	return Snapshot{
		Guests:   slices.Clone(i.last.Guests),
		Nodes:    slices.Clone(i.last.Nodes),
		Complete: false,
		Problems: []string{problem},
		TakenAt:  r.now,
	}
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
	entry   cacheEntry
	guest   model.Guest
}

// refreshGuests builds the guests for rows, which are sorted and unique, and
// the cache for the next refresh. Guests that are no longer listed are not
// carried over.
func (i *Inventory) refreshGuests(ctx context.Context, r *run, rows []pve.Resource) ([]model.Guest, map[model.GuestRef]cacheEntry) {
	ts := i.refreshConfigs(ctx, r, rows)
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
// then once per FullSweepEvery.
func (i *Inventory) refreshConfigs(ctx context.Context, r *run, rows []pve.Resource) []tracked {
	var due []int
	for j, row := range rows {
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
	for j, row := range rows {
		ref := refOf(row)
		entry, cached := i.cache[ref]
		if res := results[j]; res.attempted {
			switch {
			case res.err == nil:
				entry.cfg, entry.cfgAt, cached = res.cfg, r.now, true
			case pve.IsNotFound(res.err):
				i.log.Debug().Stringer("guest", ref).Msg("guest vanished before its config was read")
				continue
			default:
				i.log.Debug().Stringer("guest", ref).Err(res.err).Msg("reading config failed")
				r.fail("guest %s: config not refreshed: %v", label(row), res.err)
			}
		}
		if !cached {
			continue
		}
		guest, err := BuildGuest(row, entry.cfg)
		if err != nil {
			r.fail("guest %s: %v", label(row), err)
			continue
		}
		ts = append(ts, tracked{row: row, watched: i.isWatched(row, entry.cfg), entry: entry, guest: guest})
	}
	i.log.Debug().Int("guests", len(rows)).Int("configCalls", len(due)).Msg("configs refreshed")
	return ts
}

// reportedResult is the outcome of one interface fetch.
type reportedResult struct {
	addrs []model.ReportedAddr
	err   error
}

// refreshReported fills in the reported addresses of running watched guests,
// asking for those whose cached answer has expired. Anything else loses its
// cached answer, so that a guest that starts again is asked afresh.
func (i *Inventory) refreshReported(ctx context.Context, r *run, ts []tracked) {
	var due []int
	for j := range ts {
		t := &ts[j]
		c := t.entry.reported
		switch {
		case !t.watched || !t.guest.Running:
			t.entry.reported = nil
		case c == nil || c.identity != t.guest.Identity || r.now.Sub(c.at) >= i.opts.ReportedTTL:
			due = append(due, j)
		}
	}
	results := make([]reportedResult, len(ts))
	i.forEach(ctx, len(due), func(k int) {
		results[due[k]] = i.fetchReported(ctx, ts[due[k]].row)
	})
	if ctx.Err() != nil {
		return
	}
	for _, j := range due {
		i.applyReported(r, &ts[j], results[j])
	}

	var forbidden bool
	for j := range ts {
		if c := ts[j].entry.reported; c != nil {
			ts[j].guest.Reported = slices.Clone(c.addrs)
			forbidden = forbidden || c.forbidden
		}
	}
	if forbidden {
		r.note("reported addresses unavailable: the API token lacks the guest-agent privilege (VM.GuestAgent.Audit, VM.Monitor before PVE 9)")
	}
	i.log.Debug().Int("interfaceCalls", len(due)).Msg("reported addresses refreshed")
}

// fetchReported asks a guest for its interfaces, giving up after callTimeout.
func (i *Inventory) fetchReported(ctx context.Context, row pve.Resource) reportedResult {
	ctx, cancel := context.WithTimeout(ctx, i.callTimeout)
	defer cancel()
	var (
		ifaces []pve.GuestIface
		err    error
	)
	if row.Kind == model.KindQEMU {
		ifaces, err = i.src.AgentInterfaces(ctx, row.Node, row.VMID)
	} else {
		ifaces, err = i.src.LXCInterfaces(ctx, row.Node, row.VMID)
	}
	if err != nil {
		return reportedResult{err: err}
	}
	return reportedResult{addrs: reportedAddrs(ifaces)}
}

// applyReported stores the outcome of a fetch. A guest that cannot answer
// because it has no agent, the agent hangs or the token may not ask has no
// reported addresses, which is a known state. Any other failure leaves the
// state unknown: the cached answer, if any, is kept and the snapshot is
// marked incomplete.
func (i *Inventory) applyReported(r *run, t *tracked, res reportedResult) {
	identity := t.guest.Identity
	switch {
	case res.err == nil:
		t.entry.reported = &reportedEntry{identity: identity, at: r.now, addrs: res.addrs}
	case errors.Is(res.err, pve.ErrAgentUnavailable), errors.Is(res.err, context.DeadlineExceeded):
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("no interfaces reported")
		t.entry.reported = &reportedEntry{identity: identity, at: r.now}
	case pve.IsForbidden(res.err):
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("interfaces forbidden")
		t.entry.reported = &reportedEntry{identity: identity, at: r.now, forbidden: true}
	default:
		i.log.Debug().Stringer("guest", t.guest.Ref).Err(res.err).Msg("reading interfaces failed")
		r.fail("guest %s: interfaces not refreshed: %v", label(t.row), res.err)
		if c := t.entry.reported; c != nil && c.identity != identity {
			t.entry.reported = nil
		}
	}
}

// reportedAddrs flattens the interfaces of a guest into usable addresses.
// Container and VPN interfaces say nothing about where the guest itself can
// be reached, and loopback and link-local addresses cannot be reached from
// outside at all.
func reportedAddrs(ifaces []pve.GuestIface) []model.ReportedAddr {
	var out []model.ReportedAddr
	for _, ifc := range ifaces {
		if isContainerIface(ifc.Name) {
			continue
		}
		for _, a := range ifc.Addrs {
			if usable(a) {
				out = append(out, model.ReportedAddr{Iface: ifc.Name, MAC: ifc.MAC, Addr: a})
			}
		}
	}
	return out
}

// isContainerIface reports whether name starts with the name of a loopback,
// container-bridge or tunnel interface. The match is case-sensitive, as
// interface names are.
func isContainerIface(name string) bool {
	prefixes := []string{
		"lo", "docker", "br-", "veth", "cni", "cali", "flannel", "virbr",
		"lxcbr", "podman", "kube-", "tailscale", "wg",
	}
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
}

func usable(a netip.Addr) bool {
	return a.Is4() && !a.IsLoopback() && !a.IsLinkLocalUnicast() && !a.IsUnspecified() && !a.IsMulticast()
}

// nodeResult is the outcome of one node network fetch.
type nodeResult struct {
	ifaces []pve.NodeIface
	err    error
}

// refreshNodes lists the cluster nodes and the interfaces of those online. A
// failure keeps what the previous snapshot knew.
func (i *Inventory) refreshNodes(ctx context.Context, r *run) []Node {
	cluster, err := i.src.ClusterNodes(ctx)
	if err != nil {
		i.log.Debug().Err(err).Msg("listing cluster nodes failed")
		r.fail("cluster nodes not listed: %v", err)
		return slices.Clone(i.last.Nodes)
	}
	nodes := make([]Node, len(cluster))
	for j, c := range cluster {
		nodes[j] = Node{Name: c.Name, Addr: c.Addr, Online: c.Online, Local: c.Local}
	}
	slices.SortFunc(nodes, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })

	var due []int
	for j, n := range nodes {
		if n.Online {
			due = append(due, j)
		}
	}
	results := make([]nodeResult, len(nodes))
	i.forEach(ctx, len(due), func(k int) {
		ifaces, err := i.src.NodeNetwork(ctx, nodes[due[k]].Name)
		results[due[k]] = nodeResult{ifaces: ifaces, err: err}
	})
	if ctx.Err() != nil {
		return nil
	}
	for _, j := range due {
		if err := results[j].err; err != nil {
			i.log.Debug().Str("node", nodes[j].Name).Err(err).Msg("reading node network failed")
			r.fail("node %s: network not refreshed: %v", nodes[j].Name, err)
			nodes[j].Ifaces = i.knownIfaces(nodes[j].Name)
			continue
		}
		nodes[j].Ifaces = slices.Clone(results[j].ifaces)
	}
	return nodes
}

// knownIfaces returns the interfaces the previous snapshot had for a node.
func (i *Inventory) knownIfaces(name string) []pve.NodeIface {
	for _, n := range i.last.Nodes {
		if n.Name == name {
			return slices.Clone(n.Ifaces)
		}
	}
	return nil
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
