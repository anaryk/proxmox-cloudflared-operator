package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// probeTunnelAge is how old a probe tunnel must be before it counts as left
// behind; a younger one may belong to a credential check still running. It is
// also the least time between two sweeps of an account: whatever a sweep
// left, a sweep sooner would find too young as well.
const probeTunnelAge = 10 * time.Minute

// sweepProbeTunnels deletes the probe tunnels the credential check left
// behind in the accounts whose tunnel the run looked up. Nothing else is
// deleted: only a tunnel named as a probe of this install, older than
// probeTunnelAge and without connectors, each after a fresh read of the
// writer. A failure is a problem and the sweep goes on; a writer that may not
// write stops it. An account swept less than probeTunnelAge ago is left for
// a later run.
func (run *tunnelRun) sweepProbeTunnels(ctx context.Context) {
	for _, st := range run.res.Tunnels {
		api := run.r.clients[st.CredentialID]
		if st.Unknown || api == nil || ctx.Err() != nil || !run.sweepDue(st.AccountID) {
			continue
		}
		if !run.sweepAccount(ctx, api, st) {
			return
		}
	}
}

// sweepDue reports whether the probe tunnels of an account are to be swept:
// never yet, or not for probeTunnelAge. A clock that stepped back makes the
// sweep due rather than wait for the clock to catch up.
func (run *tunnelRun) sweepDue(account string) bool {
	last, ok := run.r.lastSweep[account]
	now := run.r.now()
	return !ok || now.Before(last) || now.Sub(last) >= probeTunnelAge
}

// sweepAccount sweeps the probe tunnels of the account of st and reports
// whether the sweep goes on.
func (run *tunnelRun) sweepAccount(ctx context.Context, api cfapi.API, st TunnelState) bool {
	install := run.us.InstallID
	tunnels, err := api.Tunnels(ctx, st.AccountID, planner.ProbeTunnelName(install, ""))
	if err != nil {
		run.problem(fmt.Sprintf("account %s: listing probe tunnels: %v", st.AccountID, err))
		return true
	}
	run.r.lastSweep[st.AccountID] = run.r.now()
	for _, tun := range tunnels {
		if !planner.IsProbeTunnel(install, tun.Name) || !run.leftBehind(tun) {
			continue
		}
		probe := target{account: st.AccountID, credential: st.CredentialID, name: tun.Name}
		conns, err := api.Connectors(ctx, st.AccountID, tun.ID)
		switch {
		case err != nil:
			run.problem(fmt.Sprintf("%s: listing its connectors: %v", probe, err))
			continue
		case len(conns) > 0:
			continue
		case !run.reread(probe):
			return false
		}
		run.deleteProbe(ctx, api, probe, tun)
	}
	return true
}

// leftBehind reports whether a probe tunnel is old enough to be swept. One
// whose age is not known stays.
func (run *tunnelRun) leftBehind(tun cfapi.Tunnel) bool {
	return !tun.CreatedAt.IsZero() && run.r.now().Sub(tun.CreatedAt) > probeTunnelAge
}

// deleteProbe deletes a probe tunnel; one that is gone already counts as
// deleted.
func (run *tunnelRun) deleteProbe(ctx context.Context, api cfapi.API, probe target, tun cfapi.Tunnel) {
	a := Action{
		Kind:        DeleteTunnel,
		Credential:  probe.credential,
		AccountID:   probe.account,
		Target:      tun.Name,
		Detail:      fmt.Sprintf("in account %s: probe left behind, created %s", probe.account, tun.CreatedAt.UTC().Format(time.RFC3339)),
		Destructive: true,
		Applied:     true,
	}
	if err := api.DeleteTunnel(ctx, probe.account, tun.ID); err != nil && !cfapi.IsNotFound(err) {
		a.Applied, a.Held = false, err.Error()
		run.res.Actions = append(run.res.Actions, a)
		run.problem(fmt.Sprintf("%s: deleting the probe tunnel: %v", probe, err))
		return
	}
	run.res.Actions = append(run.res.Actions, a)
	run.r.log.Debug().Str("account", probe.account).Str("tunnel", tun.Name).Msg("deleted probe tunnel left behind")
}
