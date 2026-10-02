package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// daemonWith returns a runner against a daemon whose engine is in state.
func daemonWith(t *testing.T, state engine.State) (*runner, *fakeEngine) {
	t.Helper()
	e := &fakeEngine{state: state}
	return newRunner(t, serveFake(t, e)), e
}

func TestStatusGolden(t *testing.T) {
	for _, tt := range []struct {
		name    string
		state   engine.State
		golden  string
		problem bool
	}{
		{"healthy", healthyState(), "status_healthy.golden", false},
		{"observe-only with a credential", observeState(), "status_observe.golden", false},
		{"fresh install without a credential", freshState(), "status_fresh.golden", true},
		{"problems", problemState(), "status_problems.golden", true},
		{"no cycle has run", engine.State{Mode: "observe", WriterVerdict: "ok"}, "status_no_cycle.golden", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := daemonWith(t, tt.state)

			res := r.run("", "status")

			requireGolden(t, tt.golden, res.out)
			require.Empty(t, res.errOut)
			if tt.problem {
				require.ErrorIs(t, res.err, errReported, "status exits with 1 when there are problems")
			} else {
				require.NoError(t, res.err)
			}
		})
	}
}

func TestStatusHasProblemsWithoutSayingSo(t *testing.T) {
	// A cycle that found the inventory incomplete or the writer not in order
	// says so in the problems; the exit status does not depend on that.
	for _, tt := range []struct {
		name   string
		change func(*engine.State)
	}{
		{"incomplete inventory", func(st *engine.State) { st.Complete = false }},
		{"foreign writer", func(st *engine.State) { st.WriterVerdict = "foreign" }},
		{"unknown writer", func(st *engine.State) { st.WriterVerdict = "unknown" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := healthyState()
			tt.change(&st)
			r, _ := daemonWith(t, st)

			require.ErrorIs(t, r.run("", "status").err, errReported)
		})
	}
}

func TestStatusShowsTheProfileOnlyWhenTheDaemonKnowsIt(t *testing.T) {
	st := healthyState()
	st.Profile = ""
	r, _ := daemonWith(t, st)

	res := r.run("", "status")

	require.NotContains(t, res.out, "Profile")
}

func TestStatusIsStableOverLocalTime(t *testing.T) {
	// The state carries fractions of a second and a zone of its own.
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "status")

	require.Contains(t, res.out, "Last cycle:  2026-10-01T14:00:00+02:00\n")
}

func TestRoutesGolden(t *testing.T) {
	st := healthyState()
	st.Routes = mixedRoutes()
	r, _ := daemonWith(t, st)

	res := r.run("", "routes")

	require.NoError(t, res.err)
	require.Empty(t, res.errOut)
	requireGolden(t, "routes.golden", res.out)
}

func TestRoutesStateFilter(t *testing.T) {
	st := healthyState()
	st.Routes = mixedRoutes()
	r, _ := daemonWith(t, st)

	res := r.run("", "routes", "--state", "conflict")

	require.NoError(t, res.err)
	requireGolden(t, "routes_conflict.golden", res.out)

	res = r.run("", "routes", "--state", "withdrawn")
	require.NoError(t, res.err)
	require.Equal(t, 2, strings.Count(res.out, "\n"), "the header and one route: %q", res.out)
}

func TestRoutesWithNothingToShow(t *testing.T) {
	r, _ := daemonWith(t, freshState())

	res := r.run("", "routes")
	require.NoError(t, res.err)
	require.Equal(t, "No routes.\n", res.out)

	st := healthyState()
	r, _ = daemonWith(t, st)
	res = r.run("", "routes", "--state", "held")
	require.NoError(t, res.err)
	require.Equal(t, "No routes in state held.\n", res.out)
}

func TestRoutesRefusesWhatItDoesNotKnow(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "routes", "--state", "sleeping")
	require.EqualError(t, res.err, `unknown route state "sleeping": want one of active, unreachable, withdrawn, conflict, no-zone, held, frozen`)

	res = r.run("", "routes", "--json", "--state", "held")
	require.ErrorContains(t, res.err, "--state cannot be used with --json")
}

func TestPlanGolden(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  engine.State
		golden string
	}{
		{"actions, conflicts and lost names", planState(), "plan.golden"},
		{"only a conflict", func() engine.State {
			st := healthyState()
			st.Conflicts = planState().Conflicts
			return st
		}(), "plan_conflict.golden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := daemonWith(t, tt.state)

			res := r.run("", "plan")

			require.NoError(t, res.err)
			require.Empty(t, res.errOut)
			requireGolden(t, tt.golden, res.out)
		})
	}
}

