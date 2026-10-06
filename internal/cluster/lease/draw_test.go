package lease

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestDraw(t *testing.T) {
	held := store.LeaderRecord{
		Writer:     planner.Writer{InstallID: installID, Generation: 7, Nonce: "held7", Incarnation: "pve1/boot-1"},
		Node:       "pve1",
		AcquiredAt: t0.Add(-time.Hour),
		CASHA256:   "c4",
	}
	released := held
	released.ReleasedAt = t0.Add(-time.Minute)
	handedOff := released
	handedOff.Handoff = &store.Handoff{To: "pve2", Until: t0}
	atBound := held
	atBound.Generation = agentproto.MaxGeneration
	belowBound := held
	belowBound.Generation = agentproto.MaxGeneration - 1
	epoch := func(g int, nonce string) *agentproto.Epoch {
		return &agentproto.Epoch{InstallID: installID, Generation: g, Nonce: nonce}
	}

	cases := []struct {
		name              string
		stored            store.LeaderRecord
		remembered        *agentproto.Epoch
		node, incarnation string
		// generation is the drawn one; 0 is the stored record kept.
		generation int
		behind     string
		err        string
	}{
		{name: "fresh after setup", stored: store.LeaderRecord{Writer: setupWriter},
			node: "pve1", incarnation: "pve1/boot-1", generation: 2},
		{name: "kept on the same node and boot", stored: held, node: "pve1", incarnation: "pve1/boot-1"},
		{name: "kept when the agent follows it", stored: held, remembered: epoch(7, "held7"),
			node: "pve1", incarnation: "pve1/boot-1"},
		{name: "a reboot draws", stored: held, node: "pve1", incarnation: "pve1/boot-2", generation: 8},
		{name: "another node draws", stored: held, node: "pve2", incarnation: "pve2/boot-1", generation: 8},
		{name: "a released tenure draws", stored: released, node: "pve1", incarnation: "pve1/boot-1", generation: 8},
		{name: "a hand-off draws", stored: handedOff, node: "pve2", incarnation: "pve2/boot-1", generation: 8},
		{name: "the agent remembers less", stored: held, remembered: epoch(5, "old5"),
			node: "pve2", incarnation: "pve2/boot-1", generation: 8},
		{name: "the agent remembers as much under another nonce", stored: held, remembered: epoch(7, "other"),
			node: "pve2", incarnation: "pve2/boot-1", generation: 8},
		{name: "leader.json below what the agent remembers", stored: held, remembered: epoch(9, "later"),
			node: "pve2", incarnation: "pve2/boot-1", behind: "leader.json is at generation 7, below 9.later"},
		{name: "behind on the same node and boot too", stored: held, remembered: epoch(8, "later"),
			node: "pve1", incarnation: "pve1/boot-1", behind: "below 8.later"},
		{name: "an agent of another install", stored: held,
			remembered: &agentproto.Epoch{InstallID: "ffffffffffff", Generation: 1, Nonce: "x"},
			node:       "pve2", incarnation: "pve2/boot-1", behind: "install ffffffffffff"},
		{name: "up to the bound", stored: belowBound, node: "pve2", incarnation: "pve2/boot-1",
			generation: agentproto.MaxGeneration},
		{name: "never above it", stored: atBound, node: "pve2", incarnation: "pve2/boot-1", err: "above"},
		{name: "a record of no install", stored: store.LeaderRecord{}, node: "pve1", incarnation: "pve1/boot-1",
			err: "install id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			remembered, have := agentproto.Epoch{}, c.remembered != nil
			if have {
				remembered = *c.remembered
			}
			next, behind, err := Draw(c.stored, remembered, have, c.node, c.incarnation, t0, rand.Reader)
			if c.err != "" {
				require.ErrorContains(t, err, c.err)
				return
			}
			require.NoError(t, err)
			if c.behind != "" {
				require.Contains(t, behind, c.behind)
				require.Equal(t, c.stored, next, "nothing is drawn")
				return
			}
			require.Empty(t, behind)
			if c.generation == 0 {
				require.Equal(t, c.stored, next)
				return
			}
			require.NoError(t, next.Validate())
			require.Equal(t, store.LeaderRecord{
				Writer:     planner.Writer{InstallID: installID, Generation: c.generation, Nonce: next.Nonce, Incarnation: c.incarnation},
				Node:       c.node,
				AcquiredAt: t0,
				CASHA256:   c.stored.CASHA256,
			}, next)
			require.Len(t, next.Nonce, 8)
			require.NotEqual(t, c.stored.Nonce, next.Nonce)
		})
	}
}

func TestDrawNeedsRandomness(t *testing.T) {
	_, _, err := Draw(store.LeaderRecord{Writer: setupWriter}, agentproto.Epoch{}, false, "pve1", "pve1/boot-1",
		t0, iotest.ErrReader(errors.New("no entropy")))
	require.ErrorContains(t, err, "no entropy")
}

func TestNoncesUseTheWholeAlphabet(t *testing.T) {
	seen := map[rune]bool{}
	for range 200 {
		n, err := newNonce(rand.Reader)
		require.NoError(t, err)
		require.Len(t, n, 8)
		for _, r := range n {
			require.True(t, strings.ContainsRune(nonceAlphabet, r))
			seen[r] = true
		}
	}
	require.Len(t, seen, len(nonceAlphabet))
}

