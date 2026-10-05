package reconcile

import (
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
)

// start reads the identity the run writes as and reports whether the run may
// go on. Every DNS write runs under step 1 of the write procedure, so a run
// that is not the stored writer does nothing at all, not even a listing.
func (run *dnsRun) start() bool {
	us, fault, stale := startWriter(run.r.writer)
	// A valid identity was read: the run may write, or it is stale.
	identified := fault == "" || stale
	if identified && us.InstallID != run.r.s.InstallID {
		// The marker comes from the settings, the sentinel from the writer.
		fault, stale = fmt.Sprintf("the writer is of install %s, the settings are for install %q", us.InstallID, run.r.s.InstallID), false
	}
	run.us = us
	if fault != "" && !stale {
		run.problem("dns: " + fault)
		return false
	}
	return run.admit("dns: ", fault, stale)
}

// fenced asks for the writer identity again right before a write to
// Cloudflare or to the tombstone store, and reports whether the write may go
// ahead. Once it may not, the run is stopped.
func (run *dnsRun) fenced(what string) bool {
	if run.stopped != "" {
		return false
	}
	fault, stale := recheckWriter(run.r.writer, run.us)
	return run.admit(what+": ", fault, stale)
}

// admit takes what a check of the writer found and reports whether the run
// may go on. A run that may not is stopped; as stale when another writer is
// stored.
func (run *dnsRun) admit(prefix, fault string, stale bool) bool {
	switch {
	case stale:
		run.problem(prefix + fault + "; this writer is stale and stops")
		run.res.Verdict = WriterStale
		run.stopped = heldWriter
	case fault != "":
		run.problem(prefix + fault + "; writing stops")
		run.stopped = heldUnreadable
	default:
		return true
	}
	return false
}

// proceed reports whether the run may make the calls that lead to the change
// a. When it may not, a is reported held.
func (run *dnsRun) proceed(z *dnsZone, a Action) bool {
	switch {
	case run.mode == Observe:
		z.add(a, heldObserve)
	case run.stopped != "":
		z.add(a, run.stopped)
	case run.spent[z.CredentialID]:
		z.add(a, HeldBudget)
	default:
		return true
	}
	return false
}

// spend reports whether err, of a call for zone z, is a refusal of the rate
// limit, and when it is, stops the writes of the run through the credential of
// z: the rest of them wait for a later run. Another credential has a limit of
// its own.
func (run *dnsRun) spend(z *dnsZone, err error) bool {
	if !cfapi.IsRateLimited(err) {
		return false
	}
	run.spent[z.CredentialID] = true
	return true
}

// write makes one change, or only records it when the run may not write, and
// reports whether the change was made.
func (run *dnsRun) write(z *dnsZone, a Action, call func() error) bool {
	return run.proceed(z, a) && run.commit(z, a, call)
}

// commit makes a change that proceed let through, once the writer callback
// still names this writer, and reports whether the change was made.
func (run *dnsRun) commit(z *dnsZone, a Action, call func() error) bool {
	if !run.fenced(z.about(a.Target)) {
		z.add(a, run.stopped)
		return false
	}
	if err := call(); err != nil {
		if run.spend(z, err) {
			z.add(a, HeldBudget)
			return false
		}
		z.add(a, err.Error())
		run.problem(fmt.Sprintf("%s: %s the record: %v", z.about(a.Target), verb(a.Kind), err))
		return false
	}
	z.add(a, "")
	run.r.log.Debug().Str("zone", z.Name).Str("record", a.Target).Str("action", string(a.Kind)).Str("detail", a.Detail).Msg("changed dns record")
	return true
}

func verb(k ActionKind) string {
	switch k {
	case CreateRecord:
		return "creating"
	case UpdateRecord:
		return "updating"
	}
	return "deleting"
}
