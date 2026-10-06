package doctor

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
)

// logBuffer keeps what a runner logs, for reading while it runs.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimSpace(b.buf.String()), "\n")
}

// countingClock is a clock that says on calls when it is read: a caller of
// the runner reads it once, under the lock, to decide whether it runs.
type countingClock struct {
	testClock
	calls chan struct{}
}

func newCountingClock() *countingClock {
	return &countingClock{testClock: testClock{t: now}, calls: make(chan struct{}, 64)}
}

func (c *countingClock) now() time.Time {
	c.calls <- struct{}{}
	return c.testClock.now()
}

// panicking is a state function that panics the first time, once the test
// lets it, and answers st after that.
type panicking struct {
	st      engine.State
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (p *panicking) state() engine.State {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		<-p.release
		panic("state: index out of range")
	}
	return p.st
}

func (p *panicking) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// A diagnosis that panics ends for every caller that shared it, with an
// error, and is logged once with its stack; the next caller runs it anew
// rather than wait for a run that never ends.
func TestADiagnosisThatPanicsEndsForEveryone(t *testing.T) {
	o := newOrigin(t, false, nil)
	p := &panicking{st: servedBy(t, o.Server, "http"), release: make(chan struct{})}
	clock := newCountingClock()
	logs := &logBuffer{}
	r := NewRunner(p.state, healthyEnv(), nil, clock.now, zerolog.New(logs))

	errs := make(chan error, 2)
	go func() { _, err := r.Diagnose(t.Context(), www); errs <- err }()
	<-clock.calls
	go func() { _, err := r.Diagnose(t.Context(), www); errs <- err }()
	<-clock.calls // the second caller found the run going and waits for it
	close(p.release)

	for range 2 {
		require.EqualError(t, <-errs, "the diagnosis of www.example.com stopped on an internal error; the daemon log has the details")
	}
	lines := logs.lines()
	require.Len(t, lines, 1, "logged once")
	require.Contains(t, lines[0], "the diagnosis of www.example.com panicked")
	require.Contains(t, lines[0], "state: index out of range")
	require.Contains(t, lines[0], "runtime/debug.Stack", "with the stack")

	steps, err := r.Diagnose(t.Context(), www)
	require.NoError(t, err, "the next caller runs it anew")
	require.Equal(t, Step{Name: "http", Level: LevelOK, Detail: "the origin answered 200 OK"}, steps[7])
	require.Equal(t, 2, p.count())
}

// A doctor run that panics ends for every caller that shared it, with a
// finding that says so, and the next caller runs it anew.
func TestADoctorRunThatPanicsEndsForEveryone(t *testing.T) {
	p := &panicking{st: healthyState(), release: make(chan struct{})}
	clock := newCountingClock()
	logs := &logBuffer{}
	r := NewRunner(p.state, healthyEnv(), nil, clock.now, zerolog.New(logs))

	results := make(chan []Finding, 2)
	go func() { results <- r.Doctor(t.Context()) }()
	<-clock.calls
	go func() { results <- r.Doctor(t.Context()) }()
	<-clock.calls
	close(p.release)

	for range 2 {
		require.Equal(t, []Finding{{Check: "doctor", Level: LevelFail,
			Detail: "the doctor run stopped on an internal error; the daemon log has the details",
			Fix:    "journalctl -u pco shows where it stopped"}}, <-results)
	}
	lines := logs.lines()
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "the doctor run panicked")

	require.False(t, Failed(r.Doctor(t.Context())), "the next caller runs it anew")
	require.Equal(t, 2, p.count())
}

// A result is kept from the time it was made on: a clock that steps back
// does not keep it for longer.
func TestAClockThatStepsBackEndsAKeptResult(t *testing.T) {
	o := newOrigin(t, false, nil)
	st := servedBy(t, o.Server, "http")
	clock := &testClock{t: now}
	r := NewRunner(func() engine.State { return st }, healthyEnv(), nil, clock.now, zerolog.Nop())

	_, err := r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	clock.advance(-time.Hour)
	_, err = r.Diagnose(t.Context(), www)
	require.NoError(t, err)

	require.Len(t, o.requests(), 2, "asked again after the step back")
	_, err = r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	require.Len(t, o.requests(), 2, "and kept from then on")
}

// The diagnoses of hostnames nobody asks about any more are not kept.
func TestOldDiagnosesAreForgotten(t *testing.T) {
	o := newOrigin(t, false, nil)
	st := servedBy(t, o.Server, "http")
	clock := &testClock{t: now}
	r := NewRunner(func() engine.State { return st }, healthyEnv(), nil, clock.now, zerolog.Nop())

	_, err := r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	clock.advance(keepFor)
	_, err = r.Diagnose(t.Context(), "api.example.com")
	require.ErrorIs(t, err, engine.ErrNotFound)

	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotContains(t, r.diagnoses, www)
	require.Contains(t, r.diagnoses, "api.example.com")
}

