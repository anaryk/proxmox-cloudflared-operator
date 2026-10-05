package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/api"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/doctor"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/testutil"
)

var update = flag.Bool("update", false, "write the golden files of the tests")

// t0 is the time of the tests; the zone shows in every time the commands print.
var (
	t0       = time.Date(2026, 10, 1, 12, 0, 0, 123_000_000, time.UTC)
	testZone = time.FixedZone("CEST", 2*60*60)
)

const cliVersion = "0.2.0"

// testEnv is a machine where it is 2026-10-01 12:00 UTC, in a zone two hours
// ahead, and where stdin is no terminal.
func testEnv() env {
	e := defaultEnv()
	e.now = func() time.Time { return t0 }
	e.loc = testZone
	e.version = cliVersion
	e.stdinTerminal = func(io.Reader) (int, bool) { return 0, false }
	e.readPassword = func(int) ([]byte, error) { panic("no terminal in this test") }
	e.getenv = func(string) string { return "" }
	e.profileFile = "/nonexistent/pco/profile"
	return e
}

// fakeEngine is the engine behind a daemon of a test: it answers from its
// fields and remembers what it was asked.
type fakeEngine struct {
	mu     sync.Mutex
	state  engine.State
	events []engine.Event
	since  time.Time // what the last request for the events asked for

	addView, checkView engine.CredentialView
	addErr, checkErr   error
	applyErr, adoptErr error
	removeErr          error
	applied            *engine.ApplyResult // what Apply answers instead of what the state offers

	claims                            []engine.ClaimView
	approvals                         []engine.ApprovalView
	steps                             []doctor.Step
	findings                          []doctor.Finding
	resolveErr, guestErr, diagnoseErr error
	mode                              string // the admission mode an approval answers with; "approve" when empty

	calls []string
}

func (f *fakeEngine) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeEngine) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeEngine) State() engine.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeEngine) Events(since time.Time) []engine.Event {
	f.record("events")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.since = since
	return f.events
}

func (f *fakeEngine) lastSince() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since
}

// Credentials answers with the credentials of the state.
func (f *fakeEngine) Credentials() ([]engine.CredentialView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Credentials, nil
}

func (f *fakeEngine) Trigger() { f.record("sync") }

// Apply answers as the engine does: a confirmation with another offer than
// that of the state is refused, and one with it accepts what the state shows.
func (f *fakeEngine) Apply(_ context.Context, confirmDeletes bool, offer string) (engine.ApplyResult, error) {
	call := "apply confirmDeletes=" + boolText(confirmDeletes)
	if offer != "" {
		call += " offer=" + offer
	}
	f.record(call)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.applyErr != nil:
		return engine.ApplyResult{}, f.applyErr
	case f.applied != nil:
		return *f.applied, nil
	case confirmDeletes && offer != f.state.Offer:
		return engine.ApplyResult{}, errOfferChanged
	}
	res := engine.ApplyResult{LeftObserveOnly: f.state.Mode == "observe", Accepted: []engine.Waiting{}}
	if confirmDeletes {
		res.Accepted = append(res.Accepted, f.state.Waiting...)
	}
	return res, nil
}

// errOfferChanged is how the engine refuses a confirmation of what no longer
// waits.
var errOfferChanged = fmt.Errorf("%w: what waits for a confirmation changed since it was shown; look again and repeat", engine.ErrRefused)

func (f *fakeEngine) Adopt(_ context.Context, name string) error {
	f.record("adopt " + name)
	return f.adoptErr
}

// RotateTunnel is root's alone, which no test is: the tests of pco tunnel
// rotate answer from a daemon of their own.
func (f *fakeEngine) RotateTunnel(_ context.Context, account string) (engine.TunnelRotation, error) {
	f.record("rotate " + account)
	return engine.TunnelRotation{}, fmt.Errorf("%w: not in this test", engine.ErrRefused)
}

func (f *fakeEngine) AddCredential(_ context.Context, label, token string) (engine.CredentialView, error) {
	f.record("add " + label + " " + token)
	return f.addView, f.addErr
}

func (f *fakeEngine) CheckCredential(_ context.Context, id string, deep bool) (engine.CredentialView, error) {
	f.record("check " + id + " deep=" + boolText(deep))
	return f.checkView, f.checkErr
}

