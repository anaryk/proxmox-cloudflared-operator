package lease

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// lockTimeout is how long pmxcfs keeps a lock nobody changes: an unlock
// request has it removed only when the node with the lowest node id saw no
// change for longer than this, by that node's wall clock. Directories right
// under lockParent are locks.
const (
	lockTimeout = 120
	lockParent  = "/etc/pve/priv/lock"
)

// pmxcfs models what the cluster filesystem does with the directories under
// priv/lock and with leader.json, as each node sees them, and as a cluster
// answered: the node that made a lock owns it and alone may set its mtime,
// never to an older one; every change is seen by every node at once and timed
// by that node's wall clock; an unlock request is answered EACCES whatever
// comes of it, and has the lock removed a moment later only when the quorate
// node with the lowest node id saw no change for more than two minutes by its
// own wall clock, whichever node asked; any node may remove any lock with
// rmdir, its owner or not; a node without quorum may read but not write.
type pmxcfs struct {
	mu      sync.Mutex
	version uint64
	dirs    map[string]bool
	locks   map[string]*lockDir
	nodes   map[string]*pveNode
	leader  store.LeaderRecord
	rev     int64
	calls   []fsCall
	// removing are the locks unlock requests have removed at the next call
	// into pmxcfs, if they are still at the version the request found.
	removing map[string]uint64
}

type lockDir struct {
	writer  string
	mtime   int64
	version uint64
	files   int
}

type pveNode struct {
	name    string
	id      int
	clocks  *clocks
	quorate bool
	seen    map[string]sighting
	fail    map[string]error
	hang    map[string]*hang
}

// sighting is the version of a lock a node saw last and its wall time then.
type sighting struct {
	version uint64
	at      int64
}

// hang makes the calls of one kind block until release is closed; entered
// receives once per call that blocks.
type hang struct {
	entered chan struct{}
	release chan struct{}
}

type fsCall struct {
	node, op string
	// at is the wall time of the node when it made the call.
	at    time.Time
	mtime int64
	err   error
}

func newPmxcfs() *pmxcfs {
	return &pmxcfs{
		dirs:     map[string]bool{"/etc/pve/priv": true, lockParent: true},
		locks:    map[string]*lockDir{},
		nodes:    map[string]*pveNode{},
		removing: map[string]uint64{},
	}
}

// addNode adds a node with the next node id: the first node added has the
// lowest.
func (p *pmxcfs) addNode(name string, c *clocks) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes[name] = &pveNode{name: name, id: len(p.nodes) + 1, clocks: c, quorate: true, seen: map[string]sighting{},
		fail: map[string]error{}, hang: map[string]*hang{}}
}

func (p *pmxcfs) fs(node string) FS        { return nodeFS{p: p, node: node} }
func (p *pmxcfs) store(node string) Leader { return nodeStore{p: p, node: node} }

func (p *pmxcfs) setQuorate(node string, quorate bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes[node].quorate = quorate
}

func (p *pmxcfs) quorate(node string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nodes[node].quorate
}

// failOn makes every call op of node fail with err; a nil err clears it.
func (p *pmxcfs) failOn(node, op string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.nodes[node].fail, op)
		return
	}
	p.nodes[node].fail[op] = err
}

// hangOn makes the calls op of node block until the hang is released.
func (p *pmxcfs) hangOn(node, op string) *hang {
	h := &hang{entered: make(chan struct{}, 16), release: make(chan struct{})}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nodes[node].hang[op] = h
	return h
}

// begin waits out a hang of the call and returns the error injected for it.
// The caller holds no lock.
func (p *pmxcfs) begin(node, op string) (injected error) {
	p.mu.Lock()
	h := p.nodes[node].hang[op]
	p.mu.Unlock()
	if h != nil {
		h.entered <- struct{}{}
		<-h.release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nodes[node].fail[op]
}

func (p *pmxcfs) record(node, op string, mtime int64, err error) error {
	p.calls = append(p.calls, fsCall{node: node, op: op, at: p.nodes[node].clocks.Now(), mtime: mtime, err: err})
	return err
}

// count is the number of calls op that node made.
func (p *pmxcfs) count(node, op string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.node == node && c.op == op {
			n++
		}
	}
	return n
}

// errs are the answers node got to its calls op, oldest first.
func (p *pmxcfs) errs(node, op string) []error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []error
	for _, c := range p.calls {
		if c.node == node && c.op == op {
			out = append(out, c.err)
		}
	}
	return out
}

