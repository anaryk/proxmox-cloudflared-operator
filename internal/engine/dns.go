package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
)

// minMaxGap is the least MaxGap the DNS reconciler gets; with a long poll
// interval it is six intervals.
const minMaxGap = 2 * time.Minute

// reconcileDNS brings the records in line. The admin's one-shot requests,
// confirmed deletes and adoptions, go only to an enforcing run and stay
// pending until a run decided on them; an adoption also waits until its
// tunnel can serve the name.
func (c *cycleRun) reconcileDNS() {
	mode := c.mode()
	in := reconcile.DNSInput{
		Records: slices.DeleteFunc(slices.Clone(c.plan.Records), func(rp planner.RecordPlan) bool {
			return c.zones.frozen[rp.AccountID]
		}),
		Zones:         c.zones.dns,
		Tunnels:       c.st.Tunnels,
		TunnelVerdict: c.tunnelVerdict,
		Keep:          c.kept(),
		InventoryOK:   c.snap.Complete && !c.col.PolicyInvalid,
		StillUnwanted: c.stillUnwanted,
		BeforeReplace: c.beforeReplace,
	}
	if mode == reconcile.Enforce {
		in.ConfirmDeletes = c.e.confirm != nil
		var ready []string
		ready, c.adoptWaits = c.adoptable(c.publishedThrough(in.Records))
		in.Adopt = make(map[string]bool, len(ready))
		for _, name := range ready {
			in.Adopt[name] = true
		}
	}
	res := c.e.dnsReconciler(c.dnsSettings()).Run(c.ctx, in, mode)
	c.settleRequests(in, res, mode)
	c.st.Actions = append(c.st.Actions, res.Actions...)
	c.st.Problems = append(c.st.Problems, res.Problems...)
	if res.Decided && res.Verdict == reconcile.WriterProceed {
		c.st.Conflicts, c.st.Lost = c.lookedAt(res)
	}
	if res.Verdict != reconcile.WriterProceed {
		c.st.WriterVerdict = verdictName(res.Verdict)
		return
	}
	c.writerStill("after the DNS run")
}

// publishedThrough maps every hostname with a record plan to the state of the
// tunnel its record points at.
func (c *cycleRun) publishedThrough(records []planner.RecordPlan) map[string]reconcile.TunnelState {
	out := make(map[string]reconcile.TunnelState, len(records))
	for _, rp := range records {
		for _, t := range c.st.Tunnels {
			if t.AccountID == rp.AccountID && t.Name == rp.TunnelName {
				out[strings.ToLower(rp.Name)] = t
			}
		}
	}
	return out
}

// lookedAt is what a DNS run that decided found in conflict or lost. The
// zones it did not manage in this cycle, as those of a frozen account, keep
// what was found there before.
func (c *cycleRun) lookedAt(res reconcile.DNSResult) ([]reconcile.Conflict, []string) {
	managed := make(map[string]bool, len(c.zones.dns))
	for _, z := range c.zones.dns {
		managed[z.Name] = true
	}
	conflicts := slices.Clone(res.Conflicts)
	for _, old := range c.st.Conflicts {
		if !managed[old.Zone] {
			conflicts = append(conflicts, old)
		}
	}
	slices.SortFunc(conflicts, func(a, b reconcile.Conflict) int {
		return cmp.Or(
			cmp.Compare(a.Zone, b.Zone),
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			cmp.Compare(a.Type, b.Type),
			cmp.Compare(a.Content, b.Content),
		)
	})
	lost := slices.Clone(res.Lost)
	for _, name := range c.st.Lost {
		if c.zones.zoneOf(name) == "" {
			lost = append(lost, name)
		}
	}
	return slices.Compact(conflicts), lost
}

// beforeReplace keeps a copy of the record an adoption is about to change or
// delete, in the adopted log; the adoption goes ahead only once it is kept.
func (c *cycleRun) beforeReplace(_ context.Context, zone reconcile.ZoneRef, rec cfapi.Record) error {
	return c.e.d.Store.AppendAdopted(c.now, zone.Name, rec)
}

// kept are the hostnames claimed or published in this cycle that have no
// record plan, as their rule answers 503: their records are left alone.
func (c *cycleRun) kept() map[string]bool {
	keep := make(map[string]bool, len(c.claims.Claims))
	for host := range c.claims.Claims {
		keep[host] = true
	}
	for _, st := range c.plan.Routes {
		keep[st.Hostname] = true
	}
	for _, rp := range c.plan.Records {
		delete(keep, rp.Name)
	}
	return keep
}

func (c *cycleRun) dnsSettings() reconcile.DNSSettings {
	return reconcile.DNSSettings{
		InstallID: c.install.ID,
		Grace:     time.Duration(c.settings.Grace),
		MaxGap:    max(minMaxGap, 6*time.Duration(c.settings.PollInterval)),
	}
}

// dnsReconciler returns the DNS reconciler, made anew only when its settings
// change: it remembers the names it saw wanted between runs.
func (e *Engine) dnsReconciler(s reconcile.DNSSettings) *reconcile.DNSReconciler {
	if e.dns == nil || e.dnsSet != s {
		if e.dns != nil {
			e.d.Log.Info().Dur("grace", s.Grace).Dur("maxGap", s.MaxGap).Msg("dns settings changed")
		}
		e.dns = reconcile.NewDNSReconciler(e.clients, e.d.Store.Tombstones(), e.writer, s, e.d.Now, e.d.Log)
		e.dnsSet = s
	}
	return e.dns
}

// recheck is the second look at the inventory that a cycle takes before its
// first DNS delete.
type recheck struct {
	done  bool
	names map[string]bool // lower-case hostnames a route or a held name still carries
	err   error
}

// stillUnwanted answers the DNS reconciler right before a delete: true only
// when a fresh, complete snapshot under a valid policy has no route and no
// held name for the hostname. The inventory is asked once per cycle.
func (c *cycleRun) stillUnwanted(ctx context.Context, name string) (bool, error) {
	r := &c.recheck
	if !r.done {
		r.done = true
		r.names, r.err = c.wantedNow(ctx)
	}
	if r.err != nil {
		return false, r.err
	}
	return !r.names[strings.ToLower(strings.TrimSuffix(name, "."))], nil
}

func (c *cycleRun) wantedNow(ctx context.Context) (map[string]bool, error) {
	ctx, cancel := c.e.timeout(ctx, refreshTimeout)
	defer cancel()
	snap := c.e.d.Inventory.Refresh(ctx)
	switch {
	case !snap.Complete:
		return nil, fmt.Errorf("the inventory is incomplete: %s", strings.Join(snap.Problems, "; "))
	case len(snap.Guests) == 0 && len(c.snap.Guests) > 0:
		// An empty answer is more likely a failure than every guest gone.
		return nil, errors.New("the inventory lists no guest any more")
	}
	col := c.collectFrom(snap)
	if col.PolicyInvalid {
		return nil, fmt.Errorf("the settings contain an invalid allow or deny pattern")
	}
	names := make(map[string]bool, len(col.Routes)+len(col.Held))
	for _, rt := range col.Routes {
		names[strings.ToLower(rt.Hostname)] = true
	}
	for _, h := range col.Held {
		names[strings.ToLower(h.Hostname)] = true
	}
	return names, nil
}
