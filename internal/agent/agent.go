// Package agent applies, on one node, the desired state the controller of an
// install renders for it: it resolves the targets the node chooses an address
// for, keeps the egress set, runs the connectors, carries out the controller's
// actions and says how the node fares.
//
// The agent decides nothing it was not told. On its own it only ever
// restricts: it takes a target out of its egress set when its proof is lost,
// its MAC moved or a block names it; it holds the shared connector at boot
// until it may start; it stops the shared connector while no controller
// answers and none of its targets verify; and it stands down when the
// controller refuses the node for good.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/pki"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/gateway"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// resolveTimeout bounds the resolution of one target, and
	// resolveConcurrency how many are resolved at once: as the engine does.
	resolveTimeout     = 15 * time.Second
	resolveConcurrency = 32

	// bootWait is how long after boot a node waits for a controller before a
	// verified shared target lets it start its shared connector.
	bootWait = 120 * time.Second
	// demoteSilence and demoteFailing are how long no controller has
	// answered, and how long none of the shared targets the agent had
	// verified verifies, before it stops its shared connector.
	demoteSilence = 60 * time.Second
	demoteFailing = 60 * time.Second
	// silentAfter is when the agent says that no controller answers.
	silentAfter = 60 * time.Second

	// pveWriteTimeout bounds a write of a guest's configuration.
	pveWriteTimeout = 30 * time.Second
)

// Resolver is the part of *resolve.Resolver the agent uses. It must be safe
// for concurrent use.
type Resolver interface {
	Resolve(ctx context.Context, route model.Route, snap inventory.Snapshot, prev *resolve.Binding, deny resolve.Denylist, required resolve.Level, share *resolve.Shared) resolve.Result
}

// Connectors is the part of *connector.Manager the agent uses.
type Connectors interface {
	Ensure(ctx context.Context, installID, tunnelID, token string) error
	// PruneInstall removes the connectors of the install that are not in
	// keep, and never one of another install or of none.
	PruneInstall(ctx context.Context, installID string, keep []string) error
	List(ctx context.Context) ([]string, error) // every connector on the node, of any install
	Status(ctx context.Context, tunnelID string) (connector.Status, error)
	Token(tunnelID string) (token string, found bool, err error) // the token the connector has on disk
	// StopAll stops and disables every connector of the install and keeps
	// their files.
	StopAll(ctx context.Context, installID string) error
	// Stop stops one connector gracefully and keeps its files.
	Stop(ctx context.Context, tunnelID string) error
	// Disable takes the boot enable of a connector off and leaves it
	// running: shared tunnels once a second node is registered.
	Disable(ctx context.Context, tunnelID string) error
}

// Egress is the filter that confines the connectors to the targets the agent
// verified. Its methods may be called from more than one goroutine.
type Egress interface {
	Set(ctx context.Context, targets []egress.Target) error
	Remove(ctx context.Context, addr netip.Addr) error
	FlowCounts(ctx context.Context) (generation string, counts map[netip.AddrPort]uint64, err error)
}

// Gateway is the part of gateway.Backend the agent drives.
type Gateway interface {
	Sync(ctx context.Context, legs []gateway.Leg, services []gateway.Service, pins []gateway.Pin, sticky []gateway.Sticky) ([]gateway.LegState, error)
}

// Runner runs qm or pct with argv as its arguments, without a shell, and
// returns what it wrote to stdout.
type Runner func(ctx context.Context, argv ...string) (string, error)

// Event is something the agent did or found that the admin should learn of.
type Event struct {
	At      time.Time
	Level   string // info, warn or error
	Kind    string // egress, connector or node
	Subject string
	Message string
	Tunnel  string // the tunnel's name, for an event of a connector
}

// The levels and kinds of the agent's events.
const (
	levelInfo  = "info"
	levelWarn  = "warn"
	levelError = "error"

	kindEgress    = "egress"
	kindConnector = "connector"
	kindNode      = "node"
)

// Deps is what an Agent is built from.
type Deps struct {
	Node      string
	Kind      pki.Kind // host or appliance
	InstallID string
	Store     *store.Store
	Resolver  Resolver
	// Connectors, Egress and Blocked are the node's: the connector units,
	// the egress filter, and the addresses blocked on this node alone (pco
	// egress block), which the filter refuses whatever the set holds.
	Connectors Connectors
	Egress     Egress
	Blocked    func() ([]netip.Addr, error)
	Gateway    Gateway // nil where no gateway runs
	Runner     Runner  // qm and pct; host only
	Now        func() time.Time
	Log        zerolog.Logger
	// Changed is called when what the agent reports changed outside a pass,
	// as when an address whose MAC moved failed its verification, so that it
	// is reported soon. It may be nil.
	Changed func()
	// Event receives the agent's events. It may be nil: they are logged.
	Event func(Event)
}

