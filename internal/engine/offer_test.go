package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi/cffake"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

const offerChanged = "refused: what waits for a confirmation changed since it was shown; look again and repeat"

// twoHeld is an engine in enforce mode whose last state shows the mass delete
// guard holding the removal of a.example.com and b.example.com, while the
// records of four more names wait out their grace. Ten other guests hold a
// hostname each.
func twoHeld(t *testing.T) (*env, State) {
	t.Helper()
	e := newEnv(t)
	e.enforce()
	six := guest(201, "web", "a.example.com b.example.com c.example.com d.example.com e.example.com f.example.com -> :8080")
	e.inv.set(snapshot(append(many(10), six)...))
	e.cycle()
	require.Len(t, e.records(), 16)

	// a and b go first; c to f half a minute later, so that a and b fall due
	// while the others are still in their grace.
	e.inv.set(snapshot(append(many(10), guest(201, "web", "c.example.com d.example.com e.example.com f.example.com -> :8080"))...))
	e.clock.advance(20 * time.Second)
	e.cycle()
	e.inv.set(snapshot(append(many(10), untagged(six))...))
	var st State
	for _, step := range []time.Duration{30, 31, 30, 31} {
		e.clock.advance(step * time.Second)
		st = e.cycle()
	}
	held := 0
	for _, a := range st.Actions {
		if strings.HasPrefix(a.Held, "mass delete guard") {
			held++
		}
	}
	require.Equal(t, 2, held, "%v", actionKinds(st))
	// A confirmation lets the four through too, when their grace ends: they
	// are among what it accepts.
	require.Equal(t, []Waiting{{
		Kind:   "dns-removals",
		Detail: "mass delete guard: 6 of 16 records are being removed; confirm to proceed",
		Items:  []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com"},
	}}, st.Waiting)
	require.NotEmpty(t, st.Offer)
	return e, st
}

// A confirmation of the guard lets through every removal it counted, also
// those still in their grace: they are shown with the ones it holds.
func TestAConfirmationOfTheGuardCoversTheRemovalsInTheirGrace(t *testing.T) {
	e, shown := twoHeld(t)

	res, err := e.eng.Apply(t.Context(), true, shown.Offer)
	require.NoError(t, err)
	require.Equal(t, shown.Waiting, res.Accepted)
	e.clock.advance(10 * time.Second)
	e.cycle()
	require.NotContains(t, e.recordNames(), "a.example.com")
	require.Contains(t, e.recordNames(), "c.example.com", "still in its grace")

	e.clock.advance(30 * time.Second)
	st := e.cycle()

	require.Empty(t, st.Waiting, "the guard does not ask again")
	require.Len(t, e.records(), 10, "the four went after their grace")
}

// vanishSix is the snapshot in which six of the ten guests of many(10) are
// gone, with guest 201 still listed.
func vanishSix() []model.Guest {
	return append(many(10)[6:], untagged(guest(201, "web")))
}

// The scenario of the reviews: the admin reads a state with two deletes held
// by the guard; before the confirmation arrives, a cycle holds on guests that
// vanished. The confirmation of the deletes must not mark the guests gone.
func TestAConfirmationOfAnOlderStateIsRefused(t *testing.T) {
	e, shown := twoHeld(t)
	e.inv.set(snapshot(vanishSix()...))
	e.clock.advance(10 * time.Second)
	held := e.cycle()
	require.True(t, hasProblem(held, "6 of 10 guests that hold a hostname are no longer listed by Proxmox"))
	require.NotEqual(t, shown.Offer, held.Offer)
	before, since := e.files(), e.clock.now()

	_, err := e.eng.Apply(t.Context(), true, shown.Offer)

	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, offerChanged)
	require.Empty(t, e.eng.gone, "no guest is marked gone")
	require.Nil(t, e.eng.confirm, "and no delete confirmed")
	require.Equal(t, before, e.files())
	require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
	e.clock.advance(10 * time.Second)
	st := e.cycle()
	require.True(t, hasProblem(st, "6 of 10 guests that hold a hostname are no longer listed by Proxmox"))
	require.Len(t, e.records(), 16)
}

// noticing tells when a call starts to wait on it.
type noticing struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (n *noticing) Done() <-chan struct{} {
	n.once.Do(func() { close(n.waiting) })
	return n.Context.Done()
}

