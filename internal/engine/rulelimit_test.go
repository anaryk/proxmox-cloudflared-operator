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
	e.inv.set(snapshot(many(maxIngressRules + 1)...))

	st := e.cycle()

	require.True(t, hasProblem(st, "tunnel "+tunnelName+" in account "+testAccount+" has 1001 ingress rules"), st.Problems)

	e = newEnv(t)
	e.inv.set(snapshot(many(maxIngressRules)...))
	st = e.cycle()
	require.False(t, hasProblem(st, "ingress rules, more than"), st.Problems)
}
