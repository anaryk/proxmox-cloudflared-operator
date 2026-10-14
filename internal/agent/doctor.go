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
	runs map[string]*agentproto.ActionResult // nil while it runs
}

// startDoctor runs the checks of the doctor action id, once, and has the
// status sent soon when they are done.
func (a *Agent) startDoctor(ctx context.Context, id string) {
	d := &a.docs
	d.mu.Lock()
	if d.runs == nil {
		d.runs = map[string]*agentproto.ActionResult{}
	}
	if _, ok := d.runs[id]; ok {
		d.mu.Unlock()
		return
	}
	d.runs[id] = nil
	d.mu.Unlock()
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := a.timeout(ctx, doctorTimeout)
		defer cancel()
		res := agentproto.ActionResult{ID: id}
		data, err := a.d.Doctor(ctx)
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
