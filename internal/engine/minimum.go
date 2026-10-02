package engine

import (
	"fmt"
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
	min := resolve.Level(c.settings.IdentityMinimum)
	if min == resolve.LevelFiltered && c.install.ProfileName() == store.ProfileHost {
		return resolve.LevelPort
	}
	return min
}

// holdBelowMinimum takes the address from the target of every winner that
// resolution proved below the required level, so that the plan treats it as
// one never verified: its rule answers 503 and no record is planned for it.
// The level, the candidates and the binding stay as resolution found them. A
// route without a guest has no identity to prove and is never held back. One
// problem line says how many routes are held back.
func (c *cycleRun) holdBelowMinimum() {
	min := c.requiredLevel()
	var held []resolve.Level
	for host, res := range c.results {
		if !belowMinimum(res, min) {
			continue
		}
		res.Target = planner.ResolvedTarget{Reason: fmt.Sprintf(reasonBelowMinimum, res.Level, min)}
		c.results[host] = res
		held = append(held, res.Level)
	}
	if len(held) > 0 {
		c.problem("%s", heldBack(held, min))
	}
}

// belowMinimum reports whether res would serve an address proven below min.
func belowMinimum(res resolve.Result, min resolve.Level) bool {
	t := res.Target
	if !t.Addr.IsValid() || t.Withdrawn || t.Rejected || res.Level == resolve.LevelManual {
		return false
	}
	return !res.Level.AtLeast(min)
}

// heldBack says how many routes the minimum holds back, at which levels, and
// how to serve them anyway.
func heldBack(levels []resolve.Level, min resolve.Level) string {
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
		"lower identityMinimum in the settings if serving guests on other nodes is intended",
		which, strings.Join(names, " or "), min)
}
