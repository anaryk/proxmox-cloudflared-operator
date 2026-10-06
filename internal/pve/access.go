package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"slices"
)

// ACLEntry is one line of the access control list: a role granted to a user,
// a group or an API token on a path.
type ACLEntry struct {
	Path      string
	Type      string // "user", "group", "token"
	UGID      string // user@realm, group, user@realm!token
	RoleID    string
	Propagate bool
}

// User is a user with its groups and API tokens.
type User struct {
	ID      string
	Enabled bool
	Groups  []string
	Tokens  map[string]bool // token name -> privsep
}

// Group is a group of users.
type Group struct {
	ID      string
	Members []string
}

// Role is a named set of privileges.
type Role struct {
	ID    string
	Privs []string
}

type aclWire struct {
	Path      string    `json:"path"`
	Type      string    `json:"type"`
	UGID      string    `json:"ugid"`
	RoleID    string    `json:"roleid"`
	Propagate *flexBool `json:"propagate"`
}

type userWire struct {
	ID     string      `json:"userid"`
	Enable *flexBool   `json:"enable"`
	Groups commaList   `json:"groups"`
	Tokens []tokenWire `json:"tokens"`
}

type tokenWire struct {
	ID      string    `json:"tokenid"`
	Privsep *flexBool `json:"privsep"`
}

type groupWire struct {
	ID    string     `json:"groupid"`
	Users *commaList `json:"users"`
}

type groupMembersWire struct {
	Members []string `json:"members"`
}

type roleWire struct {
	ID    string    `json:"roleid"`
	Privs commaList `json:"privs"`
}

// ACL returns the access control list.
func (c *Client) ACL(ctx context.Context) ([]ACLEntry, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "access/acl", nil, &raw); err != nil {
		return nil, fmt.Errorf("fetching the acl: %w", err)
	}
	acl, err := DecodeACL(raw)
	if err != nil {
		return nil, fmt.Errorf("fetching the acl: %w", err)
	}
	return acl, nil
}

// DecodeACL reads the access control list as /access/acl lists it, without
// the envelope of the API: what pvesh prints.
func DecodeACL(data []byte) ([]ACLEntry, error) {
	var rows []aclWire
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("decoding the acl: %w", err)
	}
	out := make([]ACLEntry, 0, len(rows))
	for i, row := range rows {
		if row.Path == "" || row.Type == "" || row.UGID == "" || row.RoleID == "" {
			return nil, fmt.Errorf("acl entry %d lacks its path, type, ugid or role", i)
		}
		out = append(out, ACLEntry{
			Path:      row.Path,
			Type:      row.Type,
			UGID:      row.UGID,
			RoleID:    row.RoleID,
			Propagate: row.Propagate == nil || bool(*row.Propagate),
		})
	}
	return out, nil
}

// Users returns every user, enabled or not, with its groups and tokens.
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "access/users", url.Values{"full": {"1"}}, &raw); err != nil {
		return nil, fmt.Errorf("fetching users: %w", err)
	}
	users, err := DecodeUsers(raw)
	if err != nil {
		return nil, fmt.Errorf("fetching users: %w", err)
	}
	return users, nil
}

// DecodeUsers reads the users as /access/users?full=1 lists them, without the
// envelope of the API.
func DecodeUsers(data []byte) ([]User, error) {
	var rows []userWire
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("decoding users: %w", err)
	}
	out := make([]User, 0, len(rows))
	for i, row := range rows {
		if row.ID == "" {
			return nil, fmt.Errorf("user %d has no id", i)
		}
		user := User{ID: row.ID, Enabled: row.Enable == nil || bool(*row.Enable), Groups: row.Groups}
		for _, tok := range row.Tokens {
			if tok.ID == "" {
				return nil, fmt.Errorf("a token of %s has no id", row.ID)
			}
			if user.Tokens == nil {
				user.Tokens = map[string]bool{}
			}
			// Proxmox separates the privileges of a token unless told not to.
			user.Tokens[tok.ID] = tok.Privsep == nil || bool(*tok.Privsep)
		}
		out = append(out, user)
	}
	return out, nil
}

