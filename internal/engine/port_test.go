package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const portHeld = "tunnel pco-abc123 in account acc1: metrics port 20300 is held by another process, which keeps its connector from starting; "

// What the line says of the remedy is what the cycle does: only a connector
// that pco keeps running gets another port.
func TestAMetricsPortHeldByAnotherProcessIsAProblem(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(e *env)
		says   string
	}{
		{"in enforce mode", func(*env) {}, "the connector gets another port in the next cycle"},
		{"in observe-only mode", func(e *env) { e.settings(func(s *store.Settings) { s.ObserveOnly = true }) },
			"pco gives the connector another port once it changes things again, after pco apply"},
		{"in a cycle that holds", func(e *env) { e.inv.set(incomplete("node pve2 did not answer")) },
			"the connector gets another port in the first cycle that keeps it running"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.enforce()
			e.cycle()
			id := e.tunnels()[0].ID
			e.conn.setPortHeld(id, true)
			tt.change(e)

			e.clock.advance(10 * time.Second)
			st := e.cycle()

			require.Contains(t, st.Problems, portHeld+tt.says)
		})
	}
}

func TestAPortNoLongerHeldIsNoProblem(t *testing.T) {
	e := newEnv(t)
	e.enforce()
	e.cycle()
	id := e.tunnels()[0].ID
	e.conn.setPortHeld(id, true)
	e.clock.advance(10 * time.Second)
	e.cycle()

	e.conn.setPortHeld(id, false)
	e.clock.advance(10 * time.Second)
	st := e.cycle()

	require.False(t, hasProblem(st, "is held by another process"))
}
