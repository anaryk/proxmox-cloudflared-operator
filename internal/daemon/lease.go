package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/lease"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/cluster/members"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	// quorumEvery is how often the members file is read for the keeper, and
	// quorumTimeout bounds a read: pmxcfs can hang.
	quorumEvery   = 2 * time.Second
	quorumTimeout = 2 * time.Second
	// firstTryWait is how long the start waits for the keeper's first try
	// for the lease before the engine runs, so that a node that takes it at
	// once does not begin with a cycle that holds.
	firstTryWait = 10 * time.Second
	// releaseWait bounds the release of the lease as the daemon stops.
	releaseWait = 10 * time.Second
	// eligibleSchemaEvery is how often the keeper reads the schema of the
	// store again for its eligibility: a walk over every object.
	eligibleSchemaEvery = time.Minute
)

// LeaseDeps are the parts of the lease on the host that a test replaces.
// The zero value is what the daemon uses on a node.
type LeaseDeps struct {
	// FS is the lock on the cluster filesystem; default lease.SystemFS.
	// Clocks are the clocks it is judged by; default lease.SystemClocks.
	// Rand draws the nonces; default crypto/rand.
	FS     lease.FS
	Clocks lease.Clocks
	Rand   io.Reader
	// BootID is the id of this boot of the node, which with the node's name
	// is its incarnation; default /proc/sys/kernel/random/boot_id.
	BootID func() (string, error)
	// PVEDir is the cluster filesystem, where the members file, the CA of
	// the cluster and the lock are; default /etc/pve.
	PVEDir string
	// ProcessLock is the file that keeps a second keeper of the node out;
	// default lease.DefaultProcessLock.
	ProcessLock string
	// FirstTry is how long the start waits for the keeper's first try;
	// default 10 s.
	FirstTry time.Duration
}

func (d LeaseDeps) withDefaults() LeaseDeps {
	if d.FS == nil {
		d.FS = lease.SystemFS()
	}
	if d.Clocks == nil {
		d.Clocks = lease.SystemClocks()
	}
	if d.Rand == nil {
		d.Rand = rand.Reader
	}
	if d.BootID == nil {
		d.BootID = readBootID
	}
	if d.PVEDir == "" {
		d.PVEDir = "/etc/pve"
	}
	if d.ProcessLock == "" {
		d.ProcessLock = lease.DefaultProcessLock
	}
	if d.FirstTry <= 0 {
		d.FirstTry = firstTryWait
	}
	return d
}

// readBootID reads the id of this boot of the kernel.
func readBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("reading the boot id: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", errors.New("the boot id is empty")
	}
	return id, nil
}

// hostLease is the lease of the host profile, as the daemon runs it: the
// keeper, the reads of the members file it and the engine go by, and the
// identity of the cluster.
type hostLease struct {
	d      LeaseDeps
	keeper *lease.Keeper
	quorum *members.Quorum
	// alone says whether the node stands alone.
	alone func() bool
	log   zerolog.Logger
	// guard is the engine whose agent says whether a live controller is
	// heard; set before the keeper runs.
	guard atomic.Pointer[engine.Engine]
	// reading is set while a read of the members file runs, which a read
	// that hangs leaves set: no second read piles up behind it.
	reading atomic.Bool

	stop func()
	done chan struct{}
}

// newHostLease makes the keeper of node for the store, which it does not
// start yet.
func newHostLease(node string, st *store.Store, paths store.Paths, d LeaseDeps, log zerolog.Logger) (*hostLease, error) {
	d = d.withDefaults()
	boot, err := d.BootID()
	if err != nil {
		return nil, err
	}
	h := &hostLease{
		d:      d,
		quorum: members.NewQuorum(filepath.Join(d.PVEDir, filepath.Base(members.MembersFile)), d.Clocks.Now),
		log:    log,
	}
	h.alone = h.standalone(st, node)
	k, err := lease.New(lease.Config{
		Node:        node,
		Incarnation: node + "/" + boot,
		Lock:        filepath.Join(d.PVEDir, "priv", "lock", filepath.Base(lease.DefaultLock)),
		FS:          d.FS,
		Store:       leaderStore{st},
		Quorum:      h.quorate,
		Remembered:  remembered(st),
		Eligible:    eligibility(st, node, paths, d.Clocks.Now),
		Guard:       h.heardLive,
		Standalone:  h.alone,
		Exclusive:   lease.ProcessLock(d.ProcessLock),
		Clocks:      d.Clocks,
		Rand:        d.Rand,
		Log:         log,
	})
	if err != nil {
		return nil, err
	}
	h.keeper = k
	return h, nil
}

// leaderStore is the store as the keeper reads and writes leader.json.
type leaderStore struct{ st *store.Store }

func (s leaderStore) Leader() (store.LeaderRecord, int64, bool, error) { return s.st.Leader() }

func (s leaderStore) SaveLeaderIf(r store.LeaderRecord, rev int64) error {
	_, err := s.st.SaveLeaderIf(r, rev)
	return err
}

// remembered is the epoch the agent of this node remembers.
func remembered(st *store.Store) func() (agentproto.Epoch, bool) {
	return func() (agentproto.Epoch, bool) {
		ep, found, err := st.AgentEpoch()
		if err != nil || !found {
			return agentproto.Epoch{}, false
		}
		return agentproto.Epoch(ep), true
	}
}

// quorate is the last read of the members file: quorate, and when the read
// began.
func (h *hostLease) quorate() (bool, time.Time) {
	m, at := h.quorum.Last()
	return m.Quorate, at
}

