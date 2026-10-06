package applianceinstall

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// nothingMade fails unless the node holds nothing of an appliance and the
// installer left no journal.
func (e *testEnv) nothingMade() {
	e.t.Helper()
	require.Empty(e.t, e.node.cts)
	require.Empty(e.t, e.node.pools)
	require.Equal(e.t, []string{"root@pam"}, e.node.userIDs())
	require.Nil(e.t, e.node.roles["PCO"])
	require.Equal(e.t, []string{"admin-only"}, e.node.tags)
	require.Empty(e.t, e.node.volumes)
	require.Empty(e.t, entries(e.t, e.journals))
	require.Equal(e.t, 0, e.node.count("pct create"))
}

func TestTheBridgeAndItsVLAN(t *testing.T) {
	for _, tt := range []struct {
		name   string
		bridge string
		vlan   int
		want   string // the error; empty: net0 in the create
	}{
		{name: "a tag on a bridge that is not VLAN-aware", bridge: "vmbr0", vlan: 20,
			want: "bridge vmbr0 is not VLAN-aware, so --vlan 20 cannot be given to the appliance's card"},
		{name: "no tag on a VLAN-aware bridge", bridge: "vmbr1",
			want: "bridge vmbr1 is VLAN-aware: name the VLAN of the appliance with --vlan"},
		{name: "a tag on a VLAN-aware bridge", bridge: "vmbr1", vlan: 20, want: "--net0 name=eth0,bridge=vmbr1,tag=20,ip=dhcp "},
		{name: "a bridge the node does not have", bridge: "vmbr7", want: "node pve1 has no bridge vmbr7"},
		{name: "a card that is no bridge", bridge: "eno1", want: "node pve1 has no bridge eno1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.node.addrs = append(e.node.addrs, "vmbr1.20 10.20.0.1/24")
			o := e.options()
			o.Bridge, o.VLAN = tt.bridge, tt.vlan

			err := e.in.Install(t.Context(), o)

			if strings.HasPrefix(tt.want, "--net0") {
				require.NoError(t, err)
				i := slices.IndexFunc(e.node.ran, func(l string) bool { return strings.HasPrefix(l, "pct create") })
				require.Contains(t, e.node.ran[i], tt.want)
				require.Equal(t, []string{"10.20.0.1:8006"}, e.probed, "the node's address on the VLAN is the API host")
				return
			}
			require.ErrorContains(t, err, tt.want)
			e.nothingMade()
		})
	}
}

func TestWithoutAnAddressOnTheBridgeTheAPIHostIsNamed(t *testing.T) {
	e := newEnv(t)
	e.node.addrs = []string{"vmbr1 10.92.0.2/24"}

	err := e.in.Install(t.Context(), e.options())

	require.ErrorContains(t, err, "the node has no address on vmbr0, which the appliance would reach the API at: name one with --api-host")
	e.nothingMade()

	e = newEnv(t)
	e.node.addrs = []string{"vmbr1 10.92.0.2/24"}
	o := e.options()
	o.APIHost = "192.0.2.10"
	e.install(o)
	require.Equal(t, []string{"192.0.2.10:8006"}, e.probed)
}

func TestTheServerNameTheCertificateVerifiesUnder(t *testing.T) {
	for _, tt := range []struct {
		name    string
		custom  bool   // the API presents the custom certificate
		apiCA   bool   // its CA is given with --api-ca
		roots   bool   // the system roots know its CA
		want    string // the server name; empty: refused
		wantCA  string // what is pushed as pve-ca.pem
		wantErr string
	}{
		{name: "pveproxy's own: the node name, against the cluster CA", want: "pve1", wantCA: "cluster"},
		{name: "a custom certificate with --api-ca", custom: true, apiCA: true, want: "pve.example.test", wantCA: "api"},
		{name: "a custom certificate the system roots know", custom: true, roots: true, want: "pve.example.test", wantCA: "cluster"},
		{name: "a custom certificate nothing verifies", custom: true,
			wantErr: "the certificate verifies neither under the node name pve1 against the cluster CA nor under a DNS name it carries (pve.example.test) against the system roots"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			o := e.options()
			apiCA := filepath.Join(t.TempDir(), "custom-ca.pem")
			require.NoError(t, os.WriteFile(apiCA, e.certs.customCA, 0o644))
			if tt.custom {
				e.presented = e.certs.custom
			}
			if tt.apiCA {
				o.APICA = apiCA
			}
			if tt.roots {
				require.True(t, e.roots.AppendCertsFromPEM(e.certs.customCA))
			}

			err := e.in.Install(t.Context(), o)

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				e.nothingMade()
				return
			}
			require.NoError(t, err)
			ep := e.bootstrap(0)["endpoints"].([]any)[0].(map[string]any)
			require.Equal(t, tt.want, ep["serverName"])
			src := filepath.Join(e.pveDir, "pve-root-ca.pem")
			if tt.wantCA == "api" {
				src = apiCA
			}
			require.Contains(t, e.node.ran, "pct push 100 "+src+" /var/lib/pco/pve-ca.pem --perms 0644")
		})
	}
}

