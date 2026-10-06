package applianceinstall

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// pcoLines are the lines of the access control list that name pco@pve or
// its tokens, but for the grant of role PCO on /.
func (f *fakeNode) pcoLines() []string {
	var out []string
	for _, a := range f.acl {
		if strings.HasPrefix(a.UGID, "pco@pve") && a.Role != "PCO" {
			out = append(out, a.Path+" "+a.Type+" "+a.UGID+" "+a.Role)
		}
	}
	slices.Sort(out)
	return out
}

// The managed network calls the helper with its own zone and vnet: the grant
// goes on that vnet, never on the zone.
func TestGrantNetworkOnTheManagedNetwork(t *testing.T) {
	e := installed(t)

	g, err := GrantNetwork(t.Context(), e.node, 100, "pco", "pconet", 0)

	require.NoError(t, err)
	require.Equal(t, setup.NetworkGrant{Zone: "pco", VNet: "pconet", CreatedRoles: []string{"PCOManaged", "PCOSDN"}}, g)
	require.Equal(t, []string{
		"/sdn/zones/pco/pconet token pco@pve!vm100 PCOSDN",
		"/sdn/zones/pco/pconet user pco@pve PCOSDN",
		"/vms/100 token pco@pve!vm100 PCOManaged",
		"/vms/100 user pco@pve PCOManaged",
	}, e.node.pcoLines())
	require.Equal(t, []string{"VM.Config.Network"}, e.node.roles["PCOManaged"])
	require.Equal(t, []string{"SDN.Use"}, e.node.roles["PCOSDN"])
	require.Equal(t, []string{
		"pveum role list --output-format json",
		"pveum role add PCOManaged --privs VM.Config.Network",
		"pveum role add PCOSDN --privs SDN.Use",
		"pveum acl modify /vms/100 --users pco@pve --tokens pco@pve!vm100 --roles PCOManaged",
		"pveum acl modify /sdn/zones/pco/pconet --users pco@pve --tokens pco@pve!vm100 --roles PCOSDN",
		"pvesh get /access/permissions --userid pco@pve!vm100 --path /vms/100 --output-format json",
		"pvesh get /access/permissions --userid pco@pve!vm100 --path /sdn/zones/pco/pconet --output-format json",
	}, e.node.ran)

	_, err = GrantNetwork(t.Context(), e.node, 100, "pco", "", 0)
	require.ErrorContains(t, err, `vnet "": want the name of a bridge or vnet`, "never a whole zone")
}

