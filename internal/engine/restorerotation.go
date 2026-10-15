package engine

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/cfapi"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	problemRestoreRotation = "the install was restored from a bundle: the new secrets of its tunnels wait for a cycle that writes: %s; " +
		"until then the run tokens the lost install held still work"
	problemRestoreRotationFailed = "the install was restored from a bundle: giving tunnel %s in account %s a new secret failed: %v; " +
		"it is tried again in the next cycle"
	problemRestoreUnsaved = "the install was restored from a bundle: the record of the new secrets of its tunnels cannot be saved: %v; " +
		"nothing more is rotated until it is"
	problemRestoreForeign = "a record of the rotation of restore %s of install %s is not the one install %s asked for (%s); " +
		"it is left as it is and rotates nothing: remove meta/restore-rotation.json once you know it is stale"
	restoreRotationDone = "the install was restored from a bundle: every tunnel has a new secret, " +
		"and the run tokens of the lost install no longer work"
)

// readRestoreRotation reads the rotation a promotion of a restored install
// asked for, before anything of the cycle is written. A record that cannot
// be read may list a tunnel whose old secret the lost install still holds:
// the cycle holds rather than take it for none. Only the record of the
// restore the install names is acted on: one of another install or restore
// is left as it is, and a record the install names that is gone is asked
// for again, so that every tunnel is rotated rather than none.
func (c *cycleRun) readRestoreRotation() {
	want := c.install.RestoreRotation
	rec, found, err := c.e.d.Store.RestoreRotation()
	switch {
	case err != nil:
		c.hold(c.storeProblem("reading the rotation of the restored install", err))
		return
	case found && (rec.InstallID != c.install.ID || rec.Restore != want):
		c.problem(problemRestoreForeign, cmp.Or(rec.Restore, "without a name"), cmp.Or(rec.InstallID, "none"), c.install.ID, cmp.Or(want, "none"))
		return
	case !found && want != "":
		rec, found, c.restoreAgain = store.RestoreRotation{Since: c.now, InstallID: c.install.ID, Restore: want}, true, true
	}
	c.restoreRec, c.restoreFound = rec, found
}

// restoreRotation gives every tunnel of a restored install, shared and
// pinned, a new secret once, as its promotion asked: copies of the run
// tokens on the disks of the lost install, which may come back, stop
// working. The record lists every tunnel the cycles find; a tunnel is done
// once Cloudflare took its new secret and the record saved that, once each
// node that runs a connector of it runs the new token, and once every
// connection of it was ended, so that a connector still in a session made
// with the old secret loses it. The record goes only when every tunnel it
// lists is done, or was found gone by a cycle that read every account. It
// waits for a cycle that may write and that read every account through, and
// keeps its progress in the store, so that a controller that takes over
// goes on from it.
func (c *cycleRun) restoreRotation() {
	if c.leaseHeld || c.storeHold {
		return
	}
	rec, found := c.restoreRec, c.restoreFound
	if kept := c.e.restoreUnsaved; kept != nil {
		// A secret was given and its record not saved: the record is
		// saved before anything more is done, else the secret would be
		// given again in every cycle.
		if err := c.e.d.Store.SaveRestoreRotation(*kept); err != nil {
			c.problem(problemRestoreUnsaved, err)
			return
		}
		rec, found, c.e.restoreUnsaved = *kept, true, nil
	}
	if !found {
		return
	}
	if why := c.restoreRotationHeld(); why != "" {
		c.problem(problemRestoreRotation, why)
		return
	}
	save := func() error {
		err := c.e.d.Store.SaveRestoreRotation(rec)
		if err != nil {
			kept := rec
			kept.Tunnels = slices.Clone(rec.Tunnels)
			c.e.restoreUnsaved = &kept
		}
		return err
	}
	if c.listTunnels(&rec) || c.restoreAgain {
		if err := save(); err != nil {
			c.problem(problemRestoreUnsaved, err)
			return
		}
	}
	for _, t := range c.tunnels {
		if !t.Exists || t.ID == "" || rotatedThrough(rec.TunnelsRotated, t.ID) {
			continue
		}
		fresh := !slices.Contains(rec.Rotated, t.ID)
		err := c.e.rotateFor(c.ctx, t, &rec.TunnelsRotated, save)
		if fresh && slices.Contains(rec.Rotated, t.ID) {
			c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindRotation, Subject: t.Name, Tunnel: t.Name, Account: t.AccountID,
				Security: true, Message: fmt.Sprintf("the secret of tunnel %s in account %s is rotated: every run token of it stops working", t.Name, t.AccountID)})
		}
		if err != nil {
			c.problem(problemRestoreRotationFailed, t.Name, t.AccountID, err)
			return
		}
	}
	if waiting := c.notRolledOut(rec.TunnelsRotated); len(waiting) > 0 {
		c.problem(problemRestoreRotation, "the connectors of "+andList(waiting)+" do not run with the new secret yet")
		return
	}
	for _, t := range c.tunnels {
		if !slices.Contains(rec.Rotated, t.ID) || slices.Contains(rec.Cleaned, t.ID) {
			continue
		}
		api := c.e.clients[t.CredentialID]
		if api == nil {
			c.problem(problemRestoreRotationFailed, t.Name, t.AccountID, fmt.Errorf("no client for credential %s", t.CredentialID))
			return
		}
		if err := api.CleanUpConnections(c.ctx, t.AccountID, t.ID); err != nil {
			c.problem(problemRestoreRotationFailed, t.Name, t.AccountID, err)
			return
		}
		rec.Cleaned = append(rec.Cleaned, t.ID)
		if err := save(); err != nil {
			c.problem(problemRestoreUnsaved, err)
			return
		}
	}
	if left := restoreLeft(rec); len(left) > 0 {
		c.problem(problemRestoreRotation, "not every tunnel it lists is done: "+strings.Join(left, ", "))
		return
	}
	// The record goes first: an install that still names the rotation
	// then asks for it again, which rotates once more rather than never.
	if err := c.e.d.Store.DeleteRestoreRotation(); err != nil {
		c.problem("ending the rotation of the restored install: %v", err)
		return
	}
	inst, rev, found, err := c.e.d.Store.InstallRev()
	switch {
	case err != nil:
		c.problem("ending the rotation of the restored install: reading the install: %v", err)
		return
	case !found || inst.ID != rec.InstallID || inst.RestoreRotation != rec.Restore:
		// Another promotion asked for another rotation meanwhile: it is
		// left for the cycles that follow.
		return
	}
	inst.RestoreRotation = ""
	if _, err := c.e.d.Store.SaveInstallIf(inst, rev); err != nil {
		c.problem("ending the rotation of the restored install: %v", err)
		return
	}
	c.install = inst
	c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindRotation, Subject: "restore", Security: true, Message: restoreRotationDone})
}

