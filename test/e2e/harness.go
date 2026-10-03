//go:build e2e

// Package e2e runs pco as it is installed on a Proxmox VE node, against a fake
// Cloudflare or the real one. README.md says how.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	bridge     = "vmbr1"
	nodeAddr   = "10.77.0.1"
	nodeCIDR   = nodeAddr + "/24"
	firstVMID  = 9100
	lastVMID   = 9199
	gateTag    = "cf-tunnel"
	dropInDir  = "/etc/systemd/system/pco.service.d"
	dropInFile = dropInDir + "/e2e.conf"
	overrideOf = "PCO_CLOUDFLARE_API_URL"
	// bridgeMarker says that the suite brought the bridge up, which
	// cleanup.sh takes down again when the suite could not.
	bridgeMarker = "/run/pco-e2e-" + bridge + "-was-down"
	cloudflared  = "/usr/bin/cloudflared"

	// The settings of the run: cycles as close as allowed, and the shortest
	// grace, so that a removal can be waited for.
	pollInterval = 5 * time.Second
	grace        = 30 * time.Second

	commandTimeout = 3 * time.Minute
)

// suite is the node the scenarios run on, with pco set up against the cloud.
type suite struct {
	cloud cloud
	fake  *fakeCloud // nil against the real API
	zone  string
	pco   string
	tmpl  string // the container template
	disk  string // the storage of the containers' disks
	dir   string // for the files of the run
	env   []string
	// setUp says that setup ran, and install is the id of the install it
	// made.
	setUp   bool
	install string
	guests  map[int]bool // made by the suite, destroyed at its end
	began   time.Time
	// bridgeDown says that the bridge was down before the run.
	bridgeDown bool
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// newSuite prepares the node: the cloud, the drop-in that points the daemon
// at the fake, the address of the node on the bridge of the guests, and pco
// set up. Everything is undone when t ends, whatever happened.
func newSuite(t *testing.T) *suite {
	s := &suite{
		pco:    envOr("PCO_E2E_PCO", "/usr/bin/pco"),
		tmpl:   envOr("PCO_E2E_TEMPLATE", "local:vztmpl/alpine-3.24-default_20260714_amd64.tar.xz"),
		disk:   envOr("PCO_E2E_STORAGE", "local-lvm"),
		dir:    t.TempDir(),
		guests: map[int]bool{},
		began:  time.Now(),
	}
	s.preflight(t)

	token, zone := os.Getenv("PCO_E2E_CF_TOKEN"), os.Getenv("PCO_E2E_ZONE")
	switch {
	case token != "" && zone != "":
		c, err := newRealCloud(t.Context(), token, zone)
		require.NoError(t, err)
		s.cloud, s.zone = c, zone
		t.Logf("against the real Cloudflare API, zone %s", zone)
	default:
		c, err := newFakeCloud()
		require.NoError(t, err)
		t.Cleanup(c.srv.stop)
		s.cloud, s.fake, s.zone, token = c, c, fakeZone, fakeToken
		s.env = []string{overrideOf + "=" + c.url()}
		t.Logf("against the fake Cloudflare at %s", c.url())
	}

	// Cleanups run last first: the guests go after pco is uninstalled, so
	// that their going away is nothing the daemon acts on.
	t.Cleanup(func() { s.removeAddress(t) })
	t.Cleanup(func() { s.removeDropIn(t) })
	t.Cleanup(func() { s.destroyGuests(t) })
	t.Cleanup(func() { s.uninstall(t) })
	t.Cleanup(func() { s.diagnose(t) })

	s.addAddress(t)
	if s.fake != nil {
		s.writeDropIn(t)
	}
	s.setup(t, token)
	return s
}

// preflight skips where the suite cannot run, and refuses a node where it
// would change what it did not make.
func (s *suite) preflight(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("the end-to-end suite runs as root on a Proxmox VE node")
	}
	for _, bin := range []string{"/usr/sbin/pct", s.pco} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("the end-to-end suite needs %s: %v", bin, err)
		}
	}
	if _, err := os.Stat(store.DefaultPaths().Cluster + "/meta/install.json"); err == nil {
		t.Fatal("pco is set up on this node already; the suite needs a node without it: run test/e2e/cleanup.sh")
	}
	out := s.must(t, "pct", "list")
	for line := range strings.Lines(out) {
		var id int
		if _, err := fmt.Sscan(line, &id); err == nil && id >= firstVMID && id <= lastVMID {
			t.Fatalf("container %d exists; the suite needs %d to %d free: run test/e2e/cleanup.sh", id, firstVMID, lastVMID)
		}
	}
	link := s.must(t, "ip", "-o", "link", "show", "dev", bridge)
	flags, _, _ := strings.Cut(link[strings.Index(link, "<")+1:], ">")
	s.bridgeDown = !slices.Contains(strings.Split(flags, ","), "UP")
	if addrs := s.must(t, "ip", "-4", "-o", "addr", "show", "dev", bridge); strings.Contains(addrs, nodeCIDR) {
		t.Fatalf("%s has %s already; the suite gives it that address and takes it away: run test/e2e/cleanup.sh", bridge, nodeCIDR)
	}
	s.must(t, "pvesm", "status", "--storage", s.disk)
	if _, err := os.Stat(cloudflared); err != nil && os.Getenv("PCO_E2E_CF_TOKEN") != "" {
		t.Fatalf("against the real API the connectors need %s: %v", cloudflared, err)
	}
}

