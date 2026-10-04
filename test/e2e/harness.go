//go:build e2e

// Package e2e runs pco as it is installed on a Proxmox VE node, against a fake
// Cloudflare or the real one. README.md says how.
package e2e

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/connector"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	bridge      = "vmbr1"
	nodeAddr    = "10.77.0.1"
	nodeCIDR    = nodeAddr + "/24"
	firstVMID   = 9100
	lastVMID    = 9199
	gateTag     = "cf-tunnel"
	dropInDir   = "/etc/systemd/system/pco.service.d"
	dropInFile  = dropInDir + "/e2e.conf"
	overrideOf  = "PCO_CLOUDFLARE_API_URL"
	cloudflared = "/usr/bin/cloudflared"

	// The token of the real API is read from the file tokenFileOf names,
	// never from the environment or the command line; no command the suite
	// runs gets a variable that begins with tokenVars.
	tokenFileOf = "PCO_E2E_CF_TOKEN_FILE"
	tokenVars   = "PCO_E2E_CF_TOKEN"

	// The settings of the run: cycles as close as allowed, and the shortest
	// grace, so that a removal can be waited for.
	pollInterval = 5 * time.Second
	grace        = 30 * time.Second
	// outage is how long S13 keeps Proxmox and Cloudflare away: longer than
	// the grace, which must not run out on what the daemon cannot see.
	outage = grace + 5*time.Second

	// A purge that fails at the end of the run is tried again this often,
	// this far apart.
	purgeAttempts = 3
	purgeRetry    = 20 * time.Second

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
// set up. Everything is undone when t ends, whatever happened, and recorded
// under markerDir for cleanup.sh until then.
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
	token := s.chooseCloud(t)
	s.own(t)

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

// chooseCloud starts the fake, or takes the real API when a token file and
// a zone are given, and returns the token for setup.
func (s *suite) chooseCloud(t *testing.T) string {
	if os.Getenv(tokenVars) != "" {
		t.Fatalf("%s is not read: put the token in a file and name it with %s, which keeps it off the command line", tokenVars, tokenFileOf)
	}
	file, zone := os.Getenv(tokenFileOf), strings.ToLower(strings.TrimSpace(os.Getenv("PCO_E2E_ZONE")))
	switch {
	case file == "" && zone == "":
		c, err := newFakeCloud()
		require.NoError(t, err)
		t.Cleanup(c.srv.stop)
		s.cloud, s.fake, s.zone = c, c, fakeZone
		s.env = []string{overrideOf + "=" + c.url()}
		t.Logf("against the fake Cloudflare at %s", c.url())
		return fakeToken
	case file == "" || zone == "":
		t.Fatalf("against the real API the suite needs both %s and PCO_E2E_ZONE", tokenFileOf)
	}
	if _, err := os.Stat(cloudflared); err != nil {
		t.Fatalf("against the real API the connectors need %s: %v", cloudflared, err)
	}
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	token := strings.TrimSpace(string(raw))
	require.NotEmpty(t, token, "%s is empty", file)
	c, err := newRealCloud(t.Context(), token, zone)
	require.NoError(t, err)
	s.cloud, s.zone = c, zone
	t.Logf("against the real Cloudflare API, zone %s", zone)
	return token
}

// setup runs pco setup with the token and gives the install the settings of
// the run. The token is on disk only while setup runs.
func (s *suite) setup(t *testing.T, token string) {
	marker(t, tokenFile, token+"\n")
	t.Cleanup(func() { unmark(t, tokenFile) })
	args := []string{"setup", "--yes", "--cf-token-file", tokenFile}
	if _, err := os.Stat(cloudflared); err != nil {
		// Setup would install it from Cloudflare's repository, which is more
		// than the run may change. The connectors then cannot start, which
		// against the fake they could not do anyway.
		t.Logf("%s is missing: the connectors do not run", cloudflared)
		args = append(args, "--skip-cloudflared")
	}
	s.setUp = true
	out, err := s.combined(s.pco, args...)
	unmark(t, tokenFile)
	t.Logf("pco setup:\n%s", out)
	require.NoError(t, err)

	st, err := store.Open(store.DefaultPaths())
	require.NoError(t, err)
	inst, found, err := st.Install()
	require.NoError(t, err)
	require.True(t, found, "setup made an install")
	s.install = inst.ID
	s.own(t)
	creds, err := st.Credentials()
	require.NoError(t, err)
	require.Len(t, creds, 1, "setup stored the token")

	settings, err := st.Settings()
	require.NoError(t, err)
	require.True(t, settings.ObserveOnly, "a new install only observes")
	settings.PollInterval = store.Duration(pollInterval)
	settings.Grace = store.Duration(grace)
	require.NoError(t, st.SaveSettings(settings))
	first := s.waitState(t, "the daemon's first cycle", time.Minute, func(st engine.State) bool { return !st.FinishedAt.IsZero() })
	s.requireOverrideLine(t, first)
}