// Agent applies the desired state of one node. Its methods are safe for
// concurrent use.
type Agent struct {
	d       Deps
	started time.Time
	// after starts the wait before a verification of a moved address; tests
	// replace it.
	after func(time.Duration) (<-chan time.Time, func() bool)

	// sem is held by every pass, by StandDown and by the verification of an
	// address whose MAC moved. It guards everything below up to rep.
	sem        chan struct{}
	remembered agentproto.Epoch // the epoch the agent follows
	applied    uint64           // the seq applied under it
	desired    *agentproto.Snapshot
	desiredRaw []byte    // desired as it was written
	seqAt      time.Time // when the seq applied last changed
	unreadable string    // why the desired state on disk could not be read
	refused    string    // why the last snapshot was refused, until one is accepted
	done       map[string]store.ActionDone
	answers    map[string]agentproto.ActionResult // the answers of this process
	set        []egress.Target                    // what the egress filter was last given
	watched    watched
	stood      string // why the agent stood down; empty while it serves
	boot       bootGate
	demote     demotion
	bootOff    map[string]bool // shared tunnels whose boot enable is off since their connector last started
	wasActive  map[string]bool // tunnels whose connector ran at the last pass
	ownReady   map[string]bool // tunnels whose connector was ready at the last pass
	gwInUse    bool            // the gateway was last given a leg or a service
	rep        view            // what the last pass found, for Status and View
	repMu      sync.Mutex      // guards rep

	heardMu sync.Mutex
	heardAt time.Time // the last snapshot or heartbeat of a controller
	hb      *agentproto.Heartbeat
	hbAt    time.Time

	// egMu guards what the watch of the network reads and changes beside the
	// passes: the pins it watches, the addresses whose MAC moved and that
	// were not verified again yet, and the verifications that wait or run.
	egMu     sync.Mutex
	pins     map[netip.Addr]egress.Pin
	suspects map[netip.Addr]bool
	checks   map[netip.Addr]*moveCheck
	moving   sync.WaitGroup
	vouch    vouching
}

// New returns the agent of a node, with the desired state, the epoch, the
// actions and the egress set it kept on disk. A desired state that cannot be
// read is said in its status: the agent then waits for the controller.
func New(d Deps) (*Agent, error) {
	switch {
	case d.Node == "":
		return nil, errors.New("agent: the node name is empty")
	case d.Kind != pki.KindHost && d.Kind != pki.KindAppliance:
		return nil, fmt.Errorf("agent: unknown kind %q", d.Kind)
	case d.InstallID == "":
		return nil, errors.New("agent: no install")
	case d.Store == nil:
		return nil, errors.New("agent: no store")
	case d.Resolver == nil:
		return nil, errors.New("agent: no resolver")
	case d.Connectors == nil:
		return nil, errors.New("agent: no connector manager")
	case d.Egress == nil:
		return nil, errors.New("agent: no egress filter")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Blocked == nil {
		d.Blocked = func() ([]netip.Addr, error) { return nil, nil }
	}
	a := &Agent{
		d:         d,
		started:   d.Now(),
		after:     startTimer,
		sem:       make(chan struct{}, 1),
		bootOff:   map[string]bool{},
		wasActive: map[string]bool{},
		ownReady:  map[string]bool{},
		answers:   map[string]agentproto.ActionResult{},
		demote:    newDemotion(),
		suspects:  map[netip.Addr]bool{},
		checks:    map[netip.Addr]*moveCheck{},
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	a.republish()
	return a, nil
}

// acquire waits for the agent's lock until ctx ends.
func (a *Agent) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case a.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) release() { <-a.sem }

func startTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// Wait returns once the verifications of moved addresses that were started
// have ended; they end with the context they were started with.
func (a *Agent) Wait() { a.moving.Wait() }

// EgressSet returns the set of targets the agent keeps. The addresses whose
// MAC moved and that were not verified again yet are in it, though the filter
// was given the set without them, and so are those blocked on this node.
func (a *Agent) EgressSet() []egress.Target {
	a.repMu.Lock()
	defer a.repMu.Unlock()
	return append([]egress.Target{}, a.rep.set...)
}

// FlowCounts reads the counters of the egress filter: by target, the
// connections the connectors opened to it.
func (a *Agent) FlowCounts(ctx context.Context) (generation string, counts map[netip.AddrPort]uint64, err error) {
	return a.d.Egress.FlowCounts(ctx)
}

// event records ev and logs it.
func (a *Agent) event(level, kind, subject, msg string) {
	a.eventOf(Event{At: a.d.Now(), Level: level, Kind: kind, Subject: subject, Message: msg})
}

func (a *Agent) eventOf(ev Event) {
	line := a.d.Log.Info()
	switch ev.Level {
	case levelWarn:
		line = a.d.Log.Warn()
	case levelError:
		line = a.d.Log.Error()
	}
	line.Str("kind", ev.Kind).Str("subject", ev.Subject).Msg(ev.Message)
	if a.d.Event != nil {
		a.d.Event(ev)
	}
}

// changed says that the report changed outside a pass.
func (a *Agent) changed() {
	if a.d.Changed != nil {
		a.d.Changed()
	}
}