func TestPlanWithNothingToDo(t *testing.T) {
	st := healthyState()
	st.Actions = planState().Actions[:1] // applied: nothing pending
	r, _ := daemonWith(t, st)

	res := r.run("", "plan")

	require.NoError(t, res.err)
	require.Equal(t, "Nothing to do.\n", res.out)
}

// The daemon of these tests sends a document the commands have no types for,
// with its keys in an order of its own: what is printed is what was sent.
const rawState = `{"writerVerdict":"ok","mode":"enforce","at":"2026-10-01T12:00:00Z","complete":true,` +
	`"routes":[{"state":"active","hostname":"www.example.com","owner":"qemu/101","zone":"example.com"}],` +
	`"issues":[],"tunnels":[],"connectors":[],"credentials":[],"actions":[],"conflicts":[],"lost":[],"problems":[],` +
	`"futureField":{"z":1,"a":[1,2]}}`

func indented(t *testing.T, raw string) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Indent(&buf, []byte(raw), "", "  "))
	return buf.String() + "\n"
}

func TestJSONPrintsThePayloadAsTheDaemonSentIt(t *testing.T) {
	socket := serveRaw(t, map[string]rawReply{"GET /v1/state": {200, rawState}})
	r := newRunner(t, socket)

	for _, cmd := range []string{"status", "routes", "plan"} {
		t.Run(cmd, func(t *testing.T) {
			res := r.run("", "--json", cmd)

			require.NoError(t, res.err)
			require.Equal(t, indented(t, rawState), res.out)
			require.Contains(t, res.out, "  \"writerVerdict\": \"ok\",\n  \"mode\": \"enforce\"", "the order of the keys is the daemon's")
			require.Contains(t, res.out, "futureField")
		})
	}
}

func TestStatusJSONStillExitsWithOneOnProblems(t *testing.T) {
	st := freshState()
	r, _ := daemonWith(t, st)

	res := r.run("", "status", "--json")

	require.ErrorIs(t, res.err, errReported)
	var back engine.State
	require.NoError(t, json.Unmarshal([]byte(res.out), &back))
	require.Equal(t, st.Problems, back.Problems)
}

func TestACommandWritesToItsOwnStreams(t *testing.T) {
	r, _ := daemonWith(t, healthyState())

	res := r.run("", "status")
	require.NotEmpty(t, res.out)
	require.Empty(t, res.errOut)

	res = r.run("", "routes", "--state", "sleeping")
	require.Empty(t, res.out)
}

func TestWithoutADaemonTheSocketIsNamed(t *testing.T) {
	socket := filepath.Join(shortDir(t), "pco", "pco.sock")
	r := newRunner(t, socket)

	for _, args := range [][]string{
		{"status"}, {"routes"}, {"plan"}, {"sync"}, {"apply"}, {"adopt", "www.example.com"},
		{"credential", "list"}, {"credential", "check", "abc"}, {"credential", "remove", "abc"},
	} {
		res := r.run("", args...)

		require.EqualError(t, res.err, "cannot reach the pco daemon at "+socket+": is it running?", strings.Join(args, " "))
		require.Empty(t, res.out)
	}
}

func TestADaemonOfAnotherVersionIsNamed(t *testing.T) {
	t.Run("one that answers its version", func(t *testing.T) {
		socket := serveRaw(t, map[string]rawReply{"GET /v1/version": {200, `{"version":"0.1.0"}`}})
		r := newRunner(t, socket)

		res := r.run("", "apply")

		require.EqualError(t, res.err,
			"the daemon does not know this command; pco and the daemon are different versions (cli 0.2.0, daemon 0.1.0)")
	})

	t.Run("one that does not", func(t *testing.T) {
		r := newRunner(t, serveRaw(t, nil))

		res := r.run("", "status")

		require.EqualError(t, res.err,
			"the daemon does not know this command; pco and the daemon are different versions (cli 0.2.0, daemon unknown)")
	})
}

func TestTheVersionCommandIsStillThere(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")

	res := r.run("", "version")

	require.NoError(t, res.err)
	require.Equal(t, "pco dev (none, unknown)\n", res.out)
}
