//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// filterOn is the first line of pco egress show when the filter is on and
// its table is loaded as pco loads it.
const filterOn = "The egress filter is on."

// state is the state of the daemon, as pco routes --json prints it.
func (s *suite) state(t testing.TB) engine.State {
	t.Helper()
	st, err := s.tryState()
	require.NoError(t, err)
	return st
}

func (s *suite) tryState() (engine.State, error) {
	out, err := s.run(s.pco, "routes", "--json")
	if err != nil {
		return engine.State{}, err
	}
	var st engine.State
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		return engine.State{}, fmt.Errorf("reading what pco routes --json printed: %w", err)
	}
	return st, nil
}

func (s *suite) sync(t testing.TB) {
	t.Helper()
	s.must(t, s.pco, "sync")
}

// waitState polls the state until ok, and fails with the last one, and the
// last error of the command line, after timeout. A daemon that does not
// answer for a while is waited for.
func (s *suite) waitState(t testing.TB, what string, timeout time.Duration, ok func(engine.State) bool) engine.State {
	t.Helper()
	var last engine.State
	failed := "none"
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.tryState()
		if err == nil {
			last = st
			if ok(st) {
				return st
			}
		} else {
			failed = fmt.Sprintf("%s: %v", time.Now().Format(time.TimeOnly), err)
		}
		if time.Now().After(deadline) {
			raw, _ := json.MarshalIndent(last, "", "  ")
			t.Fatalf("waited %s for %s; the last error of pco routes --json: %s; the last state:\n%s", timeout, what, failed, raw)
		}
		time.Sleep(time.Second)
	}
}

// waitRoute waits until a route of host is in state, and returns it.
func (s *suite) waitRoute(t testing.TB, host string, state planner.RouteState) engine.RouteView {
	t.Helper()
	return s.waitOwnerRoute(t, host, "", state)
}

// waitOwnerRoute is waitRoute for the route of one owner; any owner when owner
// is empty.
func (s *suite) waitOwnerRoute(t testing.TB, host, owner string, state planner.RouteState) engine.RouteView {
	t.Helper()
	var found engine.RouteView
	s.waitState(t, fmt.Sprintf("route %s of %q to be %s", host, owner, state), 3*time.Minute, func(st engine.State) bool {
		r, ok := routeOf(st, host, owner)
		found = r
		return ok && r.State == state
	})
	return found
}

// waitGone waits until no route of host is left.
func (s *suite) waitGone(t testing.TB, host string) {
	t.Helper()
	s.waitState(t, "no route of "+host, 3*time.Minute, func(st engine.State) bool {
		_, ok := routeOf(st, host, "")
		return !ok
	})
}

func routeOf(st engine.State, host, owner string) (engine.RouteView, bool) {
	i := slices.IndexFunc(st.Routes, func(r engine.RouteView) bool {
		return r.Hostname == host && (owner == "" || r.Owner == owner)
	})
	if i < 0 {
		return engine.RouteView{}, false
	}
	return st.Routes[i], true
}

func activeRoutes(st engine.State) []string {
	var out []string
	for _, r := range st.Routes {
		if r.State == planner.StateActive {
			out = append(out, r.Hostname+" "+r.Service)
		}
	}
	slices.Sort(out)
	return out
}

// settle waits until a cycle has nothing left to do, so that what a scenario
// takes as before is not still changing.
func (s *suite) settle(t testing.TB) {
	t.Helper()
	asked := time.Now()
	s.sync(t)
	s.waitState(t, "a cycle with nothing to do", 3*time.Minute, func(st engine.State) bool {
		return st.At.After(asked) && st.Complete && len(st.Actions) == 0
	})
}

// enforce leaves observe-only mode, unless it was left already.
func (s *suite) enforce(t testing.TB) {
	t.Helper()
	if s.state(t).Mode == engine.ModeEnforce {
		return
	}
	s.must(t, s.pco, "apply")
	s.waitState(t, "enforce mode", time.Minute, func(st engine.State) bool { return st.Mode == engine.ModeEnforce })
}

// tunnel is the tunnel of the install, once the daemon made it.
func (s *suite) tunnel(t testing.TB) string {
	t.Helper()
	var id string
	s.waitState(t, "the tunnel of the install", 2*time.Minute, func(st engine.State) bool {
		for _, tn := range st.Tunnels {
			if tn.Exists && tn.ID != "" {
				id = tn.ID
				return true
			}
		}
		return false
	})
	return id
}