func TestTheVMIDOfferedIsTakenAgainWhenAnotherGotItFirst(t *testing.T) {
	e := newEnv(t)
	// Another installer makes lxc/100 between the preflight and pct create.
	e.node.on("pct create 100 ", func(context.Context, []string) (string, error) {
		e.node.cts[100] = &fakeCT{cfg: map[string]string{"description": "a guest of an admin\n"}, files: map[string]fakeFile{}}
		return "", fmt.Errorf("unable to create CT 100 - CT 100 already exists on node 'pve1'")
	})

	e.install(e.options())

	require.Equal(t, "a guest of an admin\n", e.node.cts[100].cfg["description"], "the other one's container is left alone")
	require.Contains(t, e.node.cts[101].cfg["description"], "pco appliance vm101")
	require.Equal(t, []string{"vm101"}, e.node.tokenNames("pco@pve"))
	require.EqualValues(t, 101, e.bootstrap(0)["vmid"])
	require.Equal(t, 0, e.node.count("pct destroy"))
	require.Contains(t, e.ask.text(), "VMID 100 was taken before the container was made; taking the next free one")
}

func TestAVMIDGivenThatIsTakenIsRefused(t *testing.T) {
	e := newEnv(t)
	e.node.vms[150] = true
	o := e.options()
	o.VMID = 150

	err := e.in.Install(t.Context(), o)

	require.ErrorContains(t, err, "VMID 150 is taken: choose another with --vmid, or leave it out for the next free one")
	e.nothingMade()
}

// A principal that can reach into the appliance is
// refused, unless the admin says to deny it, at the question or with
// --deny-access; --yes never does. A token without privilege separation
// holds its user's roles, so the line names the user.
func TestPrincipalsThatCanReachInAreRefusedOrDenied(t *testing.T) {
	type principal struct {
		setup func(f *fakeNode)
		line  string // the line printed
		who   string // the principal the NoAccess line names
		flag  string
		why   string
		path  string // where NoAccess goes; empty: /vms/100
	}
	user := principal{
		setup: func(f *fakeNode) {
			f.users = append(f.users, &fakeUser{ID: "ops@pve", Enabled: true})
			f.grant("/vms/100", "user", "ops@pve", "PVEVMUser")
		},
		line: "pveum acl modify /vms/100 --users ops@pve --roles NoAccess", who: "ops@pve", flag: "user",
	}
	sep := principal{
		setup: func(f *fakeNode) {
			// The token holds what its user holds as well: both are denied.
			f.addToken("ops@pve", "ps", "", true)
			f.grant("/vms", "user", "ops@pve", "PVEVMUser")
			f.grant("/vms/100", "token", "ops@pve!ps", "PVEVMUser")
		},
		line: "pveum acl modify /vms/100 --tokens 'ops@pve!ps' --roles NoAccess", who: "ops@pve!ps", flag: "token",
	}
	nosep := principal{
		setup: func(f *fakeNode) {
			f.addToken("ops@pve", "np", "", false)
			f.grant("/vms", "user", "ops@pve", "PVEVMUser")
			f.grant("/vms", "token", "ops@pve!np", "PVEAuditor")
		},
		line: "pveum acl modify /vms/100 --users ops@pve --roles NoAccess", who: "ops@pve", flag: "user",
		why: "ops@pve!np is not privilege-separated: it holds the roles of ops@pve",
	}
	pool := principal{
		setup: func(f *fakeNode) {
			f.roles["PoolOps"] = []string{"Pool.Allocate", "Pool.Audit"}
			f.users = append(f.users, &fakeUser{ID: "ops@pve", Enabled: true})
			f.grant("/pool/pco", "user", "ops@pve", "PoolOps")
		},
		line: "pveum acl modify /pool/pco --users ops@pve --roles NoAccess", who: "ops@pve", flag: "user", path: "/pool/pco",
	}
	for _, tt := range []struct {
		name    string
		p       principal
		yes     bool
		deny    bool
		answers []answer
		refused bool
	}{
		{name: "a user, with --yes", p: user, yes: true, refused: true},
		{name: "a user, at the question answered with its default", p: user, answers: []answer{{"Add NoAccess for ops@pve on /vms/100", false}}, refused: true},
		{name: "a user, with --deny-access", p: user, yes: true, deny: true},
		{name: "a user, at the question answered yes", p: user, answers: []answer{{"Add NoAccess for ops@pve on /vms/100", true}, {"Register the gate tags", true}}},
		{name: "a token with privilege separation, with --yes", p: sep, yes: true, refused: true},
		{name: "a token with privilege separation, with --deny-access", p: sep, yes: true, deny: true},
		{name: "a token without privilege separation, with --yes", p: nosep, yes: true, refused: true},
		{name: "a token without privilege separation, with --deny-access", p: nosep, yes: true, deny: true},
		{name: "Pool.Allocate on the pool, with --yes", p: pool, yes: true, refused: true},
		{name: "Pool.Allocate on the pool, with --deny-access", p: pool, yes: true, deny: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			tt.p.setup(e.node)
			users := len(e.node.users)
			e.ask.answers = tt.answers
			o := e.options()
			o.Yes, o.DenyAccess = tt.yes, tt.deny

			err := e.in.Install(t.Context(), o)

			text := e.ask.text()
			require.Contains(t, text, tt.p.line)
			if tt.p.why != "" {
				require.Contains(t, text, tt.p.why)
			}
			if tt.refused {
				require.ErrorContains(t, err, "principals other than the admins can reach into lxc/100: run the lines above, "+
					"or let the installer add them with --deny-access (--yes never does)")
				require.Empty(t, e.node.cts)
				require.Empty(t, e.node.pools)
				require.Len(t, e.node.users, users)
				require.Empty(t, entries(t, e.journals))
				return
			}
			require.NoError(t, err, text)
			path := cmp.Or(tt.p.path, "/vms/100")
			require.True(t, e.node.has(path, tt.p.flag, tt.p.who, "NoAccess"))
			// Added once the container is there, and asked of Proxmox again.
			create := slices.IndexFunc(e.node.ran, func(l string) bool { return strings.HasPrefix(l, "pct create 100") })
			add := slices.Index(e.node.ran, "pveum acl modify "+path+" --"+tt.p.flag+"s "+tt.p.who+" --roles NoAccess")
			check := slices.Index(e.node.ran, "pvesh get /access/permissions --userid "+tt.p.who+" --path "+path+" --output-format json")
			require.True(t, create < add && add < check, "create %d, NoAccess %d, check %d", create, add, check)
		})
	}
}

