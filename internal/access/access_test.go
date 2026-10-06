package access

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/model"
	"github.com/anaryk/proxmox-cloudflared-operator/internal/pve"
)

// testRoles is a cut-down set of the built-in roles, and two an admin could
// make, small enough to read the expected privileges off.
var testRoles = []pve.Role{
	{ID: "Administrator", Privs: []string{"Permissions.Modify", "Pool.Allocate", "Sys.Modify", "VM.Audit", "VM.Config.Network", "VM.Console"}},
	{ID: "PVEVMAdmin", Privs: []string{"VM.Allocate", "VM.Audit", "VM.Config.Network", "VM.Console", "VM.Snapshot"}},
	{ID: "PVEVMUser", Privs: []string{"VM.Audit", "VM.Backup", "VM.Config.CDROM", "VM.Console", "VM.PowerMgmt"}},
	{ID: "PVETemplateUser", Privs: []string{"VM.Audit", "VM.Clone"}},
	{ID: "PVEAuditor", Privs: []string{"Sys.Audit", "VM.Audit"}},
	{ID: "PVEPoolAdmin", Privs: []string{"Pool.Allocate", "Pool.Audit"}},
	{ID: "PCO", Privs: []string{"Pool.Audit", "SDN.Audit", "Sys.Audit", "VM.Audit"}},
	{ID: "Migrate", Privs: []string{"VM.Migrate"}},
	{ID: "Delegate", Privs: []string{"Permissions.Modify"}},
	{ID: "NoAccess"},
}

var (
	vmUserPrivs  = []string{"VM.Audit", "VM.Backup", "VM.Config.CDROM", "VM.Console", "VM.PowerMgmt"}
	auditorPrivs = []string{"Sys.Audit", "VM.Audit"}
	adminPrivs   = []string{"Permissions.Modify", "Pool.Allocate", "Sys.Modify", "VM.Audit", "VM.Config.Network", "VM.Console"}
	// vmUserReach is what of PVEVMUser Refused looks for.
	vmUserReach = []string{"VM.Backup", "VM.Config.CDROM", "VM.Console", "VM.PowerMgmt"}

	web      = model.GuestRef{Kind: model.KindQEMU, VMID: 100}
	appl     = model.GuestRef{Kind: model.KindLXC, VMID: 120}
	tenant   = model.GuestRef{Kind: model.KindQEMU, VMID: 200}
	webPath  = "/vms/100"
	applPath = "/vms/120"
)

// data is a cluster with pco's user and token, alice (in group ops, with a
// privilege-separated token sep and a shared token shared), bob, and root.
func data(acl ...pve.ACLEntry) Data {
	return Data{
		ACL: append([]pve.ACLEntry{
			{Path: "/", Type: "user", UGID: "pco@pve", RoleID: "PCO", Propagate: true},
			{Path: "/", Type: "token", UGID: "pco@pve!vm120", RoleID: "PCO", Propagate: true},
		}, acl...),
		Users: []pve.User{
			{ID: "alice@pve", Enabled: true, Groups: []string{"ops"}, Tokens: map[string]bool{"sep": true, "shared": false}},
			{ID: "bob@pve", Enabled: true},
			{ID: "pco@pve", Enabled: true, Tokens: map[string]bool{"vm120": true}},
			{ID: "root@pam", Enabled: true},
		},
		Groups: []pve.Group{{ID: "ops", Members: []string{"alice@pve"}}, {ID: "devs", Members: []string{"bob@pve"}}},
		Roles:  testRoles,
		Pools:  map[model.GuestRef]string{appl: "pco", tenant: "tenants"},
	}
}

func user(path, ugid, role string) pve.ACLEntry {
	return pve.ACLEntry{Path: path, Type: "user", UGID: ugid, RoleID: role, Propagate: true}
}

func group(path, ugid, role string) pve.ACLEntry {
	return pve.ACLEntry{Path: path, Type: "group", UGID: ugid, RoleID: role, Propagate: true}
}

func token(path, ugid, role string) pve.ACLEntry {
	return pve.ACLEntry{Path: path, Type: "token", UGID: ugid, RoleID: role, Propagate: true}
}

func once(e pve.ACLEntry) pve.ACLEntry {
	e.Propagate = false
	return e
}