func TestGrantNetworkCommand(t *testing.T) {
	for _, tt := range []struct {
		name  string
		vlan  int
		path  string
		grant map[string]any
		warns []string // the addresses of the node on the network granted
	}{
		{"a bridge", 0, "/sdn/zones/localnetwork/vmbr1", map[string]any{"zone": "localnetwork", "vnet": "vmbr1", "createdRoles": []any{"PCOManaged", "PCOSDN"}},
			[]string{"vmbr1 carries 10.92.0.2/24", "vmbr1.20 carries 10.20.0.1/24"}},
		{"a VLAN of a VLAN-aware bridge", 20, "/sdn/zones/localnetwork/vmbr1/20", map[string]any{"zone": "localnetwork", "vnet": "vmbr1", "vlan": float64(20), "createdRoles": []any{"PCOManaged", "PCOSDN"}},
			[]string{"vmbr1.20 carries 10.20.0.1/24"}},
		{"the untagged VLAN of a VLAN-aware bridge", 1, "/sdn/zones/localnetwork/vmbr1/1", map[string]any{"zone": "localnetwork", "vnet": "vmbr1", "vlan": float64(1), "createdRoles": []any{"PCOManaged", "PCOSDN"}},
			[]string{"vmbr1 carries 10.92.0.2/24"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := installed(t)
			e.node.addrs = append(e.node.addrs, "vmbr1 10.92.0.2/24", "vmbr1.20 10.20.0.1/24")

			require.NoError(t, e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr1", VLAN: tt.vlan, Yes: true}), e.ask.text())

			require.Equal(t, []string{
				tt.path + " token pco@pve!vm100 PCOSDN",
				tt.path + " user pco@pve PCOSDN",
				"/vms/100 token pco@pve!vm100 PCOManaged",
				"/vms/100 user pco@pve PCOManaged",
			}, e.node.pcoLines(), "exactly the two lines, for user and token")
			var warned []string
			for _, l := range e.ask.lines {
				if w, ok := strings.CutPrefix(l, "warn: "); ok && strings.Contains(w, " carries ") {
					warned = append(warned, strings.TrimSuffix(w, ", an address of this node: a card of the appliance there reaches the node"))
				}
			}
			require.Equal(t, tt.warns, warned)
			var m setup.Manifest
			require.NoError(t, json.Unmarshal(e.node.cts[100].files[manifestFile].data, &m))
			require.Len(t, m.Appliance.Grants, 1)
			got, err := json.Marshal(m.Appliance.Grants[0])
			require.NoError(t, err)
			var grant map[string]any
			require.NoError(t, json.Unmarshal(got, &grant))
			require.Equal(t, tt.grant, grant, "recorded in the manifest")
			require.Empty(t, entries(t, e.journals))

			require.NoError(t, e.in.RevokeNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr1", VLAN: tt.vlan, Yes: true}))

			require.Empty(t, e.node.pcoLines())
			require.Nil(t, e.node.roles["PCOManaged"])
			require.Nil(t, e.node.roles["PCOSDN"])
			var after setup.Manifest
			require.NoError(t, json.Unmarshal(e.node.cts[100].files[manifestFile].data, &after))
			require.Empty(t, after.Appliance.Grants)
		})
	}
}

func TestGrantNetworkRefuses(t *testing.T) {
	e := installed(t)
	e.node.vnets = []fakeVNet{{Name: "tenants", Zone: "zone1"}}

	err := e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr0", VLAN: 20, Yes: true})
	require.ErrorContains(t, err, "vmbr0 is not VLAN-aware, so it carries no VLAN 20 to grant")
	err = e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "tenants", VLAN: 7, Yes: true})
	require.ErrorContains(t, err, "tenants is not VLAN-aware, so it carries no VLAN 7 to grant")
	err = e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr9", Yes: true})
	require.ErrorContains(t, err, "vmbr9 is neither a bridge of node pve1 nor an SDN vnet")
	require.Empty(t, e.node.pcoLines())

	e.ask.answers = []answer{{"Grant these?", false}}
	require.ErrorIs(t, e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr0"}), setup.ErrAborted)
	require.Empty(t, e.node.pcoLines())
}

func TestGrantNetworkOnAnSDNVNet(t *testing.T) {
	e := installed(t)
	e.node.vnets = []fakeVNet{{Name: "tenants", Zone: "zone1", VLANAware: true}}

	require.NoError(t, e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "tenants", VLAN: 30, Yes: true}))

	require.True(t, e.node.has("/sdn/zones/zone1/tenants/30", "token", "pco@pve!vm100", "PCOSDN"))
	require.True(t, e.node.has("/sdn/zones/zone1/tenants/30", "user", "pco@pve", "PCOSDN"))
}

