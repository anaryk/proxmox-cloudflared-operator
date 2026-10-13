package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
)

type copyFilter struct {
	sets [][]egress.Target
	err  error
}

func (f *copyFilter) Set(_ context.Context, targets []egress.Target) error {
	f.sets = append(f.sets, targets)
	return f.err
}

type copyConns struct {
	stopped []string
	err     error
}

func (c *copyConns) StopAll(_ context.Context, install string) error {
	c.stopped = append(c.stopped, install)
	return c.err
}

// Without the agent of the node, as when it cannot be made, a copy still
// empties the egress filter and stops the connectors of the install.
func TestACopyWithoutItsAgentStopsAllTheSame(t *testing.T) {
	filter, conns := &copyFilter{}, &copyConns{}

	done := stopCopy(t.Context(), "a copy", directStop{filter: filter, conns: conns, install: "abc123"}, zerolog.Nop())

	require.True(t, done)
	require.Equal(t, [][]egress.Target{nil}, filter.sets)
	require.Equal(t, []string{"abc123"}, conns.stopped)

	t.Run("a filter switched off is no failure", func(t *testing.T) {
		filter.err = egress.ErrOff
		require.True(t, stopCopy(t.Context(), "a copy", directStop{filter: filter, conns: conns, install: "abc123"}, zerolog.Nop()))
	})
	t.Run("a stop that fails is done again by the first cycle", func(t *testing.T) {
		conns.err = errors.New("systemd does not answer")
		require.False(t, stopCopy(t.Context(), "a copy", directStop{filter: filter, conns: conns, install: "abc123"}, zerolog.Nop()))
	})
}