func TestRolesAndPrivileges(t *testing.T) {
	tests := []struct {
		name      string
		acl       []pve.ACLEntry
		principal string
		path      string
		roles     []string
		privs     []string
	}{
		{"a direct grant", []pve.ACLEntry{user(webPath, "alice@pve", "PVEVMUser")}, "alice@pve", webPath, []string{"PVEVMUser"}, vmUserPrivs},
		{"a direct grant elsewhere", []pve.ACLEntry{user("/vms/101", "alice@pve", "PVEVMUser")}, "alice@pve", webPath, nil, nil},
		{"/ propagated", []pve.ACLEntry{user("/", "alice@pve", "PVEAuditor")}, "alice@pve", webPath, []string{"PVEAuditor"}, auditorPrivs},
		{"/vms propagated", []pve.ACLEntry{user("/vms", "alice@pve", "PVEVMUser")}, "alice@pve", webPath, []string{"PVEVMUser"}, vmUserPrivs},
		{"/vms is not /nodes", []pve.ACLEntry{user("/vms", "alice@pve", "PVEVMUser")}, "alice@pve", "/nodes/pve1", nil, nil},
		{"a more specific entry replaces an inherited one", []pve.ACLEntry{
			user("/vms", "alice@pve", "PVEVMAdmin"), user(webPath, "alice@pve", "PVEAuditor"),
		}, "alice@pve", webPath, []string{"PVEAuditor"}, auditorPrivs},
		{"NoAccess on the child", []pve.ACLEntry{
			user("/vms", "alice@pve", "PVEVMUser"), user(webPath, "alice@pve", "NoAccess"),
		}, "alice@pve", webPath, []string{"NoAccess"}, nil},
		{"NoAccess beside another role at one path", []pve.ACLEntry{
			user(webPath, "alice@pve", "PVEVMUser"), user(webPath, "alice@pve", "NoAccess"),
		}, "alice@pve", webPath, []string{"NoAccess"}, nil},
		{"propagate=0 on a parent is not inherited", []pve.ACLEntry{once(user("/vms", "alice@pve", "PVEVMUser"))}, "alice@pve", webPath, nil, nil},
		{"propagate=0 counts on its own path", []pve.ACLEntry{once(user("/vms", "alice@pve", "PVEVMUser"))}, "alice@pve", "/vms", []string{"PVEVMUser"}, vmUserPrivs},
		{"propagate=0 on a parent leaves the inherited one", []pve.ACLEntry{
			user("/", "alice@pve", "PVEAuditor"), once(user("/vms", "alice@pve", "PVEVMAdmin")),
		}, "alice@pve", webPath, []string{"PVEAuditor"}, auditorPrivs},
		{"group membership", []pve.ACLEntry{group("/vms", "ops", "PVEVMUser")}, "alice@pve", webPath, []string{"PVEVMUser"}, vmUserPrivs},
		{"another group", []pve.ACLEntry{group("/vms", "devs", "PVEVMUser")}, "alice@pve", webPath, nil, nil},
		// Matrix row B3: the user's own entry replaces its group's at one path.
		{"a user and a group entry at one path", []pve.ACLEntry{
			group(webPath, "ops", "PVEAuditor"), user(webPath, "alice@pve", "PVEVMUser"),
		}, "alice@pve", webPath, []string{"PVEVMUser"}, vmUserPrivs},
		{"a group entry deeper than the user's", []pve.ACLEntry{
			user("/vms", "alice@pve", "PVEVMAdmin"), group(webPath, "ops", "PVEAuditor"),
		}, "alice@pve", webPath, []string{"PVEAuditor"}, auditorPrivs},
		{"two groups at one path add up", []pve.ACLEntry{
			group(webPath, "ops", "PVEAuditor"), group(webPath, "ops2", "PVEVMUser"),
		}, "carol@pve", webPath, []string{"PVEAuditor", "PVEVMUser"}, []string{"Sys.Audit", "VM.Audit", "VM.Backup", "VM.Config.CDROM", "VM.Console", "VM.PowerMgmt"}},
		{"a privsep token intersects its user", []pve.ACLEntry{
			user("/vms", "alice@pve", "PVEVMUser"), token("/", "alice@pve!sep", "Administrator"),
		}, "alice@pve!sep", webPath, []string{"Administrator"}, []string{"VM.Audit", "VM.Console"}},
		{"a privsep token holds nothing of its own", []pve.ACLEntry{user("/vms", "alice@pve", "PVEVMUser")}, "alice@pve!sep", webPath, nil, nil},
		{"a privsep token takes no group entry", []pve.ACLEntry{
			group("/vms", "ops", "PVEVMUser"), token(webPath, "alice@pve!sep", "PVEVMUser"),
		}, "alice@pve!sep", "/vms/101", nil, nil},
		{"a non-privsep token inherits its user", []pve.ACLEntry{user("/vms", "alice@pve", "PVEVMUser")}, "alice@pve!shared", webPath, []string{"PVEVMUser"}, vmUserPrivs},
		{"a non-privsep token's own entries count for nothing", []pve.ACLEntry{
			user(webPath, "alice@pve", "PVEVMUser"), token(webPath, "alice@pve!shared", "NoAccess"),
		}, "alice@pve!shared", webPath, []string{"PVEVMUser"}, vmUserPrivs},
		{"an unknown token", []pve.ACLEntry{
			user(webPath, "alice@pve", "PVEVMUser"), token("/", "alice@pve!gone", "Administrator"),
		}, "alice@pve!gone", webPath, nil, nil},
		{"root@pam", nil, "root@pam", "/nodes/pve1", []string{"Administrator"}, adminPrivs},
		{"a pool grant reaches a member", []pve.ACLEntry{user("/pool/tenants", "alice@pve", "PVEVMUser")}, "alice@pve", "/vms/200", nil, vmUserPrivs},
		{"a pool grant does not reach another guest", []pve.ACLEntry{user("/pool/tenants", "alice@pve", "PVEVMUser")}, "alice@pve", webPath, nil, nil},
		{"a pool grant adds to the guest's own", []pve.ACLEntry{
			user("/vms/200", "alice@pve", "PVEAuditor"), user("/pool/tenants", "alice@pve", "PVEVMUser"),
		}, "alice@pve", "/vms/200", []string{"PVEAuditor"}, []string{"Sys.Audit", "VM.Audit", "VM.Backup", "VM.Config.CDROM", "VM.Console", "VM.PowerMgmt"}},
		{"NoAccess on the guest blocks a pool grant", []pve.ACLEntry{
			user("/vms/200", "alice@pve", "NoAccess"), user("/pool/tenants", "alice@pve", "PVEVMAdmin"),
		}, "alice@pve", "/vms/200", []string{"NoAccess"}, nil},
		{"NoAccess on the pool beats the guest's roles", []pve.ACLEntry{
			user("/vms", "alice@pve", "PVEVMUser"), user("/pool/tenants", "alice@pve", "NoAccess"),
		}, "alice@pve", "/vms/200", []string{"PVEVMUser"}, nil},
		// Matrix row E9: inherited roles on /pool/<p> count as pool roles.
		{"a pool role inherited from /", []pve.ACLEntry{
			user("/vms", "alice@pve", "PVEVMAdmin"),
			token("/", "alice@pve!sep", "Administrator"), token("/vms/200", "alice@pve!sep", "PVEAuditor"),
		}, "alice@pve!sep", "/vms/200", []string{"PVEAuditor"}, []string{"VM.Audit", "VM.Config.Network", "VM.Console"}},
		{"a disabled user still holds its roles", []pve.ACLEntry{user(webPath, "dave@pve", "PVEVMUser")}, "dave@pve", webPath, []string{"PVEVMUser"}, vmUserPrivs},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := data(tt.acl...)
			d.Users = append(d.Users, pve.User{ID: "carol@pve", Enabled: true}, pve.User{ID: "dave@pve"})
			d.Groups = append(d.Groups, pve.Group{ID: "ops2", Members: []string{"carol@pve"}})
			d.Groups[0].Members = append(d.Groups[0].Members, "carol@pve")
			require.Equal(t, tt.roles, Roles(d, tt.principal, tt.path), "roles")
			require.Equal(t, tt.privs, Privileges(d, tt.principal, tt.path), "privileges")
		})
	}
}