func TestACleanNodeRefusesNobody(t *testing.T) {
	e := newEnv(t)
	// An admin and an auditor are no reason to refuse.
	e.node.users = append(e.node.users, &fakeUser{ID: "admin@pve", Enabled: true}, &fakeUser{ID: "audit@pve", Enabled: true})
	e.node.grant("/", "user", "admin@pve", "Administrator")
	e.node.grant("/", "user", "audit@pve", "PVEAuditor")

	e.install(e.options())

	require.Contains(t, e.ask.text(), "no principal but the admins can reach into lxc/100")
	require.Equal(t, 0, e.node.count("pveum acl modify /vms/"))
}

// Another install of pco on the node or in the cluster would claim the same
// guests.
func TestAnotherInstallIsWarnedOf(t *testing.T) {
	host := func(e *testEnv) {
		require.NoError(t, os.MkdirAll(filepath.Join(e.pveDir, "pco", "meta"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(e.pveDir, "pco", "meta", "install.json"), []byte(`{"id":"0123456789ab"}`), 0o644))
	}
	appliance := func(e *testEnv) {
		e.node.users = append(e.node.users, &fakeUser{ID: "pco@pve", Comment: "pco operator", Enabled: true,
			Tokens: []fakeToken{{Name: "vm9240", Comment: "pco appliance vm9240", Privsep: true}}})
	}
	for _, tt := range []struct {
		name    string
		setup   func(e *testEnv)
		warning string
	}{
		{"a host install", host, "pco is set up on this node as the host profile (install 0123456789ab): both would claim every guest " +
			"with the gate tag; uninstall one, or give this one another gate tag with --gate-tag"},
		{"another appliance", appliance, "another appliance lxc/9240 exists (its token pco@pve!vm9240): one appliance serves a cluster"},
	} {
		t.Run(tt.name+", the question's default", func(t *testing.T) {
			e := newEnv(t)
			tt.setup(e)
			e.ask.answers = []answer{{"Install the appliance beside it?", false}}
			o := e.options()
			o.Yes = false

			err := e.in.Install(t.Context(), o)

			require.ErrorContains(t, err, "aborted")
			require.Contains(t, e.ask.text(), "warn: "+tt.warning)
			require.Empty(t, e.node.cts)
			require.Empty(t, entries(t, e.journals))
		})
		t.Run(tt.name+", with --yes", func(t *testing.T) {
			e := newEnv(t)
			tt.setup(e)

			e.install(e.options())

			require.Contains(t, e.ask.text(), "warn: "+tt.warning)
			require.NotNil(t, e.node.cts[100])
		})
	}
}

func TestTheClusterIsPrintedAndWithoutQuorumRefused(t *testing.T) {
	e := newEnv(t)
	e.node.cluster = []map[string]any{{"type": "cluster", "name": "lab", "nodes": 3, "quorate": 1}, {"type": "node", "name": "pve1", "local": 1}}
	e.install(e.options())
	require.Contains(t, e.ask.text(), "pve1 is a member of cluster lab with 3 nodes, quorate")

	e = newEnv(t)
	e.node.cluster = []map[string]any{{"type": "cluster", "name": "lab", "nodes": 3, "quorate": 0}}
	err := e.in.Install(t.Context(), e.options())
	require.ErrorContains(t, err, "cluster lab has no quorum")
	e.nothingMade()
}
