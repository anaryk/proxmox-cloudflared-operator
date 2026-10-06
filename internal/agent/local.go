package agent

import (
	"bytes"
	"context"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
)

// Local is the in-memory transport the engine uses for the agent of its own
// node. It never opens a socket, and carries the same messages as the
// network does: a snapshot goes through its wire form, tokens included, so
// that the agent holds its own copy and applies it with the same checks of
// node, install, epoch and seq.
type Local struct{ A *Agent }

// Publish hands the agent a snapshot and returns its status once it applied
// it.
func (l Local) Publish(ctx context.Context, s agentproto.Snapshot) (agentproto.Status, error) {
	var buf bytes.Buffer
	if err := agentproto.EncodeSnapshot(&buf, s); err != nil {
		return l.A.Status(ctx), err
	}
	copied, err := agentproto.DecodeSnapshot(&buf)
	if err != nil {
		return l.A.Status(ctx), err
	}
	return l.A.Apply(ctx, copied)
}

// Report returns the agent's status.
func (l Local) Report(ctx context.Context) agentproto.Status { return l.A.Status(ctx) }

// Heartbeat hands the agent the controller's heartbeat.
func (l Local) Heartbeat(h agentproto.Heartbeat) { l.A.Heartbeat(h) }