// The same, with the confirmation sent while the cycle that changes what
// waits is running: it is compared with what that cycle published.
func TestAConfirmationWaitingForACycleIsComparedWithWhatThatCycleShows(t *testing.T) {
	e, shown := twoHeld(t)
	e.inv.set(snapshot(vanishSix()...))
	e.clock.advance(10 * time.Second)
	inCycle, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.inv.hook(func() { once.Do(func() { close(inCycle); <-release }) })
	done := make(chan State)
	go func() { done <- e.eng.Cycle(t.Context()) }()
	<-inCycle

	ctx := &noticing{Context: t.Context(), waiting: make(chan struct{})}
	applied := make(chan error)
	go func() {
		_, err := e.eng.Apply(ctx, true, shown.Offer)
		applied <- err
	}()
	<-ctx.waiting
	close(release)
	held := <-done
	err := <-applied

	require.True(t, hasProblem(held, "6 of 10 guests that hold a hostname are no longer listed by Proxmox"))
	require.ErrorIs(t, err, ErrRefused)
	require.Empty(t, e.eng.gone, "no guest is marked gone")
	require.Nil(t, e.eng.confirm)
}

// Pin 1: a cycle that offers nothing ends the offer of the one before.
func TestAnOfferReplacedByNoneIsRefused(t *testing.T) {
	e, view, both := twoZones(t)
	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	shown := e.cycle()
	require.True(t, hasProblem(shown, "zone example.net is no longer listed by credential cred1"))
	require.NotEmpty(t, shown.Offer)
	web := guest(101, "web-1", "www.example.com www.example.net -> :8080")

	e.inv.set(incomplete("cluster status: no quorum", web))
	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Empty(t, st.Waiting)
	require.Empty(t, st.Offer)

	_, err := e.eng.Apply(t.Context(), true, shown.Offer)

	require.ErrorIs(t, err, ErrRefused)
	e.inv.set(snapshot(web))
	e.clock.advance(20 * time.Second)
	st = e.cycle()
	require.True(t, hasProblem(st, "zone example.net is no longer listed by credential cred1"), "the zone stays stale")
	require.Equal(t, both, e.rules())
}

// Pin 2: vanished guests are offered only while the vanish guard holds.
func TestVanishedGuestsAreOfferedOnlyWhileTheGuardHolds(t *testing.T) {
	t.Run("five of ten: no hold", func(t *testing.T) {
		e := publishedMany(t, 10)
		e.inv.set(snapshot(many(10)[5:]...))
		e.clock.advance(20 * time.Second)

		st := e.cycle()

		require.False(t, hasProblem(st, "no longer listed by Proxmox"))
		require.Empty(t, st.Waiting)
		require.Empty(t, st.Offer)
	})
	t.Run("six of ten: a hold", func(t *testing.T) {
		e := publishedMany(t, 10)
		e.inv.set(snapshot(many(10)[6:]...))
		e.clock.advance(20 * time.Second)

		st := e.cycle()

		require.Equal(t, []Waiting{{
			Kind: "vanished-guests",
			Detail: "6 guests that hold a hostname are no longer listed by Proxmox; " +
				"a confirmation takes them as removed, and their hostnames are released after the grace period",
			Items: []string{"qemu/101 vm-101", "qemu/102 vm-102", "qemu/103 vm-103", "qemu/104 vm-104", "qemu/105 vm-105", "qemu/106 vm-106"},
		}}, st.Waiting)
		require.NotEmpty(t, st.Offer)
	})
}

// Pin 6: an offer is accepted once; a cycle that offers the same again
// makes it good again.
func TestAnOfferIsAcceptedOnce(t *testing.T) {
	e := publishedMany(t, 10)
	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(20 * time.Second)
	shown := e.cycle()

	res, err := e.eng.Apply(t.Context(), true, shown.Offer)
	require.NoError(t, err)
	require.Equal(t, shown.Waiting, res.Accepted)
	require.Empty(t, e.eng.State().Waiting, "nothing waits until the next cycle")
	require.Empty(t, e.eng.State().Offer)
	since := e.clock.now()

	e.clock.advance(time.Second)
	_, err = e.eng.Apply(t.Context(), true, shown.Offer)
	require.ErrorIs(t, err, ErrRefused, "used up")
	require.Empty(t, adminEvents(e, since))

	// The guests come back and vanish again: the same is offered again.
	e.inv.set(snapshot(many(10)...))
	e.clock.advance(10 * time.Second)
	e.cycle()
	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(10 * time.Second)
	again := e.cycle()
	require.Equal(t, shown.Offer, again.Offer)

	res, err = e.eng.Apply(t.Context(), true, again.Offer)
	require.NoError(t, err)
	require.Equal(t, again.Waiting, res.Accepted)
}

