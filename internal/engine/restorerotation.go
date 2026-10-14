package engine

import (
	"fmt"
	"slices"
)

const (
	problemRestoreRotation = "the install was restored from a bundle: the new secrets of its tunnels wait for a cycle that writes: %s; " +
		"until then the run tokens the lost install held still work"
	problemRestoreRotationFailed = "the install was restored from a bundle: giving tunnel %s in account %s a new secret failed: %v; " +
		"it is tried again in the next cycle"
	problemRestoreUnsaved = "the install was restored from a bundle: the record of the new secrets of its tunnels cannot be saved: %v; " +
		"nothing more is rotated until it is"
	restoreRotationDone = "the install was restored from a bundle: every tunnel has a new secret, " +
		"and the run tokens of the lost install no longer work"
)

// restoreRotation gives every tunnel of a restored install, shared and
// pinned, a new secret once, as its promotion asked: copies of the run
// tokens on the disks of the lost install, which may come back, stop
// working. Once each node that runs a connector of a tunnel runs it with the
// new token, every connection of the tunnel is ended, so that a connector
// still in a session made with the old secret loses it. It waits for a cycle
// that may write and that read every account through, and keeps its
// progress in the store, so that a controller that takes over goes on.
func (c *cycleRun) restoreRotation() {
	if c.leaseHeld || c.storeHold {
		return
	}
	rec, found, err := c.e.d.Store.RestoreRotation()
	if kept := c.e.restoreUnsaved; kept != nil {
		// A secret was given and its record not saved: the record is
		// saved before anything more is done, else the secret would be
		// given again in every cycle.
		if err := c.e.d.Store.SaveRestoreRotation(*kept); err != nil {
			c.problem(problemRestoreUnsaved, err)
			return
		}
		rec, found, err, c.e.restoreUnsaved = *kept, true, nil, nil
	}
	switch {
	case err != nil:
		c.problem("reading the rotation of the restored install: %v", err)
		return
	case !found:
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
			c.e.restoreUnsaved = &kept
		}
		return err
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
			c.problem("saving the rotation of the restored install: %v", err)
			return
		}
	}
	if err := c.e.d.Store.DeleteRestoreRotation(); err != nil {
		c.problem("ending the rotation of the restored install: %v", err)
		return
	}
	c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindRotation, Subject: "restore", Security: true, Message: restoreRotationDone})
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
