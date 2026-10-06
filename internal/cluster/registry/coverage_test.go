package registry

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

const cycle = 10 * time.Second

// lab is the controller's cycle in miniature: what each node's agent last
// reported, and a clock that moves in cycles. Every cycle each agent that is
// up reports a fresh status, and every node is observed.
type lab struct {
	t     *testing.T
	cov   *Coverage
	now   time.Time
	nodes map[string]*NodeFacts
	mute  map[string]bool
}

func newLab(t *testing.T, names ...string) *lab {
	t.Helper()
	l := &lab{t: t, cov: NewCoverage(), now: t0, nodes: map[string]*NodeFacts{}, mute: map[string]bool{}}
	for _, n := range names {
		l.start(n)
	}
	return l
}

func (l *lab) observe(name string) { l.cov.Observe(name, *l.nodes[name], l.now) }

func (l *lab) facts(name string) *NodeFacts {
	if l.nodes[name] == nil {
		l.nodes[name] = &NodeFacts{Online: true}
	}
	return l.nodes[name]
}

// start is the agent of a node starting, its watch running from the start.
func (l *lab) start(name string) {
	l.startWithoutWatch(name)
	l.facts(name).WatchRunningSince = l.now.Add(-time.Second)
	l.observe(name)
}

// startWithoutWatch is an agent that is up and reports, whose watch does not
// run yet.
func (l *lab) startWithoutWatch(name string) {
	*l.facts(name) = NodeFacts{
		Online: true, HasAgent: true,
		AgentStartedAt: l.now.Add(-2 * time.Second), StatusAt: l.now,
	}
	delete(l.mute, name)
	l.observe(name)
}

func (l *lab) watchStarts(name string) {
	l.facts(name).WatchRunningSince = l.now
	l.facts(name).StatusAt = l.now
	l.observe(name)
}

func (l *lab) watchStops(name string) {
	l.facts(name).WatchRunningSince = time.Time{}
	l.facts(name).StatusAt = l.now
	l.observe(name)
}

func (l *lab) agentGone(name string) {
	*l.facts(name) = NodeFacts{Online: true}
	l.observe(name)
}

func (l *lab) goOffline(name string) {
	l.facts(name).Online = false
	l.observe(name)
}

// comeOnline is Proxmox listing the node again: its agent has not reported
// yet, so what the controller holds of it is the status from before.
func (l *lab) comeOnline(name string) {
	l.facts(name).Online = true
	l.mute[name] = true
	l.observe(name)
}

// fall is an agent that stops reporting while it keeps running.
func (l *lab) fall(name string) { l.mute[name] = true }

func (l *lab) wait(d time.Duration) {
	end := l.now.Add(d)
	for l.now.Before(end) {
		l.now = l.now.Add(min(cycle, end.Sub(l.now)))
		for name, f := range l.nodes {
			if f.Online && f.HasAgent && !l.mute[name] {
				f.StatusAt = l.now
			}
			l.observe(name)
		}
	}
}

func (l *lab) requireCovered() {
	l.t.Helper()
	ok, missing := l.cov.Covered(l.now)
	require.True(l.t, ok, "not covered: %v", missing)
	require.Empty(l.t, missing)
}

func (l *lab) requireNotCovered(names ...string) {
	l.t.Helper()
	ok, missing := l.cov.Covered(l.now)
	require.False(l.t, ok)
	require.Equal(l.t, names, missing)
}

func (l *lab) requirePromotable(want bool) {
	l.t.Helper()
	require.Equal(l.t, want, l.cov.Promotable(l.now))
}

func (l *lab) requireFlapping(names ...string) {
	l.t.Helper()
	require.Equal(l.t, names, l.cov.Flapping(l.now))
}

func TestNodesWhoseAgentsWatchAreCovered(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.requireCovered()
	l.wait(time.Hour)
	l.requireCovered()
}

func TestNothingObservedIsCoveredAndNotPromotable(t *testing.T) {
	c := NewCoverage()
	ok, missing := c.Covered(t0)
	require.True(t, ok, "every online node of none is covered")
	require.Empty(t, missing)
	require.False(t, c.Promotable(t0.Add(time.Hour)), "no node was watched for ten minutes")
	require.Empty(t, c.Flapping(t0))
}

func TestANodeOnlineWithoutAnAgentIsNotCoveredAtOnce(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.wait(time.Hour)

	l.agentGone("pve2")
	l.requireNotCovered("pve2")
	l.wait(time.Hour)
	l.requireNotCovered("pve2")
}

func TestANodeObservedFirstWithoutAnAgentIsNotGivenTheGrace(t *testing.T) {
	l := newLab(t, "pve1")
	l.cov.Observe("pve2", NodeFacts{Online: true}, l.now)
	l.requireNotCovered("pve2")
	l.wait(5 * time.Minute)
	l.requireNotCovered("pve2")
}

