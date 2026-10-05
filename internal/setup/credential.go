package setup

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/engine"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

// checkTimeout bounds the two checks of a token, the deep one of which makes
// and removes test objects in every zone and account.
const checkTimeout = 3 * time.Minute

const addLater = "add one later with pco credential add --label <label>"

// addCredential checks the Cloudflare token, if there is one, and stores it
// when it is usable. The store's credentials are the daemon's while it runs,
// so a token is only stored while it does not.
func (r *run) addCredential(ctx context.Context) error {
	creds, err := r.st.Credentials()
	if err != nil {
		return fmt.Errorf("reading the credentials: %w", err)
	}
	switch {
	case r.token != "" && known(creds, r.token):
		r.ask.Info("credentials: the token is stored already, nothing needed")
		return nil
	case r.token == "" && len(creds) > 0:
		r.ask.Info("credentials: %d stored, nothing needed", len(creds))
		return nil
	case r.token == "" && r.o.Yes:
		r.ask.Info("credentials: no Cloudflare token given; " + addLater)
		return nil
	}
	running, err := r.daemonRunning(ctx)
	if err != nil {
		return err
	}
	if running {
		r.ask.Info(`pco is running: add credentials with "pco credential add"`)
		return nil
	}
	if r.token == "" {
		token, err := r.ask.Secret("Cloudflare API token (empty to skip): ")
		if err != nil {
			return fmt.Errorf("reading the token: %w", err)
		}
		if r.token = strings.TrimSpace(token); r.token == "" {
			r.ask.Info("credentials: skipped; " + addLater)
			return nil
		}
		if known(creds, r.token) {
			r.ask.Info("credentials: the token is stored already, nothing needed")
			return nil
		}
	}
	return r.checkAndStore(ctx, creds)
}

func known(creds []store.Credential, token string) bool {
	secret := store.NewSecret(token)
	return slices.ContainsFunc(creds, func(c store.Credential) bool { return c.Token.Equal(secret) })
}

// checkAndStore checks what the token can do, shallow and then deep, and
// stores it only when it can do what pco needs. A token that cannot is no
// failure of setup: the daemon idles until one is added.
func (r *run) checkAndStore(ctx context.Context, creds []store.Credential) error {
	api, err := r.newClient(r.token)
	if err != nil {
		r.ask.Warn("credentials: the token cannot be used (%v); it is not stored, %s", err, addLater)
		return nil
	}
	suffix, err := r.randomHex(8)
	if err != nil {
		return err
	}
	checker := credentials.NewChecker(r.install.ID, r.now, func() string { return suffix })
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	report := checker.Run(ctx, api, false)
	if report.Usable {
		r.ask.Info("credentials: checking write access with a test DNS record and a test tunnel, which are removed again")
		report = checker.Run(ctx, api, true)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("checking the token: %w", err)
	}
	r.showReport(report)
	switch {
	case report.Unanswered():
		r.ask.Warn("credentials: Cloudflare did not answer every check, so the token is not stored; %s", addLater)
		return nil
	case !report.Usable:
		r.ask.Warn("credentials: the token cannot do what pco needs, so it is not stored; grant what is missing and %s", addLater)
		return nil
	}
	cred, err := engine.NewCredential(creds, credentialLabel, store.NewSecret(r.token), r.now())
	if err != nil {
		return err
	}
	if err := r.st.SaveCredential(cred); err != nil {
		return fmt.Errorf("storing the credential: %w", err)
	}
	r.ask.Info("credentials: stored the token as credential %s (%s)", cred.ID, cred.Label)
	r.keepReport(cred.ID, report)
	return nil
}

// keepReport keeps the report of the check in the engine's memory, so that the
// daemon starts knowing which zones the token may not read. A memory of
// another install is the daemon's to set aside, and one that cannot be read
// is the daemon's to report; without the report, the daemon checks the token
// before its first cycle.
func (r *run) keepReport(id string, report credentials.Report) {
	m, err := r.st.EngineMemory()
	if err != nil || (m.InstallID != "" && m.InstallID != r.install.ID) {
		return
	}
	m.InstallID = r.install.ID
	m.Reports = slices.DeleteFunc(m.Reports, func(c store.CheckedCredential) bool { return c.CredentialID == id })
	m.Reports = append(m.Reports, store.CheckedCredential{CredentialID: id, Report: report})
	if err := r.st.SaveEngineMemory(m); err != nil {
		r.ask.Warn("credentials: the check is not kept for the daemon, which checks the token again when it starts: %v", err)
	}
}

// showReport prints the checklist of a check, with what to grant where a
// check failed or a zone is left out.
func (r *run) showReport(report credentials.Report) {
	for _, c := range report.Checks {
		mark := "✓"
		switch {
		case c.OK:
		case c.Unanswered:
			mark = "?"
		default:
			mark = "✗"
		}
		what := string(c.Capability)
		if c.Scope != "" {
			what += " on " + c.Scope
		}
		r.ask.Info("  %s %s", mark, what)
		if reason := c.Reason(); reason != "" {
			r.ask.Info("      %s", reason)
		}
	}
	for _, line := range excludedLines(report.Excluded, servedZones(r.st, r.install.ID), r.o.Verbose) {
		r.ask.Info("%s", line)
	}
	for _, name := range report.Leftovers {
		r.ask.Warn("a probe record of an earlier check is left in Cloudflare, to be removed by hand: %s", name)
	}
}

// excludedLines says which zones the token leaves out. A token scoped to one
// zone of an account leaves out every other zone of the account: those the
// install never served are counted in one line for each reason, unless
// verbose; a zone it served is named, with what to grant. With served nil,
// what it served is not known and every zone is named.
func excludedLines(excluded []credentials.Exclusion, served map[string]bool, verbose bool) []string {
	var lines []string
	never := make(map[string]int)
	var reasons []string
	for _, x := range excluded {
		if !verbose && served != nil && !served[x.ZoneID] {
			if never[x.Reason] == 0 {
				reasons = append(reasons, x.Reason)
			}
			never[x.Reason]++
			continue
		}
		lines = append(lines, fmt.Sprintf("  - %s left out: %s", x.Zone, x.Reason))
		if x.Detail != "" {
			lines = append(lines, "      "+x.Detail)
		}
	}
	for _, reason := range reasons {
		if n := never[reason]; n == 1 {
			lines = append(lines, fmt.Sprintf("  - 1 zone this install never served is left out: %s; pco setup --verbose names it", reason))
		} else {
			lines = append(lines, fmt.Sprintf("  - %d zones this install never served are left out: %s; pco setup --verbose names them", n, reason))
		}
	}
	return lines
}

// servedZones are the ids of the zones the engine's memory says the install
// ever served, or nil when that is not known: the memory cannot be read or is
// of another install. With no memory saved, the engine never ran on the node
// and the install served none.
func servedZones(st *store.Store, install string) map[string]bool {
	m, err := st.EngineMemory()
	if err != nil || install == "" || (m.InstallID != "" && m.InstallID != install) {
		return nil
	}
	served := make(map[string]bool)
	for _, zones := range [][]store.RememberedZone{m.Served, m.Stale, m.EverServed} {
		for _, z := range zones {
			served[z.ID] = true
		}
	}
	return served
}