func TestGroupMembershipFromEitherSide(t *testing.T) {
	d := data(group("/vms", "devs", "PVEVMUser"), group("/vms", "qa", "PVEAuditor"))
	d.Users = append(d.Users, pve.User{ID: "erin@pve", Enabled: true, Groups: []string{"qa"}})
	require.Equal(t, []string{"PVEVMUser"}, Roles(d, "bob@pve", webPath), "the group lists bob")
	require.Equal(t, []string{"PVEAuditor"}, Roles(d, "erin@pve", webPath), "erin lists the group")
}

func TestPathsAreTakenApartAsProxmoxDoes(t *testing.T) {
	d := data(user("/vms/", "alice@pve", "PVEVMUser"), once(user("/", "bob@pve", "PVEAuditor")))
	require.Equal(t, vmUserPrivs, Privileges(d, "alice@pve", webPath), "a trailing slash names the same node")
	require.Equal(t, auditorPrivs, Privileges(d, "bob@pve", "/"))
	require.Nil(t, Privileges(d, "bob@pve", "/vms"))
}

func TestEffective(t *testing.T) {
	d := data(
		user("/vms", "alice@pve", "PVEVMUser"),
		token(webPath, "alice@pve!sep", "PVEVMAdmin"),
		user(webPath, "bob@pve", "PVEAuditor"),
		user(webPath, "dave@pve", "PVEVMAdmin"),
	)
	d.Users = append(d.Users, pve.User{ID: "dave@pve", Tokens: map[string]bool{"shared": false}})

	got := Effective(d, webPath, []string{"VM.Console", "VM.PowerMgmt"})
	require.Equal(t, []Principal{
		{ID: "alice@pve", Privs: []string{"VM.Console", "VM.PowerMgmt"}},
		{ID: "alice@pve!sep", Privs: []string{"VM.Console"}},
		{ID: "alice@pve!shared", Privs: []string{"VM.Console", "VM.PowerMgmt"}},
		{ID: "root@pam", Privs: []string{"VM.Console"}},
	}, got, "a disabled user and its tokens are left out")

	require.Empty(t, Effective(d, webPath, []string{"Datastore.Allocate"}))
	require.Empty(t, Effective(d, webPath, nil))
}