func TestTheNodesWithoutAnAgentAreNamedInOrder(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3", "pve4")
	l.agentGone("pve4")
	l.agentGone("pve2")
	l.requireNotCovered("pve2", "pve4")
}

func TestAnAgentThatComesBackAfterAWhileCoversItsNodeAtOnce(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.agentGone("pve2")
	l.wait(10 * time.Minute)
	l.start("pve2")
	l.requireCovered()
}

func TestAnAgentThatComesBackSoonNeedsSixtySecondsOfWatch(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)
	l.agentGone("pve2")
	l.wait(2 * time.Minute)
	l.start("pve2")
	l.requireNotCovered("pve2")
	l.wait(59 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(time.Second)
	l.requireCovered()
}

func TestANodeThatIsOfflineDoesNotCount(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.wait(time.Hour)

	l.goOffline("pve3")
	l.requireCovered()
	l.wait(time.Hour)
	l.requireCovered()

	l.agentGone("pve2")
	l.requireNotCovered("pve2")
	l.goOffline("pve2")
	l.requireCovered()
}

func TestAWatchThatStoppedIsNotCoveredAtOnce(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)

	l.watchStops("pve2")
	l.requireNotCovered("pve2")
	l.wait(30 * time.Second)
	l.requireNotCovered("pve2")
}

func TestAWatchThatRunsAgainIsCoveredAfterSixtySecondsWithoutABreak(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)
	l.watchStops("pve2")
	l.wait(20 * time.Second)

	l.watchStarts("pve2")
	l.requireNotCovered("pve2")
	l.wait(59 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(time.Second)
	l.requireCovered()
}

func TestANodeNeverSeenBeforeWithoutAWatchIsNotGivenTheGrace(t *testing.T) {
	l := newLab(t, "pve1")
	l.startWithoutWatch("pve2")
	l.requireNotCovered("pve2")
}

func TestANodeNeverSeenBeforeWithAWatchIsCoveredAtOnce(t *testing.T) {
	l := newLab(t, "pve1")
	l.wait(time.Hour)
	l.start("pve2")
	l.requireCovered()
}

func TestAStatusIsNotCoverageWithoutAReceiptTime(t *testing.T) {
	c := NewCoverage()
	c.Observe("pve1", NodeFacts{
		Online: true, HasAgent: true,
		AgentStartedAt: t0.Add(-time.Hour), WatchRunningSince: t0.Add(-time.Hour),
	}, t0)
	ok, missing := c.Covered(t0)
	require.False(t, ok)
	require.Equal(t, []string{"pve1"}, missing)
}

func TestOneRestartKeepsCoverageForSixtySeconds(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)

	l.startWithoutWatch("pve2")
	l.requireCovered()
	l.wait(30 * time.Second)
	l.requireCovered()
	l.wait(29 * time.Second)
	l.requireCovered()
	l.wait(time.Second)
	l.requireNotCovered("pve2")
}

func TestTheGraceEndsAtSixtySecondsWhetherOrNotTheNodeIsObservedAgain(t *testing.T) {
	l := newLab(t, "pve2")
	l.wait(5 * time.Minute)
	l.startWithoutWatch("pve2")

	ok, _ := l.cov.Covered(l.now.Add(CoverageGrace - time.Nanosecond))
	require.True(t, ok)
	ok, missing := l.cov.Covered(l.now.Add(CoverageGrace))
	require.False(t, ok)
	require.Equal(t, []string{"pve2"}, missing)
}

func TestARestartWhoseWatchRunsAtOnceStaysCovered(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)

	l.start("pve2")
	l.requireCovered()
	l.wait(2 * time.Minute)
	l.requireCovered()
}

func TestAWatchThatComesWithinTheGraceNeedsNoMoreProof(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)

	l.startWithoutWatch("pve2")
	l.wait(30 * time.Second)
	l.watchStarts("pve2")
	l.requireCovered()
	l.wait(3 * time.Minute)
	l.requireCovered()
}

func TestAWatchThatComesAfterTheGraceNeedsSixtySecondsOfProof(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)

	l.startWithoutWatch("pve2")
	l.wait(time.Minute)
	l.requireNotCovered("pve2")

	l.wait(30 * time.Second)
	l.watchStarts("pve2")
	l.requireNotCovered("pve2")
	l.wait(59 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(time.Second)
	l.requireCovered()
}

func TestASecondRestartWithinTenMinutesGetsNoGrace(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.start("pve2")
	l.requireCovered()

	l.wait(5 * time.Minute)
	l.startWithoutWatch("pve2")
	l.requireNotCovered("pve2")
}

