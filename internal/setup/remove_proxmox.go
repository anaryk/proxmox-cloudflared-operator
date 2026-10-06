package setup

import (
	"context"
	"slices"
	"strings"
)

// proxmoxState is what Proxmox holds of what the manifest says setup created,
// read before anything is removed.
type proxmoxState struct {
	user  bool     // pco@pve is there
	token bool     // its token is there
	role  *pveRole // PCO, when it is there
	acl   []pveACL
	tags  []string // the registered tags
}

// inProxmox reports whether setup created anything in Proxmox.
func (m Manifest) inProxmox() bool {
	return m.CreatedUser || m.CreatedToken || m.CreatedRole || m.GrantedACL || len(m.RegisteredTags) > 0
}

// readProxmox reads what uninstall needs to know of the objects the manifest
// lists, and nothing else.
func (u *uninstall) readProxmox(ctx context.Context) (proxmoxState, error) {
	m := u.manifest
	var s proxmoxState
	var err error
	if m.CreatedUser || m.CreatedToken || m.GrantedACL {
		if s.user, err = u.userExists(ctx); err != nil {
			return s, err
		}
	}
	if s.user && m.CreatedToken {
		if _, s.token, err = u.findToken(ctx); err != nil {
			return s, err
		}
	}
	if m.CreatedRole {
		role, found, err := u.role(ctx)
		if err != nil {
			return s, err
		}
		if found {
			s.role = &role
		}
	}
	if m.GrantedACL || m.CreatedRole {
		if s.acl, err = u.acl(ctx); err != nil {
			return s, err
		}
	}
	if len(m.RegisteredTags) > 0 {
		if s.tags, err = u.registeredTags(ctx); err != nil {
			return s, err
		}
	}
	return s, nil
}

// removeProxmox removes what the manifest says setup created in Proxmox, as
// the survey found it. Proxmox drops the grants of a user or a role that
// goes, so setup's grant is only revoked by itself when the user or the role
// stays, and is looked for again when both went.
func (u *uninstall) removeProxmox(ctx context.Context) {
	m := u.manifest
	if !m.inProxmox() {
		return
	}
	if err := u.found.proxmoxErr; err != nil {
		u.fail("reading what Proxmox holds of pco: %v; it is left as it is", err)
		return
	}
	s := u.found.proxmox
	userGoes := m.CreatedUser && s.user
	roleGoes, why := u.roleVerdict()
	bothGo := userGoes && roleGoes
	granted := m.GrantedACL && slices.ContainsFunc(s.acl, isGrant)
	revoked := true
	if granted && !bothGo {
		revoked = u.revokeGrant(ctx)
	}
	if m.CreatedToken && s.token {
		if err := u.removeToken(ctx); err != nil {
			u.fail("%v", err)
		} else {
			u.ask.Info("token %s: removed", tokenID)
		}
	}
	switch {
	case userGoes:
		if _, err := u.run.Run(ctx, "pveum", "user", "delete", UserID); err != nil {
			u.fail("removing user %s: %v", UserID, err)
		} else {
			u.ask.Info("user %s: removed", UserID)
		}
	case m.CreatedUser:
		u.ask.Info("user %s: gone already", UserID)
	}
	if m.CreatedRole {
		if roleGoes && !revoked {
			// Deleting the role would leave the grant to the user that stays in
			// user.cfg, where pveum no longer shows it and warns about it.
			roleGoes, why = false, "the grant of it to "+UserID+" could not be revoked; pco uninstall again tries again"
		}
		u.removeRole(ctx, s.role, roleGoes, why)
	}
	if granted && bothGo {
		u.checkGrantGone(ctx)
	}
	if len(m.RegisteredTags) > 0 {
		u.removeTags(ctx, s.tags)
	}
}

// revokeGrant revokes setup's grant and reports whether it did.
func (u *uninstall) revokeGrant(ctx context.Context) bool {
	if _, err := u.run.Run(ctx, "pveum", "acl", "delete", "/", "--users", UserID, "--roles", RoleID); err != nil {
		u.fail("revoking role %s on / from %s: %v", RoleID, UserID, err)
		return false
	}
	u.ask.Info("acl /: revoked role %s from %s", RoleID, UserID)
	return true
}

// checkGrantGone looks for setup's grant once its user and role are gone, with
// which Proxmox drops it.
func (u *uninstall) checkGrantGone(ctx context.Context) {
	acl, err := u.acl(ctx)
	switch {
	case err != nil:
		u.ask.Warn("whether the grant of role %s on / to %s went with them cannot be told: %v", RoleID, UserID, err)
	case slices.ContainsFunc(acl, isGrant):
		u.ask.Warn("the grant of role %s on / to %s is still there; remove it with pveum acl delete / --users %s --roles %s",
			RoleID, UserID, UserID, RoleID)
	}
}

// roleVerdict reports whether role PCO goes, and why not when it stays. It
// goes when setup created it, it grants what setup gave it and nothing else,
// and nobody but pco@pve holds it: a privilege an admin added or took away,
// or a grant to another user, group or token, says the role is used for more.
func (u *uninstall) roleVerdict() (goes bool, why string) {
	role := u.found.proxmox.role
	switch {
	case !u.manifest.CreatedRole:
		return false, "setup did not create it"
	case role == nil:
		return false, "it is gone"
	case !IsPCORole(role.Privs):
		return false, roleChange(role.Privs)
	}
	var others []string
	for _, a := range u.found.proxmox.acl {
		if a.Role == RoleID && !isGrant(a) && !slices.Contains(others, a.UGID) {
			others = append(others, a.UGID)
		}
	}
	if len(others) > 0 {
		return false, "it is granted to " + strings.Join(others, ", ") + " as well, who keep it"
	}
	return true, ""
}

// roleChange says how the privileges of a role differ from the set setup
// gives it that they are closest to.
func roleChange(privs []string) string {
	var extra, missing []string
	best := -1
	for _, set := range setupPrivileges() {
		e, m := without(privs, set), without(set, privs)
		if best < 0 || len(e)+len(m) < best {
			extra, missing, best = e, m, len(e)+len(m)
		}
	}
	var parts []string
	if len(extra) > 0 {
		parts = append(parts, "it also grants "+strings.Join(extra, ", "))
	}
	if len(missing) > 0 {
		parts = append(parts, "it no longer grants "+strings.Join(missing, ", "))
	}
	return strings.Join(parts, " and ") + ", unlike the role setup made"
}

func (u *uninstall) removeRole(ctx context.Context, role *pveRole, goes bool, why string) {
	switch {
	case role == nil:
		u.ask.Info("role %s: gone already", RoleID)
	case goes:
		if _, err := u.run.Run(ctx, "pveum", "role", "delete", RoleID); err != nil {
			u.fail("removing role %s: %v", RoleID, err)
			return
		}
		u.ask.Info("role %s: removed", RoleID)
	default:
		u.ask.Warn("role %s is kept: %s", RoleID, why)
	}
}

// removeTags removes from the registered tags the ones setup added, and keeps
// the others in their order.
func (u *uninstall) removeTags(ctx context.Context, tags []string) {
	keep := without(tags, u.manifest.RegisteredTags)
	if len(keep) == len(tags) {
		u.ask.Info("registered tags: nothing needed")
		return
	}
	if err := u.setRegisteredTags(ctx, keep); err != nil {
		u.fail("%v", err)
		return
	}
	u.ask.Info("registered tags: removed %s", strings.Join(without(tags, keep), ", "))
}
