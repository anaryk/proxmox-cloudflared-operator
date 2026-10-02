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

// isSetupRole reports whether a role grants what setup gives role PCO, on any
// version, and nothing else.
func isSetupRole(role pveRole) bool {
	return slices.ContainsFunc(setupPrivileges(), func(privs []string) bool { return sameSet(role.Privs, privs) })
}

// removeProxmox removes what the manifest says setup created in Proxmox, as
// far as it is still there. Proxmox drops the grants of a user or a role that
// goes, so setup's grant is only revoked by itself when the user or the role
// stays, and is looked for again when both went.
func (u *uninstall) removeProxmox(ctx context.Context) {
	m := u.manifest
	if !m.inProxmox() {
		return
	}
	s, err := u.readProxmox(ctx)
	if err != nil {
		u.fail("reading what Proxmox holds of pco: %v; it is left as it is", err)
		return
	}
	userGoes := m.CreatedUser && s.user
	roleGoes := m.CreatedRole && s.role != nil && isSetupRole(*s.role)
	bothGo := userGoes && roleGoes
	granted := m.GrantedACL && slices.ContainsFunc(s.acl, isGrant)
	if granted && !bothGo {
		u.revokeGrant(ctx)
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
		if _, err := u.run.Run(ctx, "pveum", "user", "delete", userID); err != nil {
			u.fail("removing user %s: %v", userID, err)
		} else {
			u.ask.Info("user %s: removed", userID)
		}
	case m.CreatedUser:
		u.ask.Info("user %s: gone already", userID)
	}
	if m.CreatedRole {
		u.removeRole(ctx, s.role, roleGoes)
	}
	if granted && bothGo {
		u.checkGrantGone(ctx)
	}
	if len(m.RegisteredTags) > 0 {
		u.removeTags(ctx, s.tags)
	}
}

func (u *uninstall) revokeGrant(ctx context.Context) {
	if _, err := u.run.Run(ctx, "pveum", "acl", "delete", "/", "--users", userID, "--roles", roleID); err != nil {
		u.fail("revoking role %s on / from %s: %v", roleID, userID, err)
		return
	}
	u.ask.Info("acl /: revoked role %s from %s", roleID, userID)
}

// checkGrantGone looks for setup's grant once its user and role are gone, with
// which Proxmox drops it.
func (u *uninstall) checkGrantGone(ctx context.Context) {
	acl, err := u.acl(ctx)
	switch {
	case err != nil:
		u.ask.Warn("whether the grant of role %s on / to %s went with them cannot be told: %v", roleID, userID, err)
	case slices.ContainsFunc(acl, isGrant):
		u.ask.Warn("the grant of role %s on / to %s is still there; remove it with pveum acl delete / --users %s --roles %s",
			roleID, userID, userID, roleID)
	}
}

// removeRole removes role PCO while it grants what setup gave it and nothing
// else: a privilege an admin added says the role is used for more.
func (u *uninstall) removeRole(ctx context.Context, role *pveRole, goes bool) {
	switch {
	case role == nil:
		u.ask.Info("role %s: gone already", roleID)
	case goes:
		if _, err := u.run.Run(ctx, "pveum", "role", "delete", roleID); err != nil {
			u.fail("removing role %s: %v", roleID, err)
			return
		}
		u.ask.Info("role %s: removed", roleID)
	default:
		u.ask.Warn("role %s is kept: it grants %s, which setup did not give it",
			roleID, strings.Join(without(role.Privs, slices.Concat(setupPrivileges()...)), ", "))
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