// Cluster is the identity of the cluster this node is in: the fingerprint of
// its CA, and the name the members file gives it.
func (h *hostLease) Cluster() (store.ClusterIdentity, error) {
	fp, err := members.CAFingerprint(filepath.Join(h.d.PVEDir, filepath.Base(members.RootCAFile)))
	if err != nil {
		return store.ClusterIdentity{}, err
	}
	m, _ := h.quorum.Last()
	return store.ClusterIdentity{CASHA256: fp, Name: m.Cluster}, nil
}

// standalone says whether this node stands alone: the last read of the
// members file names no cluster, and the registry no node but this one.
func (h *hostLease) standalone(st *store.Store, node string) func() bool {
	return func() bool {
		m, at := h.quorum.Last()
		if at.IsZero() || !m.Standalone {
			return false
		}
		nodes, err := st.Nodes()
		return err == nil && len(nodes) == 1 && nodes[0].Name == node
	}
}

func (h *hostLease) heardLive() bool {
	eng := h.guard.Load()
	return eng != nil && eng.HeardLive(h.d.Clocks.Now())
}

// readQuorum reads the members file once, unless the last read still runs.
func (h *hostLease) readQuorum() {
	if !h.reading.CompareAndSwap(false, true) {
		return
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.quorum.Read()
		h.reading.Store(false)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			h.log.Debug().Err(err).Msg("reading the members file failed")
		}
	case <-time.After(quorumTimeout):
		h.log.Warn().Dur("timeout", quorumTimeout).Msg("the members file did not answer in time")
	}
}

// start reads the members file once and then every quorumEvery, and runs
// the keeper, until stop. The engine must be set as the guard first.
func (h *hostLease) start(eng *engine.Engine) {
	h.guard.Store(eng)
	ctx, cancel := context.WithCancel(context.Background())
	h.stop, h.done = cancel, make(chan struct{})
	h.readQuorum()
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(quorumEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				h.readQuorum()
			}
		}
	})
	wg.Go(func() {
		if err := h.keeper.Run(ctx); err != nil {
			h.log.Error().Err(err).Msg("the lease keeper stopped")
		}
	})
	go func() {
		wg.Wait()
		close(h.done)
	}()
}

// awaitFirstTry waits until the keeper holds the lease or says why not, for
// at most the first try's wait.
func (h *hostLease) awaitFirstTry(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, h.d.FirstTry)
	defer cancel()
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		if st := h.keeper.State(); st.Holder || st.Waiting != "" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// regate is the gate the Cloudflare clients ask again before a write that
// the rate limit held for a while.
func regate(h *hostLease) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := h.keeper.Gate(ctx)
		return err
	}
}

// end releases the lease, when the keeper holds it, and stops the keeper:
// the engine has stopped by then, so that nothing is written after.
func (h *hostLease) end(eng *engine.Engine) {
	if h.stop == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), releaseWait)
	defer cancel()
	held, alone := h.keeper.State().Holder, h.alone()
	if err := h.keeper.Release(ctx); err != nil {
		h.log.Warn().Err(err).Msg("releasing the lease failed; it expires by itself")
	} else if held && eng != nil && !eng.Progress().DueAt.IsZero() {
		// The engine ran: its events say that the lease went.
		msg := "released as the daemon stopped"
		if alone {
			msg = "kept as the daemon stopped: this node stands alone, and its next start keeps the epoch"
		}
		eng.NoteLease(msg)
	}
	h.stop()
	<-h.done
}

// holdingClients makes the Cloudflare clients only while this node holds a
// valid lease: the credentials are taken into memory by the controller
// alone, and a holder that is fenced takes none until it resumes.
func holdingClients(newClient engine.ClientFactory, k interface{ State() lease.State }) engine.ClientFactory {
	return func(c store.Credential) (cfapi.API, error) {
		if st := k.State(); !st.Holder || !st.Valid {
			return nil, errors.New("this node does not hold a valid lease; only the controller uses the Cloudflare credentials")
		}
		return newClient(c)
	}
}

// eligibility says whether this node may be the controller: its entry in
// the registry exists and is not draining, removed or contested, and the
// admin did not take it out; and the store holds no object of a schema this
// build does not read. A store made in another cluster does not keep the
// node from the lease: the engine writes nothing under it until pco promote,
// which needs the lease to read Cloudflare. The schema is read again at most
// every eligibleSchemaEvery.
func eligibility(st *store.Store, node string, paths store.Paths, now func() time.Time) func() (bool, string) {
	var schemaAt time.Time
	var schemaWhy string
	return func() (bool, string) {
		if why := registryWhy(st, node); why != "" {
			return false, why
		}
		if t := now(); schemaAt.IsZero() || t.Sub(schemaAt) >= eligibleSchemaEvery {
			schemaAt, schemaWhy = t, schemaReason(paths)
		}
		if schemaWhy != "" {
			return false, schemaWhy
		}
		return true, ""
	}
}

func registryWhy(st *store.Store, node string) string {
	nodes, err := st.Nodes()
	if err != nil {
		return fmt.Sprintf("the node registry cannot be read: %v", err)
	}
	for _, n := range nodes {
		if n.Name != node {
			continue
		}
		switch {
		case n.State == store.NodeDraining || n.State == store.NodeRemoved || n.State == store.NodeContested:
			return fmt.Sprintf("node %s is %s", node, n.State)
		case n.ControllerEligible != nil && !*n.ControllerEligible:
			return fmt.Sprintf("node %s was set not to be the controller", node)
		}
		return ""
	}
	return fmt.Sprintf("node %s is not registered", node)
}

func schemaReason(paths store.Paths) string {
	v, at, err := store.NewestSchema(paths)
	switch {
	case err != nil:
		return fmt.Sprintf("the schema of the store cannot be read: %v", err)
	case v > store.SchemaVersion():
		return fmt.Sprintf("%s has schema %d, and this build reads up to %d", at, v, store.SchemaVersion())
	}
	return ""
}
