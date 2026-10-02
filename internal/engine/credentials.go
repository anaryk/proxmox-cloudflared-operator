package engine

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/credentials"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/planner"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/reconcile"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const credentialKind = "scoped"

// AddCredential checks a token without changing anything at Cloudflare and
// stores it only when the check finds it usable. The view of a token that is
// refused carries the report, so that the admin sees what to grant.
func (e *Engine) AddCredential(ctx context.Context, label, token string) (CredentialView, error) {
	label, token = strings.TrimSpace(label), strings.TrimSpace(token)
	switch {
	case label == "":
		return CredentialView{}, fmt.Errorf("%w: the label is empty", ErrInvalid)
	case token == "":
		return CredentialView{}, fmt.Errorf("%w: the token is empty", ErrInvalid)
	}
	install, err := e.installID()
	if err != nil {
		return CredentialView{}, err
	}
	cred := store.Credential{Label: label, Kind: credentialKind, Token: store.NewSecret(token), AddedAt: e.d.Now()}
	if err := e.refuseKnownToken(cred.Token); err != nil {
		return CredentialView{}, err
	}
	api, err := e.d.NewClient(cred)
	if err != nil {
		return CredentialView{}, fmt.Errorf("building a Cloudflare client: %w", err)
	}
	report := e.checker(install).Run(ctx, api, false)
	if err := ctx.Err(); err != nil {
		return CredentialView{}, fmt.Errorf("checking the token: %w", err)
	}
	view := CredentialView{Label: label, Kind: credentialKind, Checked: true, Report: shownReport(report)}
	if !report.Usable {
		return view, fmt.Errorf("%w: the token cannot be used: %s", ErrInvalid, failedChecks(report))
	}

	if err := e.acquire(ctx); err != nil {
		return CredentialView{}, err
	}
	defer e.Trigger()
	defer e.release()
	creds, err := e.d.Store.Credentials()
	if err != nil {
		return CredentialView{}, fmt.Errorf("reading the credentials: %w", err)
	}
	if cred, err = NewCredential(creds, label, cred.Token, cred.AddedAt); err != nil {
		return CredentialView{}, err
	}
	if err := e.d.Store.SaveCredential(cred); err != nil {
		return CredentialView{}, fmt.Errorf("storing the credential: %w", err)
	}
	view.ID = cred.ID
	e.keepReport(cred.ID, report)
	e.zones.due = true
	e.adminEvent(cred.ID, fmt.Sprintf("credential %q added", label))
	return view, nil
}

// CheckCredential checks a stored credential again. A deep check proves the
// write permissions by creating and deleting probe objects.
func (e *Engine) CheckCredential(ctx context.Context, id string, deep bool) (CredentialView, error) {
	cred, err := e.credential(id)
	if err != nil {
		return CredentialView{}, err
	}
	install, err := e.installID()
	if err != nil {
		return CredentialView{}, err
	}
	api, err := e.d.NewClient(cred)
	if err != nil {
		return CredentialView{}, fmt.Errorf("building a Cloudflare client: %w", err)
	}
	report := e.checker(install).Run(ctx, api, deep)
	if err := ctx.Err(); err != nil {
		return CredentialView{}, fmt.Errorf("checking the token: %w", err)
	}
	// The credential may have been removed during the check; its report is
	// kept only while it is stored, which the cycle lock makes sure of.
	if err := e.acquire(ctx); err != nil {
		return CredentialView{}, err
	}
	defer e.Trigger()
	defer e.release()
	if _, err := e.credential(id); err != nil {
		return CredentialView{}, err
	}
	e.keepReport(id, report)
	return CredentialView{ID: cred.ID, Label: cred.Label, Kind: cred.Kind, Checked: true, Report: shownReport(report)}, nil
}

// RemoveCredential deletes a credential, but only once nothing of this install
// is left in what it can reach: no record in its zones and no tunnel in their
// accounts. A token Cloudflare rejects reaches nothing and is removed. When
// Cloudflare cannot tell, nothing is removed.
func (e *Engine) RemoveCredential(ctx context.Context, id string) error {
	if err := e.acquire(ctx); err != nil {
		return err
	}
	defer e.Trigger()
	defer e.release()

	cred, err := e.credential(id)
	if err != nil {
		return err
	}
	install, err := e.installID()
	if err != nil {
		return err
	}
	if _, err := e.recall(install); err != nil {
		return fmt.Errorf("%w: cannot tell what credential %s managed: reading what the engine remembered: %w", ErrRefused, id, err)
	}
	// The stored token decides, not a client a cycle built from an older one.
	api, err := e.d.NewClient(cred)
	if err != nil {
		return fmt.Errorf("building a Cloudflare client: %w", err)
	}
	left, refused, err := e.leftBehind(ctx, api, install, id)
	if err != nil {
		return fmt.Errorf("%w: cannot tell what credential %s still manages: %w", ErrRefused, id, err)
	}
	if len(left) > 0 {
		return fmt.Errorf("%w: credential %s still manages %s", ErrRefused, id, strings.Join(left, ", "))
	}
	if err := e.d.Store.DeleteCredential(id); err != nil {
		return fmt.Errorf("removing the credential: %w", err)
	}
	e.dropClient(id)
	e.forgetReport(id)
	msg := fmt.Sprintf("credential %q removed", cred.Label)
	if refused {
		msg += "; Cloudflare refused its token, so what it managed could not be checked and may be left behind"
		e.d.Log.Warn().Str("credential", id).Msg("removed a credential whose token Cloudflare refused; what it managed may be left behind")
	}
	e.adminEvent(id, msg)
	return nil
}

