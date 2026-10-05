package engine

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/access"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/appliance"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/egress"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/inventory"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/store"
)

const (
	problemNotQuorate   = "the cluster is not quorate; nothing is written"
	problemBehind       = "the state of this appliance is older than its last write at Cloudflare (rollback or restore); run pco appliance recover"
	problemEarlierStart = "leader.json belongs to an earlier start of this container; a new epoch is drawn once self-identification passes"

	eventEpochDrawn = "epoch drawn after a container start; the state is the volume's"
	issueTenant     = "configured with the MAC %s of the appliance %s"
)

// identify runs the self-identification of the appliance on the snapshot the
// cycle just read, and reports whether the cycle may go on. A copy, and an
// appliance that a principal other than an admin can reach into, stop serving:
// the identity flag goes, the egress filter is emptied and the connectors are
// stopped, once for each time they stop, and the cycle ends there, so that
// nothing feeds the filter again. An appliance that is not proven, without
// being a copy, holds its writes and leaves the rest as it is. Once it is
// proven again, the flag is written and the connectors start at the next
// Ensure. On the host there is nothing to identify.
func (c *cycleRun) identify() bool {
	id := c.e.d.Identity
	if id == nil {
		return true
	}
	a := c.install.Appliance
	if a == nil {
		c.hold(c.problem("the install of this appliance records no appliance; nothing is changed"))
		return false
	}
	self := model.GuestRef{Kind: model.KindLXC, VMID: a.VMID}
	v := id.Check(c.ctx, c.snap)
	c.noteEpoch()
	acc := c.e.accessNow(c.now, c.snap)
	exposed := exposure(acc, a.VMID, c.e.d.OwnUser)
	c.st.Identity = &IdentityView{
		VMID: a.VMID, Node: a.Node, OK: v.OK, Why: v.Why, Copy: v.Copy,
		Copies: refNames(v.Copies), Tenants: refNames(v.Tenants), Exposed: principalIDs(exposed), CheckedAt: c.now,
	}
	switch {
	case v.Copy:
		c.stopServing()
		c.hold(c.problem("%s; the connectors are stopped and the egress filter is empty", v.Why))
		return false
	case len(exposed) > 0:
		for _, p := range exposed {
			c.hold(c.problem("%s", exposedLine(p, acc.data, a.VMID)))
		}
		c.stopServing()
		return false
	case !v.OK && v.Why == appliance.WhyIncomplete:
		// The cycle holds on the incomplete inventory as it does anywhere.
		return true
	case !v.OK:
		c.hold(c.problem("%s", v.Why))
		return false
	}
	if err := id.SetFlag(true); err != nil {
		c.problem("writing the identity flag: %v; the connectors do not start until it is written", err)
	} else {
		c.e.notServing = false
	}
	c.tenants = tenantMACs(c.snap, v.Tenants, a.MACs, self)
	if v.Pending {
		c.problem("a NIC of the appliance %s appeared or vanished within the last minute; "+
			"its links and its configuration in Proxmox must match by then", self)
	}
	return true
}

// stopServing removes the identity flag, so that no connector starts, and,
// unless that was done since the appliance last served, empties the egress
// filter and stops the connectors of the install.
func (c *cycleRun) stopServing() {
	if err := c.e.d.Identity.SetFlag(false); err != nil {
		c.problem("removing the identity flag: %v", err)
	}
	if c.e.notServing {
		return
	}
	c.e.egMu.Lock()
	err := c.e.d.Egress.Set(c.ctx, nil)
	c.e.egMu.Unlock()
	if err != nil && !errors.Is(err, egress.ErrOff) {
		c.problem("emptying the egress filter: %v", err)
		return
	}
	if err := c.e.d.Connectors.StopAll(c.ctx, c.install.ID); err != nil {
		c.problem("stopping the connectors: %v", err)
		return
	}
	c.e.notServing = true
}

// exposure returns the principals other than admins and pco's own that can
// reach into the appliance, as the access control last read says: only a
// principal that is known fails the appliance closed, and one known stays so
// while the access control cannot be read again.
func exposure(acc accessView, vmid int, ownUser string) []access.Principal {
	return access.Refused(acc.data, vmid, appliance.Pool, ownUser)
}

// exposedLine says what a principal holds on the appliance and the command
// that takes it away: on the appliance, and on its pool for Pool.Allocate. A
// privilege-separated token is answered with --tokens; a token without
// privilege separation holds the roles of its user, so its user is.
func exposedLine(p access.Principal, d access.Data, vmid int) string {
	flag, who, why := "--users", p.ID, ""
	if user, name, isToken := strings.Cut(p.ID, "!"); isToken {
		if privsep(d, user, name) {
			flag = "--tokens"
		} else {
			who, why = user, fmt.Sprintf("%s is not privilege-separated: it holds the roles of %s; ", p.ID, user)
		}
	}
	var paths []string
	if slices.ContainsFunc(p.Privs, func(priv string) bool { return priv != "Pool.Allocate" }) {
		paths = append(paths, fmt.Sprintf("/vms/%d", vmid))
	}
	if slices.Contains(p.Privs, "Pool.Allocate") {
		paths = append(paths, "/pool/"+appliance.Pool)
	}
	cmds := make([]string, len(paths))
	for i, path := range paths {
		cmds[i] = fmt.Sprintf("pveum acl modify %s %s %s --roles NoAccess", path, flag, who)
	}
	return fmt.Sprintf("%s holds %s on the appliance lxc/%d; pco serves nothing while it does: %s%s",
		p.ID, strings.Join(p.Privs, ", "), vmid, why, strings.Join(cmds, " and "))
}

