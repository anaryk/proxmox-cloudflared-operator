package agent

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const writeArgs = `{"guest":{"kind":"qemu","vmid":101},"nic":1,"bridge":"pconet","mac":"06:50:43:4f:76:05","firewall":false,"digest":"0123456789abcdef0123456789abcdef01234567"}`

func withAction(s agentproto.Snapshot, id, kind, args string) agentproto.Snapshot {
	s.Actions = append(s.Actions, agentproto.Action{ID: id, Kind: kind, Args: json.RawMessage(args)})
	return s
}

func TestAnActionRunsOnceAndARestartDoesNotRunItAgain(t *testing.T) {
	e := newEnv(t)
	s := withAction(e.snapshot("www.example.com -> :8080"), "a1", agentproto.ActionPVEWrite, writeArgs)

	st := e.apply(s)

	require.Len(t, e.runs.all(), 1)
	require.Equal(t, []agentproto.ActionResult{{ID: "a1", OK: true}}, st.Actions)
	done, err := e.store.AgentActions()
	require.NoError(t, err)
	require.Equal(t, map[string]store.ActionDone{"a1": {Kind: agentproto.ActionPVEWrite, At: t0}}, done)

	e.apply(s)
	e.agent = e.newAgent()
	st = e.apply(s)

	require.Len(t, e.runs.all(), 1, "neither a second delivery nor a restart runs it again")
	require.Equal(t, []agentproto.ActionResult{{ID: "a1", OK: true}}, st.Actions, "its result is still reported")
}

// An action is marked done before it runs: one cut by a crash is answered as
// such, never run twice.
func TestAnActionIsMarkedDoneBeforeItRuns(t *testing.T) {
	e := newEnv(t)
	var seen map[string]store.ActionDone
	run := e.runs.run
	e.agent.d.Runner = func(ctx context.Context, argv ...string) (string, error) {
		var err error
		seen, err = e.store.AgentActions()
		require.NoError(t, err)
		return run(ctx, argv...)
	}

	e.apply(withAction(e.snapshot("www.example.com -> :8080"), "a1", agentproto.ActionPVEWrite, writeArgs))

	require.Equal(t, map[string]store.ActionDone{"a1": {Kind: agentproto.ActionPVEWrite, At: t0, Error: "the agent stopped while it ran"}}, seen)
}

func TestDoneActionsAreForgottenOnceTheSnapshotNoLongerListsThem(t *testing.T) {
	e := newEnv(t)
	e.apply(withAction(e.snapshot("www.example.com -> :8080"), "a1", agentproto.ActionPVEWrite, writeArgs))

	st := e.apply(withAction(e.snapshot("www.example.com -> :8080"), "a2", agentproto.ActionPVEWrite, writeArgs))

	require.Equal(t, []agentproto.ActionResult{{ID: "a2", OK: true}}, st.Actions)
	require.Len(t, e.runs.all(), 2, "another id is another action")
	done, err := e.store.AgentActions()
	require.NoError(t, err)
	require.Equal(t, []string{"a2"}, slices.Collect(maps.Keys(done)))

	st = e.apply(e.snapshot("www.example.com -> :8080"))
	require.Empty(t, st.Actions)
	done, err = e.store.AgentActions()
	require.NoError(t, err)
	require.Empty(t, done)
}

func TestAFailedActionIsAnsweredWithWhy(t *testing.T) {
	e := newEnv(t)
	e.runs.err = errBoom

	st := e.apply(withAction(e.snapshot("www.example.com -> :8080"), "a1", agentproto.ActionPVEWrite, writeArgs))

	require.Equal(t, []agentproto.ActionResult{{ID: "a1", Error: "qm set 101: boom"}}, st.Actions)
	done, err := e.store.AgentActions()
	require.NoError(t, err)
	require.Equal(t, "qm set 101: boom", done["a1"].Error)

	e.agent = e.newAgent()
	st = e.apply(withAction(e.snapshot("www.example.com -> :8080"), "a1", agentproto.ActionPVEWrite, writeArgs))
	require.Equal(t, []agentproto.ActionResult{{ID: "a1", Error: "qm set 101: boom"}}, st.Actions)
}

func TestAnActionOfAKindThisAgentDoesNotCarryOutIsAnsweredSo(t *testing.T) {
	e := newEnv(t)

	st := e.apply(withAction(e.snapshot(), "a1", "teleport", `{}`))

	require.Equal(t, []agentproto.ActionResult{{ID: "a1", Error: "this agent does not carry out teleport actions"}}, st.Actions)
}

// Actions come from the controller: a pass of the agent's own, on the state
// it cached, runs none.
func TestOnlyTheControllersSnapshotRunsActions(t *testing.T) {
	e := newEnv(t)
	s := withAction(e.snapshot("www.example.com -> :8080"), "a1", agentproto.ActionPVEWrite, writeArgs)
	e.apply(s)
	require.NoError(t, e.store.SaveAgentActions(map[string]store.ActionDone{}))
	e.agent = e.newAgent()

	require.NoError(t, e.agent.Boot(t.Context()))
	_, err := e.agent.Reverify(t.Context())
	require.NoError(t, err)

	require.Len(t, e.runs.all(), 1)
}