// callsOf are the calls node made, oldest first.
func (p *pmxcfs) callsOf(node string) []fsCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []fsCall
	for _, c := range p.calls {
		if c.node == node {
			out = append(out, c)
		}
	}
	return out
}

// owner is the node that owns the lock.
func (p *pmxcfs) owner() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settle()
	l, ok := p.locks[DefaultLock]
	if !ok {
		return "", false
	}
	return l.writer, true
}

// judge is the node that decides unlock requests: the quorate node with the
// lowest node id. The caller holds mu.
func (p *pmxcfs) judge() *pveNode {
	var j *pveNode
	for _, n := range p.nodes {
		if n.quorate && (j == nil || n.id < j.id) {
			j = n
		}
	}
	return j
}

// expired reports whether an unlock request would have the lock removed now.
// The caller holds mu.
func (p *pmxcfs) expired(path string, l *lockDir) bool {
	j := p.judge()
	if j == nil {
		return false
	}
	s, ok := j.seen[path]
	now := j.clocks.Now().Unix()
	return ok && s.version == l.version && now > s.at && now-s.at > lockTimeout
}

// settle removes the locks unlock requests asked to remove, unless they
// changed since. The caller holds mu.
func (p *pmxcfs) settle() {
	for path, version := range p.removing {
		delete(p.removing, path)
		if l, ok := p.locks[path]; ok && l.version == version && l.files == 0 {
			delete(p.locks, path)
			p.version++
		}
	}
}

// lastSeen is the wall time of node at which it saw the last change of the
// lock.
func (p *pmxcfs) lastSeen(node, path string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Unix(p.nodes[node].seen[path].at, 0)
}

// changed bumps the version of a lock and has every node see it.
func (p *pmxcfs) changed(path string, l *lockDir) {
	p.version++
	l.version = p.version
	for _, n := range p.nodes {
		n.seen[path] = sighting{version: l.version, at: n.clocks.Now().Unix()}
	}
}

// putFile puts a file into a lock directory, which pmxcfs then never lets
// expire.
func (p *pmxcfs) putFile(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.locks[path].files++
}

// takeOver has pve2 own the lock, as if it had removed it after expiry and
// made it again without writing leader.json yet.
func (p *pmxcfs) takeOver() {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := &lockDir{writer: "pve2", mtime: p.nodes["pve2"].clocks.Now().Unix()}
	p.locks[DefaultLock] = l
	p.changed(DefaultLock, l)
}

// remove takes the lock away, as an unlock request of another node does.
func (p *pmxcfs) remove() {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.locks, DefaultLock)
	p.version++
}

func (p *pmxcfs) removeDir(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.dirs, path)
}

func (p *pmxcfs) hasDir(path string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dirs[path]
}

func (p *pmxcfs) leaderRecord() (store.LeaderRecord, int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leader, p.rev
}

// writeLeader stores leader.json as another process would.
func (p *pmxcfs) writeLeader(r store.LeaderRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.leader = r
	p.rev++
}

func errno(op, path string, e syscall.Errno) error {
	return &fs.PathError{Op: op, Path: path, Err: e}
}

type nodeFS struct {
	p    *pmxcfs
	node string
}

func (f nodeFS) Mkdir(path string) error {
	injected := f.p.begin(f.node, "mkdir")
	p := f.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settle()
	n := p.nodes[f.node]
	_, isLock := p.locks[path]
	switch {
	case injected != nil:
		return p.record(f.node, "mkdir", 0, injected)
	case !n.quorate:
		return p.record(f.node, "mkdir", 0, errno("mkdir", path, syscall.EACCES))
	case !p.dirs[filepath.Dir(path)]:
		return p.record(f.node, "mkdir", 0, errno("mkdir", path, syscall.ENOENT))
	case isLock, p.dirs[path]:
		return p.record(f.node, "mkdir", 0, errno("mkdir", path, syscall.EEXIST))
	case filepath.Dir(path) != lockParent:
		p.dirs[path] = true
		return p.record(f.node, "mkdir", 0, nil)
	}
	l := &lockDir{writer: f.node, mtime: n.clocks.Now().Unix()}
	p.locks[path] = l
	p.changed(path, l)
	return p.record(f.node, "mkdir", l.mtime, nil)
}

