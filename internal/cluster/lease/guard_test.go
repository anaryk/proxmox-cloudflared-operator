package lease

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

func TestMayTry(t *testing.T) {
	held := store.LeaderRecord{Writer: setupWriter, Node: "pve1", AcquiredAt: t0}
	released := held
	released.ReleasedAt = t0.Add(time.Minute)
	handedOff := released
	handedOff.Handoff = &store.Handoff{To: "pve3", Until: t0.Add(time.Minute + HandoffBackoff)}
	releasedBefore := held
	releasedBefore.ReleasedAt = t0.Add(-time.Minute)
	during, after := t0.Add(time.Minute+time.Second), t0.Add(time.Minute+HandoffBackoff)

	cases := []struct {
		name  string
		rec   store.LeaderRecord
		found bool
		node  string
		heard bool
		now   time.Time
		why   string
	}{
		{name: "nobody holds it", node: "pve2", heard: true, now: t0},
		{name: "a holder nobody hears", rec: held, found: true, node: "pve2", now: t0},
		{name: "a live holder", rec: held, found: true, node: "pve2", heard: true, now: t0, why: "live controller"},
		{name: "a release", rec: released, found: true, node: "pve2", heard: true, now: during},
		{name: "a release older than the tenure", rec: releasedBefore, found: true, node: "pve2", heard: true, now: t0,
			why: "live controller"},
		{name: "a hand-off to this node", rec: handedOff, found: true, node: "pve3", heard: true, now: during},
		{name: "a hand-off to another", rec: handedOff, found: true, node: "pve2", now: during, why: "handed off to pve3"},
		{name: "a hand-off to another, the holder too", rec: handedOff, found: true, node: "pve1", now: during,
			why: "handed off to pve3"},
		{name: "after the back-off", rec: handedOff, found: true, node: "pve2", heard: true, now: after},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			why := mayTry(c.rec, c.found, c.node, c.heard, c.now)
			if c.why == "" {
				require.Empty(t, why)
				return
			}
			require.Contains(t, why, c.why)
		})
	}
}
