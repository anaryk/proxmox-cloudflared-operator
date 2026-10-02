package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const notATerminal = "stdin is not a terminal: pass --yes to confirm"

// vanishGuardProblem is what the engine says when the guests that hold a
// hostname dropped out of the listing: nothing is planned, the cycle holds.
const vanishGuardProblem = "7 of 20 guests that hold a hostname are no longer listed by Proxmox (qemu/101, qemu/102); " +
	"nothing is changed until they are listed again, or run pco apply --confirm-deletes if they were removed on purpose"

// vanishState is a daemon whose cycle holds on the vanish guard: no action is
// planned, and no delete is held.
func vanishState() engine.State {
	st := healthyState()
	st.Actions = nil
	st.Problems = []string{
		"an unrelated problem",
		vanishGuardProblem,
	}
	return st
}

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

			res := r.tty().run(tt.in, append([]string{"apply", "--confirm-deletes"}, tt.args...)...)

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
				require.Contains(t, res.errOut, "Accept this at the next run? [y/N] ")
			} else {
				require.Empty(t, res.errOut)
			}
		})
	}
}

// What the confirmation accepts is more than the deletes of the plan: the
// engine marks everything that waits for it with the flag in its problem.
func TestApplyConfirmDeletesListsEverythingThatWaitsForTheConfirmation(t *testing.T) {
	withDeletes := vanishState()
	withDeletes.Actions = planState().Actions

	for _, tt := range []struct {
		name       string
		state      engine.State
		wantDelete bool
		wantWaits  bool
	}{
		{"only deletes are held", planState(), true, false},
		{"only the vanish guard holds the cycle", vanishState(), false, true},
		{"both", withDeletes, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, tt.state)

			res := r.tty().run("y\n", "apply", "--confirm-deletes")

			require.NoError(t, res.err)
			require.Equal(t, []string{"apply confirmDeletes=true"}, e.called())
			require.Equal(t, tt.wantDelete, strings.Contains(res.out, "Deletes pending:"), res.out)
			require.Equal(t, !tt.wantDelete, strings.Contains(res.out, "No deletes are held right now."), res.out)
			if tt.wantWaits {
				require.Contains(t, res.out, "These also wait for the confirmation, and --confirm-deletes accepts all of them:")
				require.Contains(t, res.out, "  - "+vanishGuardProblem)
				require.NotContains(t, res.out, "an unrelated problem", "only what waits for the confirmation is listed")
			} else {
				require.NotContains(t, res.out, "wait for the confirmation")
			}
			require.Contains(t, res.errOut, "Accept this at the next run? [y/N] ")
		})
	}
}

func TestApplyConfirmDeletesWithNothingWaitingAsksNothing(t *testing.T) {
	t.Run("in enforce mode there is nothing to do", func(t *testing.T) {
		r, e := daemonWith(t, healthyState())

		res := r.run("", "apply", "--confirm-deletes")

		require.NoError(t, res.err)
		require.Equal(t, "Nothing waits for a confirmation, so there is nothing to do.\n", res.out)
		require.Empty(t, res.errOut, "no question")
		require.Empty(t, e.called(), "nothing was sent to the daemon: a confirmation nobody waits for would land on deletes the admin did not see")
	})

	t.Run("in observe-only mode the daemon still starts applying, without a confirmation", func(t *testing.T) {
		r, e := daemonWith(t, observeState())

		res := r.run("", "apply", "--confirm-deletes")

		require.NoError(t, res.err)
		require.Equal(t, "Nothing waits for a confirmation, so none is given.\n"+
			"Applying: the daemon changes Cloudflare from the next cycle. Follow it with pco status.\n", res.out)
		require.Empty(t, res.errOut)
		require.Equal(t, []string{"apply confirmDeletes=false"}, e.called())
	})

	t.Run("before the first cycle the mode is not known, so the daemon is asked to apply", func(t *testing.T) {
		r, e := daemonWith(t, engine.State{Mode: "observe", WriterVerdict: "ok"})

		res := r.run("", "apply", "--confirm-deletes")

		require.NoError(t, res.err)
		require.Equal(t, []string{"apply confirmDeletes=false"}, e.called())
	})
}

func TestConfirmationsRefuseToRunWithoutATerminal(t *testing.T) {
	for _, tt := range []struct {
		name  string
		state engine.State
		args  []string
	}{
		{"apply --confirm-deletes", planState(), []string{"apply", "--confirm-deletes"}},
		{"apply --confirm-deletes with only the vanish guard", vanishState(), []string{"apply", "--confirm-deletes"}},
		{"adopt", planState(), []string{"adopt", "shop.example.com"}},
		{"credential check --deep", credentialsState(), []string{"credential", "check", "a1b2c3d4", "--deep"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, tt.state)

			// A pipe that says yes does not confirm, and one that stays silent
			// does not hang: stdin is not read at all.
			res := r.runReader(unreadable{t}, tt.args...)

			require.EqualError(t, res.err, notATerminal)
			require.Empty(t, e.called(), "nothing was sent to the daemon")
			require.NotContains(t, res.errOut, "[y/N]", "no question was asked")
		})
	}

	t.Run("a piped yes does not confirm", func(t *testing.T) {
		r, e := daemonWith(t, planState())

		res := r.run("y\n", "apply", "--confirm-deletes")

		require.EqualError(t, res.err, notATerminal)
		require.Empty(t, e.called())
	})

	t.Run("--yes confirms without a terminal", func(t *testing.T) {
		r, e := daemonWith(t, planState())

		res := r.runReader(unreadable{t}, "apply", "--confirm-deletes", "--yes")

		require.NoError(t, res.err)
		require.Equal(t, []string{"apply confirmDeletes=true"}, e.called())
	})

	t.Run("a command that asks nothing needs no terminal", func(t *testing.T) {
		r, e := daemonWith(t, planState())

		require.NoError(t, r.runReader(unreadable{t}, "apply").err)
		require.Equal(t, []string{"apply confirmDeletes=false"}, e.called())
	})
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

			res := r.tty().run(tt.in, append([]string{"adopt", "Shop.Example.com."}, tt.args...)...)

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

func TestJSONMeansNothingToACommandWithoutAnAnswerToPrint(t *testing.T) {
	for _, tt := range []struct {
		args []string
		path string
	}{
		{[]string{"sync"}, "pco sync"},
		{[]string{"apply"}, "pco apply"},
		{[]string{"adopt", "shop.example.com", "--yes"}, "pco adopt"},
		{[]string{"credential", "remove", "a1b2c3d4"}, "pco credential remove"},
		{[]string{"daemon"}, "pco daemon"},
		{[]string{"version"}, "pco version"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			r, e := daemonWith(t, planState())

			res := r.run("", append([]string{"--json"}, tt.args...)...)

			require.EqualError(t, res.err, fmt.Sprintf("--json has no meaning for %q: it prints no answer of the daemon", tt.path))
			require.Empty(t, res.out)
			require.Empty(t, e.called(), "the command did nothing")
		})
	}
}
