package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
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

func TestStatusBeforeTheFirstCycleKnowsNothing(t *testing.T) {
	// The state of a daemon that has not cycled says "observe" because that
	// is the zero of its state, not because it was found so.
	r, _ := daemonWith(t, engine.State{Mode: "observe", WriterVerdict: "ok"})

	res := r.run("", "status")

	require.NoError(t, res.err)
	require.Contains(t, res.out, "Mode:        unknown\n")
	require.Contains(t, res.out, "Inventory:   unknown\n")
	require.NotContains(t, res.out, "observe-only")
}

func TestStatusInventoryLineSaysNothingButCompleteOrIncomplete(t *testing.T) {
	r, _ := daemonWith(t, problemState())

	res := r.run("", "status")

	require.Contains(t, res.out, "Inventory:   incomplete\n")
	require.Equal(t, 1, strings.Count(res.out, "cluster status: proxmox api: HTTP 500: no quorum"), "the problem is in the list of problems only")
}

func TestStatusIssues(t *testing.T) {
	for _, tt := range []struct {
		name   string
		count  int
		golden string
	}{
		{"none", 0, "status_issues_none.golden"},
		{"a few", 3, "status_issues_few.golden"},
		{"ten are all there is room for", 10, "status_issues_ten.golden"},
		{"more than ten", 13, "status_issues_many.golden"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := healthyState()
			st.Issues = issues(tt.count)
			r, _ := daemonWith(t, st)

			res := r.run("", "status")

			require.NoError(t, res.err, "issues are no problems")
			requireGolden(t, tt.golden, res.out)
		})
	}
}

func TestStatusNamesTheGuestsThatWaitForApproval(t *testing.T) {
	r, _ := daemonWith(t, approvalState())

	res := r.run("", "status")

	require.NoError(t, res.err, "a guest that waits is no problem")
	requireGolden(t, "status_unapproved.golden", res.out)

	st := healthyState()
	st.Unapproved = approvalState().Unapproved[:1]
	r, _ = daemonWith(t, st)
	require.Contains(t, r.run("", "status").out, "Approval:    1 guest waits (pco guest list)\n")
	r, _ = daemonWith(t, healthyState())
	require.NotContains(t, r.run("", "status").out, "Approval")
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

// Owners are listed in their natural order: qemu/20 before qemu/101.
func TestRoutesAreInTheNaturalOrderOfTheirOwners(t *testing.T) {
	st := healthyState()
	st.Routes = []engine.RouteView{
		routeView("www.example.com", "qemu/101", planner.StateActive, "http://10.0.0.11:8080", "example.com", ""),
		routeView("www.example.com", "qemu/20", planner.StateConflict, "", "", "hostname is held by qemu/101"),
	}
	r, _ := daemonWith(t, st)

	res := r.run("", "routes")

	require.NoError(t, res.err)
	require.Less(t, strings.Index(res.out, "qemu/20 "), strings.Index(res.out, "qemu/101 "), res.out)
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

func TestRoutesOfAFrozenAccount(t *testing.T) {
	st := healthyState()
	st.Routes = append(mixedRoutes(), routeView("frozen.example.com", "qemu/107", engine.RouteFrozen, "", "example.com", "account frozen: Cloudflare says so"))
	r, _ := daemonWith(t, st)

	all := r.run("", "routes")
	require.NoError(t, all.err)
	requireGolden(t, "routes_frozen.golden", all.out)

	res := r.run("", "routes", "--state", "frozen")
	require.NoError(t, res.err)
	require.Equal(t, 2, strings.Count(res.out, "\n"))
	require.Contains(t, res.out, "frozen.example.com")

	help := r.run("", "routes", "--help")
	require.Contains(t, help.out, "active, unreachable, withdrawn, conflict, no-zone, held, frozen")

	status := r.run("", "status")
	require.Contains(t, status.out, "Routes:      active 1, unreachable 1, withdrawn 1, conflict 1, no-zone 1, held 1, frozen 1\n", "a frozen route is counted with the states of the others")
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

func TestPlanShowsWhatWaitsForAConfirmation(t *testing.T) {
	st := healthyState()
	st.Waiting = vanishState().Waiting
	r, e := daemonWith(t, st)

	res := r.run("", "plan")

	require.NoError(t, res.err)
	require.True(t, strings.HasPrefix(res.out, "Waits for a confirmation (pco apply --confirm-deletes accepts all of it):\n"+
		"  - 23 guests that hold a hostname are no longer listed by Proxmox;"), res.out)
	require.Contains(t, res.out, "      qemu/120 vm-120\n      ... and 3 more (pco plan --json shows all)\n")
	require.NotContains(t, res.out, "qemu/121")
	require.Empty(t, e.called(), "plan only reads")
}

// --json prints what the daemon sent; its help says how it is changed.
func TestTheHelpOfJSONSaysWhatIsChanged(t *testing.T) {
	r := newRunner(t, "/nonexistent/pco/pco.sock")
	for _, args := range [][]string{
		{"--help"}, {"status", "--help"}, {"routes", "--help"}, {"plan", "--help"},
		{"claims", "list", "--help"}, {"guest", "list", "--help"}, {"diagnose", "--help"}, {"doctor", "--help"},
	} {
		res := r.run("", args...)

		require.NoError(t, res.err)
		require.Contains(t, strings.Join(strings.Fields(res.out), " "),
			"printed as the daemon sent it, re-indented, with control and bidirectional characters escaped", args)
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

// A daemon of another version may send a state this binary cannot read: it
// can still be printed.
const undecodableState = `{"mode":7,"routes":"not a list","futureField":{"z":1,"a":2}}`

func TestJSONPrintsAStateThatDoesNotDecode(t *testing.T) {
	socket := serveRaw(t, map[string]rawReply{"GET /v1/state": {200, undecodableState}})
	r := newRunner(t, socket)

	for _, cmd := range []string{"routes", "plan"} {
		t.Run(cmd, func(t *testing.T) {
			res := r.run("", "--json", cmd)

			require.NoError(t, res.err, "the bytes are printed without being read")
			require.Equal(t, indented(t, undecodableState), res.out)
		})
	}

	t.Run("status prints it and then says why it cannot tell the exit status", func(t *testing.T) {
		res := r.run("", "--json", "status")

		require.Equal(t, indented(t, undecodableState), res.out)
		require.ErrorContains(t, res.err, "decoding the state of the daemon")
	})
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
	socket := filepath.Join(testutil.ShortDir(t), "pco", "pco.sock")
	r := newRunner(t, socket)

	for _, args := range [][]string{
		{"status"}, {"routes"}, {"plan"}, {"sync"}, {"apply"}, {"adopt", "www.example.com"},
		{"credential", "list"}, {"credential", "check", "abc"}, {"credential", "remove", "abc"},
		{"claims", "list"}, {"claims", "resolve", "www.example.com", "qemu/102"},
		{"guest", "list"}, {"guest", "approve", "qemu/101"}, {"guest", "revoke", "qemu/101"},
		{"diagnose", "www.example.com"}, {"doctor"},
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
