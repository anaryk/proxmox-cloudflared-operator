package engine

import (
	"fmt"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

func (c *cycleRun) mode() reconcile.Mode {
	if c.settings.ObserveOnly {
		return reconcile.Observe
	}
	return reconcile.Enforce
}

// reconcileTunnels brings the tunnel of every planned account in line and
// reports whether the cycle may go on: a stale or foreign writer stops it. A
// frozen account is left out: its tunnel is not touched.
func (c *cycleRun) reconcileTunnels() bool {
	plans := slices.DeleteFunc(slices.Clone(c.plan.Tunnels), func(p planner.TunnelPlan) bool { return c.zones.frozen[p.AccountID] })
	res := c.e.tunnels.Run(c.ctx, plans, c.zones.known, c.mode())
	c.tunnelVerdict = res.Verdict
	c.tunnels = res.Tunnels
	c.st.Tunnels = make([]TunnelView, 0, len(res.Tunnels))
	for _, t := range res.Tunnels {
		c.st.Tunnels = append(c.st.Tunnels, TunnelView{TunnelState: t})
	}
	c.st.Actions = append(c.st.Actions, res.Actions...)
	c.st.Problems = append(c.st.Problems, res.Problems...)
	c.st.WriterVerdict = verdictName(res.Verdict)
	if res.Verdict != reconcile.WriterProceed {
		c.hold(fmt.Sprintf("the tunnel run found a %s writer", c.st.WriterVerdict))
		return false
	}
	return c.writerStill("after the tunnel run")
}

// writerStill reads leader.json again after a reconciler run that let this
// writer proceed: a run that could not read it, or found it changed, may have
// stopped writing without saying so in its verdict. It reports whether the
// cycle may go on.
func (c *cycleRun) writerStill(when string) bool {
	us, stored, err := c.e.writer()
	switch {
	case err != nil:
		c.st.WriterVerdict = VerdictUnknown
		c.hold(c.problem("the writer identity cannot be read %s (%v); the rest is left as it is", when, err))
		return false
	case stored.Generation != us.Generation || stored.Nonce != us.Nonce:
		c.st.WriterVerdict = VerdictStale
		c.hold(c.problem("leader.json names another writer %s; the rest is left as it is", when))
		return false
	}
	return true
}