// leftBehind lists the records and tunnels of this install that credential
// id still reaches through api: the records in every zone it lists, serves or
// served, also those that left its listing, and the tunnel in every account
// it sees, those of its zones and those its tunnels were seen in. What
// Cloudflare refuses to show the token, the token cannot manage either, so a
// refusal counts as nothing reached and refused says that there was one; a
// zone Cloudflare no longer has holds no record. Any other failure is an
// error. The caller holds the cycle lock.
func (e *Engine) leftBehind(ctx context.Context, api cfapi.API, installID, id string) (left []string, refused bool, err error) {
	zones, err := api.Zones(ctx)
	switch {
	case cfapi.IsAuth(err):
		refused = true
	case err != nil:
		return nil, false, err
	}
	zones = append(zones, e.zones.servedThrough(id)...)
	if cz := e.zones.byCred[id]; cz != nil {
		zones = append(zones, slices.Collect(maps.Values(cz.stale))...)
	}
	slices.SortFunc(zones, func(a, b cfapi.Zone) int { return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID)) })
	zones = slices.CompactFunc(zones, func(a, b cfapi.Zone) bool { return a.ID == b.ID })

	marker := planner.DNSMarker(installID)
	accounts := map[string]bool{}
	for _, z := range zones {
		accounts[z.AccountID] = true
		records, err := api.Records(ctx, z.ID, cfapi.RecordFilter{CommentPrefix: marker})
		switch {
		case cfapi.IsAuth(err):
			refused = true
			continue
		case cfapi.IsNotFound(err):
			continue
		case err != nil:
			return nil, false, err
		}
		for _, rec := range records {
			if reconcile.Owned(installID, rec) && !reconcile.IsProbeRecord(installID, rec) {
				left = append(left, fmt.Sprintf("record %s in zone %s", rec.Name, z.Name))
			}
		}
	}

	seen, err := api.Accounts(ctx)
	switch {
	case cfapi.IsAuth(err):
		refused = true
	case err != nil:
		return nil, false, err
	}
	for _, a := range seen {
		accounts[a.ID] = true
	}
	for _, t := range e.seen {
		if t.credential == id {
			accounts[t.account] = true
		}
	}
	for _, account := range slices.Sorted(maps.Keys(accounts)) {
		t, found, err := api.FindTunnel(ctx, account, planner.TunnelName(installID))
		switch {
		case cfapi.IsAuth(err):
			refused = true
		case err != nil:
			return nil, false, err
		case found:
			left = append(left, fmt.Sprintf("tunnel %s in account %s", t.Name, account))
		}
	}
	return left, refused, nil
}

// refuseKnownToken refuses a token a stored credential has already.
func (e *Engine) refuseKnownToken(token store.Secret) error {
	creds, err := e.d.Store.Credentials()
	if err != nil {
		return fmt.Errorf("reading the credentials: %w", err)
	}
	return refuseToken(creds, token)
}

// refuseToken refuses a token one of stored has already: two credentials of
// one token see the same zones, which then need a pin.
func refuseToken(stored []store.Credential, token store.Secret) error {
	for _, c := range stored {
		if c.Token.Equal(token) {
			return fmt.Errorf("%w: credential %q has this token already", ErrInvalid, c.Label)
		}
	}
	return nil
}

// NewCredential returns the credential that is stored for a token that passed
// its check: a scoped one, with an id of 8 random hex characters that no
// credential of stored has in any case. A token one of stored has already is
// refused.
func NewCredential(stored []store.Credential, label string, token store.Secret, addedAt time.Time) (store.Credential, error) {
	if err := refuseToken(stored, token); err != nil {
		return store.Credential{}, err
	}
	for {
		id := randomHex(4)()
		if !slices.ContainsFunc(stored, func(c store.Credential) bool { return strings.EqualFold(c.ID, id) }) {
			return store.Credential{ID: id, Label: label, Kind: credentialKind, Token: token, AddedAt: addedAt}, nil
		}
	}
}

// credential returns the stored credential with an id.
func (e *Engine) credential(id string) (store.Credential, error) {
	creds, err := e.d.Store.Credentials()
	if err != nil {
		return store.Credential{}, fmt.Errorf("reading the credentials: %w", err)
	}
	i := slices.IndexFunc(creds, func(c store.Credential) bool { return c.ID == id })
	if i < 0 {
		return store.Credential{}, fmt.Errorf("%w: no credential %q", ErrNotFound, id)
	}
	return creds[i], nil
}

func (e *Engine) installID() (string, error) {
	inst, found, err := e.d.Store.Install()
	switch {
	case err != nil:
		return "", fmt.Errorf("reading the install identity: %w", err)
	case !found:
		return "", errors.New(problemNotSetUp)
	}
	return inst.ID, nil
}

func (e *Engine) checker(installID string) *credentials.Checker {
	return credentials.NewChecker(installID, e.d.Now, randomHex(8))
}

// randomHex returns a function that makes 2n random lower-case hex characters
// on every call.
func randomHex(n int) func() string {
	return func() string {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
}

// failedChecks says what a report found wrong.
func failedChecks(r credentials.Report) string {
	var out []string
	for _, c := range r.Checks {
		if c.OK {
			continue
		}
		what := string(c.Capability)
		if c.Scope != "" {
			what += " on " + c.Scope
		}
		out = append(out, what+": "+c.Detail)
	}
	if len(out) == 0 {
		return "no active zone"
	}
	return strings.Join(out, "; ")
}
