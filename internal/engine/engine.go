// Package engine runs the reconcile cycle of the daemon. One cycle reads the
// store and the inventory, decides which owner serves each hostname and on
// which address, brings the tunnels, the connectors and the DNS records in
// line, and keeps what it found as a State for the API.
//
// Whenever something the cycle needs cannot be read, or cannot be trusted to
// be complete, the cycle holds: it changes nothing at Cloudflare, nothing on
// disk and no connector, and says why in State.Problems.
package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/managednet"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const defaultPollInterval = 10 * time.Second

// Inventory is the part of *inventory.Inventory the engine uses.
type Inventory interface {
	Refresh(ctx context.Context) inventory.Snapshot
	// ForceRefresh has the next refresh read every guest of node again.
	ForceRefresh(node string)
}

// Identity is the self-identification of the appliance; nil on the host.
type Identity interface {
	Check(ctx context.Context, snap inventory.Snapshot) appliance.Verdict
	// SetFlag writes the flag the connectors start behind, or removes it:
	// the engine removes it as well while a principal can reach into the
	// appliance.
	SetFlag(serve bool) error
}

// ClientFactory builds a Cloudflare client for a credential. It is called
// again when the token of a credential changes, and for every check and
// removal of one; the clients of one credential id should share one
// cfapi.Limiter, so that a new token keeps the budget of the old one.
type ClientFactory func(c store.Credential) (cfapi.API, error)

// Deps is what an Engine is built from.
type Deps struct {
	Store     *store.Store
	Inventory Inventory
	// Fleet is the agents through which the engine does what it does on a
	// node: the resolution of the targets, the egress filter, the watch of
	// the network, the connectors.
	Fleet     *LocalFleet
	NewClient ClientFactory
	Node      string
	Now       func() time.Time
	Log       zerolog.Logger

	// LocalDir is the node-local state directory, /var/lib/pco: the events
	// are appended to events.log in it. Empty keeps them in memory only.
	LocalDir string

	// StartOnly names the settings in s that differ from those the daemon
	// read once, at its start, and does not apply until it starts again; a
	// cycle reports them. Nil when there are none.
	StartOnly func(s store.Settings) []string
	// StartOnlyFields names the settings StartOnly compares: those the
	// daemon reads once, at its start.
	StartOnlyFields []string
	// Restart stops the daemon, which systemd then starts again; nil where
	// it cannot be restarted that way.
	Restart func()

	// Problems are problem lines that every cycle reports: what the daemon
	// was started with that must not go unnoticed.
	Problems []string

	// Access reads the access control of Proxmox, by which a guest whose
	// network others may configure is told apart. It is nil in tests that do
	// not need it: every guest counts as delegated then.
	Access  Access
	OwnUser string // "pco@pve": its tokens are not delegates
	// OwnSoft returns the appliance's own gateway and resolvers (appliance)
	// or nil (host): they join the soft deny.
	OwnSoft func() (gateways, resolvers []netip.Addr, err error)

	Identity Identity
	// Quorate reads the cluster's quorum; nil on the host.
	Quorate func(ctx context.Context) (bool, error)
	// Incarnation is this container's incarnation; empty on the host. The
	// engine uses no leader.json of another incarnation.
	Incarnation string
	// EpochDrawn reports whether this process drew a new epoch (EpochAtStart
	// did not keep the stored one); nil on the host. Until the first write
	// of this process is verified at Cloudflare, a foreign or stale verdict
	// on a sentinel of this install id is VerdictBehind; after it, a
	// foreign verdict (a second installation with the same install id)
	// stays foreign.
	EpochDrawn func() bool

	// Gateway is the backend of the networks reached through a leg of pco's
	// own; nil where none runs, which serves no route on a leg.
	Gateway Gateway
	// Networks is what the admin actions of the networks read and run; nil
	// where they cannot run.
	Networks Networks

	// Guests changes the cards of guests for the managed network; nil
	// where the daemon cannot, which refuses the managed network. The
	// readers below are what its step reads of Proxmox each cycle: the
	// registered tags of the datacenter, the vnet of the managed network,
	// and in the appliance the firewall options of the node, of the
	// datacenter and of a guest, and the state of a service of the node. A
	// nil reader counts as one that cannot read.
	Guests          managednet.Configurer
	RegisteredTags  func(ctx context.Context) ([]string, error)
	Vnet            func(ctx context.Context) (ManagedVnet, error)
	NodeFirewall    func(ctx context.Context) (pve.NodeFirewallOptions, error)
	ClusterFirewall func(ctx context.Context) (pve.ClusterFirewallOptions, error)
	NodeService     func(ctx context.Context, name string) (string, error)
	GuestFirewall   func(ctx context.Context, g model.Guest) (pve.GuestFirewallOptions, error)
	// Firewall reads and writes the firewall of guests in the appliance,
	// which isolates the cards on the managed network there; nil where the
	// daemon cannot, which adds no card in the appliance.
	Firewall managednet.FirewallAPI

	// Lease is the lease on the host, which a cycle must hold to write
	// anything; nil in the appliance, which has none. Quorum is the newest
	// read of the members file, whether the cluster is quorate and when the
	// read began, and Cluster the identity of the cluster the node is in;
	// both nil where there is no members file to read.
	Lease   Lease
	Quorum  func() (quorate bool, at time.Time)
	Cluster func() (store.ClusterIdentity, error)
	// HoldCycle, when set, is called as a cycle begins, with the context of
	// the daemon rather than of the cycle: the test suites hold a cycle
	// there to see what a hung engine does.
	HoldCycle func(ctx context.Context)

	// Remote is the agents of the other nodes of a cluster, which the
	// controller serves their snapshots through; nil where the engine
	// drives its own node alone. Members is the last read of the members
	// file, by which a node is down; nil where there is none to read.
	// Maintenance reads which nodes Proxmox has in maintenance mode; nil
	// where it is not read.
	Remote  Remote
	Members func() members.Members
	// Fallbacks names the nodes whose Proxmox API the client falls back
	// on; nil where it falls back on none.
	Fallbacks   func() []string
	Maintenance func(ctx context.Context) (map[string]pve.Maintenance, error)

	// nodes: Close ends the connections of a node's agent, which a node
	// that was forgotten loses before anything is rotated; Corosync reads
	// corosync.conf, by which a node still in the cluster is told; Endpoints
	// names where this controller answers the agents, which a join token
	// carries; Reissue has this node issue its certificates again from the
	// current CA, at once. Each nil where the daemon has none.
	Close     func(node string)
	Corosync  func() (members.Corosync, error)
	Endpoints func() []string
	Reissue   func(ctx context.Context) error
}

