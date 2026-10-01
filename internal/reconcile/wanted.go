package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// want gives a wanted name its CNAME to the tunnel.
func (run *dnsRun) want(ctx context.Context, z *dnsZone, name string) {
	if run.twice[name] {
		return
	}
	rp := z.wanted[name]
	ours := slices.DeleteFunc(slices.Clone(z.owned[name]), func(rec cfapi.Record) bool { return !isAddress(rec) })
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
	target := st.ID + tunnelDomain
	switch {
	case len(ours) == 0:
		run.claim(ctx, z, name, target)
	case len(ours) == 1 && isType(ours[0], "CNAME"):
		run.retarget(ctx, z, ours[0], target)
	default:
		// pco writes only CNAMEs, so address records under our marker were
		// made by hand; they are reported, not replaced.
		for _, rec := range ours {
			run.conflict(z, rec)
		}
	}
}

// retarget points a CNAME of this install at target. The record is read again
// first, and changed only while it is still a CNAME of ours at that name; its
// comment, which begins with the marker, is kept.
func (run *dnsRun) retarget(ctx context.Context, z *dnsZone, rec cfapi.Record, target string) {
	if strings.EqualFold(rec.Content, target) && rec.Proxied {
		return
	}
	a := Action{Kind: UpdateRecord, Target: rec.Name, Detail: pointDetail(z, target, rec)}
	if !run.proceed(z, a) {
		return
	}
	fresh, found, err := readByID(ctx, z, rec)
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: reading the record again before changing it: %v", z.about(rec.Name), err))
		return
	case !found:
		// Gone meanwhile: the next run creates it.
		return
	case !isType(fresh, "CNAME") || !strings.EqualFold(fresh.Name, rec.Name) || !run.owns(fresh):
		run.conflict(z, fresh)
		return
	}
	fresh.Content, fresh.Proxied = target, true
	run.write(z, a, func() error {
		_, err := z.api.UpdateRecord(ctx, z.ID, fresh)
		return err
	})
}

// readByID reads the record with the id of rec again, looking it up by the
// name it had.
func readByID(ctx context.Context, z *dnsZone, rec cfapi.Record) (cfapi.Record, bool, error) {
	found, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: strings.ToLower(rec.Name)})
	if err != nil {
		return cfapi.Record{}, false, err
	}
	i := slices.IndexFunc(found, func(f cfapi.Record) bool { return f.ID == rec.ID })
	if i < 0 {
		return cfapi.Record{}, false, nil
	}
	return found[i], true, nil
}

// claim creates the CNAME of a wanted name that has no record of this install,
// unless a record of someone else holds the name.
func (run *dnsRun) claim(ctx context.Context, z *dnsZone, name, target string) {
	create := Action{Kind: CreateRecord, Target: name, Detail: pointDetail(z, target)}
	if run.stopped {
		z.add(create, heldWriter)
		return
	}
	found, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: name})
	if err != nil {
		run.problem(fmt.Sprintf("%s: looking up the records of that name: %v", z.about(name), err))
		return
	}
	holders := slices.DeleteFunc(found, func(rec cfapi.Record) bool { return !isAddress(rec) })
	switch {
	case slices.ContainsFunc(holders, run.owns):
		run.problem(fmt.Sprintf("%s: a record of this install appeared during the run; trying again on the next one", z.about(name)))
	case len(holders) == 0:
		run.create(ctx, z, create, target)
	case run.adopt[name] && len(holders) == 1:
		run.adoptRecord(ctx, z, holders[0], target)
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
// record as it was goes into Replaced once it has been changed or deleted.
func (run *dnsRun) adoptRecord(ctx context.Context, z *dnsZone, rec cfapi.Record, target string) {
	if isType(rec, "CNAME") {
		upd := cfapi.Record{ID: rec.ID, Type: "CNAME", Name: rec.Name, Content: target, Proxied: true, Comment: run.marker}
		a := Action{Kind: UpdateRecord, Target: rec.Name, Detail: pointDetail(z, target, rec), Destructive: true}
		if run.write(z, a, func() error {
			_, err := z.api.UpdateRecord(ctx, z.ID, upd)
			return err
		}) {
			run.res.Replaced = append(run.res.Replaced, rec)
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
	case run.noDeletes != "":
		held = run.noDeletes
	}
	if held != "" {
		z.add(del, held)
		z.add(add, held)
		return
	}
	if !run.write(z, del, func() error { return deleteRecord(ctx, z, rec.ID) }) {
		if run.stopped {
			z.add(add, heldWriter)
		}
		return
	}
	run.res.Replaced = append(run.res.Replaced, rec)
	if !run.create(ctx, z, add, target) {
		run.restore(ctx, z, rec)
	}
}

// restore puts back a record an adoption deleted when the CNAME that was to
// replace it could not be created. When that is not possible either, the
// problem carries the whole record, so that the admin can restore it by hand.
func (run *dnsRun) restore(ctx context.Context, z *dnsZone, rec cfapi.Record) {
	if !run.stopped {
		back := cfapi.Record{Type: rec.Type, Name: rec.Name, Content: rec.Content, Proxied: rec.Proxied, Comment: rec.Comment}
		a := Action{Kind: CreateRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: put back %s %s", z.Name, rec.Type, rec.Content)}
		if run.write(z, a, func() error {
			_, err := z.api.CreateRecord(ctx, z.ID, back)
			return err
		}) {
			run.problem(fmt.Sprintf("%s: adoption failed; the original record was put back", z.about(rec.Name)))
			return
		}
	}
	run.problem(fmt.Sprintf("%s: adoption failed and the original record is gone; restore it by hand: %s", z.about(rec.Name), describe(rec)))
}

// describe spells out a record with everything needed to create it again.
func describe(rec cfapi.Record) string {
	return fmt.Sprintf("%s %s %s (proxied %t, comment %q, id %s)", rec.Type, rec.Name, rec.Content, rec.Proxied, rec.Comment, rec.ID)
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
