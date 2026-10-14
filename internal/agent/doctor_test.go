package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// A doctor action runs beside the pass and is answered in the status once
// its checks are done; a second delivery runs it no more.
func TestADoctorActionIsAnsweredWithTheFindings(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	runs := 0
	changed := make(chan struct{}, 4)
	e.agent.d.Changed = func() { changed <- struct{}{} }
	e.agent.d.Doctor = func(context.Context) (json.RawMessage, error) {
		runs++
		<-release
		return json.RawMessage(`[{"check":"cloudflared","level":"ok","detail":"cloudflared 2026.9.0"}]`), nil
	}
	s := withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`)

	st := e.apply(s)
	require.Empty(t, st.Actions, "not answered while it runs")

	close(release)
	<-changed
	st = e.agent.Status(t.Context())
	require.Equal(t, []agentproto.ActionResult{{ID: "d1", OK: true,
		Data: json.RawMessage(`[{"check":"cloudflared","level":"ok","detail":"cloudflared 2026.9.0"}]`)}}, st.Actions)

	st = e.apply(s)
	require.Len(t, st.Actions, 1)
	require.Equal(t, 1, runs)

	st = e.apply(e.snapshot())
	require.Empty(t, st.Actions, "forgotten once the snapshot no longer lists it")
	require.Empty(t, e.agent.Status(t.Context()).Actions)
}

func TestADoctorActionThatFailsSaysWhy(t *testing.T) {
	e := newEnv(t)
	changed := make(chan struct{}, 1)
	e.agent.d.Changed = func() { changed <- struct{}{} }
	e.agent.d.Doctor = func(context.Context) (json.RawMessage, error) { return nil, errors.New("the daemon is starting") }

	e.apply(withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`))
	<-changed

	require.Equal(t, []agentproto.ActionResult{{ID: "d1", Error: "the daemon is starting"}}, e.agent.Status(t.Context()).Actions)
}

// An agent without the checks refuses the action, as one of any kind it
// does not carry out.
func TestAnAgentWithoutTheChecksRefusesTheDoctorAction(t *testing.T) {
	e := newEnv(t)

	st := e.apply(withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`))

	require.Equal(t, []agentproto.ActionResult{{ID: "d1", Error: "this agent does not carry out doctor actions"}}, st.Actions)
}

// The agent makes the checks of one doctor action at a time.
func TestDoctorActionsRunOneAtATime(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	running, most := 0, 0
	release := make(chan struct{})
	changed := make(chan struct{}, 4)
	e.agent.d.Changed = func() { changed <- struct{}{} }
	e.agent.d.Doctor = func(context.Context) (json.RawMessage, error) {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return json.RawMessage(`[]`), nil
	}

	e.apply(withAction(withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`), "d2", agentproto.ActionDoctor, `{}`))
	release <- struct{}{}
	<-changed
	release <- struct{}{}
	<-changed

	require.Equal(t, 1, most)
	require.Len(t, e.agent.Status(t.Context()).Actions, 2)
}

// A check that does not heed its deadline, as one that reads a stalled
// mount, is answered as timed out, and the next run does not wait for it.
func TestAStalledDoctorRunIsAnsweredAndHoldsUpNoOther(t *testing.T) {
	e := newEnv(t)
	deadline, expire := context.WithCancel(t.Context())
	e.agent.timeout = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return deadline, func() {}
	}
	stalled := make(chan struct{})
	defer close(stalled)
	var calls atomic.Int32
	changed := make(chan struct{}, 4)
	e.agent.d.Changed = func() { changed <- struct{}{} }
	e.agent.d.Doctor = func(context.Context) (json.RawMessage, error) {
		if calls.Add(1) == 1 {
			<-stalled // it never looks at its context
		}
		return json.RawMessage(`[]`), nil
	}

	e.apply(withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`))
	expire()
	<-changed
	require.Equal(t, []agentproto.ActionResult{{ID: "d1", Error: "the checks timed out: they did not end within 25s"}},
		e.agent.Status(t.Context()).Actions)

	e.apply(withAction(e.snapshot(), "d2", agentproto.ActionDoctor, `{}`))
	<-changed
	got := e.agent.Status(t.Context()).Actions
	require.Len(t, got, 1, "the stalled run holds up no other")
	require.Equal(t, "d2", got[0].ID)
}

// A run the controller took back while it waited for the one before is not
// made.
func TestADoctorRunTakenBackIsNotMade(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	changed := make(chan struct{}, 4)
	e.agent.d.Changed = func() { changed <- struct{}{} }
	e.agent.d.Doctor = func(context.Context) (json.RawMessage, error) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			<-release
		}
		return json.RawMessage(`[]`), nil
	}

	e.apply(withAction(withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`), "d2", agentproto.ActionDoctor, `{}`))
	e.apply(withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`)) // d2 taken back
	close(release)
	<-changed
	e.apply(withAction(withAction(e.snapshot(), "d1", agentproto.ActionDoctor, `{}`), "d3", agentproto.ActionDoctor, `{}`))
	<-changed

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, calls, "d1 and d3, not d2")
}
