package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// adoptTimeout bounds the create that refills a name an adoption emptied,
// and the put-back after it, which outlive a cancelled run.
const adoptTimeout = 30 * time.Second

// want gives a wanted name its CNAME to the tunnel.
func (run *dnsRun) want(ctx context.Context, z *dnsZone, name string) {
	if run.twice[name] {
		return
	}
	rp := z.wanted[name]
	ours := slices.DeleteFunc(slices.Clone(z.owned[name]), func(rec cfapi.Record) bool { return !isAddress(rec) })
	if len(ours) > 1 || len(ours) == 1 && !isType(ours[0], "CNAME") {
		// pco writes only CNAMEs, so address records under our marker were
		// made by hand; they are reported, not replaced, whatever the tunnel.
		for _, rec := range ours {
			run.conflict(z, rec)
		}
		return
	}
	st, known := run.tunnels[tunnelRef{rp.AccountID, rp.TunnelName}]
	held := ""
	switch {
	case known && st.Unknown:
		held = heldTunnelUnknown
	case !known || !st.Exists || st.ID == "":
		held = heldNoTunnel
	}
	if held != "" {
		kind := CreateRecord
		if len(ours) > 0 {
			kind = UpdateRecord
		}
		z.add(Action{Kind: kind, Target: name, Detail: fmt.Sprintf("in zone %s: tunnel %s has no known id", z.Name, rp.TunnelName)}, held)
		return
	}
	// A tunnel whose configuration was not verified may answer the name
	// with the catch-all: a working record is not pointed at it.
	p := pointing{target: st.ID + tunnelDomain}
	if run.mode == Enforce && !st.Verified {
		p.held = heldUnverified
	}
	if len(ours) == 0 {
		run.claim(ctx, z, name, p)
		return
	}
	run.retarget(ctx, z, ours[0], p)
}

// pointing is where a wanted name is to point, and why it may not yet when
// held is set.
type pointing struct {
	target string
	held   string
}

// retarget points a CNAME of this install at the target. The record is read
// again first, and changed only while it is still a CNAME of ours at that
// name; its comment, which begins with the marker, is kept.
func (run *dnsRun) retarget(ctx context.Context, z *dnsZone, rec cfapi.Record, p pointing) {
	if strings.EqualFold(rec.Content, p.target) && rec.Proxied {
		return
	}
	a := Action{Kind: UpdateRecord, Target: rec.Name, Detail: pointDetail(z, p.target, rec)}
	if p.held != "" {
		z.add(a, p.held)
		return
	}
	if !run.proceed(z, a) {
		return
	}
	fresh, found, ours, err := run.recheck(ctx, z, rec)
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: reading the record again before changing it: %v", z.about(rec.Name), err))
		return
	case !found:
		run.r.log.Debug().Str("zone", z.Name).Str("record", rec.Name).Str("id", rec.ID).
			Msg("record to point at the tunnel is gone or renamed; the next run looks again")
		return
	case !ours:
		run.conflict(z, fresh)
		return
	}
	// A proxied record has no TTL of its own: Cloudflare spells that 1.
	fresh.Content, fresh.Proxied, fresh.TTL = p.target, true, 1
	run.write(z, a, func() error {
		_, err := z.api.UpdateRecord(ctx, z.ID, fresh)
		return err
	})
}

// claim creates the CNAME of a wanted name that has no record of this install,
// unless a record of someone else holds the name. Only an address record holds
// it: a TXT record stays beside the proxied CNAME, as Cloudflare allows, and
// is neither a conflict nor adopted; beside a record of another type the
// create is tried and Cloudflare's answer decides.
func (run *dnsRun) claim(ctx context.Context, z *dnsZone, name string, p pointing) {
	create := Action{Kind: CreateRecord, Target: name, Detail: pointDetail(z, p.target)}
	if run.stopped != "" {
		z.add(create, run.stopped)
		return
	}
	found, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: name})
	if err != nil {
		run.unlisted(z)
		run.problem(fmt.Sprintf("%s: looking up the records of that name: %v", z.about(name), err))
		return
	}
	holders := slices.DeleteFunc(found, func(rec cfapi.Record) bool { return !isAddress(rec) })
	switch {
	case slices.ContainsFunc(holders, run.owns):
		run.problem(fmt.Sprintf("%s: a record of this install appeared during the run; trying again on the next one", z.about(name)))
	case len(holders) == 0 && p.held != "":
		z.add(create, p.held)
	case len(holders) == 0:
		run.create(ctx, z, create, p.target)
	case run.adopt[name] && len(holders) == 1:
		run.adoptRecord(ctx, z, holders[0], p)
	default:
		for _, rec := range holders {
			run.conflict(z, rec)
		}
	}
}

// create creates the CNAME of a and reports whether it was created.
func (run *dnsRun) create(ctx context.Context, z *dnsZone, a Action, target string) bool {
	rec := cfapi.Record{Type: "CNAME", Name: a.Target, Content: target, Proxied: true, Comment: run.marker}
	return run.write(z, a, func() error {
		_, err := z.api.CreateRecord(ctx, z.ID, rec)
		return err
	})
}

