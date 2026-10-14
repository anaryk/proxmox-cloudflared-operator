package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A cycle that got through to Cloudflare says when, and the first cycle when
// the engine started.
func TestTheFactsOfTheClusterSayWhenCloudflareWasReached(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	require.Zero(t, e.eng.ClusterFacts().CloudflareAt)

	e.cycle()
	facts := e.eng.ClusterFacts()
	require.Equal(t, t0, facts.CloudflareAt)
	require.Equal(t, t0, facts.Started)

	e.clock.advance(time.Minute)
	e.cycle()
	facts = e.eng.ClusterFacts()
	require.Equal(t, t0.Add(time.Minute), facts.CloudflareAt)
	require.Equal(t, t0, facts.Started)
	require.Empty(t, facts.PinnedEmpty)
}
