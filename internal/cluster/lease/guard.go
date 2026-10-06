package lease

import (
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// mayTry says why node may not try to acquire now, from leader.json as
// stored and from its guard. A node never takes over from a controller its
// agent still hears, whatever the clocks or its view of the lock say: the
// expiry of a lock is judged by the wall clock of the node with the lowest
// node id, and a step forward there, on a waiting node or on the holder
// itself, lets an unlock request remove a fresh lock. A
// controller that let go says so in leader.json: a release lets every node
// try, a hand-off only the node it names until the back-off ends.
func mayTry(rec store.LeaderRecord, found bool, node string, heardLive bool, now time.Time) string {
	if !found {
		return ""
	}
	if h := rec.Handoff; h != nil && now.Before(h.Until) {
		if h.To == node {
			return ""
		}
		return fmt.Sprintf("the lease is handed off to %s until %s", h.To, h.Until.Format(time.RFC3339))
	}
	if rec.ReleasedAt.After(rec.AcquiredAt) {
		return ""
	}
	if heardLive {
		return "the agent of this node hears a live controller"
	}
	return ""
}
