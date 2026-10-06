package pve

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestACL(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/access/acl": okReply(t, "access_acl.json")})
	got, err := c.ACL(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ACLEntry{
		{Path: "/", Type: "user", UGID: "pco@pve", RoleID: "PCO", Propagate: true},
		{Path: "/", Type: "token", UGID: "pco@pve!vm9201", RoleID: "PCO", Propagate: true},
	}, got)
	require.Equal(t, "/api2/json/access/acl", rec.requests()[0].uri)
}

func TestACLPropagate(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/access/acl": {http.StatusOK, `{"data":[
		{"path":"/vms","type":"user","ugid":"a@pve","roleid":"PVEVMUser","propagate":0},
		{"path":"/vms","type":"group","ugid":"ops","roleid":"PVEVMUser","propagate":"1"},
		{"path":"/pool/pco","type":"token","ugid":"a@pve!t","roleid":"NoAccess","extra":"ignored"}
	]}`}})
	got, err := c.ACL(context.Background())
	require.NoError(t, err)
	require.Equal(t, []ACLEntry{
		{Path: "/vms", Type: "user", UGID: "a@pve", RoleID: "PVEVMUser"},
		{Path: "/vms", Type: "group", UGID: "ops", RoleID: "PVEVMUser", Propagate: true},
		{Path: "/pool/pco", Type: "token", UGID: "a@pve!t", RoleID: "NoAccess", Propagate: true},
	}, got, "a missing propagate means propagated")
}

func TestACLRejectsIncompleteEntries(t *testing.T) {
	for _, entry := range []string{
		`{"type":"user","ugid":"a@pve","roleid":"PVEVMUser"}`,
		`{"path":"/","ugid":"a@pve","roleid":"PVEVMUser"}`,
		`{"path":"/","type":"user","roleid":"PVEVMUser"}`,
		`{"path":"/","type":"user","ugid":"a@pve"}`,
	} {
		t.Run(entry, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/access/acl": {http.StatusOK, `{"data":[` + entry + `]}`}})
			got, err := c.ACL(context.Background())
			require.ErrorContains(t, err, "acl entry")
			require.Nil(t, got)
		})
	}
}

func TestUsers(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/access/users?full=1": okReply(t, "access_users.json")})
	got, err := c.Users(context.Background())
	require.NoError(t, err)
	require.Equal(t, []User{
		{ID: "pco@pve", Enabled: true, Tokens: map[string]bool{"vm9201": true}},
		{ID: "pcotest@pve", Enabled: true, Groups: []string{"pcotestg", "pcotestg2"}, Tokens: map[string]bool{"nosep": false, "sep": true}},
		{ID: "root@pam", Enabled: true, Tokens: map[string]bool{"pcotest": false}},
	}, got)
	require.Equal(t, "/api2/json/access/users?full=1", rec.requests()[0].uri)
}

func TestUsersForms(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/access/users?full=1": {http.StatusOK, `{"data":[
		{"userid":"a@pve","enable":0,"groups":["ops","dev"],"tokens":[{"tokenid":"t1"},{"tokenid":"t2","privsep":"0"}]},
		{"userid":"b@pve","groups":"ops, dev;qa"},
		{"userid":"c@pve","enable":"1","groups":null,"tokens":null}
	]}`}})
	got, err := c.Users(context.Background())
	require.NoError(t, err)
	require.Equal(t, []User{
		{ID: "a@pve", Groups: []string{"ops", "dev"}, Tokens: map[string]bool{"t1": true, "t2": false}},
		{ID: "b@pve", Enabled: true, Groups: []string{"ops", "dev", "qa"}},
		{ID: "c@pve", Enabled: true},
	}, got, "enabled and privsep are the defaults when absent")
}

func TestUsersRejectsIncompleteEntries(t *testing.T) {
	for _, user := range []string{
		`{"enable":1}`,
		`{"userid":"a@pve","tokens":[{"privsep":1}]}`,
		`{"userid":"a@pve","enable":"maybe"}`,
	} {
		t.Run(user, func(t *testing.T) {
			c, _ := newTestClient(t, map[string]reply{"/access/users?full=1": {http.StatusOK, `{"data":[` + user + `]}`}})
			got, err := c.Users(context.Background())
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}

func TestGroups(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/access/groups": okReply(t, "access_groups.json")})
	got, err := c.Groups(context.Background())
	require.NoError(t, err)
	require.Equal(t, []Group{
		{ID: "pcotestg2", Members: []string{"pcotest@pve"}},
		{ID: "pcotestg", Members: []string{"pcotest@pve"}},
	}, got)
	require.Len(t, rec.requests(), 1, "the members come from the listing")
}

func TestGroupsAskEachGroupWhenTheListingHasNoMembers(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/access/groups":       {http.StatusOK, `{"data":[{"groupid":"ops"},{"groupid":"empty","users":""}]}`},
		"/access/groups/ops":   {http.StatusOK, `{"data":{"members":["a@pve","b@pam"],"comment":"x"}}`},
		"/access/groups/empty": {http.StatusInternalServerError, `{"data":null}`},
	})
	got, err := c.Groups(context.Background())
	require.NoError(t, err)
	require.Equal(t, []Group{{ID: "ops", Members: []string{"a@pve", "b@pam"}}, {ID: "empty"}}, got)
	require.Len(t, rec.requests(), 2, "an empty member list is an answer")
}

func TestGroupsRejectBadNames(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"comment":"no id"}]}`,
		`{"data":[{"groupid":"../users"}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			c, rec := newTestClient(t, map[string]reply{"/access/groups": {http.StatusOK, body}})
			got, err := c.Groups(context.Background())
			require.ErrorContains(t, err, "group")
			require.Nil(t, got)
			require.Len(t, rec.requests(), 1)
		})
	}
}

func TestRoles(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{"/access/roles": okReply(t, "access_roles.json")})
	got, err := c.Roles(context.Background())
	require.NoError(t, err)
	require.Equal(t, "/api2/json/access/roles", rec.requests()[0].uri)

	byID := map[string][]string{}
	for _, r := range got {
		byID[r.ID] = r.Privs
	}
	require.Equal(t, []string{"SDN.Audit", "Sys.Audit", "VM.Audit", "VM.GuestAgent.Audit"}, byID["PCO"])
	require.Equal(t, []string{"Pool.Audit"}, byID["PVEPoolUser"])
	require.Contains(t, byID, "NoAccess")
	require.Empty(t, byID["NoAccess"])
	require.Contains(t, byID["Administrator"], "Sys.Modify")
}

func TestRolesRejectAnEntryWithoutID(t *testing.T) {
	c, _ := newTestClient(t, map[string]reply{"/access/roles": {http.StatusOK, `{"data":[{"privs":"VM.Audit"}]}`}})
	_, err := c.Roles(context.Background())
	require.ErrorContains(t, err, "role")
}

func TestPermissions(t *testing.T) {
	c, rec := newTestClient(t, map[string]reply{
		"/access/permissions":                                               okReply(t, "access_permissions.json"),
		"/access/permissions?path=%2Fvms%2F9201":                            okReply(t, "access_permissions_path.json"),
		"/access/permissions?path=%2Fvms%2F9201&userid=pcotest%40pve%21sep": {http.StatusOK, `{"data":{"/vms/9201":{}}}`},
	})
	ctx := context.Background()
	own := []string{"SDN.Audit", "Sys.Audit", "VM.Audit", "VM.GuestAgent.Audit"}

	all, err := c.Permissions(ctx, "", "")
	require.NoError(t, err)
	require.Equal(t, own, all["/"])
	require.Equal(t, own, all["/pool"])

	one, err := c.Permissions(ctx, "", "/vms/9201")
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"/vms/9201": own}, one)

	other, err := c.Permissions(ctx, "pcotest@pve!sep", "/vms/9201")
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"/vms/9201": nil}, other)

	uris := []string{}
	for _, r := range rec.requests() {
		uris = append(uris, r.uri)
	}
	require.Equal(t, []string{
		"/api2/json/access/permissions",
		"/api2/json/access/permissions?path=%2Fvms%2F9201",
		"/api2/json/access/permissions?path=%2Fvms%2F9201&userid=pcotest%40pve%21sep",
	}, uris)
}

// pvesh prints what the API answers without its envelope; the installer on the
// node reads the access control that way.
func TestDecodeWhatPveshPrints(t *testing.T) {
	acl, err := DecodeACL([]byte(`[{"path":"/vms/100","type":"token","ugid":"a@pve!t","roleid":"NoAccess","propagate":1}]`))
	require.NoError(t, err)
	require.Equal(t, []ACLEntry{{Path: "/vms/100", Type: "token", UGID: "a@pve!t", RoleID: "NoAccess", Propagate: true}}, acl)

	users, err := DecodeUsers([]byte(`[{"userid":"a@pve","enable":0,"groups":"g1,g2","tokens":[{"tokenid":"t","privsep":0}]}]`))
	require.NoError(t, err)
	require.Equal(t, []User{{ID: "a@pve", Groups: []string{"g1", "g2"}, Tokens: map[string]bool{"t": false}}}, users)

	asked := ""
	groups, err := DecodeGroups([]byte(`[{"groupid":"g1","users":"a@pve"},{"groupid":"g2"}]`), func(id string) ([]string, error) {
		asked = id
		return DecodeGroupMembers([]byte(`{"members":["b@pve"]}`))
	})
	require.NoError(t, err)
	require.Equal(t, "g2", asked)
	require.Equal(t, []Group{{ID: "g1", Members: []string{"a@pve"}}, {ID: "g2", Members: []string{"b@pve"}}}, groups)

	roles, err := DecodeRoles([]byte(`[{"roleid":"PCO","privs":"VM.Audit,Sys.Audit"}]`))
	require.NoError(t, err)
	require.Equal(t, []Role{{ID: "PCO", Privs: []string{"VM.Audit", "Sys.Audit"}}}, roles)

	perms, err := DecodePermissions([]byte(`{"/vms/100":{"VM.Console":1,"VM.Audit":1}}`))
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"/vms/100": {"VM.Audit", "VM.Console"}}, perms)

	_, err = DecodeACL([]byte(`{"data":[]}`))
	require.Error(t, err, "an answer with its envelope is not what pvesh prints")
}
