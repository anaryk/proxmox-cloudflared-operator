package lease

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// standaloneLab is a node that stands alone, holding the lease.
func standaloneLab(t *testing.T) (*lab, *member) {
	t.Helper()
	l := newLab(t)
	l.alone.Store(true)
	m := l.add("pve1", "pve1/boot-1")
	l.run(time.Second)
	require.True(t, m.holds())
	return l, m
}

// A node that stands alone has nobody to give the lease to: a clean stop
// leaves the lock and leader.json as they are, so that the next start in the
// same boot keeps the epoch, as one after a crash does.
func TestAStandaloneNodeKeepsTheLockAcrossACleanStop(t *testing.T) {
	l, m := standaloneLab(t)
	epoch := m.epoch()

	require.NoError(t, m.k.Release(l.ctx))
	m.exit()

	_, kept := l.cfs.owner()
	require.True(t, kept, "the lock stays")
	require.Zero(t, l.cfs.count("pve1", "rmdir"))
	require.True(t, l.leader().ReleasedAt.IsZero(), "no release is written")
	require.False(t, m.holds())
	require.ErrorIs(t, m.gate(), ErrNotHolder, "nothing is written after the stop")

	again := l.add("pve1", "pve1/boot-1")
	l.run(time.Second)
	require.True(t, again.holds())
	require.Equal(t, epoch, again.epoch(), "the same epoch")
	require.Zero(t, l.cfs.count("pve1", "unlock"))
}

// After a reboot or a power loss, a node that stands alone finds its own
// lock: it takes it at once and draws one new epoch for the new boot, rather
// than waiting two minutes for it to expire.
func TestAStandaloneNodeTakesItsOwnLockAtOnceAfterAReboot(t *testing.T) {
	l, m := standaloneLab(t)
	before := m.epoch()
	m.exit()

	booted := l.add("pve1", "pve1/boot-2")
	l.run(time.Second)

	require.True(t, booted.holds())
	require.Equal(t, before.Generation+1, booted.epoch().Generation)
	require.NotEqual(t, before.Nonce, booted.epoch().Nonce)
	require.Equal(t, "pve1/boot-2", l.leader().Incarnation)
	require.Zero(t, l.cfs.count("pve1", "unlock"))
	require.NoError(t, booted.gate())

	drawn := booted.epoch()
	booted.exit()
	same := l.add("pve1", "pve1/boot-2")
	l.run(time.Second)
	require.Equal(t, drawn, same.epoch(), "one new epoch a boot")
}

// A node of a cluster gives the lease back on a clean stop, and waits for its
// own lock to expire after a reboot: another node may hold it by then.
func TestANodeOfAClusterReleasesAndWaitsAfterAReboot(t *testing.T) {
	l := newLab(t, "pve1")
	m := l.m("pve1")
	l.run(time.Second)
	m.exit()

	booted := l.add("pve1", "pve1/boot-2")
	l.run(10 * time.Second)

	require.False(t, booted.holds())
	require.Positive(t, l.cfs.count("pve1", "unlock"))
}