// privsep says whether a token of a user separates its privileges; an unknown
// one is taken to, as Proxmox makes them.
func privsep(d access.Data, user, name string) bool {
	for _, u := range d.Users {
		if u.ID == user {
			sep, known := u.Tokens[name]
			return sep || !known
		}
	}
	return true
}

// tenantMACs maps each tenant to the MAC of the appliance it is configured
// with.
func tenantMACs(snap inventory.Snapshot, tenants []model.GuestRef, recorded []string, self model.GuestRef) map[model.GuestRef]string {
	if len(tenants) == 0 {
		return nil
	}
	ours := slices.Clone(recorded)
	if g, ok := snap.Guest(self); ok {
		for _, n := range g.NICs {
			ours = append(ours, n.MAC)
		}
	}
	out := make(map[model.GuestRef]string, len(tenants))
	for _, ref := range tenants {
		g, ok := snap.Guest(ref)
		if !ok {
			continue
		}
		for _, n := range g.NICs {
			if slices.Contains(ours, n.MAC) {
				out[ref] = n.MAC
				break
			}
		}
	}
	return out
}

// noteEpoch tells, once, of an epoch this process drew after the container
// started: the state is the volume's, as old as the volume is.
func (c *cycleRun) noteEpoch() {
	drawn := c.e.d.EpochDrawn
	if drawn == nil || !drawn() || !c.e.epochAt.IsZero() {
		c.st.EpochDrawnAt = c.e.epochAt
		return
	}
	c.e.epochAt = c.now
	c.st.EpochDrawnAt = c.now
	msg := eventEpochDrawn
	if w, found, err := c.e.d.Store.Writer(); err == nil && found {
		msg = fmt.Sprintf("%s (generation %d)", eventEpochDrawn, w.Generation)
	}
	c.events = append(c.events, Event{At: c.now, Level: levelWarn, Kind: kindWriter, Subject: "leader.json", Message: msg})
}

// quorate holds the cycle while the cluster is not quorate (ruling 24).
func (c *cycleRun) quorate() bool {
	if c.e.d.Quorate == nil {
		return true
	}
	ok, err := c.e.d.Quorate(c.ctx)
	switch {
	case err != nil:
		c.hold(c.problem("the quorum of the cluster could not be read (%v); nothing is written", err))
		return false
	case !ok:
		c.hold(c.problem(problemNotQuorate))
		return false
	}
	return true
}

// appliance says whether the install is an appliance.
func (c *cycleRun) appliance() bool { return c.install.ProfileName() == store.ProfileAppliance }

// recoverCommand is the command that recovers the install of this profile.
func (c *cycleRun) recoverCommand() string {
	if c.appliance() {
		return "pco appliance recover"
	}
	return "pco setup --recover"
}

// recoverHint is what the tunnel run says to do about a configuration of a
// writer leader.json does not know: on the host its own words; in an
// appliance that drew its epoch at this start and has not written since, that
// its state fell behind; in any other appliance, the host's words with the
// appliance's command.
func (c *cycleRun) recoverHint() string {
	switch {
	case !c.appliance():
		return ""
	case c.epochDrawn():
		return problemBehind
	}
	return fmt.Sprintf("another installation uses install id %s, or the store was lost (pco appliance recover)", c.install.ID)
}

// epochDrawn says whether this process drew its epoch and no write of it was
// verified at Cloudflare yet.
func (c *cycleRun) epochDrawn() bool {
	return c.e.d.EpochDrawn != nil && !c.e.firstWrite && c.e.d.EpochDrawn()
}

// refusedAsCopy is why an admin action that writes to Cloudflare by itself
// is refused in an appliance whose last self-identification did not pass;
// nil when it may go on.
func (e *Engine) refusedAsCopy() error {
	if e.d.Identity == nil {
		return nil
	}
	e.stateMu.RLock()
	id := e.state.Identity
	e.stateMu.RUnlock()
	if id == nil || !id.OK || e.notServing {
		return fmt.Errorf("%w: the appliance changes nothing at Cloudflare until its self-identification passes "+
			"and no principal can reach into it; pco status says why", ErrRefused)
	}
	return nil
}

func refNames(refs []model.GuestRef) []string {
	var out []string
	for _, r := range refs {
		out = append(out, r.String())
	}
	return out
}

func principalIDs(ps []access.Principal) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}
