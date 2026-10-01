package reconcile

import (
	"context"
	"fmt"
	"slices"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// retire deletes the records of this install at a name nobody wants any more,
// unless the inventory, the mode, the grace or the mass delete guard holds
// them.
func (run *dnsRun) retire(ctx context.Context, z *dnsZone, name string) {
	records := z.owned[name]
	if held := run.retireHold(z, name); held != "" {
		for _, rec := range records {
			z.add(deleteAction(z, rec), held)
		}
		return
	}

	// The listing may be old by now: delete only what a fresh read still shows
	// as ours and unwanted.
	fresh, err := z.api.Records(ctx, z.ID, cfapi.RecordFilter{Name: name})
	if err != nil {
		run.problem(fmt.Sprintf("%s: reading the records again before deleting them: %v", z.about(name), err))
		return
	}
	done := true
	for _, rec := range records {
		i := slices.IndexFunc(fresh, func(f cfapi.Record) bool { return f.ID == rec.ID })
		if i < 0 || !run.owns(fresh[i]) || z.isWanted(fresh[i].Name) {
			continue
		}
		id := fresh[i].ID
		if !run.write(z, deleteAction(z, fresh[i]), func() error { return deleteRecord(ctx, z, id) }) {
			done = false
		}
	}
	if done {
		run.stones.drop(tombstoneKey(z.ID, name))
	}
}

// retireHold says why the records at an unwanted name are not deleted in this
// run, starting their grace when it has not started yet. It is empty when they
// may be deleted.
func (run *dnsRun) retireHold(z *dnsZone, name string) string {
	switch {
	case !run.in.InventoryOK:
		return heldInventory
	case run.mode == Observe:
		return heldObserve
	}
	at := run.stones.since(tombstoneKey(z.ID, name), run.now)
	if left := run.graceLeft(at); left > 0 {
		return fmt.Sprintf("grace period: %s left", left)
	}
	return run.guard
}

func deleteAction(z *dnsZone, rec cfapi.Record) Action {
	return Action{Kind: DeleteRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: %s %s", z.Name, rec.Type, rec.Content), Destructive: true}
}
