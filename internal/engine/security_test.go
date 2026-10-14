package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// An event of the audit trail goes into the file of the node that made it,
// in the cluster root, and its line in the journal says it is one.
func TestTheAuditTrailGoesToTheFileOfTheNodeThatActed(t *testing.T) {
	e := newEnv(t)
	lines := logged(e, zerolog.InfoLevel)

	e.eng.NoteEnrol("pve2", "took the certificate of pve2")
	e.eng.NoteNode("pve2", "refused a status of pve2")
	e.eng.audit.flush()

	got, err := e.store.SecurityEvents()
	require.NoError(t, err)
	require.Equal(t, []store.SecurityEvent{{At: t0, Level: levelInfo, Kind: kindEnrol, Subject: "pve2", Message: "took the certificate of pve2", Node: testNode}}, got,
		"only the event of the trail, in the file of this node")

	all := lines()
	line := withMessage(all, "took the certificate of pve2")
	require.Len(t, line, 1)
	require.Equal(t, true, line[0]["security"])
	other := withMessage(all, "refused a status of pve2")
	require.Len(t, other, 1)
	require.NotContains(t, other[0], "security")
}

// A query of the kind security merges the files of every node by time, on
// any node, with what else it asks of the events.
func TestTheSecurityKindMergesTheTrailOfEveryNode(t *testing.T) {
	e := newEnv(t)
	require.NoError(t, e.store.AppendSecurityEvent("pve2", store.SecurityEvent{At: t0.Add(time.Minute), Level: levelWarn, Kind: kindNode, Subject: "pve3", Message: "forgot pve3"}))
	e.eng.NoteEnrol("pve2", "took the certificate of pve2")
	e.eng.audit.flush()

	got, err := e.eng.QueryEvents(EventQuery{Kind: []string{KindSecurity}})
	require.NoError(t, err)
	require.Equal(t, []Event{
		{At: t0, Level: levelInfo, Kind: kindEnrol, Subject: "pve2", Message: "took the certificate of pve2", Node: testNode, Security: true},
		{At: t0.Add(time.Minute), Level: levelWarn, Kind: kindNode, Subject: "pve3", Message: "forgot pve3", Node: "pve2", Security: true},
	}, got)

	got, err = e.eng.QueryEvents(EventQuery{Kind: []string{KindSecurity}, Since: t0})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "pve2", got[0].Node)

	got, err = e.eng.QueryEvents(EventQuery{Kind: []string{KindSecurity}, Limit: 1})
	require.NoError(t, err)
	require.Len(t, got, 1, "the newest")
	require.Equal(t, "forgot pve3", got[0].Message)
}

// What an admin did and what happened to the lease are of the trail without
// a mark.
func TestAdminActionsAndTheLeaseAreOfTheTrail(t *testing.T) {
	for _, kind := range []string{kindAdmin, kindLease} {
		require.True(t, audited(Event{Kind: kind}), kind)
	}
	require.True(t, audited(Event{Kind: kindRotation, Security: true}))
	require.False(t, audited(Event{Kind: kindRoute}))
}

// An event an agent of another node sent is that node's to record: the
// controller writes none of them to its own file, whatever its kind.
func TestTheEventsOfAnotherNodeAreNotWrittenByTheController(t *testing.T) {
	e := newEnv(t)
	raw, err := json.Marshal(map[string]any{"cursor": 1, "at": t0, "level": "info", "kind": kindAdmin, "subject": "x", "message": "an admin did it"})
	require.NoError(t, err)
	st := agentproto.Status{Node: "pve2", AgentStartedAt: t0, Events: []json.RawMessage{raw}, EventCursor: 1}

	e.eng.TakeAgentEvents("pve2", st, agentproto.Epoch{InstallID: testInstall, Generation: 1, Nonce: "n1"})
	e.eng.audit.flush()

	got, err := e.store.SecurityEvents()
	require.NoError(t, err)
	require.Empty(t, got)
}