// everythingWaits is an engine in enforce mode whose last state shows a stale
// zone, a tunnel no credential sees and the mass delete guard together.
func everythingWaits(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	e.cf.AddAccount("acc3", "Third")
	e.cf.AddZone("zone3", "example.info", "acc3")
	view := newZoneView(e.cf)
	e.useAPI(testToken, view)
	other := cffake.New()
	other.AddAccount("acc2", "Other")
	other.AddZone("zone2", "example.org", "acc2")
	// Each fake numbers its tunnels from one; Cloudflare's ids are unique.
	for range 5 {
		other.SeedTunnel("acc9", "unrelated", nil)
	}
	e.addSecondCredential("other-token", other)
	e.enforce()
	hosts := []string{"a", "b", "c", "d", "e", "f"}
	for i := range hosts {
		hosts[i] += ".example.com"
	}
	gone := guest(101, "web-1", strings.Join(hosts, " ")+" -> :8080")
	org, info := guest(102, "org", "www.example.org -> :8080"), guest(103, "info", "www.example.info -> :8080")
	e.inv.set(snapshot(gone, org, info))
	e.cycle()
	invisible := other.TunnelsIn("acc2")[0].ID

	e.inv.set(snapshot(untagged(gone), org, info))
	require.NoError(t, e.store.DeleteCredential("cred2"))
	view.hide("zone3", true)
	e.clock.advance(zoneRefreshEvery)
	e.cycle()
	e.clock.advance(61 * time.Second)
	e.cycle()
	return e, invisible
}

// Pin 8: what waits, unchanged, keeps its offer from cycle to cycle, and a
// confirmation of it accepts exactly it.
func TestAnUnchangedOfferStaysAndIsAcceptedAsShown(t *testing.T) {
	e, invisible := everythingWaits(t)
	var states []State
	for range 3 {
		e.clock.advance(61 * time.Second)
		states = append(states, e.cycle())
	}
	st := states[2]
	require.Equal(t, []Waiting{
		{
			Kind:   "dns-removals",
			Detail: "mass delete guard: 6 of 6 records are being removed; confirm to proceed",
			Items:  []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com", "f.example.com"},
		},
		{
			Kind: "stale-zone", Subject: "example.info",
			Detail: "zone example.info is no longer listed by credential cred1; a confirmation takes it as gone, " +
				"and its hostnames are taken off the tunnel",
			Items: []string{},
		},
		{
			Kind: "unseen-tunnel", Subject: "pco-abc123",
			Detail: "tunnel pco-abc123 (" + invisible + ") in account acc2 is not visible through any credential; " +
				"a confirmation takes it as gone and removes its connector",
			Items: []string{},
		},
	}, st.Waiting)
	for _, other := range states[:2] {
		require.Equal(t, st.Waiting, other.Waiting)
		require.Equal(t, st.Offer, other.Offer, "one offer for the same")
	}

	res, err := e.eng.Apply(t.Context(), true, st.Offer)

	require.NoError(t, err)
	require.Equal(t, ApplyResult{Accepted: st.Waiting}, res)
	// What was kept in the store is what the engine holds now: saving it
	// again writes nothing.
	kept, err := os.ReadFile(e.memoryFile())
	require.NoError(t, err)
	require.NoError(t, e.store.SaveEngineMemory(e.eng.memory()))
	again, err := os.ReadFile(e.memoryFile())
	require.NoError(t, err)
	require.Equal(t, string(kept), string(again))
	require.NotContains(t, string(kept), "example.info")
	require.NotContains(t, string(kept), invisible)

	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.Empty(t, e.records(), "the removals the guard held went ahead")
	require.False(t, hasProblem(st, "no longer listed by credential"))
	require.False(t, hasProblem(st, "not visible through any credential"))
}

