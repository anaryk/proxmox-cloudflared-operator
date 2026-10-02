package engine

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/resolve"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const reasonBelowMinimum = "identity level %s is below the required %s"

// requiredLevel is the identity level the settings ask of a guest's address.
// Filtered is the level of the managed network, which the host profile does
// not have: there it asks for port.
func (c *cycleRun) requiredLevel() resolve.Level {
	required := resolve.Level(c.settings.IdentityMinimum)
	if required == resolve.LevelFiltered && c.install.ProfileName() == store.ProfileHost {
		return resolve.LevelPort
	}
	return required
}

// holdBelowMinimum takes the address from the target of every winner whose
// guest's address stands on a proof below the required level, so that the
// plan treats it as one never verified: its rule answers 503 and no record is
// planned for it. A target served now gets the reason that says so; a
// withdrawn one keeps the reason resolution gave, and is held back by the
// level of its last proof, so that a withdrawal does not publish a route the
// minimum held back. The level, the candidates and the binding stay as
// resolution found them. A route without a guest has no identity to prove
// and is never held back, whatever level its result says. One problem line
// says how many routes are held back.
func (c *cycleRun) holdBelowMinimum() {
	required := c.requiredLevel()
	var held []resolve.Level
	for _, rt := range c.claims.Winners {
		res, ok := c.results[rt.Hostname]
		if !ok || rt.Guest == nil {
			continue
		}
		level, published := standsOn(res)
		if !published || level.AtLeast(required) {
			continue
		}
		if res.Target.Withdrawn {
			res.Target.Addr = netip.Addr{}
		} else {
			res.Target = planner.ResolvedTarget{Reason: fmt.Sprintf(reasonBelowMinimum, level, required)}
		}
		c.results[rt.Hostname] = res
		held = append(held, level)
	}
	if len(held) > 0 {
		c.problem("%s", heldBack(held, required))
	}
}

// standsOn returns the level of the proof the plan would publish res's
// target on, and false when it would publish nothing: no address, or one
// that must never be served. A withdrawn address keeps its record on the
// strength of its binding's last proof.
func standsOn(res resolve.Result) (resolve.Level, bool) {
	t := res.Target
	switch {
	case !t.Addr.IsValid() || t.Rejected:
		return "", false
	case !t.Withdrawn:
		return res.Level, true
	case res.Binding == nil:
		return "", false
	}
	return res.Binding.Proven(), true
}

// heldBack says how many routes the minimum holds back, at which levels, and
// how to serve them anyway.
func heldBack(levels []resolve.Level, required resolve.Level) string {
	which := "1 route is held back: its"
	if len(levels) != 1 {
		which = fmt.Sprintf("%d routes are held back: their", len(levels))
	}
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = string(l)
	}
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	return fmt.Sprintf("%s identity level is %s, below the required %s; "+
		"guests on other nodes and trusted static addresses are proven at observed only: "+
		"lower identityMinimum in the settings to serve them",
		which, strings.Join(names, " or "), required)
}
