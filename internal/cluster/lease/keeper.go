package lease

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// Keeper takes the lease when this node may have it, keeps it refreshed, and
// answers the gate. One keeper runs per node; the nonce in leader.json keeps
// any other process of the node from refreshing as it.
type Keeper struct {
	cfg Config
	log zerolog.Logger

	// op admits one operation on the lease at a time: a refresh, an
	// acquisition, a release, a promotion. It is a channel so that a gate
	// can stop waiting for it.
	op chan struct{}
	// busy is set while a call to the cluster filesystem runs, also one that
	// was given up on.
	busy atomic.Bool

	// ticker drives Run and after bounds the calls; tests replace them.
	ticker func() (<-chan time.Time, func())
	after  func(time.Duration) <-chan time.Time

	mu sync.Mutex

	// held is set while this keeper holds the process lock of the node, and
	// release gives it back. Without it the keeper does nothing.
	held    bool
	release func()

	// The newest read of the members file.
	quorate  bool
	quorumAt time.Time

	// The pause detector: the monotonic time of the last look, by a tick or
	// a gate, and the difference of boot and monotonic time then.
	looked   bool
	lookedAt time.Duration
	offset   time.Duration
	// wallOffset is the difference of wall and monotonic time at the last
	// look; stepped is set when it moved by more than SuspendJump since, until
	// the refresh it calls for.
	wallOffset time.Duration
	stepped    bool

	// The tenure. rec is leader.json as this tenure wrote or found it.
	holder     bool
	rec        store.LeaderRecord
	firstWrite bool
	behind     string
	alarm      string
	// highest is the highest generation an agent reported in the tenure.
	highest int
	// stopping is set while a release or a hand-off runs: no more writes.
	stopping bool

	// lastSet is the newest mtime this process set on the lock: pmxcfs
	// refuses an older one, also after the wall clock stepped back.
	lastSet time.Time

	// The last good refresh, on both clocks. good is false before one and
	// after a pause seen at pauseAt, until a refresh that began after it.
	good     bool
	goodMono time.Duration
	goodWall time.Time
	pause    string
	pauseAt  time.Duration

	// The refresh attempts: the start of the last one, the last synchronous
	// one and its answer, and the last failure.
	attemptAt time.Duration
	synced    bool
	syncAt    time.Duration
	syncErr   error
	failAt    time.Duration
	failErr   error
	// refused is a refresh pmxcfs refused with EACCES, begun at refusedAt;
	// it is judged by the first quorum read begun after it.
	refused   bool
	refusedAt time.Time

	// Outside a tenure. recheck is when the mkdir after an unlock request is
	// due, zero for none.
	nextTry  time.Duration
	recheck  time.Duration
	holdBack time.Duration
	waiting  string
	// closed is set by Release: the keeper acquires no more.
	closed bool
}

