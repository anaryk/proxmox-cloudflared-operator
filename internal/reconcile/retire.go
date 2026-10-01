package reconcile

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// retireHold says why the records at an unwanted name may not be deleted in
// this run, confirming their tombstone when the run keeps tombstones. It is
// empty when they are due.
func (run *dnsRun) retireHold(z *dnsZone, name string) string {
	switch {
	case !run.in.InventoryOK:
		return heldInventory
	case run.mode == Observe:
		return heldObserve
	}
	t := run.confirm(tombstoneKey(z.ID, name))
	if left := run.graceLeft(t); left > 0 {
		return fmt.Sprintf("grace period: %s left", left)
	}
	return ""
}

// retire deletes the records of this install at a name nobody wants any more,
// when nothing holds them. Right before the delete it reads them again and
// asks the inventory once more.
func (run *dnsRun) retire(ctx context.Context, z *dnsZone, name string) {
	records := z.owned[name]
	held := z.holds[name]
	switch {
	case held != "":
	case run.noDeletes != "":
		held = run.noDeletes
	case run.stopped:
		held = heldWriter
	case run.in.StillUnwanted == nil:
		held = heldUnconfirmed
	}
	if held != "" {
		for _, rec := range records {
			z.add(deleteAction(z, rec), held)
		}
		return
	}

	key := tombstoneKey(z.ID, name)
	doomed, err := run.stillOurs(ctx, z, name, records)
	if err != nil {
		run.problem(fmt.Sprintf("%s: reading the records again before deleting them: %v", z.about(name), err))
		return
	}
	if len(doomed) == 0 {
		run.stones.drop(key) // gone, or no longer ours
		return
	}
	unwanted, err := run.in.StillUnwanted(ctx, name)
	switch {
	case err != nil:
		run.problem(fmt.Sprintf("%s: asking the inventory before deleting: %v", z.about(name), err))
		for _, rec := range doomed {
			z.add(deleteAction(z, rec), heldAskFailed)
		}
		return
	case !unwanted:
		run.stones.drop(key) // published again
		return
	}
	done := true
	for _, rec := range doomed {
		if !run.write(z, deleteAction(z, rec), func() error { return deleteRecord(ctx, z, rec.ID) }) {
			done = false
		}
	}
	if done {
		run.stones.drop(key)
	}
}

// stillOurs reads the records at name again and returns those of list that
// it still shows with their name, their type and the marker of this install.
func (run *dnsRun) stillOurs(ctx context.Context, z *dnsZone, name string, list []cfapi.Record) ([]cfapi.Record, error) {
	fresh, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: name})
	if err != nil {
		return nil, err
	}
	var out []cfapi.Record
	for _, rec := range list {
		i := slices.IndexFunc(fresh, func(f cfapi.Record) bool { return f.ID == rec.ID })
		if i >= 0 && strings.EqualFold(fresh[i].Name, rec.Name) && isType(fresh[i], rec.Type) && run.owns(fresh[i]) {
			out = append(out, fresh[i])
		}
	}
	return out, nil
}

func deleteAction(z *dnsZone, rec cfapi.Record) Action {
	return Action{Kind: DeleteRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: %s %s", z.Name, rec.Type, rec.Content), Destructive: true}
}

// deleteRecord deletes a record; one that is gone already counts as deleted.
func deleteRecord(ctx context.Context, z *dnsZone, id string) error {
	if err := z.api.DeleteRecord(ctx, z.ID, id); err != nil && !cfapi.IsNotFound(err) {
		return err
	}
	return nil
}
