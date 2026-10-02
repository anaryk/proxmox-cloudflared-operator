package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// Routes a held cycle carries, or that an incomplete inventory left as they
// were, are said to be of an earlier cycle.
func TestRoutesOfAnEarlierCycleSaySo(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*engine.State)
		line   string
	}{
		{"a cycle that held", func(st *engine.State) { st.Hold = "no writer identity; run pco setup" },
			"The last cycle held (no writer identity; run pco setup): these are the routes of an earlier cycle.\n"},
		{"an incomplete inventory", func(st *engine.State) { st.Complete = false },
			"The inventory is incomplete: these are the routes of an earlier cycle.\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := healthyState()
			tt.change(&st)
			r, _ := daemonWith(t, st)

			res := r.run("", "routes")

			require.NoError(t, res.err)
			require.True(t, strings.HasPrefix(res.out, tt.line+"HOSTNAME "), res.out)

			res = r.run("", "routes", "--state", "withdrawn")
			require.True(t, strings.HasPrefix(res.out, tt.line+"No routes in state withdrawn.\n"), res.out)
		})
	}

	r, _ := daemonWith(t, healthyState())
	res := r.run("", "routes")
	require.True(t, strings.HasPrefix(res.out, "HOSTNAME "), "a cycle that checked says nothing: %s", res.out)
}