// New checks cfg and returns a keeper that has not started.
func New(cfg Config) (*Keeper, error) {
	if cfg.Lock == "" {
		cfg.Lock = DefaultLock
	}
	var missing []string
	for _, f := range []struct {
		name   string
		absent bool
	}{
		{"node", cfg.Node == ""}, {"incarnation", cfg.Incarnation == ""}, {"filesystem", cfg.FS == nil},
		{"store", cfg.Store == nil}, {"quorum", cfg.Quorum == nil}, {"remembered epoch", cfg.Remembered == nil},
		{"eligibility", cfg.Eligible == nil}, {"guard", cfg.Guard == nil},
		{"process lock", cfg.Exclusive == nil}, {"clocks", cfg.Clocks == nil},
		{"randomness", cfg.Rand == nil},
	} {
		if f.absent {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("lease: the configuration has no %s", strings.Join(missing, ", "))
	}
	return &Keeper{
		cfg: cfg,
		log: cfg.Log.With().Str("lease", cfg.Lock).Logger(),
		op:  make(chan struct{}, 1),
		ticker: func() (<-chan time.Time, func()) {
			t := time.NewTicker(Tick)
			return t.C, t.Stop
		},
		after: time.After,
	}, nil
}

// Run keeps the lease until ctx ends or Release ran. It takes the process
// lock of the node first and fails with ErrRunning while another keeper holds
// it. It ticks at once and then every Tick: it looks for a pause, reads the
// quorum, refreshes the lease every RefreshEvery while it holds it and tries
// to acquire it every AcquireEvery while it does not. When it returns, the
// keeper lets no write through; the lock on the cluster filesystem stays as
// it is, so that a keeper started in the same boot keeps the epoch.
func (k *Keeper) Run(ctx context.Context) error {
	if err := k.hold(); err != nil {
		return err
	}
	defer k.unhold()
	ticks, stop := k.ticker()
	defer stop()
	for {
		k.tick(ctx)
		k.mu.Lock()
		closed := k.closed
		k.mu.Unlock()
		if closed {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
		}
	}
}

// hold takes the process lock of the node.
func (k *Keeper) hold() error {
	release, err := k.cfg.Exclusive()
	if err != nil {
		return fmt.Errorf("lease: %w", err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.held, k.release = true, release
	return nil
}

// unhold gives the process lock back. A tenure ends with it, without
// touching the lock on the cluster filesystem.
func (k *Keeper) unhold() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.held {
		return
	}
	if k.holder {
		k.endTenure("the lease keeper stopped")
	}
	k.held = false
	k.release()
}

func (k *Keeper) tick(ctx context.Context) {
	quorate, at := k.cfg.Quorum()
	k.mu.Lock()
	if !k.held {
		k.mu.Unlock()
		return
	}
	k.look()
	k.noteQuorum(quorate, at)
	k.judgeRefusal()
	refresh := k.refreshDue()
	try, recheck := k.acquireDue()
	k.mu.Unlock()
	if !refresh && !try && !recheck {
		return
	}
	if err := k.takeOp(ctx); err != nil {
		return
	}
	defer k.putOp()
	k.mu.Lock()
	refresh = k.refreshDue()
	if refresh {
		k.stepped = false
	}
	try, recheck = k.acquireDue()
	if try {
		k.nextTry = k.mono() + AcquireEvery
	}
	if try || recheck {
		k.recheck = 0
	}
	k.mu.Unlock()
	switch {
	case refresh:
		_ = k.refresh(ctx, false)
	case try, recheck:
		k.acquire(ctx, recheck)
	}
}

// refreshDue says whether the holder refreshes now: every RefreshEvery, and
// at once after a step of the wall clock. On the node with the lowest node
// id, whose wall clock judges the expiry of every lock, a step forward makes
// the lock look expired until the next change.
func (k *Keeper) refreshDue() bool {
	return k.holder && (k.stepped || k.mono()-k.attemptAt >= RefreshEvery)
}

// acquireDue says whether an attempt to acquire is due, and whether only the
// mkdir after an unlock request is.
func (k *Keeper) acquireDue() (try, recheck bool) {
	if k.holder || k.closed {
		return false, false
	}
	mono := k.mono()
	try = mono >= k.nextTry
	return try, !try && k.recheck != 0 && mono >= k.recheck
}

// State is what the keeper knows now. A pause since the last tick counts: the
// heartbeat reports the lease from it.
func (k *Keeper) State() State {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.look()
	s := State{Holder: k.holder, Waiting: k.waiting}
	if !k.holder {
		return s
	}
	mono := k.mono()
	s.Writer, s.Record, s.Behind = k.rec.Writer, k.rec, k.behind
	s.LastRefresh = k.goodWall
	if k.valid(mono) {
		s.Valid, s.ValidFor = true, k.goodMono+Validity-mono
	}
	s.Fenced = k.fencedWhy(mono)
	return s
}

// FirstWrite tells the keeper that the tenure wrote: from now on an agent
// that reports a higher epoch stops the writes but does not make the
// controller behind.
func (k *Keeper) FirstWrite() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.holder {
		k.firstWrite = true
	}
}

func (k *Keeper) takeOp(ctx context.Context) error {
	select {
	case k.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (k *Keeper) putOp() { <-k.op }

func (k *Keeper) mono() time.Duration { return k.cfg.Clocks.Monotonic() }

// stamp is the start of an operation on both clocks.
type stamp struct {
	mono time.Duration
	wall time.Time
}

func (k *Keeper) stamp() stamp {
	return stamp{mono: k.cfg.Clocks.Monotonic(), wall: k.cfg.Clocks.Now()}
}

// nextMtime is the mtime of a refresh that begins at now: never older than
// one this process set before.
func (k *Keeper) nextMtime(now time.Time) time.Time {
	mtime := now.Truncate(time.Second)
	if mtime.Before(k.lastSet) {
		mtime = k.lastSet
	}
	k.lastSet = mtime
	return mtime
}

// begin starts a tenure at a claim of the lock that began at start.
func (k *Keeper) begin(rec store.LeaderRecord, behind string, start stamp) {
	k.holder, k.rec, k.behind = true, rec, behind
	k.alarm, k.firstWrite, k.highest, k.stopping = "", false, 0, false
	k.attemptAt, k.synced, k.failErr, k.refused = start.mono, false, nil, false
	k.waiting, k.recheck = "", 0
	k.goodWall = start.wall
	k.goodMono = start.mono
	k.good = start.mono >= k.pauseAt
	if k.lastSet.Before(start.wall.Truncate(time.Second)) {
		k.lastSet = start.wall.Truncate(time.Second)
	}
}

// endTenure ends the tenure without touching the lock: what ends a tenure
// is another node's, or one that has expired.
func (k *Keeper) endTenure(why string) {
	k.holder, k.good, k.stopping = false, false, false
	k.behind, k.alarm, k.firstWrite, k.highest = "", "", false, 0
	k.synced, k.failErr, k.refused = false, nil, false
	k.waiting = why
	k.nextTry = k.mono()
}

// standalone says whether the node stands alone; it may read files, so the
// caller holds no lock.
func (k *Keeper) standalone() bool {
	return k.cfg.Standalone != nil && k.cfg.Standalone()
}

// epochOf names the epoch of a writer in the logs.
func epochOf(w planner.Writer) string { return fmt.Sprintf("%d.%s", w.Generation, w.Nonce) }

// sameEpoch reports whether two writers are of one epoch, whatever their
// incarnations.
func sameEpoch(a, b planner.Writer) bool {
	return a.InstallID == b.InstallID && a.Generation == b.Generation && a.Nonce == b.Nonce
}
