package present

import (
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/agentproto"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
)

// NodeStateOrder is the order the nodes' states are listed in; a state that
// is not listed follows, by name.
var NodeStateOrder = []agentproto.NodeState{
	agentproto.Active,
	agentproto.Draining,
	agentproto.Joining,
	agentproto.Unknown,
	agentproto.Down,
	agentproto.Contested,
	agentproto.Removed,
}

// NodeStateText says what a node's state means; a state of another version
// is shown as it is, and an entry without one is an earlier release's.
func NodeStateText(state string) string {
	switch agentproto.NodeState(state) {
	case agentproto.Joining:
		return "joining (enrolled, not admitted yet)"
	case agentproto.Active:
		return "active"
	case agentproto.Draining:
		return "draining (out of the shared tunnel's replicas)"
	case agentproto.Unknown:
		return "unknown (its agent sent no status for 30 seconds)"
	case agentproto.Down:
		return "down (the cluster lists it offline)"
	case agentproto.Contested:
		return "contested (its join token was misused; pco node forget ends it)"
	case agentproto.Removed:
		return "removed"
	case "":
		return "not numbered yet (an entry of an earlier release)"
	}
	return state
}

// NodeRoleText says what a node is to the install: the controller, which
// writes, or an agent, which serves its node.
func NodeRoleText(n engine.NodeView) string {
	if n.Controller {
		return "controller"
	}
	return "agent"
}

// NodeAgentText says what the controller last heard of a node's agent: how
// long ago, and whether its watch runs; a node never heard says so.
func NodeAgentText(n engine.NodeView) string {
	if n.Build == "" && n.StatusAge == 0 && !n.WatchRunning {
		return "no status yet"
	}
	text := fmt.Sprintf("heard %s ago", n.StatusAge.Round(time.Second))
	if n.WatchRunning {
		text += ", watching"
	}
	return text
}

// RotationText says where the rotation of a compromised node's secrets is.
func RotationText(r *engine.CARotationView) string {
	switch {
	case r == nil:
		return ""
	case !r.FinishedAt.IsZero() && len(r.Refused) > 0:
		return fmt.Sprintf("finished for node %s; the new CA refuses %d nodes still", r.Node, len(r.Refused))
	case !r.FinishedAt.IsZero():
		return "finished for node " + r.Node
	}
	return fmt.Sprintf("running for node %s: %d of 6 steps done, waiting on %s", r.Node, len(r.Done), r.Next)
}