// waitRecord waits until name has a CNAME to the tunnel, or none when tunnel
// is empty.
func (s *suite) waitRecord(t testing.TB, name, tunnel string, timeout time.Duration) {
	t.Helper()
	want := []string(nil)
	if tunnel != "" {
		want = []string{"CNAME " + tunnel + ".cfargotunnel.com"}
	}
	deadline := time.Now().Add(timeout)
	for {
		got := s.records(t, name)
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for the records of %s to be %v; they are %v", timeout, name, want, got)
		}
		time.Sleep(s.every())
	}
}

// waitIngress waits until the rule of host in the tunnel's configuration
// sends it to service.
func (s *suite) waitIngress(t testing.TB, tunnel, host, service string) []planner.IngressRule {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		rules := s.cloud.Ingress(t, tunnel)
		if r, _, ok := ruleOf(rules, host); ok && r.Service == service {
			return rules
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ingress does not send %s to %s: %+v", host, service, rules)
		}
		time.Sleep(s.every())
	}
}

// egress returns the state of the filter, from the first line of pco egress
// show, and its targets; ok says the command exited 0.
func (s *suite) egress(t testing.TB) (summary string, targets []string, ok bool) {
	t.Helper()
	out, err := s.run(s.pco, "egress", "show")
	require.Contains(t, []int{0, 1}, exitCode(err), "pco egress show: %v", err)
	in := false
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\n")
		switch {
		case summary == "":
			summary = line
		case line == "Targets:":
			in = true
		case in && strings.HasPrefix(line, "  "):
			targets = append(targets, strings.TrimSpace(line))
		default:
			in = false
		}
	}
	return summary, targets, err == nil
}

// waitTarget waits until the egress set has the target, or has it no more,
// looking every so often. Either needs the filter on and its table as pco
// loads it: a table that is not loaded has no target either.
func (s *suite) waitTarget(t testing.TB, target string, in bool, timeout time.Duration, every time.Duration) {
	t.Helper()
	began := time.Now()
	for {
		summary, targets, ok := s.egress(t)
		if ok && summary == filterOn && slices.Contains(targets, target) == in {
			return
		}
		if time.Since(began) > timeout {
			t.Fatalf("waited %s for %s to be in the egress set: %v; the filter: %q, exit 0: %v; the set: %v",
				timeout, target, in, summary, ok, targets)
		}
		time.Sleep(every)
	}
}

// requireNoTarget checks that the filter is on and that its set does not
// have the target.
func (s *suite) requireNoTarget(t testing.TB, target string) {
	t.Helper()
	summary, targets, ok := s.egress(t)
	require.True(t, ok && summary == filterOn, "the filter is on: %q, exit 0: %v", summary, ok)
	require.NotContains(t, targets, target)
}

// neighbourSeen watches the neighbour table of the node on the bridge, and
// sends the time it first gives addr the MAC mac.
func (s *suite) neighbourSeen(t testing.TB, addr, mac string) <-chan time.Time {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "ip", "monitor", "neigh", "dev", bridge)
	cmd.Env = s.childEnv()
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
	})
	seen := make(chan time.Time, 1)
	go func() {
		lines := bufio.NewScanner(out)
		for lines.Scan() {
			f := strings.Fields(strings.ToLower(lines.Text()))
			if len(f) > 0 && f[0] == addr && slices.Contains(f, mac) {
				seen <- time.Now()
				break
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	return seen
}

// events returns the events since a time.
func (s *suite) events(t testing.TB, since time.Time) []engine.Event {
	t.Helper()
	out := s.must(t, s.pco, "events", "--json", "--since", since.UTC().Format(time.RFC3339))
	var evs []engine.Event
	require.NoError(t, json.Unmarshal([]byte(out), &evs), out)
	return evs
}

// waitEvent waits for an event of a kind whose message has text.
func (s *suite) waitEvent(t testing.TB, since time.Time, kind, text string) engine.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		evs := s.events(t, since)
		if i := slices.IndexFunc(evs, func(ev engine.Event) bool {
			return ev.Kind == kind && strings.Contains(ev.Message, text)
		}); i >= 0 {
			return evs[i]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event with %q since %s; the events: %+v", kind, text, since, evs)
		}
		time.Sleep(time.Second)
	}
}