// Errors the admin actions return, for callers that map them to answers.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrRefused  = errors.New("refused")
	// ErrBusy says that an admin action gave up waiting for the cycle that
	// runs, or that every stream is taken; nothing was done.
	ErrBusy = errors.New("a cycle is running")
)

// lockWait is how long an admin action waits for the cycle that runs. With the
// fresh look of lookTimeout, which a move of a claim takes first, it is less than
// the command line waits for an answer, so that the admin is told why and the
// move does not go through after the command line gave up.
const lockWait = 35 * time.Second

// Engine runs reconcile cycles, one at a time. Its methods are safe for
// concurrent use.
type Engine struct {
	d       Deps
	boot    string
	events  *eventLog
	notify  *notifier
	trigger chan struct{}
	// after starts the wait between two cycles of Run, and timeout gives a
	// call its deadline; tests replace them.
	after   func(time.Duration) (<-chan time.Time, func() bool)
	timeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	// interval is the poll interval of the last settings read, in
	// nanoseconds; Run reads it outside the cycle lock.
	interval atomic.Int64

	// sem is held by the cycle that runs and by every admin action that
	// changes what a cycle reads. Everything below up to the reports is
	// guarded by it.
	sem     chan struct{}
	clients reconcile.Clients
	tokens  map[string]store.Secret // the token each client was built with
	tunnels *reconcile.TunnelReconciler
	dns     *reconcile.DNSReconciler
	dnsSet  reconcile.DNSSettings
	zones   *zoneCache
	addrs   nodeAddrs
	// us is the writer identity read at the start of the running cycle; the
	// writer callback of both reconcilers answers with it.
	us planner.Writer
	// confirm and adopt are the admin's one-shot requests that wait for a
	// DNS run to decide on them.
	confirm   *request
	adopt     map[string]*request    // by hostname
	rolledOut map[string]RolloutView // by tunnel id: the configuration version confirmed on its connectors
	asked     map[string]time.Time   // by tunnel id: when its connectors were last listed
	// retries are, by tunnel id, the reads of a run token again that failed
	// and have not worked since.
	retries map[string]tokenRetry
	// ownIDs holds, by tunnel id, the ids /ready gave the connector of the
	// node, with when each was last seen.
	ownIDs map[string]map[string]time.Time
	// gone are the guests holding a claim the admin confirmed removed.
	gone map[model.GuestRef]bool
	// seen are the tunnels of this install seen to exist, by id, so that a
	// connector is kept until Cloudflare shows its tunnel gone.
	seen map[string]seenTunnel
	// offered is what the last published state showed waiting for the
	// admin's confirmation: a confirmation that names it accepts that and
	// nothing else, once.
	offered offer
	// remembered says that the memory in the store was read: the served and
	// stale zones, the tunnels seen and the guests confirmed gone.
	remembered bool
	memoryOf   string // the install the memory is of
	// setAside says that the memory read was of another install, until a
	// cycle says so.
	setAside string
	// fleet is the agent of this node; seqs number the snapshots it is
	// given.
	fleet *LocalFleet
	seqs  seqs
	// egress is the set of targets the agent last gave the egress filter, and
	// verified, by tunnel, the targets of the tunnel configuration last
	// verified at Cloudflare. Both are kept in the memory, the set for an
	// earlier release, whose engine kept it.
	egress   []egress.Target
	verified map[tunnelRef][]egress.Target
	// moving are the verifications of addresses whose MAC moved that the
	// agent runs, which Run waits for.
	moving sync.WaitGroup

	// notes is what the checks of the egress table found since the last
	// cycle.
	noteMu sync.Mutex
	notes  egressNotes

	repMu   sync.Mutex
	reports map[string]credentials.Report // by credential id: the last check, also of an earlier process
	// refusedAgain holds, by credential id, the ids of the zones its last
	// check left out and the check before it did as well.
	refusedAgain map[string]map[string]bool
	// recheckAt is, by credential id, when its token is checked again; a
	// credential not checked by this process is not in it, and due.
	recheckAt map[string]time.Time
	// tried is, by credential id, the last check of its token this process
	// made, whether or not Cloudflare answered it.
	tried map[string]checkTry
	// rechecking is set while a recheck runs; storeHeld says that the last
	// cycle held because the store could not be read or written.
	rechecking atomic.Bool
	storeHeld  atomic.Bool
	// unchecked says that a credential may be stored that no check is known
	// of: it is set at a start and by a cycle that reads such a credential, and
	// cleared by a look at the store that finds none.
	unchecked atomic.Bool

	// access is what was last read of the access control of Proxmox; it is
	// refreshed outside the cycle lock.
	access accessState

	// The appliance: notServing says that it serves nothing, as a copy or
	// while principals can reach into it, from the verdict that says so to the
	// next one that writes the identity flag. Under the cycle lock, stopped
	// says that its connectors were stopped and its egress filter emptied
	// since it last served; firstWrite says that a write of this process was
	// verified at Cloudflare; epochAt is when this process drew its epoch,
	// zero until it did.
	notServing atomic.Bool
	stopped    bool
	firstWrite bool
	epochAt    time.Time

	// lastVerdict is what the tunnel run of the last cycle found of the
	// writer, which the gate of an expanding write reads: empty before the
	// first cycle of this process, verdictNoRun after a cycle that did not get
	// to the run. gwInUse says that the gateway was last synced with a leg or
	// a service address. Both are guarded by the cycle lock; legStates, what
	// the last sync said of the legs, by legMu.
	lastVerdict string
	gwInUse     bool
	legMu       sync.Mutex
	legStates   []gateway.LegState
	// freedMarks are the marks of the networks detached since the gateway
	// was last synced, which no attach takes until it was: guarded by the
	// cycle lock.
	freedMarks map[int]bool
	// pendingCards are, by owner, the cards on the managed network whose
	// addition waits for the guest's restart, and exhausted says that the
	// managed subnet ran out of addresses, as the last cycle found them;
	// both guarded by the cycle lock.
	pendingCards map[string]bool
	exhausted    bool
	// attachedSaid are the owners whose card this process said is attached;
	// vnetRead is what the step last read of the vnet, and impacts what it
	// last read of the firewall of each guest it would attach, both kept
	// for a while: guarded by the cycle lock.
	attachedSaid map[string]bool
	vnetRead     vnetRead
	impacts      map[string]impactRead
	// unknownMACs are, by MAC, when the source-MAC watch last reported a
	// frame of a MAC no lease knows on the managed network, and
	// unknownDropped how many reports since the last cycle had no event as
	// the record was full; guarded by macMu.
	macMu          sync.Mutex
	unknownMACs    map[string]time.Time
	unknownDropped int
	// lastUnknown is the last report of any unknown MAC, events or not,
	// which the filtered predicate of the next cycle reads; guarded by
	// macMu.
	lastUnknown unknownReport
	// dhcp is what the DHCP server of the managed network asks and tells,
	// beside the cycles; it has a lock of its own.
	dhcp dhcpBook
	// filtered is what the appliance's isolation keeps between its steps and
	// cycles: guarded by the cycle lock.
	filtered filteredMemory

	stateMu sync.RWMutex
	state   State
	// listed is what the last cycle saw of the guests; served is, by
	// hostname, the owner that won it when claims were last settled.
	listed listing
	served map[string]string

	// traffic is what the daemon sampled of the connectors' traffic.
	traffic traffic

	// cycleCtx is the context of the cycle that runs, for the gate the
	// writer callback asks; guarded by the cycle lock.
	cycleCtx context.Context
	// tenure is the epoch of the lease this process last wrote under, which
	// the store keeps the revisions of, tenureAt when a cycle first saw it,
	// and tenureWrote that every agent reported since; leaseSaid is what the
	// last event said of the lease. All guarded by the cycle lock.
	tenure      planner.Writer
	tenureAt    time.Time
	tenureWrote bool
	leaseSaid   leaseSaidAs
	// wentBack is the step back of the store the tenure saw, until a
	// promotion; guarded by the cycle lock.
	wentBack error
	// progress is what the watchdog of the daemon and the heartbeat read of
	// the cycles.
	progress progressBook
	// registry is what the registry step keeps between cycles, guarded by
	// the cycle lock; taken is what the controller took of the events of
	// every agent, which has a lock of its own.
	registry registryMemory
	taken    takenEvents
	// above are the epochs agents reported above this controller's.
	above aboveReports
	// place is what the placement of routes keeps between cycles, guarded
	// by the cycle lock.
	place placementMemory
	// nodes is what the admin's decisions about the nodes keep between
	// cycles, guarded by the cycle lock.
	nodes nodeMemory
	// ruleLimit is the most rules of hostnames a tunnel is kept under;
	// zero is maxIngressRules. Tests lower it.
	ruleLimit int
}