// Groups returns every group with its members. The members come from the
// listing; a group the listing gives none for is asked on its own.
func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "access/groups", nil, &raw); err != nil {
		return nil, fmt.Errorf("fetching groups: %w", err)
	}
	groups, err := DecodeGroups(raw, func(id string) ([]string, error) {
		var w groupMembersWire
		if err := c.get(ctx, "access/groups/"+id, nil, &w); err != nil {
			return nil, err
		}
		return w.Members, nil
	})
	if err != nil {
		return nil, fmt.Errorf("fetching groups: %w", err)
	}
	return groups, nil
}

// DecodeGroups reads the groups as /access/groups lists them, without the
// envelope of the API. members is asked for the members of a group the
// listing gives none for, as GET /access/groups/<id> answers them.
func DecodeGroups(data []byte, members func(id string) ([]string, error)) ([]Group, error) {
	var rows []groupWire
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("decoding groups: %w", err)
	}
	out := make([]Group, 0, len(rows))
	for i, row := range rows {
		if !validSegment(row.ID) {
			return nil, fmt.Errorf("group %d has an invalid id %q", i, row.ID)
		}
		group := Group{ID: row.ID}
		if row.Users != nil {
			group.Members = *row.Users
		} else {
			m, err := members(row.ID)
			if err != nil {
				return nil, fmt.Errorf("fetching the members of group %s: %w", row.ID, err)
			}
			group.Members = m
		}
		out = append(out, group)
	}
	return out, nil
}

// DecodeGroupMembers reads the members of a group as /access/groups/<id>
// answers them, without the envelope of the API.
func DecodeGroupMembers(data []byte) ([]string, error) {
	var w groupMembersWire
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("decoding the members of a group: %w", err)
	}
	return w.Members, nil
}

// Roles returns every role with its privileges.
func (c *Client) Roles(ctx context.Context) ([]Role, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "access/roles", nil, &raw); err != nil {
		return nil, fmt.Errorf("fetching roles: %w", err)
	}
	roles, err := DecodeRoles(raw)
	if err != nil {
		return nil, fmt.Errorf("fetching roles: %w", err)
	}
	return roles, nil
}

// DecodeRoles reads the roles as /access/roles lists them, without the
// envelope of the API.
func DecodeRoles(data []byte) ([]Role, error) {
	var rows []roleWire
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("decoding roles: %w", err)
	}
	out := make([]Role, 0, len(rows))
	for i, row := range rows {
		if row.ID == "" {
			return nil, fmt.Errorf("role %d has no id", i)
		}
		out = append(out, Role{ID: row.ID, Privs: row.Privs})
	}
	return out, nil
}

// Permissions dumps the effective privileges of a principal, or of the token
// itself when userid is empty: GET access/permissions[?userid=&path=]. Without
// a path it covers every path of the ACL and a few Proxmox always lists. The
// privileges of each path are sorted.
func (c *Client) Permissions(ctx context.Context, userid, path string) (map[string][]string, error) {
	query := url.Values{}
	if userid != "" {
		query.Set("userid", userid)
	}
	if path != "" {
		query.Set("path", path)
	}
	var raw json.RawMessage
	if err := c.get(ctx, "access/permissions", query, &raw); err != nil {
		return nil, fmt.Errorf("fetching permissions: %w", err)
	}
	perms, err := DecodePermissions(raw)
	if err != nil {
		return nil, fmt.Errorf("fetching permissions: %w", err)
	}
	return perms, nil
}

// DecodePermissions reads what /access/permissions answers, without the
// envelope of the API: the privileges by path, each list sorted.
func DecodePermissions(data []byte) (map[string][]string, error) {
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decoding permissions: %w", err)
	}
	out := make(map[string][]string, len(raw))
	for p, privs := range raw {
		out[p] = slices.Sorted(maps.Keys(privs))
	}
	return out, nil
}