func TestAdmins(t *testing.T) {
	d := data(
		user("/", "alice@pve", "Administrator"),
		token("/", "alice@pve!sep", "PVEAuditor"),
		user("/vms", "bob@pve", "Administrator"),
		group("/", "devs", "PVEAuditor"),
	)
	require.Equal(t, []string{"alice@pve", "alice@pve!shared", "root@pam"}, Admins(d),
		"Sys.Modify on / makes an admin; on /vms it does not")

	d.Users[0].Enabled = false
	require.Equal(t, []string{"root@pam"}, Admins(d))
}

func TestDelegated(t *testing.T) {
	d := data(
		user("/vms/100", "bob@pve", "PVEVMAdmin"),
		user("/vms/100", "alice@pve", "PVEVMUser"),
		group("/pool/tenants", "ops", "PVEVMAdmin"),
		user("/", "pco@pve", "Administrator"),
		user("/vms", "pco@pve", "PVEVMAdmin"),
		token("/vms", "pco@pve!vm120", "PVEVMAdmin"),
		token("/vms/100", "pco@pve!other", "PVEVMAdmin"),
		user("/", "carol@pve", "Administrator"),
	)
	d.Users = append(d.Users, pve.User{ID: "carol@pve", Enabled: true})
	d.Users[2].Tokens["other"] = true

	require.Equal(t, []Principal{{ID: "bob@pve", Privs: []string{"VM.Config.Network"}}}, Delegated(d, web, "pco@pve"),
		"pco's user and tokens, admins and a grant without VM.Config.Network are left out")
	require.Equal(t, []Principal{
		{ID: "alice@pve", Privs: []string{"VM.Config.Network"}},
		{ID: "alice@pve!shared", Privs: []string{"VM.Config.Network"}},
	}, Delegated(d, tenant, "pco@pve"), "a group's pool grant reaches the pool's guests")
	require.Empty(t, Delegated(d, model.GuestRef{Kind: model.KindQEMU, VMID: 300}, "pco@pve"))
}