// New returns an engine. It reads nothing yet: the first cycle does.
func New(d Deps) (*Engine, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("engine: no store")
	case d.Inventory == nil:
		return nil, errors.New("engine: no inventory")
	case d.Fleet == nil:
		return nil, errors.New("engine: no fleet of agents")
	case d.NewClient == nil:
		return nil, errors.New("engine: no Cloudflare client factory")
	case d.Node == "":
		return nil, errors.New("engine: the node name is empty")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	d.Problems = slices.Clone(d.Problems)
	// Until the first cycle, the state says what the daemon was started with.
	first := emptyState()
	first.Problems, first.Node = slices.Clone(d.Problems), d.Node
	first = first.normalized()
	first.Digest = DigestOf(first)
	e := &Engine{
		d:         d,
		boot:      randomHex(8)(),
		events:    newEventLog(d.LocalDir, d.Log),
		notify:    newNotifier(first.stateNotice()),
		trigger:   make(chan struct{}, 1),
		after:     startTimer,
		timeout:   context.WithTimeout,
		sem:       make(chan struct{}, 1),
		clients:   reconcile.Clients{},
		tokens:    make(map[string]store.Secret),
		zones:     newZoneCache(),
		adopt:     make(map[string]*request),
		asked:     make(map[string]time.Time),
		retries:   make(map[string]tokenRetry),
		ownIDs:    make(map[string]map[string]time.Time),
		rolledOut: make(map[string]RolloutView),
		gone:      make(map[model.GuestRef]bool),
		seen:      make(map[string]seenTunnel),
		reports:   make(map[string]credentials.Report),
		recheckAt: make(map[string]time.Time),
		verified:  make(map[tunnelRef][]egress.Target),
		fleet:     d.Fleet,
		state:     first,

		pendingCards: make(map[string]bool),
		unknownMACs:  make(map[string]time.Time),
		attachedSaid: make(map[string]bool),
		impacts:      make(map[string]impactRead),

		refusedAgain: make(map[string]map[string]bool),
		tried:        make(map[string]checkTry),
	}
	e.events.boot, e.events.sent = e.boot, e.notify.events
	e.interval.Store(int64(defaultPollInterval))
	e.unchecked.Store(true)
	// The reconciler keeps the time of its last write per tunnel, so it lives
	// as long as the engine. The DNS reconciler is made by the first cycle
	// that knows the install and the settings.
	e.tunnels = reconcile.NewTunnelReconciler(e.clients, e.writer, d.Now, d.Log)
	if err := d.Fleet.attach(e); err != nil {
		return nil, fmt.Errorf("engine: starting the agent of this node: %w", err)
	}
	return e, nil
}

func startTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// acquire waits for the cycle lock until ctx ends.
func (e *Engine) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case e.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) release() { <-e.sem }

// acquireAdmin waits for the cycle lock as an admin action does: until ctx
// ends, and no longer than lockWait.
func (e *Engine) acquireAdmin(ctx context.Context) error {
	wait, cancel := e.timeout(ctx, lockWait)
	defer cancel()
	err := e.acquire(wait)
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("%w and took longer than %s; try again", ErrBusy, lockWait)
	}
	return err
}

// Cycle runs one full pass and stores the resulting state. A call waits for a
// cycle or admin action that is running; when ctx ends first, it runs nothing
// and returns the last state. The access control of Proxmox is read before,
// when it is due, without the cycle lock.
func (e *Engine) Cycle(ctx context.Context) State { return e.cycle(ctx, 0) }

// cycle is Cycle, with a deadline for the cycle once it holds the lock when
// deadline is above zero.
func (e *Engine) cycle(ctx context.Context, deadline time.Duration) State {
	// The read of the access control has a deadline of its own, and runs;
	// the wait for the lock behind an admin action does not.
	e.progress.started(e.d.Now())
	e.refreshAccess(ctx)
	e.progress.due(e.d.Now())
	if err := e.acquire(ctx); err != nil {
		return e.State()
	}
	defer e.release()
	defer e.progress.ended()
	ctx, cancel := e.beginCycle(ctx, deadline)
	defer cancel()
	e.cycleCtx = ctx
	defer func() { e.cycleCtx = nil }()

	c := e.newCycle(ctx)
	st := c.run()
	e.storeHeld.Store(c.storeHold)
	e.offered = offer{what: c.offer, waiting: st.clone().Waiting, token: st.Offer}
	e.publish(st.clone(), c.events, c.listing, c.served())
	e.logCycle(st)
	return st
}

