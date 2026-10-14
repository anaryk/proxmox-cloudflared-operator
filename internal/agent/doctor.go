package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

const (
	// doctorTimeout bounds the checks a doctor action makes, within the
	// half minute the controller waits for the answer.
	doctorTimeout = 25 * time.Second
	// maxDoctorAnswer bounds the findings an answer carries, well within
	// the bound of a status.
	maxDoctorAnswer = 256 << 10
)

// doctorRuns are the doctor actions of this process, by id: running, or
// answered. They run beside the passes, which a check that waits for the
// network would hold up, and are answered in the status once done.
type doctorRuns struct {
	mu   sync.Mutex
	runs map[string]*agentproto.ActionResult // nil while it runs or waits
	// one is held by the run that makes its checks: one at a time.
	one chan struct{}
}

// startDoctor runs the checks of the doctor action id, once, after the run
// before it ended, and has the status sent soon when they are done.
func (a *Agent) startDoctor(ctx context.Context, id string) {
	d := &a.docs
	d.mu.Lock()
	if d.runs == nil {
		d.runs, d.one = map[string]*agentproto.ActionResult{}, make(chan struct{}, 1)
	}
	if _, ok := d.runs[id]; ok {
		d.mu.Unlock()
		return
	}
	d.runs[id] = nil
	one := d.one
	d.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	go func() {
		one <- struct{}{}
		defer func() { <-one }()
		// The controller may have taken the action back while it waited.
		d.mu.Lock()
		_, listed := d.runs[id]
		d.mu.Unlock()
		if !listed {
			return
		}
		ctx, cancel := a.timeout(ctx, doctorTimeout)
		defer cancel()
		res := agentproto.ActionResult{ID: id}
		data, err := a.checksWithin(ctx)
		switch {
		case err != nil:
			res.Error = err.Error()
		case len(data) > maxDoctorAnswer:
			res.Error = fmt.Sprintf("the findings are %d bytes, over the %d an answer carries", len(data), maxDoctorAnswer)
		case !json.Valid(data):
			res.Error = "the findings are not JSON"
		default:
			res.OK, res.Data = true, data
		}
		d.mu.Lock()
		if _, listed := d.runs[id]; listed {
			d.runs[id] = &res
		}
		d.mu.Unlock()
		if a.d.Changed != nil {
			a.d.Changed()
		}
	}()
}

// checksWithin makes the checks of the doctor until ctx ends. A check that
// does not heed ctx, as one that reads a stalled mount, is left to end on
// its own: the run is answered as timed out, and the next one is not held
// up by it.
func (a *Agent) checksWithin(ctx context.Context) (json.RawMessage, error) {
	type result struct {
		data json.RawMessage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := a.d.Doctor(ctx)
		done <- result{data, err}
	}()
	select {
	case r := <-done:
		return r.data, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("the checks timed out: they did not end within %s", doctorTimeout)
	}
}

// doctorOwn says whether the doctor runs answer for the action id.
func (a *Agent) doctorOwn(id string) bool {
	d := &a.docs
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.runs[id]
	return ok
}

// keepDoctors forgets the runs of actions the snapshot no longer lists.
func (a *Agent) keepDoctors(listed map[string]bool) {
	d := &a.docs
	d.mu.Lock()
	defer d.mu.Unlock()
	maps.DeleteFunc(d.runs, func(id string, _ *agentproto.ActionResult) bool { return !listed[id] })
}

// doctorAnswers are the answers of the doctor runs that are done, by id.
func (a *Agent) doctorAnswers() []agentproto.ActionResult {
	d := &a.docs
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []agentproto.ActionResult
	for _, id := range slices.Sorted(maps.Keys(d.runs)) {
		if res := d.runs[id]; res != nil {
			out = append(out, *res)
		}
	}
	return out
}
