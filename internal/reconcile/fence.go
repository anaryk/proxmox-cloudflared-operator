package reconcile

import (
	"fmt"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
)

// start reads the identity the run writes as and reports whether the run may
// go on. Every DNS write runs under step 1 of the write procedure, so a run
// that is not the stored writer does nothing at all, not even a listing.
func (run *dnsRun) start() bool {
	us, stored, err := run.r.writer()
	if err != nil {
		run.problem(fmt.Sprintf("dns: reading the writer identity: %v", err))
		return false
	}
	if err := us.Validate(); err != nil {
		run.problem(fmt.Sprintf("dns: cannot write as this writer: %v", err))
		return false
	}
	if us.InstallID != run.r.s.InstallID {
		// The marker comes from the settings, the sentinel from the writer.
		run.problem(fmt.Sprintf("dns: the writer is of install %s, the settings are for install %q", us.InstallID, run.r.s.InstallID))
		return false
	}
	run.us = us
	return run.admit("dns: ", us, stored)
}

// fenced asks for the writer identity again right before a write to
// Cloudflare or to the tombstone store, and reports whether the write may go
// ahead. Once it may not, the run is stopped.
func (run *dnsRun) fenced(what string) bool {
	if run.stopped {
		return false
	}
	us, stored, err := run.r.writer()
	if err != nil {
		run.problem(fmt.Sprintf("%s: reading the writer identity: %v; writing stops", what, err))
		run.stopped = true
		return false
	}
	return run.admit(what+": ", us, stored)
}

// admit takes an answer of the writer callback and reports whether the run
// may go on. If it may not, the run stops as stale.
func (run *dnsRun) admit(prefix string, us, stored planner.Writer) bool {
	// The rule is the tunnel reconciler's; it needs only the identity the run
	// started with.
	fault := (&tunnelRun{us: run.us}).writerFault(us, stored)
	if fault == "" {
		return true
	}
	run.problem(prefix + fault + "; this writer is stale and stops")
	run.res.Verdict = WriterStale
	run.stopped = true
	return false
}

// proceed reports whether the run may make the calls that lead to the change
// a. When it may not, a is reported held.
func (run *dnsRun) proceed(z *dnsZone, a Action) bool {
	switch {
	case run.mode == Observe:
		z.add(a, heldObserve)
	case run.stopped:
		z.add(a, heldWriter)
	default:
		return true
	}
	return false
}

// write makes one change, or only records it when the run may not write, and
// reports whether the change was made.
func (run *dnsRun) write(z *dnsZone, a Action, call func() error) bool {
	if !run.proceed(z, a) {
		return false
	}
	if !run.fenced(z.about(a.Target)) {
		z.add(a, heldWriter)
		return false
	}
	if err := call(); err != nil {
		z.add(a, err.Error())
		run.problem(fmt.Sprintf("%s: %s the record: %v", z.about(a.Target), verb(a.Kind), err))
		return false
	}
	z.add(a, "")
	run.r.log.Info().Str("zone", z.Name).Str("record", a.Target).Str("action", string(a.Kind)).Str("detail", a.Detail).Msg("changed dns record")
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
