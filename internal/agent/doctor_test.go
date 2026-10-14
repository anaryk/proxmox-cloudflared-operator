package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