// listTunnels adds to the record every tunnel of the install this cycle
// found. One it lists that this cycle did not find is crossed off only once
// Cloudflare, asked with a credential that reaches its account, says it is
// gone: deleted, it has no secret left. Anything else keeps it. It says
// whether the record changed.
func (c *cycleRun) listTunnels(rec *store.RestoreRotation) bool {
	seen := map[string]bool{}
	reach := map[string]string{} // account -> a credential that reaches it
	changed := false
	for _, t := range c.tunnels {
		if !t.Exists || t.ID == "" {
			continue
		}
		seen[t.ID] = true
		if t.CredentialID != "" {
			reach[t.AccountID] = t.CredentialID
		}
		if !slices.ContainsFunc(rec.Tunnels, func(r store.RestoreTunnel) bool { return r.ID == t.ID }) {
			rec.Tunnels = append(rec.Tunnels, store.RestoreTunnel{ID: t.ID, AccountID: t.AccountID})
			changed = true
		}
	}
	kept := slices.DeleteFunc(slices.Clone(rec.Tunnels), func(r store.RestoreTunnel) bool {
		return !seen[r.ID] && c.tunnelGone(r, reach[r.AccountID])
	})
	if len(kept) != len(rec.Tunnels) {
		rec.Tunnels, changed = kept, true
	}
	return changed
}

// tunnelGone says that Cloudflare answered, to a credential that reaches
// the account, that a tunnel the record lists does not exist.
func (c *cycleRun) tunnelGone(r store.RestoreTunnel, credentialID string) bool {
	api := c.e.clients[credentialID]
	if api == nil {
		c.problem(problemRestoreRotation, fmt.Sprintf("tunnel %s in account %s is listed and was not found, and no credential "+
			"that reaches the account tells whether it is gone", r.ID, r.AccountID))
		return false
	}
	_, err := api.TunnelConfig(c.ctx, r.AccountID, r.ID)
	if cfapi.IsNotFound(err) {
		return true
	}
	if err != nil {
		c.problem(problemRestoreRotation, fmt.Sprintf("tunnel %s in account %s is listed and was not found, and Cloudflare "+
			"cannot tell whether it is gone: %v", r.ID, r.AccountID, err))
	}
	return false
}

// restoreLeft are the tunnels the record lists that are not rotated, taken
// up and cleaned yet; a record that lists none is not done either, as an
// empty list proves nothing rotated.
func restoreLeft(rec store.RestoreRotation) []string {
	if len(rec.Tunnels) == 0 {
		return []string{"no tunnel is listed yet"}
	}
	var left []string
	for _, r := range rec.Tunnels {
		if !rotatedThrough(rec.TunnelsRotated, r.ID) || !slices.Contains(rec.Cleaned, r.ID) {
			left = append(left, r.ID)
		}
	}
	return left
}

// restoreRotationHeld says why this cycle may not give the tunnels of a
// restored install new secrets: it may not write at Cloudflare, or it may not
// know every tunnel, one of which would keep the secret the lost install
// knew.
func (c *cycleRun) restoreRotationHeld() string {
	if why := c.rotationHeld(); why != "" {
		return why
	}
	switch {
	case !c.checked:
		return "the cycle did not get through to Cloudflare, so not every tunnel is known"
	case len(c.unreported) > 0:
		return fmt.Sprintf("%d tunnels are in accounts the tunnel run could not read", len(c.unreported))
	}
	var unreached []string
	for _, t := range c.tunnels {
		if t.Exists && t.ID != "" && t.CredentialID == "" {
			unreached = append(unreached, fmt.Sprintf("tunnel %s in account %s", t.Name, t.AccountID))
		}
	}
	if len(unreached) > 0 {
		return "no credential reaches " + andList(unreached) + ": add one for the account with pco credential add"
	}
	return ""
}