// The trail is written as the events come, until the engine stops.
func TestTheAuditTrailIsWrittenAsEventsCome(t *testing.T) {
	written := make(chan string, 4)
	a := newAuditTrail(testNode, func(_ string, events []store.SecurityEvent) error {
		for _, e := range events {
			written <- e.Message
		}
		return nil
	}, zerolog.Nop())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		a.run(ctx, startTimer)
		close(done)
	}()

	a.add([]Event{{At: t0, Kind: kindAdmin, Message: "one"}})
	require.Equal(t, "one", <-written)
	cancel()
	<-done
}

// A write that fails keeps the events, in their order, for the next try;
// the queue keeps the newest as many as the file does, and once a write
// works again says how many it dropped. A flush is one write.
func TestTheAuditTrailKeepsWhatItCouldNotWrite(t *testing.T) {
	var written []string
	writes, lost := 0, 0
	broken := true
	a := newAuditTrail(testNode, func(_ string, events []store.SecurityEvent) error {
		writes++
		if broken {
			return errors.New("no quorum")
		}
		for _, e := range events {
			written = append(written, e.Message)
		}
		return nil
	}, zerolog.Nop())
	a.lost = func(n int) { lost += n }

	a.add([]Event{{At: t0, Kind: kindAdmin, Message: "one"}, {At: t0, Kind: kindRoute, Message: "not of the trail"}})
	a.add([]Event{{At: t0, Kind: kindLease, Message: "two"}})
	a.flush()
	require.Empty(t, written)

	broken, writes = false, 0
	a.flush()
	require.Equal(t, []string{"one", "two"}, written)
	require.Equal(t, 1, writes, "one write for the whole queue")
	require.Zero(t, lost)

	broken, written = true, nil
	for i := range store.MaxSecurityEvents + 10 {
		a.add([]Event{{At: t0, Kind: kindAdmin, Message: fmt.Sprint(i)}})
	}
	a.flush()
	a.add([]Event{{At: t0, Kind: kindAdmin, Message: "one more"}})
	require.Zero(t, lost, "said once the trail is written again")
	broken = false
	a.flush()
	require.Len(t, written, store.MaxSecurityEvents)
	require.Equal(t, "11", written[0], "the oldest are dropped")
	require.Equal(t, 11, lost)
	a.flush()
	require.Equal(t, 11, lost, "said once")
}

// The engine says in the trail itself that events of it were lost.
func TestLostAuditEventsAreSaidInTheTrail(t *testing.T) {
	e := newEnv(t)

	e.eng.audit.lost(7)
	e.eng.audit.flush()

	got, err := e.store.SecurityEvents()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, kindAudit, got[0].Kind)
	require.Equal(t, levelWarn, got[0].Level)
	require.Contains(t, got[0].Message, "7 audit events were lost")
}

// An agent cannot pass its events off as the controller's: one of a kind of
// the audit trail or of the controller's findings is kept as agent:<kind>.
func TestAnAgentsEventOfAReservedKindIsKeptAsItsOwn(t *testing.T) {
	e := newEnv(t)
	var raws []json.RawMessage
	for i, kind := range []string{kindAdmin, KindSecurity, kindBlock, "egress"} {
		raw, err := json.Marshal(map[string]any{"cursor": i + 1, "at": t0, "level": "info", "kind": kind, "subject": "x", "message": "m" + kind})
		require.NoError(t, err)
		raws = append(raws, raw)
	}
	st := agentproto.Status{Node: "pve2", AgentStartedAt: t0, Events: raws, EventCursor: 4}

	e.eng.TakeAgentEvents("pve2", st, agentproto.Epoch{InstallID: testInstall, Generation: 1, Nonce: "n1"})

	var kinds []string
	for _, ev := range e.eng.Events(time.Time{}) {
		if ev.Node == "pve2" {
			kinds = append(kinds, ev.Kind)
			require.False(t, ev.Security)
		}
	}
	require.Equal(t, []string{"agent:admin", "agent:security", "agent:block", "egress"}, kinds)
}

// The trail has no numbers to read on from.
func TestTheTrailIsNotReadFromAPosition(t *testing.T) {
	e := newEnv(t)

	_, err := e.eng.QueryEvents(EventQuery{Kind: []string{KindSecurity}, After: 3})

	require.ErrorIs(t, err, ErrInvalid)
}
