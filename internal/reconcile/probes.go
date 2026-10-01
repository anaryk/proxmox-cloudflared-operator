package reconcile

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
)

// probeAge is how old a probe record must be before it counts as left
// behind; a younger one may belong to a check still running.
const probeAge = 10 * time.Minute

// isProbe reports whether a record is one the credential check leaves behind
// when it cannot delete it: a TXT record named like a probe that carries
// exactly the probe comment of this install. Anything else with that comment
// is an ordinary record of this install.
func (run *dnsRun) isProbe(rec cfapi.Record) bool {
	return isType(rec, "TXT") &&
		strings.HasPrefix(strings.ToLower(rec.Name), credentials.ProbeRecordPrefix) &&
		rec.Comment == run.marker+" probe"
}

// leftBehind reports whether a probe is old enough to be swept. A probe whose
// age is not known stays.
func (run *dnsRun) leftBehind(rec cfapi.Record) bool {
	return !rec.ModifiedOn.IsZero() && run.now.Sub(rec.ModifiedOn) > probeAge
}

// sweepProbes deletes probe records left behind. They have no grace and do
// not count towards the mass delete guard, but each is read again right
// before its delete.
func (run *dnsRun) sweepProbes(ctx context.Context, z *dnsZone) {
	for _, rec := range z.probes {
		if !run.leftBehind(rec) {
			continue
		}
		a := Action{Kind: DeleteRecord, Target: rec.Name, Detail: fmt.Sprintf("in zone %s: %s probe left behind", z.Name, rec.Type), Destructive: true}
		if run.noDeletes != "" {
			z.add(a, run.noDeletes)
			continue
		}
		if !run.proceed(z, a) {
			continue
		}
		fresh, found, ours, err := run.recheck(ctx, z, rec)
		switch {
		case err != nil:
			run.problem(fmt.Sprintf("%s: reading the probe again before deleting it: %v", z.about(rec.Name), err))
		case found && ours && run.isProbe(fresh) && run.leftBehind(fresh):
			run.write(z, a, func() error { return deleteRecord(ctx, z, fresh.ID) })
		}
	}
}
