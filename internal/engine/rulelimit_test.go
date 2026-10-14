package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A tunnel with more ingress rules than pco keeps one under is named, with
// its count: the 503 blocks of an account go into every tunnel of it, and
// Cloudflare documents no limit.
func TestATunnelWithTooManyRulesIsNamed(t *testing.T) {
	e := newEnv(t)
	e.eng.ruleLimit = 10
	e.inv.set(snapshot(many(11)...))

	st := e.cycle()

	require.True(t, hasProblem(st, "tunnel "+tunnelName+" in account "+testAccount+" has 11 ingress rules, more than the 10"), st.Problems)

	e = newEnv(t)
	e.eng.ruleLimit = 10
	e.inv.set(snapshot(many(10)...))
	st = e.cycle()
	require.False(t, hasProblem(st, "ingress rules, more than"), st.Problems)
}
