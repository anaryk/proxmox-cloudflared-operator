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
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const defaultPollInterval = 10 * time.Second

// Inventory is the part of *inventory.Inventory the engine uses.
type Inventory interface {
	Refresh(ctx context.Context) inventory.Snapshot
}

// Resolver is the part of *resolve.Resolver the engine uses. It must be safe
// for concurrent use.
type Resolver interface {
	Resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist, required resolve.Level) resolve.Result
}

// Connectors is the part of *connector.Manager the engine uses.
type Connectors interface {
	Ensure(ctx context.Context, installID, tunnelID, token string) error
	// PruneInstall removes the connectors of the install that are not in
	// keep, and never one of another install or of none.
	PruneInstall(ctx context.Context, installID string, keep []string) error
	List(ctx context.Context) ([]string, error) // every connector on the node, of any install
	Status(ctx context.Context, tunnelID string) (connector.Status, error)
	Token(tunnelID string) (token string, found bool, err error) // the token the connector has on disk
}

// ClientFactory builds a Cloudflare client for a credential. It is called
// again when the token of a credential changes, and for every check and
// removal of one; the clients of one credential id should share one
// cfapi.Limiter, so that a new token keeps the budget of the old one.
type ClientFactory func(c store.Credential) (cfapi.API, error)

// Deps is what an Engine is built from.
type Deps struct {
	Store      *store.Store
	Inventory  Inventory
	Resolver   Resolver
	Connectors Connectors
	Egress     Egress
	NewClient  ClientFactory
	Node       string
	Now        func() time.Time
	Log        zerolog.Logger

	// LocalDir is the node-local state directory, /var/lib/pco: the events
	// are appended to events.log in it. Empty keeps them in memory only.
	LocalDir string

	// StartOnly names the settings in s that differ from those the daemon
	// read once, at its start, and does not apply until it starts again; a
	// cycle reports them. Nil when there are none.
	StartOnly func(s store.Settings) []string
}

// Errors the admin actions return, for callers that map them to answers.
var (
	ErrInvalid  = errors.New("invalid request")
	ErrNotFound = errors.New("not found")
	ErrRefused  = errors.New("refused")
	// ErrBusy says that an admin action gave up waiting for the cycle that
	// runs; nothing was done.
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
	events  *eventLog
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
	adopt     map[string]*request  // by hostname
	rolledOut map[string]int       // by tunnel id: the configuration version confirmed on its connectors
	asked     map[string]time.Time // by tunnel id: when its connectors were last asked for the version
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
	// egress is the set of targets the egress filter was last given, and
	// verified, by account, the targets of the tunnel configuration last
	// verified at Cloudflare. Both are kept in the memory.
	egress   []egress.Target
	verified map[string][]egress.Target
	watched  watched

	// egMu guards what the watch of the network reads and changes beside
	// the cycles: the pins it watches, the addresses whose MAC moved and that
	// were not verified again yet, and the verifications that wait or run.
	// It is held around every change of the egress filter that depends on
	// the suspects.
	egMu     sync.Mutex
	pins     map[netip.Addr]egress.Pin
	suspects map[netip.Addr]bool
	checks   map[netip.Addr]*moveCheck
	moving   sync.WaitGroup

	// notes is what the checks of the egress table found since the last
	// cycle.
	noteMu sync.Mutex
	notes  egressNotes

	repMu   sync.Mutex
	reports map[string]credentials.Report // by credential id: the last check, also of an earlier process
	// recheckAt is, by credential id, when its token is checked again; a
	// credential not checked by this process is not in it, and due.
	recheckAt map[string]time.Time
	// rechecking is set while a recheck runs; storeHeld says that the last
	// cycle held because the store could not be read or written.
	rechecking atomic.Bool
	storeHeld  atomic.Bool

	stateMu sync.RWMutex
	state   State
	// listed is what the last cycle saw of the guests; served is, by
	// hostname, the owner that won it when claims were last settled.
	listed listing
	served map[string]string
}