// logCycle logs a cycle that changed something at info level, with the
// counts, and any other at debug level.
func (e *Engine) logCycle(st State) {
	applied := 0
	for _, a := range st.Actions {
		if a.Applied {
			applied++
		}
	}
	line, msg := e.d.Log.Debug().Bool("complete", st.Complete), "cycle done"
	if applied > 0 {
		line, msg = e.d.Log.Info(), "cycle changed something"
	}
	line.Str("mode", st.Mode).
		Int("applied", applied).
		Int("held", len(st.Actions)-applied).
		Int("routes", len(st.Routes)).
		Int("problems", len(st.Problems)).
		Msg(msg)
}

// Run cycles on the poll interval until ctx ends; Trigger asks for an early
// cycle. Before a cycle it checks the credentials never checked, and between
// cycles those that are due again, beside the cycles; it returns once that
// check has ended too. A cycle runs with a deadline, and Progress says what
// the loop does.
func (e *Engine) Run(ctx context.Context) error {
	var checks sync.WaitGroup
	defer checks.Wait()
	defer e.moving.Wait()
	e.progress.due(e.d.Now())
	for {
		// The first checks of the credentials have a deadline of their own.
		e.progress.started(e.d.Now())
		e.checkNew(ctx)
		e.timedCycle(ctx)
		if ctx.Err() != nil {
			return nil
		}
		checks.Go(func() { e.recheck(ctx) })
		interval := time.Duration(e.interval.Load())
		e.progress.due(e.d.Now().Add(interval))
		wait, stop := e.after(interval)
		select {
		case <-ctx.Done():
			stop()
			return nil
		case <-e.trigger:
			stop()
		case <-wait:
		}
	}
}