// addAddress gives the node its address on the bridge, and brings the
// bridge up for the run when it is down.
func (s *suite) addAddress(t *testing.T) {
	if s.bridgeDown {
		require.NoError(t, os.WriteFile(bridgeMarker, nil, 0o600))
		s.must(t, "ip", "link", "set", "dev", bridge, "up")
	}
	s.must(t, "ip", "addr", "add", nodeCIDR, "dev", bridge)
}

func (s *suite) removeAddress(t *testing.T) {
	if _, err := s.run("ip", "addr", "del", nodeCIDR, "dev", bridge); err != nil {
		t.Errorf("removing %s from %s: %v", nodeCIDR, bridge, err)
	}
	if !s.bridgeDown {
		return
	}
	if _, err := s.run("ip", "link", "set", "dev", bridge, "down"); err != nil {
		t.Errorf("taking %s down again: %v", bridge, err)
		return
	}
	_ = os.Remove(bridgeMarker)
}

func (s *suite) writeDropIn(t *testing.T) {
	require.NoError(t, os.MkdirAll(dropInDir, 0o755))
	unit := "[Service]\nEnvironment=" + s.env[0] + "\n"
	require.NoError(t, os.WriteFile(dropInFile, []byte(unit), 0o644))
	s.must(t, "systemctl", "daemon-reload")
}

func (s *suite) removeDropIn(t *testing.T) {
	err := os.Remove(dropInFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removing the drop-in: %v", err)
	}
	_ = os.Remove(dropInDir) // only when it is empty: it may hold drop-ins of the admin
	if _, err := s.run("systemctl", "daemon-reload"); err != nil {
		t.Errorf("reloading systemd: %v", err)
	}
}

// setup runs pco setup with the token and gives the install the settings of
// the run.
func (s *suite) setup(t *testing.T, token string) {
	file := filepath.Join(s.dir, "cf-token")
	require.NoError(t, os.WriteFile(file, []byte(token+"\n"), 0o600))
	args := []string{"setup", "--yes", "--cf-token-file", file}
	if _, err := os.Stat(cloudflared); err != nil {
		// Setup would install it from Cloudflare's repository, which is more
		// than the run may change. The connectors then cannot start, which
		// against the fake they could not do anyway.
		t.Logf("%s is missing: the connectors do not run", cloudflared)
		args = append(args, "--skip-cloudflared")
	}
	s.setUp = true
	out, err := s.combined(s.pco, args...)
	t.Logf("pco setup:\n%s", out)
	require.NoError(t, err)

	st, err := store.Open(store.DefaultPaths())
	require.NoError(t, err)
	inst, found, err := st.Install()
	require.NoError(t, err)
	require.True(t, found, "setup made an install")
	s.install = inst.ID
	creds, err := st.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1, "setup stored the token")

	settings, err := st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly, "a new install only observes")
	settings.PollInterval = store.Duration(pollInterval)
	settings.Grace = store.Duration(grace)
	require.NoError(t, st.SaveSettings(settings))
	s.waitState(t, "the daemon's first cycle", time.Minute, func(st engine.State) bool { return !st.FinishedAt.IsZero() })
}

func (s *suite) uninstall(t *testing.T) {
	if !s.setUp {
		return
	}
	out, err := s.combined(s.pco, "uninstall", "--yes", "--purge-cloudflare")
	t.Logf("pco uninstall:\n%s", out)
	if err != nil {
		t.Errorf("pco uninstall: %v", err)
		return
	}
	if s.install == "" {
		return
	}
	if tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install)); found {
		t.Errorf("the tunnel %s is left at Cloudflare after the purge", tn.ID)
	}
	marker := planner.DNSMarker(s.install)
	for _, rec := range s.cloud.Records(t, s.zone) {
		if strings.HasPrefix(rec.Comment, marker) {
			t.Errorf("the record %s is left at Cloudflare after the purge", rec.Name)
		}
	}
}