func TestASecondRestartLosesCoverageUntilSixtySecondsOfUnbrokenWatch(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.start("pve2")

	l.wait(5 * time.Minute)
	l.start("pve2")
	l.requireNotCovered("pve2")
	l.wait(30 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(29 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(time.Second)
	l.requireCovered()
}

func TestARestartJustBeforeTenMinutesAfterTheLastGetsNoGrace(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.start("pve2")

	l.wait(10*time.Minute - time.Second)
	l.startWithoutWatch("pve2")
	l.requireNotCovered("pve2")
}

func TestARestartTenMinutesAfterTheLastGetsTheGraceAgain(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.start("pve2")

	l.wait(10 * time.Minute)
	l.startWithoutWatch("pve2")
	l.requireCovered()
	l.wait(59 * time.Second)
	l.requireCovered()
	l.wait(time.Second)
	l.requireNotCovered("pve2")
}

func TestARestartOfTheWatchAloneIsARestartToo(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)

	l.watchStarts("pve2")
	l.requireCovered()
	l.wait(5 * time.Minute)
	l.watchStarts("pve2")
	l.requireNotCovered("pve2")
}

func TestANodeThatCameOnlineKeepsCoverageForSixtySecondsOnce(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)

	l.goOffline("pve2")
	l.wait(2 * time.Minute)
	l.requireCovered()

	l.comeOnline("pve2")
	l.requireCovered()
	l.wait(59 * time.Second)
	l.requireCovered()
	l.wait(time.Second)
	l.requireNotCovered("pve2")
}

func TestANodeThatReturnsAndItsAgentReportsStaysCovered(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.goOffline("pve2")
	l.wait(2 * time.Minute)

	l.comeOnline("pve2")
	l.wait(20 * time.Second)
	l.start("pve2")
	l.requireCovered()
	l.wait(5 * time.Minute)
	l.requireCovered()
}

func TestASecondReturnWithinTenMinutesGetsNoGrace(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.goOffline("pve2")
	l.wait(2 * time.Minute)
	l.comeOnline("pve2")
	l.wait(20 * time.Second)
	l.start("pve2")

	l.wait(time.Minute)
	l.goOffline("pve2")
	l.wait(time.Minute)
	l.comeOnline("pve2")
	l.requireNotCovered("pve2")

	l.start("pve2")
	l.requireNotCovered("pve2")
	l.wait(59 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(time.Second)
	l.requireCovered()
}

func TestEachNodeHasItsOwnGraceToSpend(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.wait(5 * time.Minute)
	l.goOffline("pve2")
	l.goOffline("pve3")
	l.wait(time.Minute)

	l.comeOnline("pve2")
	l.start("pve2")
	l.wait(5 * time.Minute)
	l.goOffline("pve2")
	l.wait(time.Minute)

	l.comeOnline("pve2")
	l.comeOnline("pve3")
	l.requireNotCovered("pve2")
	l.wait(30 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(30 * time.Second)
	l.requireNotCovered("pve2", "pve3")
}

func TestARestartAfterAReturnIsTheSameEventAsTheReturn(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.goOffline("pve2")
	l.wait(2 * time.Minute)
	l.comeOnline("pve2")
	l.requireCovered()

	l.start("pve2")
	l.requireCovered()
	l.requireFlapping()
}

func TestAStatusOlderThanThirtySecondsIsNoCoverage(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)

	l.fall("pve2")
	l.wait(30 * time.Second)
	l.requireCovered()
	l.wait(time.Second)
	l.requireNotCovered("pve2")
}

func TestAnAgentThatReportsAgainAfterAGapNeedsSixtySecondsOfProof(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.fall("pve2")
	l.wait(45 * time.Second)
	l.requireNotCovered("pve2")

	delete(l.mute, "pve2")
	l.wait(cycle)
	l.requireNotCovered("pve2")
	l.wait(50 * time.Second)
	l.requireNotCovered("pve2")
	l.wait(10 * time.Second)
	l.requireCovered()
}

func TestPromotableOnlyAfterTenMinutesOfCoverage(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.requirePromotable(false)
	l.wait(5 * time.Minute)
	l.requirePromotable(false)
	l.wait(5*time.Minute - time.Second)
	l.requirePromotable(false)
	l.wait(time.Second)
	l.requirePromotable(true)
	l.wait(time.Hour)
	l.requirePromotable(true)
}

func TestAnyLossRestartsThePromotionClock(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.wait(10 * time.Minute)
	l.requirePromotable(true)

	l.watchStops("pve3")
	l.requirePromotable(false)
	l.wait(5 * time.Minute)
	l.watchStarts("pve3")
	l.requireNotCovered("pve3")
	l.requirePromotable(false)

	l.wait(10*time.Minute - time.Second)
	l.requirePromotable(false)
	l.wait(time.Second)
	l.requirePromotable(true)
}

func TestACoverageHeldByTheGraceIsNotPromotable(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)
	l.startWithoutWatch("pve2")
	l.requireCovered()
	l.requirePromotable(false)
	l.wait(30 * time.Second)
	l.requirePromotable(false)
}

