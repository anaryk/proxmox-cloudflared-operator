package engine

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ExpiryWarning is how long before its expiry a token is pointed out: by a
// recheck, by the doctor and by the command line alike.
const ExpiryWarning = 30 * 24 * time.Hour

const (
	// recheckEvery is how often the token of a credential is checked again.
	recheckEvery = 24 * time.Hour
	// recheckFailedEvery is how soon a check that found a token unusable, got
	// no answer or left a zone out is repeated: the failure may have been
	// Cloudflare's own, and a permission granted since is picked up.
	recheckFailedEvery = 15 * time.Minute
	// recheckTimeout bounds one check.
	recheckTimeout = time.Minute
)

// recheck checks the token of every credential again that is due: each once
// after the daemon started, and then about once a day. The checks are shallow,
// so they change nothing at Cloudflare, and run one after the other outside
// the cycle lock, which is taken only to keep a report. Nothing is checked
// while the last cycle held because of the store. A call while another one
// runs returns at once.
func (e *Engine) recheck(ctx context.Context) {
	if !e.rechecking.CompareAndSwap(false, true) {
		return
	}
	defer e.rechecking.Store(false)
	if e.storeHeld.Load() {
		return
	}
	install, err := e.installID()
	if err != nil {
		return
	}
	creds, err := e.d.Store.Credentials()
	if err != nil {
		return
	}
	for _, cred := range creds {
		if ctx.Err() != nil {
			return
		}
		if e.recheckDue(cred.ID) {
			e.recheckOne(ctx, install, cred)
		}
	}
}

// recheckDue reports whether the token of credential id is to be checked now.
// A clock that went back far enough makes it due as well.
func (e *Engine) recheckDue(id string) bool {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	at, checked := e.recheckAt[id]
	now := e.d.Now()
	return !checked || !now.Before(at) || at.Sub(now) > recheckEvery
}

func (e *Engine) recheckOne(ctx context.Context, install string, cred store.Credential) {
	log := e.d.Log.With().Str("credential", cred.ID).Logger()
	api, err := e.d.NewClient(cred)
	if err != nil {
		log.Warn().Err(err).Msg("checking the token again: building a Cloudflare client failed")
		e.recheckLater(cred.ID)
		return
	}
	cctx, cancel := e.timeout(ctx, recheckTimeout)
	report := e.checker(install).Run(cctx, api, false)
	cut := cctx.Err()
	cancel()
	switch {
	case ctx.Err() != nil:
		return
	case cut != nil:
		log.Warn().Dur("timeout", recheckTimeout).Msg("checking the token again did not finish in time")
		e.recheckLater(cred.ID)
		return
	}
	if err := e.acquire(ctx); err != nil {
		return
	}
	defer e.release()
	// The credential may have been removed or given another token meanwhile:
	// the report is of a token it no longer has.
	now, err := e.credential(cred.ID)
	if err != nil || !now.Token.Equal(cred.Token) {
		return
	}
	if report.Unanswered() {
		// Cloudflare saying nothing is no verdict on the token: what the last
		// check found stays.
		e.noteUnanswered(cred, report)
		e.recheckLater(cred.ID)
		return
	}
	e.keepReport(cred, report)
	e.noteExpiry(cred, report)
}

// noteUnanswered says, as an event, that a check of a token got no answer.
func (e *Engine) noteUnanswered(cred store.Credential, r credentials.Report) {
	e.events.add(Event{
		At: e.d.Now(), Level: levelWarn, Kind: kindCredential, Subject: cred.ID,
		Message: fmt.Sprintf("the token of credential %q could not be checked again: %s; it is checked again in %s",
			cred.Label, failedChecks(r), recheckFailedEvery),
	})
}

// recheckLater puts the next check of a credential whose check could not be
// made off for a while.
func (e *Engine) recheckLater(id string) {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	e.recheckAt[id] = e.d.Now().Add(recheckFailedEvery)
}

