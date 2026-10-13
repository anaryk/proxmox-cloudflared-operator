package reconcile

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// driftInterval is how often a pinned tunnel that was verified, and whose
// plan has not changed since, is read again for a change made behind pco's
// back, and how often the pinned tunnels of an account are listed. The
// shared tunnel is read every run; a pinned tunnel is one of many, and each
// read counts against the budget of the token.
const driftInterval = time.Minute

// pinnedMemory is what the last read of a pinned tunnel found, with the
// rules it was planned to have then.
type pinnedMemory struct {
	state TunnelState
	rules []planner.IngressRule
	at    time.Time
}

// pinnedListing is the tunnels of the install pinned to a node in one
// account, as last listed. An account is listed from the first run that
// plans a pinned tunnel there and from then on, so that a tunnel left
// without a plan is still found and emptied.
type pinnedListing struct {
	tunnels []cfapi.Tunnel
	at      time.Time // zero while no listing succeeded
}

// listPinned lists the tunnels of this install pinned to a node in every
// known account that a plan pins a tunnel in, or that was listed before,
// once a minute. A failure is a problem and the run goes on with what it
// last listed; the planned tunnels are looked up by name as any other.
func (run *tunnelRun) listPinned(ctx context.Context, plans []planner.TunnelPlan, known map[string]string) {
	r := run.r
	for _, p := range plans {
		if _, ok := r.listed[p.AccountID]; !ok && p.Kind == planner.PlacementPinned && known[p.AccountID] != "" {
			r.listed[p.AccountID] = pinnedListing{}
		}
	}
	for _, account := range slices.Sorted(maps.Keys(r.listed)) {
		credential, ok := known[account]
		api := r.clients[credential]
		if !ok || api == nil || ctx.Err() != nil || !run.listingDue(account) {
			continue
		}
		tunnels, err := api.Tunnels(ctx, account, planner.PinnedTunnelPrefix(run.us.InstallID))
		switch {
		case err != nil && cfapi.IsRateLimited(err):
			run.waitRead(account)
			continue
		case err != nil:
			run.problem(fmt.Sprintf("account %s: listing its pinned tunnels: %v", account, err))
			continue
		}
		tunnels = slices.DeleteFunc(tunnels, func(t cfapi.Tunnel) bool {
			_, ok := planner.PinnedTunnelNode(run.us.InstallID, t.Name)
			return !ok
		})
		r.listed[account] = pinnedListing{tunnels: tunnels, at: r.now()}
		run.listedNow[account] = true
	}
}

// listingDue reports whether the pinned tunnels of an account are to be
// listed: never yet, or not for driftInterval. A clock that stepped back
// makes the listing due rather than wait for the clock to catch up.
func (run *tunnelRun) listingDue(account string) bool {
	at := run.r.listed[account].at
	now := run.r.now()
	return at.IsZero() || now.Before(at) || now.Sub(at) >= driftInterval
}

// listedTunnel returns the tunnel of t that the listing of this run found,
// if the listing was made in this run and holds it.
func (run *tunnelRun) listedTunnel(t target) (cfapi.Tunnel, bool) {
	if !run.listedNow[t.account] {
		return cfapi.Tunnel{}, false
	}
	tunnels := run.r.listed[t.account].tunnels
	i := slices.IndexFunc(tunnels, func(tun cfapi.Tunnel) bool { return tun.Name == t.name })
	if i < 0 {
		return cfapi.Tunnel{}, false
	}
	return tunnels[i], true
}

// lastSeen returns what the last read of a pinned tunnel found when no read
// is due: the configuration was verified, the plan is what it was then, and
// the read is less than driftInterval old. A plan the run would refuse is
// never answered from memory.
func (run *tunnelRun) lastSeen(t target) (TunnelState, bool) {
	m, ok := run.r.pinned[t.key()]
	now := run.r.now()
	if !ok || !m.state.Verified || !EqualIngress(m.rules, t.rules) || now.Before(m.at) || now.Sub(m.at) >= driftInterval || run.planFault(t) != "" {
		return TunnelState{}, false
	}
	return m.state, true
}

// remember keeps what the read of a pinned tunnel found, for the runs until
// its next read is due.
func (run *tunnelRun) remember(t target, st TunnelState) {
	run.r.pinned[t.key()] = pinnedMemory{state: st, rules: slices.Clone(t.rules), at: run.r.now()}
}