// requireOverrideLine checks that a state shows the override of the API as a
// problem against the fake, and that nothing is overridden against
// Cloudflare.
func (s *suite) requireOverrideLine(t *testing.T, st engine.State) {
	t.Helper()
	if s.fake == nil {
		require.Empty(t, slices.DeleteFunc(slices.Clone(st.Problems), func(p string) bool { return !strings.Contains(p, overrideOf) }))
		return
	}
	line := "the Cloudflare API is overridden to " + s.fake.url() + " (" + overrideOf + "); this is for tests only"
	require.Contains(t, st.Problems, line)
}

// uninstall removes pco and purges what the install has at Cloudflare. A
// purge that fails is tried again, and what is left is named.
func (s *suite) uninstall(t *testing.T) {
	if !s.setUp {
		return
	}
	var err error
	for attempt := 1; attempt <= purgeAttempts; attempt++ {
		if attempt > 1 {
			time.Sleep(purgeRetry)
		}
		var out string
		out, err = s.combined(s.pco, "uninstall", "--yes", "--purge-cloudflare")
		t.Logf("pco uninstall --purge-cloudflare, attempt %d of %d:\n%s", attempt, purgeAttempts, out)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Errorf("pco uninstall: %v; %s names the install, and cleanup.sh purges it again", err, ownedFile)
	}
	if s.install == "" {
		return
	}
	if left := s.leftAtCloudflare(t); len(left) > 0 {
		t.Errorf("left at Cloudflare after the purge:\n  %s", strings.Join(left, "\n  "))
	}
}

// leftAtCloudflare names what the install and the suite itself have at
// Cloudflare.
func (s *suite) leftAtCloudflare(t testing.TB) []string {
	t.Helper()
	var left []string
	if tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install)); found {
		left = append(left, fmt.Sprintf("the tunnel %s (%s)", tn.Name, tn.ID))
	}
	for _, prefix := range []string{planner.DNSMarker(s.install), foreignComment} {
		for _, rec := range s.cloud.Records(t, s.zone, cfapi.RecordFilter{CommentPrefix: prefix}) {
			left = append(left, fmt.Sprintf("the record %s %s %s (%s)", rec.Type, rec.Name, rec.Content, rec.ID))
		}
	}
	return left
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

// childEnv is the environment of a command the suite runs: the suite's own
// without the variables of the token, and the override of the run.
func (s *suite) childEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, tokenVars) })
	return append(env, s.env...)
}

// run runs a command of the node and returns what it wrote to stdout; the
// error carries stderr.
func (s *suite) run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = s.childEnv()
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
	cmd.Env = s.childEnv()
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

// every is how often a wait asks Cloudflare: the real API no more than every
// 3 seconds.
func (s *suite) every() time.Duration {
	if s.fake != nil {
		return time.Second
	}
	return 3 * time.Second
}

// host is a hostname of the run in its zone.
func (s *suite) host(name string) string { return "e2e-" + name + "." + s.zone }

// in puts the zone of the run into the notes of g.
func (s *suite) in(g guest) guest {
	g.notes = strings.ReplaceAll(g.notes, "ZONE", s.zone)
	return g
}

// s1Guest is the guest of S1, which the scenarios that look at the whole node
// make when S1 did not run.
func (s *suite) s1Guest() guest {
	return s.in(guest{vmid: 9101, notes: "cf-tunnel: e2e-s1.ZONE -> :8080"})
}

// ensureS1 makes the guest of S1 unless the run has it, and waits until it is
// served and until nothing is left to do.
func (s *suite) ensureS1(t *testing.T) guest {
	t.Helper()
	g := s.s1Guest()
	if !s.guests[g.vmid] {
		s.create(t, g)
	}
	s.enforce(t)
	s.waitRoute(t, s.host("s1"), planner.StateActive)
	s.settle(t)
	return g
}

// records returns the records the zone has of name.
func (s *suite) records(t testing.TB, name string) []string {
	t.Helper()
	var out []string
	for _, rec := range s.cloud.Records(t, s.zone, cfapi.RecordFilter{Name: name}) {
		out = append(out, rec.Type+" "+rec.Content)
	}
	return out
}

