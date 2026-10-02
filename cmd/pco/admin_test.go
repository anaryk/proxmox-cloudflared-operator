package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

const notATerminal = "stdin is not a terminal: pass --yes to confirm"

// vanishState is a daemon whose cycle holds on the vanish guard: no action is
// planned, and 23 guests wait for a confirmation.
func vanishState() engine.State {
	st := healthyState()
	st.Actions = nil
	st.Problems = []string{
		"an unrelated problem",
		"23 of 40 guests that hold a hostname are no longer listed by Proxmox (qemu/101, qemu/102, qemu/103, qemu/104, qemu/105 and 18 more); " +
			"nothing is changed until they are listed again, or run pco apply --confirm-deletes if they were removed on purpose",
	}
	st.Waiting = []engine.Waiting{vanishedGuests(23)}
	st.Offer = "5c5c5c5c00000001"
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

func TestApplyConfirmDeletesShowsWhatWaitsAndAsks(t *testing.T) {
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

			// What the daemon offers, and not the other pending actions.
			require.Contains(t, res.out, "Waits for a confirmation (pco apply --confirm-deletes accepts all of it):\n"+
				"  - mass delete guard: 6 of 9 records are being removed; confirm to proceed\n"+
				"      gone.example.com\n"+
				"      old.example.com\n")
			require.NotContains(t, res.out, "blog.example.com")
			require.NotContains(t, res.out, "update-record")
			if tt.applied {
				require.NoError(t, res.err)
				require.Contains(t, res.out, "Confirmed for the next run:\n"+
					"  - mass delete guard: 6 of 9 records are being removed; confirm to proceed\n")
				require.Equal(t, []string{"apply confirmDeletes=true offer=1a2b3c4d5e6f7a8b"}, e.called(), "the offer of what was shown")
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

func TestApplyConfirmDeletesGolden(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  engine.State
		in     string
		golden string
		call   []string
	}{
		{"only removals the guard holds", guardState(), "y\n", "apply_guard.golden", []string{"apply confirmDeletes=true offer=0f1e2d3c4b5a6978"}},
		{"only a vanish hold", vanishState(), "y\n", "apply_vanished.golden", []string{"apply confirmDeletes=true offer=5c5c5c5c00000001"}},
		{"a stale zone and an unseen tunnel", zoneAndTunnelState(), "y\n", "apply_zone_tunnel.golden", []string{"apply confirmDeletes=true offer=9a8b7c6d5e4f3a2b"}},
		{"nothing waits", graceState(), "", "apply_nothing.golden", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, e := daemonWith(t, tt.state)

			res := r.tty().run(tt.in, "apply", "--confirm-deletes")

			require.NoError(t, res.err)
			require.Equal(t, tt.call, e.called())
			requireClean(t, res.out, "stdout")
			requireGolden(t, tt.golden, res.out+"--- stderr\n"+res.errOut)
		})
	}
}

// Reproduced: a removal in its grace and an adoption made the command ask,
// send a confirmation and say that deletes were confirmed, while the daemon
// had nothing to confirm. Neither is offered, so nothing is asked.
func TestApplyConfirmDeletesAsksNothingForWhatAConfirmationDoesNotAffect(t *testing.T) {
	r, e := daemonWith(t, graceState())

	res := r.tty().run("y\n", "apply", "--confirm-deletes")

	require.NoError(t, res.err)
	require.Empty(t, e.called())
	require.Empty(t, res.errOut, "no question")
	require.Contains(t, res.out, "Destructive actions that are pending; a confirmation does not affect them:\n")
	require.Contains(t, res.out, "old.example.com")
	require.Contains(t, res.out, "shop.example.com")
	require.True(t, strings.HasSuffix(res.out, "Nothing waits for a confirmation, so there is nothing to do.\n"), res.out)
	require.NotContains(t, res.out, "onfirmed")
}

// The problems name the flag; what waits is told by the daemon alone.
func TestApplyConfirmDeletesDoesNotReadTheProblems(t *testing.T) {
	st := vanishState()
	st.Waiting, st.Offer = []engine.Waiting{}, ""
	r, e := daemonWith(t, st)

	res := r.tty().run("y\n", "apply", "--confirm-deletes")

	require.NoError(t, res.err)
	require.Equal(t, "Nothing waits for a confirmation, so there is nothing to do.\n", res.out)
	require.Empty(t, e.called())
}

// Only what the daemon accepted is told as confirmed.
func TestApplyConfirmDeletesSaysWhatTheDaemonAccepted(t *testing.T) {
	t.Run("nothing", func(t *testing.T) {
		e := &fakeEngine{state: planState(), applied: &engine.ApplyResult{Accepted: []engine.Waiting{}}}
		r := newRunner(t, serveFake(t, e))

		res := r.tty().run("y\n", "apply", "--confirm-deletes")

		require.NoError(t, res.err)
		require.Contains(t, res.out, "Nothing was confirmed.\n")
		require.NotContains(t, res.out, "Confirmed for the next run")
	})
	t.Run("part of it", func(t *testing.T) {
		st := zoneAndTunnelState()
		e := &fakeEngine{state: st, applied: &engine.ApplyResult{Accepted: st.Waiting[1:]}}
		r := newRunner(t, serveFake(t, e))

		res := r.tty().run("y\n", "apply", "--confirm-deletes")

		require.NoError(t, res.err)
		_, confirmed, found := strings.Cut(res.out, "Confirmed for the next run:\n")
		require.True(t, found, res.out)
		require.Contains(t, confirmed, "tunnel pco-abc123")
		require.NotContains(t, confirmed, "zone example.net", "not accepted, so not said")
	})
}

func TestApplyConfirmDeletesOfWhatChangedMeanwhile(t *testing.T) {
	// A cycle ran between the state that was shown and the confirmation.
	e := &fakeEngine{state: planState(), applyErr: errOfferChanged}
	r := newRunner(t, serveFake(t, e))

	res := r.tty().run("y\n", "apply", "--confirm-deletes")

	require.ErrorIs(t, res.err, engine.ErrRefused)
	require.EqualError(t, res.err, "refused: what waits for a confirmation changed since it was shown; look again and repeat")
	var stderr strings.Builder
	require.Equal(t, 1, exitCode(res.err, &stderr))
	require.Equal(t, "pco: refused: what waits for a confirmation changed since it was shown; look again and repeat\n", stderr.String())
	require.NotContains(t, res.out, "onfirmed")
	require.Equal(t, []string{"apply confirmDeletes=true offer=1a2b3c4d5e6f7a8b"}, e.called())
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
		require.Equal(t, []string{"apply confirmDeletes=true offer=1a2b3c4d5e6f7a8b"}, e.called())
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
