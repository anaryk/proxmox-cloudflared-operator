package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// ExpiryWarning is how long before its expiry a token is pointed out: by a
// recheck, by the doctor and by the command line alike.
const ExpiryWarning = 30 * 24 * time.Hour

const (
	// recheckEvery is how often the token of a credential is checked again.
	recheckEvery = 24 * time.Hour
	// recheckFailedEvery is how soon a check that found a token unusable, or
	// got no answer, is repeated: the failure may have been Cloudflare's own.
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
	e.keepReport(cred.ID, report)
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
func (e *Engine) keepReport(id string, r credentials.Report) {
	e.setReport(id, r)
	if !e.remembered {
		return
	}
	if err := e.d.Store.SaveEngineMemory(e.memory()); err != nil {
		e.d.Log.Warn().Err(err).Str("credential", id).Msg("saving the report of a credential check failed; the next cycle saves it")
	}
}

// setReport keeps the report of a check of a credential and when its token is
// checked again.
func (e *Engine) setReport(id string, r credentials.Report) {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	e.reports[id] = r
	every := recheckEvery
	if !r.Usable {
		every = recheckFailedEvery
	}
	e.recheckAt[id] = e.d.Now().Add(every)
}

// forgetReport drops what is known of the checks of a credential.
func (e *Engine) forgetReport(id string) {
	e.repMu.Lock()
	defer e.repMu.Unlock()
	delete(e.reports, id)
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
