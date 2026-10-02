package engine

import (
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// HeldUnchecked begins the held reason of a tunnel the last cycle did not
// check: what the state shows of it is what an earlier cycle found, or only
// what the plan wants.
const HeldUnchecked = "not checked in the last cycle"

// markUnchecked marks the tunnels of a cycle that did not get through to its
// DNS run, whatever held it: none of what the state shows of the tunnels, the
// connectors or the records was checked, so the tunnels the state carries,
// and those the plan wants that it has not seen yet, are shown as held, with
// why, and none as verified.
func (c *cycleRun) markUnchecked() {
	why := "the cycle did not get to Cloudflare"
	if len(c.st.Problems) > 0 {
		why = c.st.Problems[0]
	}
	held := HeldUnchecked + ": " + why
	shown := make(map[string]bool, len(c.st.Tunnels))
	for i := range c.st.Tunnels {
		t := &c.st.Tunnels[i]
		t.Held, t.Verified = held, false
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
		})
	}
}
