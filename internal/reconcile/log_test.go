package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// logged returns what a buffer holds, one entry per line, as level and message.
func logged(t *testing.T, buf *bytes.Buffer) (levels, messages []string) {
	t.Helper()
	for line := range strings.Lines(buf.String()) {
		var entry struct{ Level, Message string }
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		levels = append(levels, entry.Level)
		messages = append(messages, entry.Message)
	}
	return levels, messages
}

// The engine says each change once, as an event; the reconcilers keep their
// own lines for a debugging run.
func requireOnlyDebugLines(t *testing.T, buf *bytes.Buffer, want ...string) {
	t.Helper()
	levels, messages := logged(t, buf)
	for i, level := range levels {
		require.Equal(t, "debug", level, "%q is no line for an info log: a write is the engine's event", messages[i])
	}
	for _, msg := range want {
		require.Contains(t, messages, msg)
	}
}

func TestTheWritesOfTunnelsAreDebugLines(t *testing.T) {
	var buf bytes.Buffer
	f := newFake("acct1")
	r := NewTunnelReconciler(Clients{"cred1": f}, writerOf(ours, ours), (&clock{t0}).now, zerolog.New(&buf))

	res := r.Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Equal(t, []Action{action(CreateTunnel, ""), action(PutConfig, "")}, withoutDetail(res.Actions))
	requireOnlyDebugLines(t, &buf, "created tunnel", "wrote tunnel configuration")
}

func TestTheDeleteOfAProbeTunnelIsADebugLine(t *testing.T) {
	var buf bytes.Buffer
	f := newFake("acct1")
	f.SeedTunnel("acct1", testTunnel, rulesOf(ours, app))
	seedAt(f, "acct1", "pco-abc_probe_old", t0.Add(-11*time.Minute))
	r := NewTunnelReconciler(Clients{"cred1": f}, writerOf(ours, ours), (&clock{t0}).now, zerolog.New(&buf))

	res := r.Run(context.Background(), []planner.TunnelPlan{planFor("acct1", "cred1", app)}, nil, Enforce)

	require.Equal(t, []Action{deleteTunnel("pco-abc_probe_old", "")}, withoutDetail(res.Actions))
	requireOnlyDebugLines(t, &buf, "deleted probe tunnel left behind")
}

func TestTheWritesOfRecordsAreDebugLines(t *testing.T) {
	var buf bytes.Buffer
	f := newDNSFake()
	f.SeedRecord(zone1.ID, probeRecord("_pco-probe-old.example.com", t0.Add(-11*time.Minute)))
	r := NewDNSReconciler(Clients{"cred1": f}, &memStore{}, writerOf(ours, ours), DNSSettings{InstallID: testInstall}, (&clock{t0}).now, zerolog.New(&buf))

	res := r.Run(context.Background(), dnsIn("app.example.com"), Enforce)

	var kinds []ActionKind
	for _, a := range res.Actions {
		require.True(t, a.Applied, "%v", a)
		kinds = append(kinds, a.Kind)
	}
	require.ElementsMatch(t, []ActionKind{CreateRecord, DeleteRecord}, kinds)
	requireOnlyDebugLines(t, &buf, "changed dns record")
}
