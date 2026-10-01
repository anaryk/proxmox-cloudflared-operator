package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// probeAge is how old a probe record must be before it counts as left
// behind; a younger one may belong to a check still running.
const probeAge = 10 * time.Minute

// isProbe reports whether a record is one the credential check leaves behind
// when it cannot delete it.
func isProbe(rec cfapi.Record, marker string) bool { return rec.Comment == marker+" probe" }

// sweepProbes deletes probe records old enough to be left behind. They have no
// grace and do not count towards the mass delete guard.
func (run *dnsRun) sweepProbes(ctx context.Context, z *dnsZone) {
	for _, rec := range z.probes {
		if run.now.Sub(rec.ModifiedOn) <= probeAge {
			continue
		}
		a := Action{Kind: DeleteRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: %s probe left behind", z.Name, rec.Type), Destructive: true}
		run.write(z, a, func() error { return deleteRecord(ctx, z, rec.ID) })
	}
}
