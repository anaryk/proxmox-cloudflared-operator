package doctor

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/hostname"
)

// keepFor is how long the result of a doctor run, or of the diagnosis of a
// hostname, answers the requests that follow it.
const keepFor = 5 * time.Second

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

	mu        sync.Mutex
	diagnoses map[string]*flight[[]Step] // by hostname
	doctor    *flight[[]Finding]
}

// NewRunner returns a runner. httpc lends the diagnosis the certificate
// authorities it trusts; nil trusts those of the system.
func NewRunner(state func() engine.State, env Env, httpc *http.Client, now func() time.Time) *Runner {
	return &Runner{state: state, env: env, httpc: httpc, now: now, diagnoses: make(map[string]*flight[[]Step])}
}

// flight is one run and, once it is done, its result.
type flight[T any] struct {
	done chan struct{}
	// Set under the lock of the runner before done is closed.
	finished bool
	at       time.Time
	val      T
	err      error
}

// fresh reports whether f still answers at now: it runs, or ended less than
// keepFor ago. The caller holds the lock of the runner.
func (f *flight[T]) fresh(now time.Time) bool {
	return f != nil && (!f.finished || now.Sub(f.at) < keepFor && !now.Before(f.at))
}

// Diagnose walks the chain of the route of a hostname.
func (r *Runner) Diagnose(ctx context.Context, name string) ([]Step, error) {
	host, err := hostname.Normalize(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrInvalid, err)
	}
	r.mu.Lock()
	f := r.diagnoses[host]
	lead := !f.fresh(r.now())
	if lead {
		maps.DeleteFunc(r.diagnoses, func(_ string, old *flight[[]Step]) bool { return !old.fresh(r.now()) })
		f = &flight[[]Step]{done: make(chan struct{})}
		r.diagnoses[host] = f
	}
	r.mu.Unlock()
	if lead {
		// The run is the daemon's own once it starts: a caller that leaves
		// does not cut it short for the others.
		steps, err := DiagnoseRoute(context.WithoutCancel(ctx), r.state(), host, r.httpc)
		r.finish(f, steps, err)
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
		findings := Run(context.WithoutCancel(ctx), r.state(), r.env)
		r.finishDoctor(f, findings)
	}
	// A caller whose request ended reads no answer.
	findings, _ := await(ctx, f)
	return findings
}

func (r *Runner) finish(f *flight[[]Step], steps []Step, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.finished, f.at, f.val, f.err = true, r.now(), steps, err
	close(f.done)
}

func (r *Runner) finishDoctor(f *flight[[]Finding], findings []Finding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f.finished, f.at, f.val = true, r.now(), findings
	close(f.done)
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