func TestANodeThatReturnsIsPromotableTenMinutesAfterItsWatchRuns(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)
	l.goOffline("pve2")
	l.requirePromotable(true)

	l.wait(time.Hour)
	l.comeOnline("pve2")
	l.wait(20 * time.Second)
	l.start("pve2")
	l.requirePromotable(false)
	l.wait(10*time.Minute - time.Second)
	l.requirePromotable(false)
	l.wait(time.Second)
	l.requirePromotable(true)
}

func TestAnOfflineNodeDoesNotHoldBackThePromotion(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)
	l.goOffline("pve2")
	l.requirePromotable(true)
}

func TestWithoutOnlineNodesNothingIsPromotable(t *testing.T) {
	l := newLab(t, "pve1")
	l.wait(time.Hour)
	l.goOffline("pve1")
	l.requirePromotable(false)
}

func TestAnAgentRestartedEveryFiftySecondsNeverPromotes(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	for i := range 15 {
		l.wait(50 * time.Second)
		l.start("pve2")
		l.requirePromotable(false)
		if i == 0 {
			l.requireCovered()
			l.requireFlapping()
			continue
		}
		l.requireNotCovered("pve2")
		l.requireFlapping("pve2")
	}
	l.requirePromotable(false)

	l.wait(10*time.Minute - time.Second)
	l.requirePromotable(false)
	l.wait(time.Second)
	l.requirePromotable(true)
}

func TestANodeIsNotFlappingBeforeItLosesCoverageTwice(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.requireFlapping()
	l.wait(5 * time.Minute)
	l.start("pve2")
	l.requireFlapping()
	l.wait(time.Hour)
	l.requireFlapping()
}

func TestANodeThatLostCoverageTwiceWithinTenMinutesIsFlapping(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.wait(5 * time.Minute)
	l.start("pve3")
	l.wait(5 * time.Minute)
	l.start("pve3")
	l.requireFlapping("pve3")

	l.wait(5*time.Minute - time.Second)
	l.requireFlapping("pve3")
	l.wait(time.Second)
	l.requireFlapping()
}

func TestTwoLossesTenMinutesApartAreNotFlapping(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(5 * time.Minute)
	l.start("pve2")
	l.wait(10 * time.Minute)
	l.start("pve2")
	l.requireFlapping()
}

func TestAWatchThatStopsTwiceWithinTenMinutesIsFlapping(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)
	l.watchStops("pve2")
	l.wait(30 * time.Second)
	l.watchStarts("pve2")
	l.wait(2 * time.Minute)
	l.requireFlapping()
	l.watchStops("pve2")
	l.requireFlapping("pve2")
}

func TestAGraceThatRunsOutIsNotASecondLoss(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Hour)
	l.startWithoutWatch("pve2")
	l.wait(2 * time.Minute)
	l.requireNotCovered("pve2")
	l.requireFlapping()
}

func TestTheFlappingNodesAreNamedInOrder(t *testing.T) {
	l := newLab(t, "pve1", "pve2", "pve3")
	l.wait(time.Minute)
	for _, n := range []string{"pve3", "pve1"} {
		l.start(n)
	}
	l.wait(time.Minute)
	for _, n := range []string{"pve3", "pve1"} {
		l.start(n)
	}
	l.requireFlapping("pve1", "pve3")
	require.True(t, slices.IsSorted(l.cov.Flapping(l.now)))
}

func TestAnOfflineNodeThatFlappedIsStillNamedWhileItsLossesAreRecent(t *testing.T) {
	l := newLab(t, "pve1", "pve2")
	l.wait(time.Minute)
	l.start("pve2")
	l.wait(time.Minute)
	l.start("pve2")
	l.goOffline("pve2")
	l.requireFlapping("pve2")
}

func TestCoverageCanBeUsedFromSeveralGoroutines(t *testing.T) {
	c := NewCoverage()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			name := string(rune('a' + i))
			for j := range 200 {
				now := t0.Add(time.Duration(j) * cycle)
				c.Observe(name, NodeFacts{
					Online: true, HasAgent: true, AgentStartedAt: t0.Add(-time.Hour),
					WatchRunningSince: t0.Add(-time.Hour), StatusAt: now,
				}, now)
				c.Covered(now)
				c.Promotable(now)
				c.Flapping(now)
			}
		})
	}
	wg.Wait()
	ok, _ := c.Covered(t0.Add(199 * cycle))
	require.True(t, ok)
}
