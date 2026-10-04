package engine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheProblemsTheEngineIsStartedWithShowInEveryCycle(t *testing.T) {
	const line = "the daemon was started in an unusual way"
	e := newEnv(t)
	e.problems = []string{line}
	e.eng = e.newEngine()

	st := e.cycle()
	require.Contains(t, st.Problems, line)

	e.inv.set(incomplete("listing the guests failed"))
	st = e.cycle()
	require.Contains(t, st.Problems, line, "also in a cycle that holds")
	require.Contains(t, st.Problems, problemIncomplete)

	e.inv.set(snapshot(guest(101, "web-1", "www.example.com -> :8080")))
	st = e.cycle()
	require.Equal(t, []string{line}, st.Problems, "once, beside nothing else in a cycle that went through")
}

func TestTheProblemsTheEngineIsStartedWithShowBeforeTheFirstCycle(t *testing.T) {
	const line = "the daemon was started in an unusual way"
	e := newEnv(t)
	e.problems = []string{line}
	e.eng = e.newEngine()
	e.problems[0] = "changed by the caller after New"

	require.Equal(t, []string{line}, e.eng.State().Problems)
	require.True(t, e.eng.State().FinishedAt.IsZero(), "no cycle ran")
	require.Equal(t, []string{line}, e.cycle().Problems, "New keeps a copy of the lines")
}