func TestAnAgentAboveTheDrawBeforeTheFirstWriteMakesTheControllerBehind(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	w := pve1.epoch()

	pve1.k.ReportedEpoch(agentproto.Epoch{InstallID: installID, Generation: w.Generation, Nonce: w.Nonce})
	pve1.k.ReportedEpoch(agentproto.Epoch{InstallID: installID, Generation: 1, Nonce: setupWriter.Nonce})
	require.Empty(t, pve1.k.State().Behind, "its own epoch and older ones are no news")
	require.NoError(t, pve1.gate())

	pve1.k.ReportedEpoch(agentproto.Epoch{InstallID: installID, Generation: w.Generation, Nonce: "twin"})
	s := pve1.k.State()
	require.Contains(t, s.Behind, "twin")
	require.Empty(t, s.Fenced)
	require.True(t, s.Holder, "it stays the controller, observing")
	require.ErrorIs(t, pve1.gate(), ErrBehind)
	l.run(time.Minute)
	require.True(t, pve1.holds())
	require.ErrorIs(t, pve1.gate(), ErrBehind, "until promote")
}

func TestAfterTheFirstWriteAHigherReportStopsWritesWithoutBehind(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	w := pve1.epoch()
	pve1.k.FirstWrite()

	pve1.k.ReportedEpoch(agentproto.Epoch{InstallID: installID, Generation: w.Generation + 3, Nonce: "split"})
	s := pve1.k.State()
	require.Empty(t, s.Behind)
	require.Contains(t, s.Fenced, "5.split")
	require.ErrorIs(t, pve1.gate(), ErrFenced)
	require.True(t, s.Valid, "the lease itself is fine")
}

func TestPromoteDrawsAboveTheSeedAndClearsBehind(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	pve1.k.ReportedEpoch(agentproto.Epoch{InstallID: installID, Generation: 30, Nonce: "later"})
	require.ErrorIs(t, pve1.gate(), ErrBehind)
	before := l.leader()

	require.NoError(t, pve1.k.Promote(l.ctx, 30))
	rec := l.leader()
	require.Equal(t, 31, rec.Generation)
	require.NotEqual(t, before.Nonce, rec.Nonce)
	require.Equal(t, "pve1", rec.Node)
	require.Equal(t, "pve1/boot-1", rec.Incarnation)
	require.Equal(t, pve1.clocks.Now(), rec.AcquiredAt)
	require.Equal(t, rec.Writer, pve1.epoch())
	require.Empty(t, pve1.k.State().Behind)
	require.NoError(t, pve1.gate())

	pve1.k.ReportedEpoch(agentproto.Epoch{InstallID: installID, Generation: 30, Nonce: "later"})
	require.NoError(t, pve1.gate(), "the promoted epoch is above that report")
	l.run(RefreshEvery)
	require.True(t, pve1.holds(), "the new nonce is its own")
}

func TestPromoteDrawsAboveWhatTheLocalAgentRemembers(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	pve1.remember(agentproto.Epoch{InstallID: installID, Generation: 12, Nonce: "later"})
	l.run(time.Second)
	require.Contains(t, pve1.k.State().Behind, "below 12.later")
	require.NoError(t, pve1.k.Promote(l.ctx, 0))
	require.Equal(t, 13, l.leader().Generation)
}

func TestPromoteRefusesAForgedSeed(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	l.run(time.Second)
	rec := l.leader()
	require.ErrorContains(t, pve1.k.Promote(l.ctx, agentproto.MaxGeneration+1), "forged")
	require.ErrorContains(t, pve1.k.Promote(l.ctx, agentproto.MaxGeneration), "above")
	require.Equal(t, rec, l.leader())
	require.NoError(t, pve1.k.Promote(l.ctx, agentproto.MaxGeneration-1))
	require.Equal(t, agentproto.MaxGeneration, l.leader().Generation)
}

func TestPromoteNeedsTheLease(t *testing.T) {
	l := newLab(t, "pve1")
	pve1 := l.m("pve1")
	require.ErrorIs(t, pve1.k.Promote(l.ctx, 3), ErrNotHolder)
	l.run(time.Second)
	l.cfs.setQuorate("pve1", false)
	require.ErrorIs(t, pve1.k.Promote(l.ctx, 3), ErrNotQuorate)
}

// A store restored under the agents' memory: the node holds the lease, so
// that the admin can promote there, and writes nothing, leader.json included.
func TestBehindAtTheAcquisitionHoldsTheLeaseAndWritesNothing(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	pve1, pve2 := l.m("pve1"), l.m("pve2")
	pve2.setHeard(true)
	pve1.remember(agentproto.Epoch{InstallID: installID, Generation: 4, Nonce: "gone4"})
	rev := l.rev()
	l.run(time.Second)

	s := pve1.k.State()
	require.True(t, s.Holder)
	require.Contains(t, s.Behind, "leader.json is at generation 1, below 4.gone4")
	require.Equal(t, setupWriter, s.Writer)
	require.Equal(t, rev, l.rev())
	require.ErrorIs(t, pve1.gate(), ErrBehind)

	l.run(time.Minute)
	require.True(t, pve1.holds(), "it refreshes as any holder")
	require.Equal(t, rev, l.rev())

	require.NoError(t, pve1.k.Release(l.ctx))
	require.Equal(t, rev, l.rev(), "not even the release is written")
	_, held := l.cfs.owner()
	require.False(t, held, "the lock is given back")
}