// keepReport keeps the report of a check of a credential, and when the
// memory of the engine was read, saves it with that: a restart keeps the last
// report. A memory that cannot be saved now is saved by the next cycle. The
// caller holds the cycle lock.
func (e *Engine) keepReport(cred store.Credential, r credentials.Report) {
	first := e.setReport(cred.ID, r)
	e.noteRefusals(cred, r, first)
	if !e.remembered {
		return
	}
	if err := e.d.Store.SaveEngineMemory(e.memory()); err != nil {
		e.d.Log.Warn().Err(err).Str("credential", cred.ID).Msg("saving the report of a credential check failed; the next cycle saves it")
	}
}

// setReport keeps the report of a check of a credential and when its token is
// checked again, and reports whether it is the first one of the credential.
func (e *Engine) setReport(id string, r credentials.Report) (first bool) {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	prev, had := e.reports[id]
	e.refusedAgain[id] = leftOutByBoth(prev, r)
	e.reports[id] = r
	every := recheckEvery
	if !r.Usable || len(r.Excluded) > 0 {
		every = recheckFailedEvery
	}
	e.recheckAt[id] = e.d.Now().Add(every)
	return !had
}

// leftOutByBoth returns the ids of the zones both reports left out.
func leftOutByBoth(prev, next credentials.Report) map[string]bool {
	out := make(map[string]bool)
	for _, x := range next.Excluded {
		if slices.ContainsFunc(prev.Excluded, func(p credentials.Exclusion) bool { return p.ZoneID == x.ZoneID }) {
			out[x.ZoneID] = true
		}
	}
	return out
}

// noteRefusals looks at the zones that credential cred serves and whose DNS
// its last check was refused. After the first check of the credential they
// are taken as never served: no check showed their DNS readable, so pco cannot
// have managed their records. After a later one a zone refused for the first
// time is a warning; its account is frozen until the next check, due within
// recheckFailedEvery, confirms the refusal or clears it. The caller holds the
// cycle lock.
func (e *Engine) noteRefusals(cred store.Credential, r credentials.Report, first bool) {
	e.repMu.Lock()
	again := e.refusedAgain[cred.ID]
	e.repMu.Unlock()
	for _, x := range r.Excluded {
		name := zoneName(cfapi.Zone{Name: x.Zone})
		served, ok := e.zones.served[name]
		switch {
		case !ok || served.CredentialID != cred.ID || again[x.ZoneID]:
		case first:
			delete(e.zones.served, name)
		default:
			e.events.add(Event{
				At: e.d.Now(), Level: levelWarn, Kind: kindCredential, Subject: cred.ID, Account: served.AccountID,
				Message: fmt.Sprintf("the check of credential %q was refused the DNS of zone %s, which it serves; "+
					"account %s is left as it is, and the token is checked again in %s", cred.Label, name, served.AccountID, recheckFailedEvery),
			})
		}
	}
}

// recheckBy makes the next check of credential id due at by, when it was due
// later.
func (e *Engine) recheckBy(id string, by time.Time) {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	if at, ok := e.recheckAt[id]; ok && by.Before(at) {
		e.recheckAt[id] = by
	}
}

// forgetReport drops what is known of the checks of a credential.
func (e *Engine) forgetReport(id string) {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	delete(e.reports, id)
	delete(e.refusedAgain, id)
	delete(e.recheckAt, id)
}

// noteExpiry warns, as an event, of a token that expires within
// ExpiryWarning. One that has expired cannot be used, which a problem line
// says.
func (e *Engine) noteExpiry(cred store.Credential, r credentials.Report) {
	if !r.Usable || r.Token.ExpiresOn == nil {
		return
	}
	expires := *r.Token.ExpiresOn
	left := expires.Sub(e.d.Now())
	if left >= ExpiryWarning {
		return
	}
	e.events.add(Event{
		At: e.d.Now(), Level: levelWarn, Kind: kindCredential, Subject: cred.ID,
		Message: fmt.Sprintf("the token of credential %q expires at %s, in %s; add a new token with pco credential add, then remove this one",
			cred.Label, expires.UTC().Format(time.RFC3339), daysText(max(left, 0))),
	})
}

// daysText says how many whole days d is: "less than a day", "1 day", "3 days".
func daysText(d time.Duration) string {
	switch n := int(d / (24 * time.Hour)); n {
	case 0:
		return "less than a day"
	case 1:
		return "1 day"
	default:
		return fmt.Sprintf("%d days", n)
	}
}