// PollInterval is the time between two cycles, as the last settings read say.
func (e *Engine) PollInterval() time.Duration { return time.Duration(e.interval.Load()) }

// Trigger asks Run for a cycle now. Requests made while one is pending are
// merged into it.
func (e *Engine) Trigger() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

// State returns the state of the last cycle.
func (e *Engine) State() State {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.state.clone()
}

// RunsAs says, without a copy of the state, what the daemon runs as: the
// profile of the install, empty until a cycle has read it, and the node it
// is registered as.
func (e *Engine) RunsAs() (profile, node string) {
	e.stateMu.RLock()
	defer e.stateMu.RUnlock()
	return e.state.Profile, e.state.Node
}

// Events returns the events kept in memory that happened after since, oldest
// first.
func (e *Engine) Events(since time.Time) []Event { return e.events.since(since) }

// publish stores the state of a cycle with what it saw of the guests and, when
// it settled the claims, who won each hostname, and records what changed
// against the state before, after the events the cycle itself reported. The
// stream is told of the state, changed or not, and then of the events.
func (e *Engine) publish(st State, events []Event, l listing, served map[string]string) {
	e.stateMu.Lock()
	// A check of the egress table may have come while the cycle ran.
	e.noteMu.Lock()
	st.Egress = e.notes.view
	e.noteMu.Unlock()
	st.Node = e.d.Node
	st.Digest = DigestOf(st)
	prev := e.state
	e.state, e.listed = st, l
	if served != nil {
		e.served = served
	}
	e.notify.state(st.stateNotice())
	e.stateMu.Unlock()
	e.events.add(append(events, changes(prev, st)...)...)
}

// request is a one-shot request of the admin.
type request struct {
	at  time.Time // when it was made
	why string    // why it waits, as the admin was last told
}

// confirmable is what one state showed waiting for the admin's
// confirmation, each with a problem line of its own.
type confirmable struct {
	vanished  []model.GuestRef // guests that hold a claim and no longer listed, behind a vanish hold
	stale     []staleZone      // zones that left their listing, or whose DNS can no longer be read
	invisible []unseenTunnel   // tunnels kept that no credential sees
	// guard is what the mass delete guard said when it held removals, and
	// removals the names of the records a confirmation lets through: those
	// it held and those in their grace, of which there are inGrace. All are
	// empty when it held none.
	guard    string
	removals []string
	inGrace  int
	// lines are the problem lines of the state that ask for the
	// confirmation of what is here.
	lines []string
}

// offer is what one published state showed waiting for a confirmation: what
// the engine accepts, what the admin was shown and the name of that.
type offer struct {
	what    confirmable
	waiting []Waiting
	token   string
}

// staleZone names a zone that left the listing of a credential or, when
// refused, a zone the credential serves and can no longer read the DNS of;
// readers then names the other credentials that can, as "a" or "a or b".
type staleZone struct {
	credential, name string
	refused          bool
	readers          string
}

// unseenTunnel is a tunnel of the install that no credential sees.
type unseenTunnel struct{ id, name, account string }

// seenTunnel is where a tunnel of this install was seen, and through which
// credential.
type seenTunnel struct{ account, name, credential string }

// nodeAddrs is every node address seen since the daemon started, together
// with the ones saved before. It never shrinks.
type nodeAddrs struct {
	loaded bool
	dirty  bool // holds an address the store does not have yet
	list   []netip.Addr
}
