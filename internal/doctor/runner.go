package doctor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

// keepFor is how long the result of a doctor run, or of the diagnosis of a
// hostname, answers the requests that follow it.
const keepFor = 5 * time.Second

// errStopped is the end of a run that panicked, for those that waited for it.
var errStopped = errors.New("stopped on an internal error; the daemon log has the details")

// Runner runs the doctor and the diagnosis for the API, against the state the
// engine shows when it is asked. It runs one doctor at a time and one
// diagnosis of a hostname at a time, and a result answers for keepFor: whoever
// may use the socket cannot make the daemon ask an origin, or start
// cloudflared, in a loop. Requests that come while one runs wait for it.
type Runner struct {
	state func() engine.State
	env   Env
	httpc *http.Client
	now   func() time.Time
	log   zerolog.Logger

	mu        sync.Mutex
	diagnoses map[string]*flight[[]Step] // by hostname
	doctor    *flight[[]Finding]
}

// NewRunner returns a runner. httpc lends the diagnosis the certificate
// authorities it trusts; nil trusts those of the system. A run that panics is
// logged to log.
func NewRunner(state func() engine.State, env Env, httpc *http.Client, now func() time.Time, log zerolog.Logger) *Runner {
	return &Runner{state: state, env: env, httpc: httpc, now: now, log: log, diagnoses: make(map[string]*flight[[]Step])}
}

// flight is one run and, once it is done, its result.
type flight[T any] struct {
	done chan struct{}
	// Set under the lock of the runner before done is closed.
	finished bool
	failed   bool // it panicked: its result is for those that waited for it only
	at       time.Time
	val      T
	err      error
}

// fresh reports whether f still answers at now: it runs, or it ended without
// a panic less than keepFor before now. The caller holds the lock of the
// runner.
func (f *flight[T]) fresh(now time.Time) bool {
	return f != nil && (!f.finished || !f.failed && now.Sub(f.at) < keepFor && !now.Before(f.at))
}

// ErrHolderChanged is a diagnosis refused because the route that holds the
// hostname is not the one the caller named any more.
var ErrHolderChanged = errors.New("the holder changed")

type holderKey struct{}

// WithHolder names the owner of the route the caller expects to hold the
// hostname it diagnoses: the web interface lets a reader diagnose only the
// route of a guest the reader sees, and the daemon must not walk another's
// that took the hostname over in between.
func WithHolder(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, holderKey{}, owner)
}

// ExpectedHolder is the owner WithHolder named, if any.
func ExpectedHolder(ctx context.Context) (string, bool) {
	owner, ok := ctx.Value(holderKey{}).(string)
	return owner, ok
}

// Diagnose walks the chain of the route of a hostname. With a holder named
// (WithHolder), it walks the route of that owner or refuses with
// ErrHolderChanged, deciding on the state it walks, and shares the run only
// with callers that named the same.
func (r *Runner) Diagnose(ctx context.Context, name string) ([]Step, error) {
	host, err := hostname.Normalize(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrInvalid, err)
	}
	owner, named := ExpectedHolder(ctx)
	key := host
	if named {
		key = host + " " + owner
	}
	r.mu.Lock()
	f := r.diagnoses[key]
	lead := !f.fresh(r.now())
	if lead {
		maps.DeleteFunc(r.diagnoses, func(_ string, old *flight[[]Step]) bool { return !old.fresh(r.now()) })
		f = &flight[[]Step]{done: make(chan struct{})}
		r.diagnoses[key] = f
	}
	r.mu.Unlock()
	if lead {
		// The run is the daemon's own once it starts: a caller that leaves
		// does not cut it short for the others.
		fly(r, f, "the diagnosis of "+host, func() ([]Step, error) {
			st := r.state()
			if rt, found := HolderOf(st, host); named && found && rt.Owner != owner {
				return nil, fmt.Errorf("%w: %s is no longer held by %s", ErrHolderChanged, host, owner)
			}
			return DiagnoseRoute(context.WithoutCancel(ctx), st, host, r.httpc)
		})
	}
	return await(ctx, f)
}

// Doctor checks the installation.
func (r *Runner) Doctor(ctx context.Context) []Finding {
	r.mu.Lock()
	f := r.doctor
	lead := !f.fresh(r.now())
	if lead {
		f = &flight[[]Finding]{done: make(chan struct{})}
		r.doctor = f
	}
	r.mu.Unlock()
	if lead {
		fly(r, f, "the doctor run", func() ([]Finding, error) {
			return Run(context.WithoutCancel(ctx), r.state(), r.env), nil
		})
	}
	findings, err := await(ctx, f)
	if errors.Is(err, errStopped) {
		return []Finding{fail("doctor", err.Error(), "journalctl -u pco shows where it stopped")}
	}
	// A caller whose request ended reads no answer.
	return findings
}

// fly runs the flight f to its end, also when run panics: the panic is
// logged once, with its stack, and is errStopped for all that wait for f,
// which answers no later caller.
func fly[T any](r *Runner, f *flight[T], what string, run func() (T, error)) {
	var (
		val T
		err error
	)
	ended := false
	defer func() {
		if !ended {
			p := recover()
			r.log.Error().Str("panic", fmt.Sprint(p)).Bytes("stack", debug.Stack()).Msg(what + " panicked")
			err = fmt.Errorf("%s %w", what, errStopped)
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		f.finished, f.failed, f.at, f.val, f.err = true, !ended, r.now(), val, err
		close(f.done)
	}()
	val, err = run()
	ended = true
}

// await waits for a flight, or for ctx, and returns a copy of its result.
func await[T any](ctx context.Context, f *flight[[]T]) ([]T, error) {
	select {
	case <-f.done:
		return slices.Clone(f.val), f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