// runRecords returns the records of the run's names that pco made for the
// install and that the suite made itself, without the time of their last
// change, so that two reads compare.
func (s *suite) runRecords(t testing.TB) []cfapi.Record {
	t.Helper()
	var out []cfapi.Record
	for _, prefix := range []string{planner.DNSMarker(s.install), foreignComment} {
		for _, rec := range s.cloud.Records(t, s.zone, cfapi.RecordFilter{CommentPrefix: prefix}) {
			if strings.HasPrefix(rec.Name, "e2e-") || strings.Contains(rec.Name, ".e2e-") {
				rec.ModifiedOn = time.Time{}
				out = append(out, rec)
			}
		}
	}
	slices.SortFunc(out, func(a, b cfapi.Record) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Type, b.Type), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// ruleOf returns the rule of the ingress for host.
func ruleOf(rules []planner.IngressRule, host string) (planner.IngressRule, int, bool) {
	i := slices.IndexFunc(rules, func(r planner.IngressRule) bool { return r.Hostname == host })
	if i < 0 {
		return planner.IngressRule{}, -1, false
	}
	return rules[i], i, true
}

// otherProblems returns the problems of st that the run does not cause
// itself: the line of the override is in every state against the fake.
func otherProblems(st engine.State) []string {
	return slices.DeleteFunc(slices.Clone(st.Problems), func(p string) bool { return strings.Contains(p, overrideOf) })
}

// requireConnector checks the connector of the tunnel: its unit is enabled
// and runs with the token of the tunnel. Against the fake it cannot connect,
// so it is never ready; against Cloudflare it has to be. Neither token is
// ever printed.
func (s *suite) requireConnector(t *testing.T, tunnel string) {
	t.Helper()
	unit := "pco-cloudflared@" + tunnel + ".service"
	enabled, _ := s.run("systemctl", "is-enabled", unit)
	require.Equal(t, "enabled", strings.TrimSpace(enabled), unit)
	token, err := os.ReadFile("/var/lib/pco/tunnels/" + tunnel + ".token")
	require.NoError(t, err)
	same := s.cloud.RunToken(t, tunnel) == strings.TrimSpace(string(token))
	require.True(t, same, "the token file of the connector of %s does not hold the run token of the tunnel", tunnel)

	st := s.waitState(t, "the connector in the state", time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == tunnel })
	})
	i := slices.IndexFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == tunnel })
	conn := st.Connectors[i]
	require.Equal(t, s.install, conn.Install)
	if s.fake != nil {
		require.False(t, conn.Ready, "a connector with the token of the fake does not connect")
		return
	}
	s.waitState(t, "the connector to be ready", 2*time.Minute, func(st engine.State) bool {
		return slices.ContainsFunc(st.Connectors, func(c connector.Status) bool { return c.TunnelID == tunnel && c.Ready })
	})
}

// requireServed fetches the hostname through Cloudflare.
func (s *suite) requireServed(t *testing.T, host, body string) {
	t.Helper()
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + host + "/")
		if err == nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			_ = resp.Body.Close()
			last = fmt.Sprintf("%d %s", resp.StatusCode, raw)
			if resp.StatusCode == http.StatusOK && strings.TrimSpace(string(raw)) == body {
				return
			}
		} else {
			last = err.Error()
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("https://%s/ did not answer %q; the last answer: %s", host, body, last)
}

// before is what S13 expects to find unchanged after each outage.
type before struct {
	tunnel  string
	active  []string
	records []cfapi.Record
	ingress []planner.IngressRule
}

func (s *suite) takeBefore(t *testing.T) before {
	t.Helper()
	tunnel := s.tunnel(t)
	return before{tunnel: tunnel, active: activeRoutes(s.state(t)), records: s.runRecords(t), ingress: s.cloud.Ingress(t, tunnel)}
}

// requireSame checks that the routes, the records and the ingress are as they
// were before an outage.
func (s *suite) requireSame(t *testing.T, b before) {
	t.Helper()
	s.waitState(t, "the routes that were active", 2*time.Minute, func(st engine.State) bool {
		return slices.Equal(activeRoutes(st), b.active)
	})
	require.Equal(t, b.records, s.runRecords(t))
	require.Equal(t, b.ingress, s.cloud.Ingress(t, b.tunnel))
	tn, found := s.cloud.Tunnel(t, planner.TunnelName(s.install))
	require.True(t, found)
	require.Equal(t, b.tunnel, tn.ID)
	enabled, _ := s.run("systemctl", "is-enabled", "pco-cloudflared@"+b.tunnel+".service")
	require.Equal(t, "enabled", strings.TrimSpace(enabled))
}

// holdThrough keeps an outage up for longer than the grace, and checks
// meanwhile that nothing changes at Cloudflare.
func (s *suite) holdThrough(t *testing.T, b before) {
	t.Helper()
	for until := time.Now().Add(outage); time.Now().Before(until); time.Sleep(s.every()) {
		require.Equal(t, b.records, s.runRecords(t))
		require.Equal(t, b.ingress, s.cloud.Ingress(t, b.tunnel))
	}
}

// killInCycle kills the daemon with SIGKILL in a cycle, and returns when.
// Against the fake the cycle is caught in its read of the tunnel's
// configuration, which only a cycle makes, and the read is answered after
// the kill. Against Cloudflare the kill follows a requested cycle closely,
// which lands in it most of the time.
func (s *suite) killInCycle(t *testing.T) time.Time {
	t.Helper()
	if s.fake != nil {
		reached, release := s.fake.srv.hold(func(r *http.Request) bool {
			return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/configurations")
		})
		defer release()
		s.sync(t)
		select {
		case <-reached:
		case <-time.After(2 * time.Minute):
			t.Fatal("no cycle read the configuration of the tunnel")
		}
	} else {
		s.sync(t)
		time.Sleep(100 * time.Millisecond)
	}
	killed := time.Now()
	s.must(t, "systemctl", "kill", "--signal=SIGKILL", "pco.service")
	return killed
}