func (f nodeFS) Refresh(path string, mtime time.Time) error {
	injected := f.p.begin(f.node, "refresh")
	p := f.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settle()
	n := p.nodes[f.node]
	sec := mtime.Unix()
	l, ok := p.locks[path]
	switch {
	case injected != nil:
		return p.record(f.node, "refresh", sec, injected)
	case !n.quorate:
		return p.record(f.node, "refresh", sec, errno("utime", path, syscall.EACCES))
	case !ok:
		return p.record(f.node, "refresh", sec, errno("utime", path, syscall.ENOENT))
	case sec == 0:
		panic("a refresh with mtime 0 is an unlock request")
	case sec < l.mtime, l.writer != f.node:
		return p.record(f.node, "refresh", sec, errno("utime", path, syscall.EACCES))
	}
	l.mtime = sec
	p.changed(path, l)
	return p.record(f.node, "refresh", sec, nil)
}

func (f nodeFS) UnlockRequest(path string) error {
	injected := f.p.begin(f.node, "unlock")
	p := f.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settle()
	n := p.nodes[f.node]
	l, ok := p.locks[path]
	switch {
	case injected != nil:
		return p.record(f.node, "unlock", 0, injected)
	case !n.quorate:
		return p.record(f.node, "unlock", 0, errno("utime", path, syscall.EACCES))
	case !ok:
		return p.record(f.node, "unlock", 0, errno("utime", path, syscall.ENOENT))
	}
	if p.expired(path, l) {
		p.removing[path] = l.version
	}
	return p.record(f.node, "unlock", 0, errno("utime", path, syscall.EACCES))
}

func (f nodeFS) Rmdir(path string) error {
	injected := f.p.begin(f.node, "rmdir")
	p := f.p
	p.mu.Lock()
	defer p.mu.Unlock()
	p.settle()
	n := p.nodes[f.node]
	l, ok := p.locks[path]
	switch {
	case injected != nil:
		return p.record(f.node, "rmdir", 0, injected)
	case !n.quorate:
		return p.record(f.node, "rmdir", 0, errno("rmdir", path, syscall.EACCES))
	case !ok:
		return p.record(f.node, "rmdir", 0, errno("rmdir", path, syscall.ENOENT))
	case l.files > 0:
		return p.record(f.node, "rmdir", 0, errno("rmdir", path, syscall.ENOTEMPTY))
	}
	delete(p.locks, path)
	p.version++
	return p.record(f.node, "rmdir", 0, nil)
}

// nodeStore is leader.json as node reads and writes it: readable without
// quorum, written only with it.
type nodeStore struct {
	p    *pmxcfs
	node string
}

func (s nodeStore) Leader() (store.LeaderRecord, int64, bool, error) {
	if err := s.p.begin(s.node, "leader"); err != nil {
		return store.LeaderRecord{}, 0, false, err
	}
	s.p.mu.Lock()
	defer s.p.mu.Unlock()
	if s.p.rev == 0 {
		return store.LeaderRecord{}, 0, false, nil
	}
	return s.p.leader, s.p.rev, true, nil
}

func (s nodeStore) SaveLeaderIf(r store.LeaderRecord, rev int64) error {
	injected := s.p.begin(s.node, "save")
	p := s.p
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case injected != nil:
		return p.record(s.node, "save", 0, injected)
	case !p.nodes[s.node].quorate:
		return p.record(s.node, "save", 0, errno("rename", "leader.json", syscall.EACCES))
	case rev != p.rev:
		return p.record(s.node, "save", 0, store.ErrRevision)
	}
	if err := r.Validate(); err != nil {
		return p.record(s.node, "save", 0, err)
	}
	if r.Handoff != nil && r.Handoff.To == "" {
		return p.record(s.node, "save", 0, errors.New("leader: the hand-off names no node"))
	}
	p.leader = r
	p.rev++
	return p.record(s.node, "save", 0, nil)
}

// clocks are the clocks of one node: wall, monotonic and boot time, moved by
// the test.
type clocks struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
	boot time.Duration
}

func newClocks(wall time.Time) *clocks {
	return &clocks{wall: wall, mono: time.Hour, boot: time.Hour}
}

func (c *clocks) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *clocks) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *clocks) Boottime() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.boot
}

// advance moves every clock by d.
func (c *clocks) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.mono += d
	c.boot += d
}

// step moves the wall clock alone, as an administrator or NTP does.
func (c *clocks) step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
}

// suspend is a suspend of the node for d: the monotonic clock stops.
func (c *clocks) suspend(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.boot += d
}
