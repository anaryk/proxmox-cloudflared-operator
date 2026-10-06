package lease

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The model of pmxcfs answers what a three-node cluster answered: the keeper
// is only as right as the model it is tested against.
func TestTheModelAnswersAsTheClusterDid(t *testing.T) {
	type answer struct {
		node, op string
		mtime    time.Duration // relative to the node's wall clock; zero is now
		want     syscall.Errno // zero for success
	}
	cases := []struct {
		name string
		// before runs on a cluster of pve1, pve2 and pve3, in that order of
		// node ids, where pve2 made the lock at t0.
		before  func(p *pmxcfs, c map[string]*clocks)
		answers []answer
		// owner is who owns the lock afterwards, empty for nobody.
		owner string
	}{
		{
			name: "the owner refreshes with now, a later and the same mtime",
			answers: []answer{
				{node: "pve2", op: "refresh"},
				{node: "pve2", op: "refresh", mtime: time.Hour},
				{node: "pve2", op: "refresh", mtime: time.Hour},
			},
			owner: "pve2",
		},
		{
			name: "the owner's older mtime is refused with EACCES",
			answers: []answer{
				{node: "pve2", op: "refresh", mtime: time.Hour},
				{node: "pve2", op: "refresh", want: syscall.EACCES},
			},
			owner: "pve2",
		},
		{
			name: "another node cannot refresh with any mtime",
			answers: []answer{
				{node: "pve1", op: "refresh", want: syscall.EACCES},
				{node: "pve1", op: "refresh", mtime: time.Hour, want: syscall.EACCES},
				{node: "pve1", op: "refresh", mtime: -time.Hour, want: syscall.EACCES},
			},
			owner: "pve2",
		},
		{
			name: "a second process of the owner's node refreshes too, its mkdir finds the lock",
			answers: []answer{
				{node: "pve2", op: "mkdir", want: syscall.EEXIST},
				{node: "pve2", op: "refresh"},
			},
			owner: "pve2",
		},
		{
			name:    "an unlock request on a fresh lock is refused",
			answers: []answer{{node: "pve3", op: "unlock", want: syscall.EACCES}},
			owner:   "pve2",
		},
		{
			name: "an unlock request on an expired lock is refused too, and the lock goes after it",
			before: func(_ *pmxcfs, c map[string]*clocks) {
				advanceAll(c, 121*time.Second)
			},
			answers: []answer{
				{node: "pve3", op: "unlock", want: syscall.EACCES},
				{node: "pve3", op: "mkdir"},
			},
			owner: "pve3",
		},
		{
			name: "an unlock request at 120 s leaves the lock",
			before: func(_ *pmxcfs, c map[string]*clocks) {
				advanceAll(c, 120*time.Second)
			},
			answers: []answer{
				{node: "pve3", op: "unlock", want: syscall.EACCES},
				{node: "pve3", op: "mkdir", want: syscall.EEXIST},
			},
			owner: "pve2",
		},
		{
			name: "the lowest node id's clock decides: a step forward there expires a fresh lock",
			before: func(_ *pmxcfs, c map[string]*clocks) {
				c["pve1"].step(time.Hour)
			},
			answers: []answer{
				{node: "pve3", op: "unlock", want: syscall.EACCES},
				{node: "pve2", op: "refresh", want: syscall.ENOENT},
				{node: "pve3", op: "mkdir"},
			},
			owner: "pve3",
		},
		{
			name: "a step forward on the asking node decides nothing",
			before: func(_ *pmxcfs, c map[string]*clocks) {
				c["pve3"].step(time.Hour)
			},
			answers: []answer{
				{node: "pve3", op: "unlock", want: syscall.EACCES},
				{node: "pve3", op: "mkdir", want: syscall.EEXIST},
			},
			owner: "pve2",
		},
		{
			name: "a step forward on the owner decides nothing",
			before: func(_ *pmxcfs, c map[string]*clocks) {
				c["pve2"].step(time.Hour)
			},
			answers: []answer{
				{node: "pve3", op: "unlock", want: syscall.EACCES},
				{node: "pve3", op: "mkdir", want: syscall.EEXIST},
				{node: "pve2", op: "refresh"},
			},
			owner: "pve2",
		},
		{
			name: "any node removes another's lock with rmdir",
			answers: []answer{
				{node: "pve3", op: "rmdir"},
				{node: "pve2", op: "refresh", want: syscall.ENOENT},
			},
		},
		{
			name: "an old owner's rmdir after a takeover removes the new holder's lock",
			before: func(_ *pmxcfs, c map[string]*clocks) {
				advanceAll(c, 121*time.Second)
			},
			answers: []answer{
				{node: "pve3", op: "unlock", want: syscall.EACCES},
				{node: "pve3", op: "mkdir"},
				{node: "pve2", op: "refresh", want: syscall.EACCES},
				{node: "pve2", op: "unlock", want: syscall.EACCES},
				{node: "pve2", op: "rmdir"},
				{node: "pve3", op: "refresh", want: syscall.ENOENT},
			},
		},
		{
			name: "without quorum every write is refused with EACCES",
			before: func(p *pmxcfs, _ map[string]*clocks) {
				p.setQuorate("pve2", false)
				p.setQuorate("pve3", false)
			},
			answers: []answer{
				{node: "pve2", op: "refresh", want: syscall.EACCES},
				{node: "pve2", op: "rmdir", want: syscall.EACCES},
				{node: "pve3", op: "unlock", want: syscall.EACCES},
				{node: "pve3", op: "mkdir", want: syscall.EACCES},
			},
			owner: "pve2",
		},
		{
			name:    "a lock that is gone",
			before:  func(p *pmxcfs, _ map[string]*clocks) { p.remove() },
			answers: []answer{{node: "pve2", op: "refresh", want: syscall.ENOENT}, {node: "pve3", op: "unlock", want: syscall.ENOENT}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPmxcfs()
			cl := map[string]*clocks{}
			for _, n := range []string{"pve1", "pve2", "pve3"} {
				cl[n] = newClocks(t0)
				p.addNode(n, cl[n])
			}
			require.NoError(t, p.fs("pve2").Mkdir(DefaultLock))
			if c.before != nil {
				c.before(p, cl)
			}
			for i, a := range c.answers {
				f := p.fs(a.node)
				var err error
				switch a.op {
				case "mkdir":
					err = f.Mkdir(DefaultLock)
				case "refresh":
					err = f.Refresh(DefaultLock, cl[a.node].Now().Add(a.mtime))
				case "unlock":
					err = f.UnlockRequest(DefaultLock)
				case "rmdir":
					err = f.Rmdir(DefaultLock)
				}
				if a.want == 0 {
					require.NoError(t, err, "answer %d", i)
				} else {
					require.ErrorIs(t, err, a.want, "answer %d", i)
				}
			}
			owner, held := p.owner()
			require.Equal(t, c.owner != "", held)
			require.Equal(t, c.owner, owner)
		})
	}
}

func advanceAll(c map[string]*clocks, d time.Duration) {
	for _, n := range c {
		n.advance(d)
	}
}