func TestUninstallTakesBackTheNetworkGrants(t *testing.T) {
	e := installed(t)
	require.NoError(t, e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr1", VLAN: 20, Yes: true}))

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Empty(t, e.node.pcoLines())
	require.Nil(t, e.node.roles["PCOManaged"])
	require.Nil(t, e.node.roles["PCOSDN"])
	e.takenBack()
}

// A pool grant is one pair of lines, for the user and the token, on the pool
// and nowhere else, with the role of the cards alone.
func TestGrantPoolGrantsTheCardsOfThePoolsMembers(t *testing.T) {
	e := installed(t)
	e.node.pools["web"] = &fakePool{Members: []int{200}}

	g, err := GrantPool(t.Context(), e.node, 100, "web")

	require.NoError(t, err)
	require.Equal(t, setup.NetworkGrant{Pool: "web", CreatedRoles: []string{"PCOManaged"}}, g)
	require.Equal(t, []string{
		"/pool/web token pco@pve!vm100 PCOManaged",
		"/pool/web user pco@pve PCOManaged",
	}, e.node.pcoLines())
	require.Equal(t, []string{
		"pveum pool list --output-format json",
		"pveum role list --output-format json",
		"pveum role add PCOManaged --privs VM.Config.Network",
		"pveum acl modify /pool/web --users pco@pve --tokens pco@pve!vm100 --roles PCOManaged",
		"pvesh get /access/permissions --userid pco@pve!vm100 --path /pool/web --output-format json",
	}, e.node.ran)
	require.Nil(t, e.node.roles["PCOSDN"])

	require.NoError(t, RevokePool(t.Context(), e.node, 100, "web"))

	require.Empty(t, e.node.pcoLines())
	require.Nil(t, e.node.roles["PCOManaged"])
}

func TestGrantPoolRefuses(t *testing.T) {
	e := installed(t)
	for _, tt := range []struct{ pool, want string }{
		{"pco", "pool pco holds the appliances: grant the pool of the guests"},
		{"", `pool "": want the name of a pool`},
		{"a/b", `pool "a/b": want the name of a pool`},
		{"web", "pool web does not exist"},
	} {
		_, err := GrantPool(t.Context(), e.node, 100, tt.pool)
		require.EqualError(t, err, tt.want)
	}
	require.Empty(t, e.node.pcoLines())
	require.False(t, slices.ContainsFunc(e.node.acl, func(a fakeACL) bool { return a.Path == "/" && a.Role == "PCOManaged" }),
		"the cards are never granted on /")
}

// The role of the cards stays while a network grant names it, and the line
// of pco@pve on the pool while another appliance's token has it as well.
func TestRevokePoolKeepsWhatOthersNeed(t *testing.T) {
	e := installed(t)
	e.node.pools["web"] = &fakePool{}
	_, err := GrantNetwork(t.Context(), e.node, 100, "pco", "pconet", 0)
	require.NoError(t, err)
	_, err = GrantPool(t.Context(), e.node, 100, "web")
	require.NoError(t, err)
	e.node.addToken("pco@pve", "vm200", "pco appliance vm200", true)
	e.node.grant("/pool/web", "token", "pco@pve!vm200", "PCOManaged")

	require.NoError(t, RevokePool(t.Context(), e.node, 100, "web"))

	require.Equal(t, []string{
		"/pool/web token pco@pve!vm200 PCOManaged",
		"/pool/web user pco@pve PCOManaged",
		"/sdn/zones/pco/pconet token pco@pve!vm100 PCOSDN",
		"/sdn/zones/pco/pconet user pco@pve PCOSDN",
		"/vms/100 token pco@pve!vm100 PCOManaged",
		"/vms/100 user pco@pve PCOManaged",
	}, e.node.pcoLines())
	require.NotNil(t, e.node.roles["PCOManaged"])
}

// The uninstall finds a pool grant by the line of the appliance's token, as
// it finds a network grant, and takes back the line of pco@pve with it.
func TestUninstallTakesBackThePoolGrant(t *testing.T) {
	e := installed(t)
	e.node.pools["web"] = &fakePool{Members: []int{200}}
	_, err := GrantPool(t.Context(), e.node, 100, "web")
	require.NoError(t, err)

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Empty(t, e.node.pcoLines())
	require.Nil(t, e.node.roles["PCOManaged"])
	require.Contains(t, e.ask.text(), "the grant of role PCOManaged on /pool/web to pco@pve")
	require.NotNil(t, e.node.pools["web"], "the pool is the admin's")
}

// A pool grant has the shape of a network grant in the manifest, with the
// pool in place of the network.
func TestAPoolGrantInTheManifest(t *testing.T) {
	for _, tt := range []struct {
		name string
		g    setup.NetworkGrant
		want string
	}{
		{"a pool", setup.NetworkGrant{Pool: "web", CreatedRoles: []string{"PCOManaged"}}, ""},
		{"a pool and a network", setup.NetworkGrant{Pool: "web", Zone: "pco", VNet: "pconet"}, "pool web: a pool grant names no network"},
		{"the role of the vnets", setup.NetworkGrant{Pool: "web", CreatedRoles: []string{"PCOSDN"}}, `role "PCOSDN": a pool grant makes PCOManaged only`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.g.Check()
			if tt.want == "" {
				require.NoError(t, err)
				require.Equal(t, "/pool/web", tt.g.Path())
				return
			}
			require.EqualError(t, err, tt.want)
		})
	}
	require.False(t, setup.NetworkGrant{Pool: "web"}.Same(setup.NetworkGrant{Pool: "db"}))
	require.True(t, setup.NetworkGrant{Pool: "web"}.Same(setup.NetworkGrant{Pool: "web", CreatedRoles: []string{"PCOManaged"}}))
}

// The grant of pco@pve on a network another token of it has as well stays
// for that token, which holds only what its user holds too.
func TestAGrantAnotherTokenNeedsStays(t *testing.T) {
	e := installed(t)
	require.NoError(t, e.in.GrantNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr1", Yes: true}))
	e.node.addToken("pco@pve", "vm200", "pco appliance vm200", true)
	e.node.grant("/sdn/zones/localnetwork/vmbr1", "token", "pco@pve!vm200", "PCOSDN")

	require.NoError(t, e.in.RevokeNetworkCommand(t.Context(), NetworkOptions{VMID: 100, Bridge: "vmbr1", Yes: true}))

	require.Equal(t, []string{
		"/sdn/zones/localnetwork/vmbr1 token pco@pve!vm200 PCOSDN",
		"/sdn/zones/localnetwork/vmbr1 user pco@pve PCOSDN",
	}, e.node.pcoLines())
	require.NotNil(t, e.node.roles["PCOSDN"])
	require.Nil(t, e.node.roles["PCOManaged"])
}
