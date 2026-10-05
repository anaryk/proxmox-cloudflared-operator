package engine

import (
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// heldUnchecked begins the held reason of a tunnel the last cycle did not
// check: what the state shows of it is what an earlier cycle found, or only
// what the plan wants.
const heldUnchecked = "not checked in the last cycle"

// markUnchecked marks the tunnels of a cycle that did not check Cloudflare,
// whatever held it: none of what the state shows of the tunnels, the
// connectors or the records was checked, so the tunnels the state carries,
// and those the plan wants that it has not seen yet, are shown as unchecked
// and none as verified. The hold of the state is the reason of the step that
// held the cycle, and so is the held reason of each tunnel but one left as
// it is for a reason of its own, which keeps that.
func (c *cycleRun) markUnchecked() {
	why := c.holdWhy
	if why == "" {
		why = "the cycle did not get to Cloudflare"
	}
	c.st.Hold = why
	held := heldUnchecked + ": " + why
	shown := make(map[string]bool, len(c.st.Tunnels))
	for i := range c.st.Tunnels {
		t := &c.st.Tunnels[i]
		if !t.LeftAsIs {
			t.Held = held
		}
		t.Unchecked, t.Verified = true, false
		shown[t.AccountID] = true
	}
	for _, p := range c.plan.Tunnels {
		if shown[p.AccountID] {
			continue
		}
		shown[p.AccountID] = true
		c.st.Tunnels = append(c.st.Tunnels, TunnelView{
			TunnelState: reconcile.TunnelState{AccountID: p.AccountID, CredentialID: p.CredentialID, Name: p.Name, Unknown: true},
			Held:        held,
			Unchecked:   true,
		})
	}
}