// New returns an engine. It reads nothing yet: the first cycle does.
func New(d Deps) (*Engine, error) {
	switch {
	case d.Store == nil:
		return nil, errors.New("engine: no store")
	case d.Inventory == nil:
		return nil, errors.New("engine: no inventory")
	case d.Resolver == nil:
		return nil, errors.New("engine: no resolver")
	case d.Connectors == nil:
		return nil, errors.New("engine: no connector manager")
	case d.Egress == nil:
		return nil, errors.New("engine: no egress filter")
	case d.NewClient == nil:
		return nil, errors.New("engine: no Cloudflare client factory")
	case d.Node == "":
		return nil, errors.New("engine: the node name is empty")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	e := &Engine{
		d:         d,
		events:    newEventLog(d.LocalDir, d.Log),
		trigger:   make(chan struct{}, 1),
		after:     startTimer,
		timeout:   context.WithTimeout,
		sem:       make(chan struct{}, 1),
		clients:   reconcile.Clients{},
		tokens:    make(map[string]store.Secret),
		zones:     newZoneCache(),
		adopt:     make(map[string]*request),
		asked:     make(map[string]time.Time),
		rolledOut: make(map[string]int),
		gone:      make(map[model.GuestRef]bool),
		seen:      make(map[string]seenTunnel),
		reports:   make(map[string]credentials.Report),
		recheckAt: make(map[string]time.Time),
		verified:  make(map[string][]egress.Target),
		suspects:  make(map[netip.Addr]bool),
		checks:    make(map[netip.Addr]*moveCheck),
		state:     emptyState(),
	}
	e.interval.Store(int64(defaultPollInterval))
	// The reconciler keeps the time of its last write per tunnel, so it lives
	// as long as the engine. The DNS reconciler is made by the first cycle
	// that knows the install and the settings.
	e.tunnels = reconcile.NewTunnelReconciler(e.clients, e.writer, d.Now, d.Log)
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
// and returns the last state.
func (e *Engine) Cycle(ctx context.Context) State {
	if err := e.acquire(ctx); err != nil {
		return e.State()
	}
	defer e.release()

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
// cycle. Between cycles it checks the credentials that are due again, beside
// the cycles; it returns once that check has ended too.
func (e *Engine) Run(ctx context.Context) error {
	var checks sync.WaitGroup
	defer checks.Wait()
	defer e.moving.Wait()
	for {
		e.Cycle(ctx)
		if ctx.Err() != nil {
			return nil
		}
		checks.Go(func() { e.recheck(ctx) })
		wait, stop := e.after(time.Duration(e.interval.Load()))
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

// Events returns the events kept in memory that happened after since, oldest
// first.
func (e *Engine) Events(since time.Time) []Event { return e.events.since(since) }

// publish stores the state of a cycle with what it saw of the guests and, when
// it settled the claims, who won each hostname, and records what changed
// against the state before, after the events the cycle itself reported.
func (e *Engine) publish(st State, events []Event, l listing, served map[string]string) {
	e.stateMu.Lock()
	// A check of the egress table may have come while the cycle ran.
	e.noteMu.Lock()
	st.Egress = e.notes.view
	e.noteMu.Unlock()
	prev := e.state
	e.state, e.listed = st, l
	if served != nil {
		e.served = served
	}
	e.stateMu.Unlock()
	e.events.add(append(events, changes(prev, st)...)...)
}

// writer is the callback of both reconcilers: the identity read at the start
// of the cycle and leader.json as it is stored now, read afresh on every call.
// The reconcilers run only within a cycle, under the cycle lock.
func (e *Engine) writer() (us, stored planner.Writer, err error) {
	w, found, err := e.d.Store.Writer()
	switch {
	case err != nil:
		return e.us, planner.Writer{}, err
	case !found:
		return e.us, planner.Writer{}, errors.New("leader.json is missing")
	}
	return e.us, w, nil
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
	stale     []staleZone      // zones that left their listing
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

// staleZone names a zone that left the listing of a credential.
type staleZone struct{ credential, name string }

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
