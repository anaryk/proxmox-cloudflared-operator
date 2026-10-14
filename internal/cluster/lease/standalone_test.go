package lease

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
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

// Whether the node stands alone is asked at every decision, never kept: a
// node that joined a cluster while it held the lease gives it back on a clean
// stop, and after a reboot waits for its lock to expire as a node of a
// cluster does.
func TestANodeThatJoinsAClusterWhileHoldingFollowsTheClusterRules(t *testing.T) {
	l, m := standaloneLab(t)
	l.alone.Store(false)

	require.NoError(t, m.k.Release(l.ctx))
	m.exit()
	_, kept := l.cfs.owner()
	require.False(t, kept, "given back")
	require.False(t, l.leader().ReleasedAt.IsZero())

	l2, m2 := standaloneLab(t)
	m2.exit()
	l2.alone.Store(false)
	booted := l2.add("pve1", "pve1/boot-2")
	l2.run(10 * time.Second)
	require.False(t, booted.holds(), "its own lock of another boot waits for the expiry")
}

// The rule of a node that stands alone takes no gate away: without quorum it
// does not take its lock after a reboot, and once it holds it the gate asks
// what it asks of any holder.
func TestAStandaloneNodeKeepsEveryGate(t *testing.T) {
	l, m := standaloneLab(t)
	m.exit()
	l.cfs.setQuorate("pve1", false)

	booted := l.add("pve1", "pve1/boot-2")
	l.run(5 * time.Second)
	require.False(t, booted.holds())

	l.cfs.setQuorate("pve1", true)
	l.run(6 * time.Second)
	require.True(t, booted.holds())
	require.NoError(t, booted.gate())
	booted.clocks.suspend(2 * time.Second)
	l.cfs.setQuorate("pve1", false)
	require.Error(t, booted.gate(), "a pause and no quorum fence it as any holder")
}

// After a reboot the epoch a standalone node draws is judged as any draw: an
// agent that remembers more than leader.json makes it behind.
func TestAStandaloneNodeAfterARebootIsBehindAsAnyNode(t *testing.T) {
	l, m := standaloneLab(t)
	ahead := m.epoch()
	m.exit()
	booted := l.build("pve1", "pve1/boot-2", l.processLock("pve1"))
	booted.remember(agentproto.Epoch{InstallID: installID, Generation: ahead.Generation + 5, Nonce: "later"})
	require.NoError(t, booted.k.hold())

	l.run(time.Second)

	require.True(t, booted.holds())
	require.NotEmpty(t, booted.k.State().Behind)
	require.ErrorIs(t, booted.gate(), ErrBehind)
}