func (f *fakeEngine) RemoveCredential(_ context.Context, id string) error {
	f.record("remove " + id)
	return f.removeErr
}

func (f *fakeEngine) Claims() ([]engine.ClaimView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.claims, nil
}

func (f *fakeEngine) ResolveClaim(_ context.Context, hostname, owner string) error {
	f.record("resolve " + hostname + " " + owner)
	return f.resolveErr
}

func (f *fakeEngine) Approvals() ([]engine.ApprovalView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.approvals, nil
}

func (f *fakeEngine) ApproveGuest(_ context.Context, owner, identity string) (engine.Approval, error) {
	call := "approve " + owner
	if identity != "" {
		call += " identity=" + identity
	}
	f.record(call)
	if f.guestErr != nil {
		return engine.Approval{}, f.guestErr
	}
	mode := f.mode
	if mode == "" {
		mode = "approve"
	}
	return engine.Approval{Owner: owner, Identity: "uuid:" + strings.TrimPrefix(owner, "qemu/"), Mode: mode}, nil
}

func (f *fakeEngine) RevokeGuest(_ context.Context, owner string) error {
	f.record("revoke " + owner)
	return f.guestErr
}

func (f *fakeEngine) Diagnose(_ context.Context, hostname string) ([]doctor.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.steps, f.diagnoseErr
}

func (f *fakeEngine) Doctor(context.Context) []doctor.Finding {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.findings
}

func (f *fakeEngine) PollInterval() time.Duration { return 10 * time.Second }

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// serveFake runs the API of the daemon over the engine on a socket of its own
// until the test ends, and returns the path of the socket.
func serveFake(t *testing.T, e *fakeEngine) string {
	t.Helper()
	socket := filepath.Join(testutil.ShortDir(t), "pco", "pco.sock")
	srv := api.New(e, "1.2.3", []uint32{uint32(os.Getuid())}, zerolog.Nop())
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	srv.OnListening(func() { close(ready) })
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, socket, os.Getgid()) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("the API ended before it listened: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return socket
}

// serveRaw runs a daemon that answers every request from replies, by path, with
// a status and a body; a path it has no reply for is an unknown route.
func serveRaw(t *testing.T, replies map[string]rawReply) string {
	t.Helper()
	socket := filepath.Join(testutil.ShortDir(t), "pco.sock")
	ln, err := net.Listen("unix", socket)
	require.NoError(t, err)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply, ok := replies[r.Method+" "+r.URL.Path]
		if !ok {
			reply = rawReply{status: http.StatusNotFound, body: `{"error":"no such route","code":"no_route"}`}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return socket
}

type rawReply struct {
	status int
	body   string
}

// result is what a command did.
type result struct {
	out    string // standard output
	errOut string // standard error
	err    error  // what Execute returned, which main turns into the exit code
}

// runner runs the commands of pco against a socket.
type runner struct {
	t      *testing.T
	socket string
	env    env
}

func newRunner(t *testing.T, socket string) *runner {
	return &runner{t: t, socket: socket, env: testEnv()}
}

// run runs pco with args, reading stdin from in.
func (r *runner) run(in string, args ...string) result {
	r.t.Helper()
	return r.runReader(strings.NewReader(in), args...)
}

// runReader runs pco with args, reading stdin from in.
func (r *runner) runReader(in io.Reader, args ...string) result {
	r.t.Helper()
	cmd := newRootCmdWith(r.env)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(in)
	cmd.SetArgs(append([]string{"--socket", r.socket}, args...))
	err := cmd.ExecuteContext(r.t.Context())
	return result{out: out.String(), errOut: errOut.String(), err: err}
}

// tty makes stdin a terminal, which the commands that ask a question need.
func (r *runner) tty() *runner {
	r.env.stdinTerminal = func(io.Reader) (int, bool) { return 0, true }
	return r
}

// unreadable is a stdin that fails the test when it is read.
type unreadable struct{ t *testing.T }

func (u unreadable) Read([]byte) (int, error) {
	u.t.Helper()
	u.t.Fatal("stdin was read")
	return 0, io.EOF
}

// requireGolden compares got with a file of testdata; run the tests with
// -update to write them.
func requireGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), got, "the output changed; run the test with -update when that is intended")
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
