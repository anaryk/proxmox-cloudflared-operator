package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAMetricsPortHeldByAnotherProcessIsAProblem(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	e.conn.setPortHeld(id, true)

	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.Contains(t, st.Problems, "tunnel pco-abc123 in account acc1: metrics port 20300 is held by another process, "+
		"which keeps its connector from starting; the connector gets another port in the next cycle")

	e.conn.setPortHeld(id, false)
	e.clock.advance(10 * time.Second)
	st = e.cycle()
	require.False(t, hasProblem(st, "is held by another process"))
}