// adoptRecord takes over the one record that holds a name the admin asked to
// adopt: a CNAME is changed in place, an address record is replaced. The
// record as it was goes into Replaced as the call that changes or deletes it
// goes out: a call whose answer is lost may have landed.
func (run *dnsRun) adoptRecord(ctx context.Context, z *dnsZone, rec cfapi.Record, p pointing) {
	target := p.target
	if isType(rec, "CNAME") {
		upd := cfapi.Record{ID: rec.ID, Type: "CNAME", Name: rec.Name, Content: target, Proxied: true, Comment: run.marker}
		a := Action{Kind: UpdateRecord, Target: rec.Name, Detail: pointDetail(z, target, rec), Destructive: true}
		switch {
		case p.held != "":
			z.add(a, p.held)
		case run.proceed(z, a) && run.snapshot(ctx, z, rec, a):
			run.commit(z, a, func() error {
				run.res.Replaced = append(run.res.Replaced, rec)
				_, err := z.api.UpdateRecord(ctx, z.ID, upd)
				return err
			})
		}
		return
	}

	del := Action{Kind: DeleteRecord, Target: rec.Name, Detail: pointDetail(z, target, rec), Destructive: true}
	add := Action{Kind: CreateRecord, Target: rec.Name, Detail: pointDetail(z, target, rec), Destructive: true}
	held := ""
	switch {
	case !run.in.InventoryOK:
		held = heldInventory
	case run.mode == Observe:
		held = heldObserve
	case p.held != "":
		held = p.held
	case run.noDeletes != "":
		held = run.noDeletes
	}
	if held != "" {
		z.add(del, held)
		z.add(add, held)
		return
	}
	if !run.snapshot(ctx, z, rec, del, add) {
		return
	}
	if !run.write(z, del, func() error {
		run.res.Replaced = append(run.res.Replaced, rec)
		return deleteRecord(ctx, z, rec.ID)
	}) {
		if run.stopped != "" {
			z.add(add, run.stopped)
		}
		return
	}
	// The name is empty now: a run cancelled from here on must still fill it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), adoptTimeout)
	defer cancel()
	if !run.create(ctx, z, add, target) {
		run.restore(ctx, z, rec)
	}
}

// snapshot hands the record an adoption is about to replace to BeforeReplace,
// when the input has one, and reports whether the adoption may go on. When
// it may not, the actions of the adoption are held.
func (run *dnsRun) snapshot(ctx context.Context, z *dnsZone, rec cfapi.Record, actions ...Action) bool {
	if run.in.BeforeReplace == nil {
		return true
	}
	if err := run.in.BeforeReplace(ctx, z.ZoneRef, rec); err != nil {
		for _, a := range actions {
			z.add(a, heldSnapshot)
		}
		run.problem(fmt.Sprintf("%s: storing the record before the adoption: %v", z.about(rec.Name), err))
		return false
	}
	return true
}

// restore puts back a record an adoption deleted when the CNAME that was to
// replace it could not be created. When that is not possible either, the
// problem carries the whole record, so that the admin can restore it by hand.
// A run that stopped as the writer changed leaves the name empty: the writer
// that runs next creates the CNAME there.
func (run *dnsRun) restore(ctx context.Context, z *dnsZone, rec cfapi.Record) {
	if run.stopped == "" {
		back := cfapi.Record{Type: rec.Type, Name: rec.Name, Content: rec.Content, Proxied: rec.Proxied, TTL: rec.TTL, Comment: rec.Comment}
		a := Action{Kind: CreateRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: put back %s %s", z.Name, rec.Type, rec.Content)}
		if run.write(z, a, func() error {
			_, err := z.api.CreateRecord(ctx, z.ID, back)
			return err
		}) {
			run.problem(fmt.Sprintf("%s: adoption failed; the original record was put back", z.about(rec.Name)))
			return
		}
	}
	if run.stopped != "" {
		run.problem(fmt.Sprintf("%s: the record was removed for the adoption; the current writer creates the CNAME on its next run; the original was %s",
			z.about(rec.Name), describe(rec)))
		return
	}
	run.problem(fmt.Sprintf("%s: adoption failed and the original record is gone; restore it by hand: %s", z.about(rec.Name), describe(rec)))
}

// describe spells out a record with everything needed to create it again.
func describe(rec cfapi.Record) string {
	return fmt.Sprintf("%s %s %s (proxied %t, ttl %d, comment %q, id %s)", rec.Type, rec.Name, rec.Content, rec.Proxied, rec.TTL, rec.Comment, rec.ID)
}

// conflict reports a record at a wanted name that pco will not change. One
// that is not ours and still points at one of our tunnels has lost its marker.
func (run *dnsRun) conflict(z *dnsZone, rec cfapi.Record) {
	run.res.Conflicts = append(run.res.Conflicts, Conflict{Zone: z.Name, Name: rec.Name, Type: rec.Type, Content: rec.Content})
	if run.pointsAtOurs(rec) && !run.owns(rec) {
		run.res.Lost = append(run.res.Lost, rec.Name)
	}
}

// pointDetail describes pointing a name at target, and the record that was
// there before, if any.
func pointDetail(z *dnsZone, target string, was ...cfapi.Record) string {
	d := fmt.Sprintf("in zone %s: CNAME %s", z.Name, target)
	for _, rec := range was {
		d += fmt.Sprintf(", was %s %s", rec.Type, rec.Content)
	}
	return d
}
