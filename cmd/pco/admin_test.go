package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

func TestSync(t *testing.T) {
	r, e := daemonWith(t, healthyState())

	res := r.run("", "sync")

	require.NoError(t, res.err)
	require.Equal(t, "A cycle was requested.\n", res.out)
	require.Equal(t, []string{"sync"}, e.called())
}

func TestApplyAsksNothingWithoutConfirmDeletes(t *testing.T) {
	r, e := daemonWith(t, observeState())

	res := r.run("", "apply")

	require.NoError(t, res.err)
	require.Equal(t, "Applying: the daemon changes Cloudflare from the next cycle. Follow it with pco status.\n", res.out)
	require.Empty(t, res.errOut, "no question was asked")
	require.Equal(t, []string{"apply confirmDeletes=false"}, e.called())
}

func TestApplyConfirmDeletesShowsWhatIsPendingAndAsks(t *testing.T) {
	for _, tt := range []struct {
		name    string
		in      string
		args    []string
		applied bool
	}{
		{"yes", "y\n", nil, true},
		{"yes in full, in capitals", "YES\n", nil, true},
		{"no", "n\n", nil, false},
		{"an empty answer", "\n", nil, false},
		{"the input ends", "", nil, false},
		{"anything else", "sure\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, planState())

			res := r.run(tt.in, append([]string{"apply", "--confirm-deletes"}, tt.args...)...)

			// Only the deletes that were not carried out, not the other pending
			// actions and not the applied ones.
			require.Contains(t, res.out, "Deletes pending:")
			require.Contains(t, res.out, "old.example.com")
			require.Contains(t, res.out, "gone.example.com")
			require.NotContains(t, res.out, "blog.example.com")
			require.NotContains(t, res.out, "update-record")
			if tt.applied {
				require.NoError(t, res.err)
				require.Contains(t, res.out, "Deletes confirmed for the next run.")
				require.Equal(t, []string{"apply confirmDeletes=true"}, e.called())
			} else {
				require.ErrorIs(t, res.err, errAborted)
				require.Empty(t, e.called(), "nothing was sent to the daemon")
			}
			if len(tt.args) == 0 {
				require.Contains(t, res.errOut, "Let these deletes through at the next run? [y/N] ")
			} else {
				require.Empty(t, res.errOut)
			}
		})
	}
}

func TestApplyConfirmDeletesWithNothingHeld(t *testing.T) {
	r, e := daemonWith(t, healthyState())

	res := r.run("", "apply", "--confirm-deletes", "--yes")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "No deletes are held right now.\n")
	require.Equal(t, []string{"apply confirmDeletes=true"}, e.called())
}

func TestApplyReportsWhatTheDaemonRefused(t *testing.T) {
	r, e := daemonWith(t, healthyState())
	e.applyErr = fmt.Errorf("%w: the settings cannot be saved", engine.ErrRefused)

	res := r.run("", "apply")

	require.ErrorIs(t, res.err, engine.ErrRefused)
	require.EqualError(t, res.err, "refused: the settings cannot be saved")
}

func TestAdoptShowsTheConflictAndAsks(t *testing.T) {
	for _, tt := range []struct {
		name    string
		in      string
		args    []string
		adopted bool
	}{
		{"yes", "yes\n", nil, true},
		{"no", "\n", nil, false},
		{"--yes asks nothing", "", []string{"--yes"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, planState())

			res := r.run(tt.in, append([]string{"adopt", "Shop.Example.com."}, tt.args...)...)

			require.Contains(t, res.out, "shop.example.com is held by a record of someone else in zone example.com: A 192.0.2.10.")
			require.Contains(t, res.out, "Adopting replaces it with a record that points at the tunnel.")
			if tt.adopted {
				require.NoError(t, res.err)
				require.Equal(t, []string{"adopt shop.example.com"}, e.called(), "the name is sent in its normal form")
				require.Contains(t, res.out, "Adoption of shop.example.com requested")
			} else {
				require.ErrorIs(t, res.err, errAborted)
				require.Empty(t, e.called())
			}
			if len(tt.args) == 0 {
				require.Contains(t, res.errOut, "Adopt shop.example.com? [y/N] ")
			}
		})
	}
}

func TestAdoptOfARecordThatLostItsMarker(t *testing.T) {
	r, e := daemonWith(t, planState())

	res := r.run("", "adopt", "lost.example.com", "--yes")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "lost.example.com points at the tunnel of this install but lost its marker.")
	require.Equal(t, []string{"adopt lost.example.com"}, e.called())
}

func TestAdoptOfANameThatIsNotInTheWayLeavesTheAnswerToTheDaemon(t *testing.T) {
	r, e := daemonWith(t, planState())
	e.adoptErr = fmt.Errorf("%w: no record of someone else holds nothing.example.com", engine.ErrNotFound)

	res := r.run("", "adopt", "nothing.example.com")

	require.ErrorIs(t, res.err, engine.ErrNotFound)
	require.Empty(t, res.out, "there is no conflict to show")
	require.Empty(t, res.errOut, "and nothing to ask about")
	require.Equal(t, []string{"adopt nothing.example.com"}, e.called())
}

func TestAdoptRefusesANameThatIsNoHostname(t *testing.T) {
	r, e := daemonWith(t, planState())

	res := r.run("", "adopt", "not a host")

	require.ErrorContains(t, res.err, `"not a host" is not a hostname`)
	require.Empty(t, e.called())
}

func TestAdoptNeedsOneName(t *testing.T) {
	r, _ := daemonWith(t, planState())

	require.Error(t, r.run("", "adopt").err)
	require.Error(t, r.run("", "adopt", "a.example.com", "b.example.com").err)
}