// The caller that started a diagnosis may leave: the run goes on for those
// that share it.
func TestACallerThatLeavesDoesNotCutTheDiagnosisShort(t *testing.T) {
	arrived, release := make(chan struct{}, 1), make(chan struct{})
	o := newOrigin(t, false, func(http.ResponseWriter, *http.Request) {
		arrived <- struct{}{}
		<-release
	})
	st := servedBy(t, o.Server, "http")
	r := NewRunner(func() engine.State { return st }, healthyEnv(), nil, (&testClock{t: now}).now, zerolog.Nop())
	ctx, cancel := context.WithCancel(t.Context())
	left := make(chan struct{})
	go func() {
		defer close(left)
		_, _ = r.Diagnose(ctx, www)
	}()
	<-arrived

	cancel()
	close(release)
	<-left

	steps, err := r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	require.Equal(t, Step{Name: "http", Level: LevelOK, Detail: "the origin answered 200 OK"}, steps[7])
	require.Len(t, o.requests(), 1, "the run that was kept")
}

// leavingEnv is an Env whose cloudflared answers when the test lets it, and
// notes whether the run was cut short by then.
type leavingEnv struct {
	*fakeEnv
	arrived chan struct{}
	release chan struct{}
	cut     error
}

func (l *leavingEnv) CloudflaredVersion(ctx context.Context) (string, error) {
	l.arrived <- struct{}{}
	<-l.release
	l.cut = ctx.Err()
	return l.fakeEnv.CloudflaredVersion(ctx)
}

// The same holds for a doctor run.
func TestACallerThatLeavesDoesNotCutTheDoctorRunShort(t *testing.T) {
	env := &leavingEnv{fakeEnv: healthyEnv(), arrived: make(chan struct{}, 1), release: make(chan struct{})}
	r := NewRunner(healthyState, env, nil, (&testClock{t: now}).now, zerolog.Nop())
	ctx, cancel := context.WithCancel(t.Context())
	left := make(chan struct{})
	go func() {
		defer close(left)
		r.Doctor(ctx)
	}()
	<-env.arrived

	cancel()
	close(env.release)
	<-left

	require.NoError(t, env.cut)
	require.False(t, Failed(r.Doctor(t.Context())))
}

// A caller that names the holder it saw gets the chain of that holder or a
// refusal, from the same state: never the chain of a route that took the
// hostname over in between, nor one kept from before.
func TestADiagnosisOfTheHolderTheCallerSaw(t *testing.T) {
	o := newOrigin(t, false, nil)
	var (
		mu sync.Mutex
		st = servedBy(t, o.Server, "http")
	)
	state := func() engine.State {
		mu.Lock()
		defer mu.Unlock()
		return st
	}
	clock := &testClock{t: now}
	r := NewRunner(state, healthyEnv(), nil, clock.now, zerolog.Nop())

	steps, err := r.Diagnose(WithHolder(t.Context(), "qemu/101"), www)
	require.NoError(t, err)
	require.Equal(t, "qemu/101 (web-1) holds it; state active", steps[0].Detail)

	// qemu/102 holds it now: once the chain of qemu/101 is no longer kept, a
	// caller who saw qemu/101 is refused. While it is kept, such a caller
	// gets that chain, which is of the route it named.
	mu.Lock()
	st.Routes[0].Owner, st.Routes[0].Guest = "qemu/102", &engine.GuestView{GuestRef: model.GuestRef{Kind: model.KindQEMU, VMID: 102}, Name: "web-2"}
	mu.Unlock()
	steps, err = r.Diagnose(WithHolder(t.Context(), "qemu/101"), www)
	require.NoError(t, err)
	require.Equal(t, "qemu/101 (web-1) holds it; state active", steps[0].Detail)
	clock.advance(keepFor)
	_, err = r.Diagnose(WithHolder(t.Context(), "qemu/101"), www)
	require.ErrorIs(t, err, ErrHolderChanged)
	require.EqualError(t, err, "the holder changed: www.example.com is no longer held by qemu/101")

	// One who saw qemu/102 gets its chain, not the one kept of qemu/101.
	steps, err = r.Diagnose(WithHolder(t.Context(), "qemu/102"), www)
	require.NoError(t, err)
	require.Equal(t, "qemu/102 (web-2) holds it; state active", steps[0].Detail)

	// Without a holder named, whoever holds it now.
	steps, err = r.Diagnose(t.Context(), www)
	require.NoError(t, err)
	require.Equal(t, "qemu/102 (web-2) holds it; state active", steps[0].Detail)
	require.Len(t, o.requests(), 3)

	holder, ok := ExpectedHolder(WithHolder(t.Context(), "qemu/102"))
	require.True(t, ok)
	require.Equal(t, "qemu/102", holder)
	_, ok = ExpectedHolder(t.Context())
	require.False(t, ok)
}