// diagnose logs what helps to understand a failed run.
func (s *suite) diagnose(t *testing.T) {
	if !t.Failed() {
		return
	}
	since := s.began.Format("2006-01-02 15:04:05")
	out, _ := s.run("journalctl", "-u", "pco", "--since", since, "--no-pager", "-n", "200")
	t.Logf("journal of pco:\n%s", out)
	if s.fake != nil {
		calls := s.fake.f.Calls()
		t.Logf("the last calls of the fake:\n%s", strings.Join(calls[max(0, len(calls)-60):], "\n"))
	}
}

// run runs a command of the node and returns what it wrote to stdout; the
// error carries stderr.
func (s *suite) run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), s.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// combined runs a command and returns what it wrote to stdout and stderr.
func (s *suite) combined(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), s.env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (s *suite) must(t testing.TB, name string, args ...string) string {
	t.Helper()
	out, err := s.run(name, args...)
	require.NoError(t, err)
	return out
}

// exitCode is the status a command exited with, or -1 when it did not run.
func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

// state is the state of the daemon, as pco routes --json prints it.
func (s *suite) state(t testing.TB) engine.State {
	t.Helper()
	out := s.must(t, s.pco, "routes", "--json")
	var st engine.State
	require.NoError(t, json.Unmarshal([]byte(out), &st), out)
	return st
}

// tryState is state for a wait, which goes on while the daemon does not
// answer.
func (s *suite) tryState() (engine.State, bool) {
	out, err := s.run(s.pco, "routes", "--json")
	if err != nil {
		return engine.State{}, false
	}
	var st engine.State
	return st, json.Unmarshal([]byte(out), &st) == nil
}

func (s *suite) sync(t testing.TB) {
	t.Helper()
	s.must(t, s.pco, "sync")
}

// waitState polls the state until ok, and fails with the last one after
// timeout.
func (s *suite) waitState(t testing.TB, what string, timeout time.Duration, ok func(engine.State) bool) engine.State {
	t.Helper()
	var last engine.State
	deadline := time.Now().Add(timeout)
	for {
		if st, answered := s.tryState(); answered {
			last = st
			if ok(st) {
				return st
			}
		}
		if time.Now().After(deadline) {
			raw, _ := json.MarshalIndent(last, "", "  ")
			t.Fatalf("waited %s for %s; the last state:\n%s", timeout, what, raw)
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

// host is a hostname of the run in its zone.
func (s *suite) host(name string) string { return "e2e-" + name + "." + s.zone }

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

// records returns the records of the zone that name has.
func (s *suite) records(t testing.TB, name string) []string {
	t.Helper()
	var out []string
	for _, rec := range s.cloud.Records(t, s.zone) {
		if strings.EqualFold(rec.Name, name) {
			out = append(out, rec.Type+" "+rec.Content)
		}
	}
	return out
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
		time.Sleep(time.Second)
	}
}

// ruleOf returns the rule of the ingress for host.
func ruleOf(rules []planner.IngressRule, host string) (planner.IngressRule, int, bool) {
	i := slices.IndexFunc(rules, func(r planner.IngressRule) bool { return r.Hostname == host })
	if i < 0 {
		return planner.IngressRule{}, -1, false
	}
	return rules[i], i, true
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
// looking every so often.
func (s *suite) waitTarget(t testing.TB, target string, in bool, timeout time.Duration, every time.Duration) {
	t.Helper()
	began := time.Now()
	for {
		_, targets, _ := s.egress(t)
		if slices.Contains(targets, target) == in {
			return
		}
		if time.Since(began) > timeout {
			t.Fatalf("waited %s for %s to be in the egress set: %v; the set: %v", timeout, target, in, targets)
		}
		time.Sleep(every)
	}
}

// neighbourSeen watches the neighbour table of the node on the bridge, and
// sends the time it first gives addr the MAC mac.
func (s *suite) neighbourSeen(t testing.TB, addr, mac string) <-chan time.Time {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "ip", "monitor", "neigh", "dev", bridge)
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

// otherProblems returns the problems of st that the run does not cause
// itself: the line of the override is in every state.
func otherProblems(st engine.State) []string {
	return slices.DeleteFunc(slices.Clone(st.Problems), func(p string) bool { return strings.Contains(p, overrideOf) })
}