// A restarted daemon offers nothing before its first cycle: what an earlier
// process offered is refused.
func TestAnOfferDoesNotOutliveARestart(t *testing.T) {
	e, view, _ := twoZones(t)
	view.hide("zone2", true)
	e.clock.advance(zoneRefreshEvery)
	shown := e.cycle()
	require.NotEmpty(t, shown.Offer)

	e.restart()
	_, err := e.eng.Apply(t.Context(), true, shown.Offer)
	require.ErrorIs(t, err, ErrRefused)

	e.clock.advance(20 * time.Second)
	st := e.cycle()
	require.Equal(t, shown.Waiting, st.Waiting)
	require.Equal(t, shown.Offer, st.Offer, "offered again by the new process")
	res, err := e.eng.Apply(t.Context(), true, st.Offer)
	require.NoError(t, err)
	require.Len(t, res.Accepted, 1)
}

// With nothing waiting, a confirmation without an offer is no error and
// accepts nothing.
func TestAConfirmationOfNothingAcceptsNothing(t *testing.T) {
	e := newEnv(t)
	e.cycle()

	res, err := e.eng.Apply(t.Context(), true, "")

	require.NoError(t, err)
	require.Equal(t, ApplyResult{LeftObserveOnly: true, Accepted: []Waiting{}}, res)
	_, err = e.eng.Apply(t.Context(), true, "0123456789abcdef")
	require.ErrorIs(t, err, ErrRefused, "an offer of nothing names nothing")
}

// A refused confirmation changes nothing, not even the mode, and says
// nothing.
func TestARefusedConfirmationChangesNothing(t *testing.T) {
	for name, offer := range map[string]string{"no offer": "", "another offer": "0123456789abcdef"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.inv.set(snapshot(many(10)...))
			e.cycle()
			e.inv.set(snapshot(many(10)[6:]...))
			e.clock.advance(20 * time.Second)
			st := e.cycle()
			require.Equal(t, "observe", st.Mode)
			require.NotEmpty(t, st.Offer)
			before, since := e.files(), e.clock.now()

			_, err := e.eng.Apply(t.Context(), true, offer)

			require.EqualError(t, err, offerChanged)
			require.Equal(t, before, e.files(), "the settings and the memory are as they were")
			s, err := e.store.Settings()
			require.NoError(t, err)
			require.True(t, s.ObserveOnly)
			require.Empty(t, e.eng.gone)
			require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
			require.Equal(t, st.Offer, e.eng.State().Offer, "the offer still stands")
		})
	}
}

// A confirmation that cannot be kept in the store takes no effect at all, and
// the offer can be confirmed again once the store works.
func TestAConfirmationThatCannotBeKeptTakesNoEffect(t *testing.T) {
	e := publishedMany(t, 10)
	e.inv.set(snapshot(many(10)[6:]...))
	e.clock.advance(20 * time.Second)
	shown := e.cycle()
	// A directory where the file should be: the memory cannot be written.
	require.NoError(t, os.Remove(e.memoryFile()))
	require.NoError(t, os.Mkdir(e.memoryFile(), 0o700))
	since := e.clock.now()

	_, err := e.eng.Apply(t.Context(), true, shown.Offer)

	require.ErrorContains(t, err, "keeping the confirmation")
	require.Empty(t, e.eng.gone)
	require.Empty(t, adminEvents(e, since.Add(-time.Nanosecond)))
	require.Equal(t, shown.Offer, e.eng.State().Offer)

	require.NoError(t, os.Remove(e.memoryFile()))
	res, err := e.eng.Apply(t.Context(), true, shown.Offer)
	require.NoError(t, err)
	require.Equal(t, shown.Waiting, res.Accepted)
	m, err := e.store.EngineMemory()
	require.NoError(t, err)
	require.Len(t, m.GoneGuests, 6)
	require.Len(t, e.eng.gone, 6)
}

func TestTheOfferNamesWhatWaits(t *testing.T) {
	waiting := []Waiting{{Kind: "stale-zone", Subject: "example.net", Detail: "a sentence", Items: []string{}}}
	b, err := json.Marshal(waiting)
	require.NoError(t, err)
	sum := sha256.Sum256(b)

	require.Equal(t, hex.EncodeToString(sum[:])[:16], offerOf(waiting))
	require.Empty(t, offerOf(nil))
	require.Empty(t, offerOf([]Waiting{}))
	other := []Waiting{{Kind: "stale-zone", Subject: "example.net", Detail: "a sentence", Items: []string{"x"}}}
	require.NotEqual(t, offerOf(waiting), offerOf(other))
}
