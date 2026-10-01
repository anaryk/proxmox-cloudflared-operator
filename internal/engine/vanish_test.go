package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// publishedMany is an engine that serves n guests in enforce mode.
func publishedMany(t *testing.T, n int) *env {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	e.inv.set(snapshot(many(n)...))
	st := e.cycle()
	require.Empty(t, st.Problems)
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Len(t, claims, n)
	return e
}

func claimEventsAfter(e *env, t time.Time) []Event {
	var out []Event
	for _, ev := range e.eng.Events(t) {
		if ev.Kind == "claim" {
			out = append(out, ev)
		}
	}
	return out
}

// Reproduced: Proxmox answers with success and lists nothing.
func TestAListingWithoutAnyGuestHolds(t *testing.T) {
	e := publishedMany(t, 3)
	before := freeze(e)
	e.inv.set(snapshot())

	for range 3 {
		e.clock.advance(61 * time.Second)
		st := e.cycle()

		require.True(t, st.Complete)
		require.Contains(t, st.Problems, "Proxmox lists no guest at all, but 3 guests hold a hostname (qemu/101, qemu/102, qemu/103); "+
			"nothing is changed: check the privileges of the Proxmox API token, or run pco apply --confirm-deletes if they were removed on purpose")
		before.requireUnchanged(t, e, st)
	}
}

func TestOneDeletedGuestOutOfManyProceeds(t *testing.T) {
	e := publishedMany(t, 10)

	e.inv.set(snapshot(many(10)[1:]...))
	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Empty(t, st.Problems)
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.NotNil(t, claims["g101.example.com"].MissingSince, "its claim waits out the grace")
	require.NotContains(t, e.rules(), hostRule("g101.example.com"))
}

func TestSixOfTenGuestsVanishing(t *testing.T) {
	e := publishedMany(t, 10)
	before := freeze(e)
	e.inv.set(snapshot(many(10)[6:]...))

	e.clock.advance(20 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems, "6 of 10 guests that hold a hostname are no longer listed by Proxmox "+
		"(qemu/101, qemu/102, qemu/103, qemu/104, qemu/105 and 1 more); "+
		"nothing is changed until they are listed again, or run pco apply --confirm-deletes if they were removed on purpose")
	before.requireUnchanged(t, e, st)

	e.apply(true)
	e.clock.advance(10 * time.Second)
	st = e.cycle()

	require.False(t, hasProblem(st, "no longer listed by Proxmox"))
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.NotNil(t, claims["g101.example.com"].MissingSince)

	e.clock.advance(61 * time.Second)
	e.cycle()
	claims, err = e.store.Claims()
	require.NoError(t, err)
	require.Len(t, claims, 4, "released after the grace")

	// The confirmation was for those six: the other four count anew.
	e.inv.set(snapshot())
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.True(t, hasProblem(st, "Proxmox lists no guest at all, but 4 guests hold a hostname"))
	claims, err = e.store.Claims()
	require.NoError(t, err)
	require.Len(t, claims, 4)
	for _, c := range claims {
		require.Nil(t, c.MissingSince)
	}
}

func TestGuestsReturningBeforeAConfirmationLoseNothing(t *testing.T) {
	e := publishedMany(t, 10)
	stored, err := e.store.Claims()
	require.NoError(t, err)

	e.inv.set(snapshot(many(10)[6:]...))
	for range 2 {
		e.clock.advance(61 * time.Second)
		e.cycle()
	}
	e.inv.set(snapshot(many(10)...))
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Empty(t, st.Problems)
	claims, err := e.store.Claims()
	require.NoError(t, err)
	require.Equal(t, stored, claims, "no claim moved")
	require.Empty(t, claimEventsAfter(e, t0))
}
