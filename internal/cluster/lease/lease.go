// Package lease holds the lease that makes one node of a Proxmox VE cluster
// the controller of an install, the only one that writes. The lease is a
// lock directory on the cluster filesystem, pmxcfs, taken and kept the way
// the Proxmox HA manager keeps its own: made with a plain mkdir, refreshed by
// setting its mtime, and removed by pmxcfs on another node's unlock request
// once nobody changed it for 120 seconds by the wall clock of the node with
// the lowest node id, which decides every unlock request. pmxcfs lets only
// the node that made a lock set its mtime, but any node remove it.
//
// A step forward of that one clock lets an unlock request remove a fresh
// lock. A node therefore sends none while its agent hears a controller that
// calls its lease valid, and a holder whose lock went anyway stops at its
// next refresh: a lost tenure, not a second writer.
//
// The keeper stops writing long before another node can take over: its
// lease is valid for Validity after the start of its last good refresh, on
// the monotonic clock, and the gate in front of every write wants a refresh
// that started at most GateAge ago. Each tenure has an epoch, a generation
// and a nonce in leader.json, which the sentinel at Cloudflare and the
// agents check, so that a writer the lease could not stop is still caught.
package lease

import (
	"errors"
	"io"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// DefaultLock is the lock directory. Older builds read the same name;
	// nothing is ever put inside it, as pmxcfs never expires a lock that
	// holds a file.
	DefaultLock = "/etc/pve/priv/lock/pco-leader"

	// RefreshEvery is the period of the refresh, Validity how long the
	// holder trusts a good one.
	RefreshEvery = 10 * time.Second
	Validity     = 60 * time.Second

	// GateAge is the oldest refresh the gate lets a write through on. A
	// synchronous refresh is made at most once per ShareRefresh; the gates
	// inside that window share it, as every refresh is a change the whole
	// cluster sees.
	GateAge      = 5 * time.Second
	ShareRefresh = 4 * time.Second

	// AcquireEvery is the poll of a node that waits for the lease.
	AcquireEvery = 5 * time.Second

	// Tick is the period of the keeper. A gap of more than TickGap between
	// two ticks, or a change of more than SuspendJump of the difference
	// between boot time and monotonic time, is a pause: the process or the
	// node stood still, so the last refresh says nothing any more.
	Tick        = time.Second
	TickGap     = 3 * time.Second
	SuspendJump = time.Second

	// HandoffBackoff is how long the nodes not named by a hand-off hold
	// back from acquiring.
	HandoffBackoff = 30 * time.Second

	// HeardWithin is how recently the agent of a node must have heard a
	// controller that called its lease valid for the node not to try.
	HeardWithin = 30 * time.Second
)

const (
	// callTimeout bounds every call to the cluster filesystem and to
	// leader.json.
	callTimeout = 2 * time.Second
	// retryAfter is the soonest a failed refresh is tried again.
	retryAfter = time.Second
	// removalWait is how long after an unlock request the waiting node
	// tries its mkdir once more. pmxcfs answers every request with EACCES,
	// also the one that has an expired lock removed, which happens a moment
	// later: the extra mkdir takes the lock then, not a whole poll later.
	removalWait = time.Second
	// removeWithin is the longest a refresh that confirmed the lock this
	// node's may have begun before the rmdir that relies on it. pmxcfs has
	// no check-and-remove, and lets any node remove any lock.
	removeWithin = time.Second
	// quorumAge is the oldest read of the members file that still answers
	// whether the cluster is quorate.
	quorumAge = 5 * time.Second
)

var (
	ErrNotHolder  = errors.New("this node does not hold the lease")
	ErrFenced     = errors.New("the lease is fenced")
	ErrNotQuorate = errors.New("the cluster is not quorate")
	ErrBehind     = errors.New("the store is behind what the agents remember")
)

// Leader is leader.json in the store.
type Leader interface {
	// Leader reads leader.json with its revision; a missing file is not
	// found.
	Leader() (store.LeaderRecord, int64, bool, error)
	// SaveLeaderIf writes leader.json while it still has revision rev, 0
	// for none, and fails with store.ErrRevision otherwise.
	SaveLeaderIf(r store.LeaderRecord, rev int64) error
}

// Guard answers whether this node may try to acquire: its agent heard no
// live controller within HeardWithin. A controller is live while it reports
// its lease valid.
type Guard func() (heardLive bool)

// Config is what a keeper needs. Every field but Lock and Log is required.
type Config struct {
	Node string
	// Incarnation is "<node>/<boot id>": a restart of the daemon in the same
	// boot keeps the epoch, a reboot draws a new one.
	Incarnation string
	// Lock is the lock directory, DefaultLock when empty.
	Lock  string
	FS    FS
	Store Leader
	// Quorum is the newest read of the members file: whether the cluster is
	// quorate, and when that read began. The keeper asks on every tick, at
	// every gate and after pmxcfs refused a refresh, and it must not block:
	// whoever reads the file bounds the read.
	Quorum func() (quorate bool, at time.Time)
	// Remembered is the epoch the agent of this node remembers, its
	// epoch.json.
	Remembered func() (agentproto.Epoch, bool)
	// Eligible says whether this node may be the controller at all: its
	// state in the registry, the schema of the store, the protocol, the
	// identity of the cluster. why says what keeps it from being one.
	Eligible func() (ok bool, why string)
	Guard    Guard
	// Standalone says that the node stands alone: the members file names no
	// cluster and the registry no other node. Such a node has nobody to give
	// the lease to: a clean stop keeps the lock, and after a reboot it takes
	// its own lock at once with a new epoch. nil is a node of a cluster.
	Standalone func() bool
	// Exclusive takes the lock that keeps a second keeper of the node out,
	// ProcessLock(DefaultProcessLock) on a node; it fails with ErrRunning
	// while another one holds it. Run holds it as long as it runs.
	Exclusive func() (release func(), err error)
	Clocks    Clocks
	// Rand draws the nonces.
	Rand io.Reader
	Log  zerolog.Logger
}

// State is what the keeper knows now.
type State struct {
	Holder bool
	Writer planner.Writer
	Record store.LeaderRecord
	// LastRefresh is the wall time of the start of the last good refresh.
	LastRefresh time.Time
	// Valid is the lease valid: refreshed less than Validity ago and no
	// pause since. ValidFor is how much longer it is.
	Valid    bool
	ValidFor time.Duration
	// Fenced says why the holder may not write now.
	Fenced string
	// Behind says why this controller observes only until pco promote.
	Behind string
	// Waiting says why a node that does not hold the lease does not have it.
	Waiting string
}
