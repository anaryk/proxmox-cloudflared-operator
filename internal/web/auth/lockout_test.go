package auth

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLockoutLocksAfterFiveFailuresAndDoubles(t *testing.T) {
	l := newLockout()
	now := t0
	for i := 1; i <= 4; i++ {
		require.Zero(t, l.fail("alice@pve", now), "failure %d", i)
		locked, _ := l.locked("alice@pve", now)
		require.False(t, locked, "failure %d does not lock yet", i)
	}

	require.Equal(t, time.Minute, l.fail("alice@pve", now), "the fifth failure locks for a minute")
	locked, wait := l.locked("alice@pve", now.Add(59*time.Second))
	require.True(t, locked)
	require.Equal(t, time.Second, wait)
	locked, _ = l.locked("alice@pve", now.Add(time.Minute))
	require.False(t, locked, "the minute is over")

	now = now.Add(time.Minute)
	require.Equal(t, 2*time.Minute, l.fail("alice@pve", now), "the sixth doubles it")
	for _, want := range []time.Duration{4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		now = now.Add(time.Minute)
		require.Equal(t, want, l.fail("alice@pve", now))
	}
	locked, wait = l.locked("alice@pve", now.Add(14*time.Minute))
	require.True(t, locked, "the lock tops at 15 minutes")
	require.Equal(t, time.Minute, wait)
}

func TestLockoutSuccessClearsAndAccountsAreApart(t *testing.T) {
	l := newLockout()
	for range 5 {
		l.fail("alice@pve", t0)
	}
	locked, _ := l.locked("alice@pve", t0)
	require.True(t, locked)
	locked, _ = l.locked("bob@pve", t0)
	require.False(t, locked, "another account is not affected")

	l.clear("alice@pve")
	locked, _ = l.locked("alice@pve", t0)
	require.False(t, locked, "a success clears the lock")
	require.Zero(t, l.fail("alice@pve", t0), "and the count starts again")
}

func TestLockoutKeyIgnoresCase(t *testing.T) {
	require.Equal(t, accountKey("Alice", "PVE"), accountKey("alice", "pve"))
}

func TestLockoutForgetsAnAccount15MinutesAfterItsLastFailure(t *testing.T) {
	l := newLockout()
	for range 9 {
		l.fail("alice@pve", t0)
	}
	locked, _ := l.locked("alice@pve", t0.Add(15*time.Minute-time.Second))
	require.True(t, locked)
	locked, _ = l.locked("alice@pve", t0.Add(15*time.Minute))
	require.False(t, locked)

	l.fail("bob@pve", t0.Add(15*time.Minute))
	require.Equal(t, 1, l.len(), "the next failure of any account dropped alice")
	require.Zero(t, l.fail("alice@pve", t0.Add(15*time.Minute)), "alice starts from nothing")
}

func TestLockoutIsBounded(t *testing.T) {
	l := newLockout()
	for range 5 {
		l.fail("first@pve", t0)
	}
	for i := 1; i < maxLockedAcc; i++ {
		l.fail("user"+strconv.Itoa(i)+"@pve", t0.Add(time.Second))
	}
	require.Equal(t, maxLockedAcc, l.len())
	locked, _ := l.locked("first@pve", t0.Add(2*time.Second))
	require.True(t, locked)

	l.fail("another@pve", t0.Add(2*time.Second))

	require.Equal(t, maxLockedAcc, l.len())
	locked, _ = l.locked("first@pve", t0.Add(2*time.Second))
	require.False(t, locked, "the account with the oldest last failure went first, which only loosens its lock")
	_, ok := l.byAccount["user1@pve"]
	require.True(t, ok)
}