func TestRefused(t *testing.T) {
	for _, tt := range []struct {
		name string
		acl  []pve.ACLEntry
		want []Principal
	}{
		{"PVEVMUser on the appliance", []pve.ACLEntry{user(applPath, "bob@pve", "PVEVMUser")},
			[]Principal{{ID: "bob@pve", Privs: vmUserReach, At: []string{applPath}}}},
		{"PVEAuditor on the appliance", []pve.ACLEntry{user(applPath, "bob@pve", "PVEAuditor")}, nil},
		{"PVETemplateUser on the appliance", []pve.ACLEntry{user(applPath, "bob@pve", "PVETemplateUser")},
			[]Principal{{ID: "bob@pve", Privs: []string{"VM.Clone"}, At: []string{applPath}}}},
		{"a role that only migrates", []pve.ACLEntry{user(applPath, "bob@pve", "Migrate")},
			[]Principal{{ID: "bob@pve", Privs: []string{"VM.Migrate"}, At: []string{applPath}}}},
		{"PVEVMUser on all guests", []pve.ACLEntry{user("/vms", "bob@pve", "PVEVMUser")},
			[]Principal{{ID: "bob@pve", Privs: vmUserReach, At: []string{applPath}}}},
		{"PVEVMUser on the pool", []pve.ACLEntry{group("/pool/pco", "devs", "PVEVMUser")},
			[]Principal{{ID: "bob@pve", Privs: vmUserReach, At: []string{applPath}}}},
		{"Pool.Allocate on the pool", []pve.ACLEntry{user("/pool/pco", "bob@pve", "PVEPoolAdmin")},
			[]Principal{{ID: "bob@pve", Privs: []string{"Pool.Allocate"}, At: []string{"/pool/pco"}}}},
		{"both", []pve.ACLEntry{user("/pool", "bob@pve", "PVEPoolAdmin"), user("/vms", "bob@pve", "PVEVMAdmin")},
			[]Principal{{ID: "bob@pve", Privs: []string{"Pool.Allocate", "VM.Allocate", "VM.Config.Network", "VM.Console", "VM.Snapshot"},
				At: []string{"/pool/pco", applPath}}}},
		{"another guest", []pve.ACLEntry{user(webPath, "bob@pve", "PVEVMAdmin")}, nil},
		{"a token through its user", []pve.ACLEntry{user(applPath, "alice@pve", "PVEVMUser"), token(applPath, "alice@pve!sep", "PVEAuditor")},
			[]Principal{
				{ID: "alice@pve", Privs: vmUserReach, At: []string{applPath}},
				{ID: "alice@pve!shared", Privs: vmUserReach, At: []string{applPath}},
			}},
		{"an admin", []pve.ACLEntry{user("/", "bob@pve", "Administrator")}, nil},
		{"pco itself", []pve.ACLEntry{user(applPath, "pco@pve", "PVEVMAdmin"), token(applPath, "pco@pve!vm120", "PVEVMAdmin")}, nil},
		// Permissions.Modify on a path lets its holder grant itself any role
		// there; on / and on /vms or /pool it may then propagate one down.
		// NoAccess where it is granted takes it away on every path below that
		// is granted nothing of its own.
		{"Permissions.Modify on / alone", []pve.ACLEntry{once(user("/", "bob@pve", "Delegate"))},
			[]Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/"}}}},
		{"Permissions.Modify on / and below", []pve.ACLEntry{user("/", "bob@pve", "Delegate")},
			[]Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/"}}}},
		{"Permissions.Modify on / through a group", []pve.ACLEntry{group("/", "devs", "Delegate")},
			[]Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/"}}}},
		{"Permissions.Modify on /vms alone", []pve.ACLEntry{once(user("/vms", "bob@pve", "Delegate"))},
			[]Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/vms"}}}},
		{"Permissions.Modify on /vms and below", []pve.ACLEntry{user("/vms", "bob@pve", "Delegate")},
			[]Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/vms"}}}},
		{"Permissions.Modify on the appliance", []pve.ACLEntry{user(applPath, "bob@pve", "Delegate")},
			[]Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{applPath}}}},
		{"Permissions.Modify on /pool alone", []pve.ACLEntry{once(user("/pool", "bob@pve", "Delegate"))},
			[]Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/pool"}}}},
		{"Permissions.Modify on the pool, NoAccess on the appliance", []pve.ACLEntry{
			user(applPath, "bob@pve", "NoAccess"), user("/pool/pco", "bob@pve", "Delegate"),
		}, []Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/pool/pco"}}}},
		{"Permissions.Modify beside a console", []pve.ACLEntry{user("/", "bob@pve", "Delegate"), user(applPath, "bob@pve", "PVEVMUser")},
			[]Principal{{ID: "bob@pve", Privs: append([]string{"Permissions.Modify"}, vmUserReach...), At: []string{"/", applPath}}}},
		{"Permissions.Modify on /vms beside a console", []pve.ACLEntry{user("/vms", "bob@pve", "Delegate"), user(applPath, "bob@pve", "PVEVMUser")},
			[]Principal{{ID: "bob@pve", Privs: append([]string{"Permissions.Modify"}, vmUserReach...), At: []string{"/vms", applPath}}}},
		{"Permissions.Modify on another guest", []pve.ACLEntry{user(webPath, "bob@pve", "Delegate")}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := data(tt.acl...)
			require.Equal(t, tt.want, Refused(d, 120, "pco", "pco@pve"))

			for _, p := range tt.want {
				d.ACL = append(d.ACL, takenAway(d, p)...)
			}
			require.Empty(t, Refused(d, 120, "pco", "pco@pve"), "NoAccess where At says takes it all away")
		})
	}
}

// takenAway is what pveum acl modify <path> --roles NoAccess adds for a
// principal at each of its At: for a token without privilege separation,
// for its user.
func takenAway(d Data, p Principal) []pve.ACLEntry {
	who, kind := p.ID, "user"
	if user, name, isToken := strings.Cut(p.ID, "!"); isToken {
		who = user
		for _, u := range d.Users {
			if u.ID == user && u.Tokens[name] {
				who, kind = p.ID, "token"
			}
		}
	}
	var out []pve.ACLEntry
	for _, path := range p.At {
		out = append(out, pve.ACLEntry{Path: path, Type: kind, UGID: who, RoleID: "NoAccess", Propagate: true})
	}
	return out
}

func TestRefusedTakesTheGuestAsAMemberOfThePool(t *testing.T) {
	d := data(user("/pool/pco", "bob@pve", "PVEVMUser"), user("/pool/pco", "carol@pve", "PVEPoolAdmin"))
	d.Users = append(d.Users, pve.User{ID: "carol@pve", Enabled: true})
	d.Pools = nil
	require.Equal(t, []Principal{
		{ID: "bob@pve", Privs: vmUserReach, At: []string{applPath}},
		{ID: "carol@pve", Privs: []string{"Pool.Allocate"}, At: []string{"/pool/pco"}},
	}, Refused(d, 120, "pco", "pco@pve"), "before the appliance is in its pool")
	require.Empty(t, Refused(d, 120, "", "pco@pve"))

	d.ACL = append(d.ACL, user(applPath, "bob@pve", "Migrate"))
	require.Equal(t, []Principal{{ID: "bob@pve", Privs: []string{"VM.Migrate"}, At: []string{applPath}}}, Refused(d, 120, "", "pco@pve"), "in no pool at all")
}

// The daemon asks with pool pco, but an admin may have moved the appliance
// into another pool: whoever reaches it through either is refused.
func TestRefusedChecksBothPoolsOfAMovedGuest(t *testing.T) {
	moved := func(acl ...pve.ACLEntry) Data {
		d := data(acl...)
		d.Pools[appl] = "infra"
		return d
	}
	require.Equal(t, []Principal{
		{ID: "alice@pve", Privs: vmUserReach, At: []string{applPath}},
		{ID: "alice@pve!shared", Privs: vmUserReach, At: []string{applPath}},
	}, Refused(moved(group("/pool/infra", "ops", "PVEVMUser")), 120, "pco", "pco@pve"), "the pool it is in")
	require.Equal(t, []Principal{{ID: "bob@pve", Privs: vmUserReach, At: []string{applPath}}},
		Refused(moved(user("/pool/pco", "bob@pve", "PVEVMUser")), 120, "pco", "pco@pve"), "the pool it is to be in")
	require.Equal(t, []Principal{{ID: "bob@pve", Privs: []string{"Pool.Allocate"}, At: []string{"/pool/infra"}}},
		Refused(moved(user("/pool/infra", "bob@pve", "PVEPoolAdmin")), 120, "pco", "pco@pve"))
	require.Equal(t, []Principal{{ID: "bob@pve", Privs: []string{"Permissions.Modify"}, At: []string{"/pool/infra"}}},
		Refused(moved(user(applPath, "bob@pve", "NoAccess"), user("/pool/infra", "bob@pve", "Delegate")), 120, "pco", "pco@pve"))
	require.Equal(t, []Principal{{ID: "bob@pve", Privs: []string{"VM.Clone", "VM.Migrate"}, At: []string{applPath}}},
		Refused(moved(user("/pool/infra", "bob@pve", "PVETemplateUser"), user("/pool/pco", "bob@pve", "Migrate")), 120, "pco", "pco@pve"),
		"what either pool gives adds up")

	require.Equal(t, []Principal{{ID: "bob@pve", Privs: []string{"Pool.Allocate"}, At: []string{"/pool/infra"}}},
		Refused(moved(user("/pool/infra", "bob@pve", "PVEPoolAdmin")), 120, "", "pco@pve"), "without a pool to take it in")
	require.Empty(t, Refused(moved(user("/pool/other", "bob@pve", "PVEVMAdmin")), 120, "pco", "pco@pve"))
}
