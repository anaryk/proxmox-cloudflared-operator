package applianceinstall

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

// installed is a node with appliance 100 installed, the commands of the
// install forgotten.
func installed(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t)
	e.install(e.options())
	e.node.ran, e.node.inits, e.node.pushes, e.probed = nil, nil, nil, nil
	return e
}

// restored is a restore of appliance 100 to vmid: the same configuration but
// the MAC, and a new, empty state volume, or none.
func (e *testEnv) restored(vmid int, withVolume bool) {
	e.t.Helper()
	cfg := maps.Clone(e.node.cts[100].cfg)
	cfg["net0"] = strings.Replace(cfg["net0"], "BC:24:11:00:00:64", "BC:24:11:00:00:65", 1)
	delete(cfg, "protection")
	if !withVolume {
		delete(cfg, "mp0")
	}
	e.node.cts[vmid] = &fakeCT{cfg: cfg, files: map[string]fakeFile{}, state: "degraded", failed: []string{"pco.service"}, version: testVersion}
	e.node.pools["pco"].Members = append(e.node.pools["pco"].Members, vmid)
}

// cloned is a full clone of appliance 100 to lxc/121, stopped: its state
// volume with every secret on it, a new MAC, the description of 100.
func (e *testEnv) cloned() {
	e.t.Helper()
	e.restored(121, true)
	ct, original := e.node.cts[121], e.node.cts[100]
	ct.files, ct.meta = maps.Clone(original.files), original.meta
}

// lose takes appliance 100 away as a lost node and an admin who removed what
// was left of it would: the container and what refers to it, not its token.
func (e *testEnv) lose() {
	e.t.Helper()
	delete(e.node.cts, 100)
	e.node.acl = slices.DeleteFunc(e.node.acl, func(a fakeACL) bool { return a.Path == "/vms/100" })
	e.node.pools["pco"].Members = slices.DeleteFunc(e.node.pools["pco"].Members, func(m int) bool { return m == 100 })
}

func TestRepairWithStateMakesTheTokenAnew(t *testing.T) {
	e := installed(t)
	e.node.addrs = append(e.node.addrs, "vmbr1 10.92.0.2/24")

	require.NoError(t, e.in.Repair(t.Context(), 100, Options{Yes: true}), e.ask.text())

	require.Contains(t, e.node.ran, "pct exec 100 --keep-env 0 -- test -f /var/lib/pco/.volume")
	require.Contains(t, e.node.ran, "pct exec 100 --keep-env 0 -- test -d /var/lib/pco/cluster/meta")
	require.Equal(t, 1, e.node.count("pct pull 100 /var/lib/pco/manifest.json "))
	require.Equal(t, 1, e.node.count("pveum user token remove pco@pve vm100"))
	require.Equal(t, 1, e.node.count("pveum user token add pco@pve vm100"))
	require.Equal(t, []string{"100 /var/lib/pco/pve-ca.pem 0644", "100 /var/lib/pco/bootstrap.json 0600"}, e.node.pushes)
	b := e.bootstrap(0)
	require.Equal(t, "repair", b["mode"])
	require.Equal(t, pveSecret+"2", b["pveToken"].(map[string]any)["secret"])
	require.Equal(t, []any{"bc:24:11:00:00:64"}, b["macs"])
	require.Equal(t, []any{"192.0.2.10", "10.92.0.2"}, b["nodeAddrs"])
	m := b["manifest"].(map[string]any)
	require.Equal(t, true, m["createdRole"], "the manifest pulled is carried on")
	require.Equal(t, "local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst", m["appliance"].(map[string]any)["template"])
	require.Empty(t, entries(t, e.journals))
	require.Empty(t, entries(t, e.runDir))
	require.True(t, e.node.cts[100].running)
}

// The certificate of the API changed; the repair
// probes it again, and the bootstrap carries the name it verifies under now
// and the CA that verifies it.
func TestRepairTakesTheCertificateAsItIsNow(t *testing.T) {
	e := installed(t)
	e.presented = e.certs.custom
	apiCA := filepath.Join(t.TempDir(), "custom-ca.pem")
	require.NoError(t, os.WriteFile(apiCA, e.certs.customCA, 0o644))

	err := e.in.Repair(t.Context(), 100, Options{Yes: true})
	require.ErrorContains(t, err, "the certificate verifies neither under the node name pve1")
	require.Empty(t, e.node.inits)

	require.NoError(t, e.in.Repair(t.Context(), 100, Options{Yes: true, APICA: apiCA}), e.ask.text())

	ep := e.bootstrap(0)["endpoints"].([]any)[0].(map[string]any)
	require.Equal(t, map[string]any{"address": "192.0.2.10:8006", "serverName": "pve.example.test"}, ep)
	require.Contains(t, e.node.ran, "pct push 100 "+apiCA+" /var/lib/pco/pve-ca.pem --perms 0644")
}

// An ACME certificate the system roots know: the name it verifies under now.
func TestRepairTakesACertificateOfThePublicRoots(t *testing.T) {
	e := installed(t)
	e.presented = e.certs.custom
	require.True(t, e.roots.AppendCertsFromPEM(e.certs.customCA))

	require.NoError(t, e.in.Repair(t.Context(), 100, Options{Yes: true}), e.ask.text())

	ep := e.bootstrap(0)["endpoints"].([]any)[0].(map[string]any)
	require.Equal(t, "pve.example.test", ep["serverName"])
	require.Contains(t, e.ask.text(), "verifies as pve.example.test against the system roots")
}

func TestRepairStartsAStoppedContainerAndStopsItAgain(t *testing.T) {
	e := installed(t)
	e.node.cts[100].running = false

	require.NoError(t, e.in.Repair(t.Context(), 100, Options{Yes: true}), e.ask.text())

	require.Equal(t, "pct start 100", e.node.ran[indexOf(t, e.node.ran, "pct start 100")])
	require.Equal(t, "pct stop 100", e.node.ran[len(e.node.ran)-1])
	require.False(t, e.node.cts[100].running)
	require.Len(t, e.node.inits, 1)
}

// A restore to a new VMID, the appliance it was made from lost, brings a new,
// empty volume: no marker, no state.
func TestRepairRecoversARestoreToAnotherVMID(t *testing.T) {
	e := installed(t)
	e.restored(101, true)
	e.lose()

	err := e.in.Repair(t.Context(), 101, Options{Yes: true})
	require.ErrorContains(t, err, "the volume of lxc/101 holds no state (a restore, or a lost volume): pco appliance repair --vmid 101 --recover --cf-token-file <file>")

	e.node.ran, e.node.pushes = nil, nil
	require.NoError(t, e.in.Repair(t.Context(), 101, Options{Yes: true, Recover: true, CloudflareToken: cfToken}), e.ask.text())

	require.Equal(t, "pco appliance vm101, installed 2026-10-01 by pco appliance install", e.node.cts[101].cfg["description"])
	require.Equal(t, 0, e.node.count("pct set 101 --mp0"), "it has a volume")
	require.Equal(t, []string{"101 /var/lib/pco/.volume 0600", "101 /var/lib/pco/pve-ca.pem 0644", "101 /var/lib/pco/bootstrap.json 0600"}, e.node.pushes)
	require.Equal(t, []string{"vm100", "vm101"}, e.node.tokenNames("pco@pve"), "the token of the appliance it was made from stays")
	b := e.bootstrap(0)
	require.Equal(t, "recover", b["mode"])
	require.EqualValues(t, 101, b["vmid"])
	require.Equal(t, cfToken, b["cloudflareToken"])
	require.Equal(t, []any{"bc:24:11:00:00:65"}, b["macs"])
	// The manifest is rebuilt from the marked objects in Proxmox.
	m := b["manifest"].(map[string]any)
	require.Equal(t, true, m["createdRole"])
	require.Equal(t, true, m["createdUser"])
	require.Equal(t, true, m["grantedACL"])
	require.Equal(t, true, m["createdToken"])
	require.Nil(t, m["registeredTags"], "tags carry no mark")
	require.Equal(t, map[string]any{
		"vmid": float64(101), "node": "pve1", "token": "pco@pve!vm101", "pool": "pco", "createdPool": true,
		"template": "local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst",
	}, m["appliance"])
	require.False(t, e.node.cts[101].running, "stopped again, as it was")
}

// A restore without the volume, or one over the appliance that lost it: the
// container gets a volume of its own again.
func TestRepairGivesARestoreWithoutItsVolumeOneAgain(t *testing.T) {
	e := installed(t)
	e.restored(102, false)
	e.lose()

	require.NoError(t, e.in.Repair(t.Context(), 102, Options{Yes: true, Recover: true, CloudflareToken: cfToken, InstallID: "0123456789ab"}), e.ask.text())

	i := indexOf(t, e.node.ran, "pct set 102 --mp0 local-zfs:1,mp=/var/lib/pco,backup=0")
	require.Equal(t, "pct stop 102", e.node.ran[i-1], "a volume is added to a stopped container")
	require.Equal(t, "pct start 102", e.node.ran[i+1])
	require.Contains(t, e.node.cts[102].cfg["mp0"], "mp=/var/lib/pco,backup=0")
	require.Equal(t, "recover", e.bootstrap(0)["mode"])
	require.Equal(t, "0123456789ab", e.bootstrap(0)["installId"])
}

func TestRepairRefuses(t *testing.T) {
	e := installed(t)
	err := e.in.Repair(t.Context(), 100, Options{Yes: true, Recover: true, CloudflareToken: cfToken})
	require.ErrorContains(t, err, "the volume of lxc/100 holds the state of an install: repair it without --recover")

	e.restored(103, true)
	e.lose()
	err = e.in.Repair(t.Context(), 103, Options{Yes: true, Recover: true})
	require.ErrorContains(t, err, "--recover needs a Cloudflare token to find the install with: pass --cf-token-file")

	e.node.cts[104] = &fakeCT{cfg: map[string]string{"description": "a guest of an admin\n"}, files: map[string]fakeFile{}}
	err = e.in.Repair(t.Context(), 104, Options{Yes: true})
	require.EqualError(t, err, "lxc/104 is not a pco appliance (its description lacks the mark of the installer): nothing was changed")

	err = e.in.Repair(t.Context(), 105, Options{Yes: true})
	require.EqualError(t, err, "there is no container lxc/105 in the cluster")
	require.Empty(t, entries(t, e.journals))
}

func TestRepairOfAContainerOnAnotherNodeIsRefused(t *testing.T) {
	e := installed(t)
	e.migrated(100, "pve2")

	err := e.in.Repair(t.Context(), 100, Options{Yes: true})

	require.EqualError(t, err, "lxc/100 is on node pve2, not on pve1: run the repair there; nothing was changed")
	require.Equal(t, 0, e.node.count("pveum"))
	require.Empty(t, e.node.inits)
}

// A copy beside the appliance it was made from carries the same install:
// repaired, it would write that install as a second writer, with the
// original's credentials. The repair refuses it before it changes anything.
func TestRepairRefusesACopyBesideItsOriginal(t *testing.T) {
	refusal := func(node string) string {
		return "lxc/121 is a copy of lxc/100, which is still there, on node " + node + ": repaired, the copy would write the " +
			"install of lxc/100 beside it, with its credentials. Remove the copy with pco appliance uninstall --vmid 121 " +
			"--keep-cloudflare, or, for an appliance of its own, install one anew with pco appliance install; nothing was changed"
	}
	for _, tt := range []struct {
		name  string
		setup func(e *testEnv)
		o     Options
		node  string
	}{
		{"a clone with the state", func(e *testEnv) { e.cloned() }, Options{Yes: true}, "pve1"},
		{"a restore, with --recover", func(e *testEnv) { e.restored(121, true) },
			Options{Yes: true, Recover: true, CloudflareToken: cfToken}, "pve1"},
		{"a clone of an appliance on another node", func(e *testEnv) {
			e.cloned()
			e.migrated(100, "pve2")
		}, Options{Yes: true}, "pve2"},
		// A repair of an earlier version marked the copy as itself: its
		// manifest still names the original.
		{"a clone marked as itself", func(e *testEnv) {
			e.cloned()
			e.node.cts[121].cfg["description"] = description(121, t0)
		}, Options{Yes: true}, "pve1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := installed(t)
			tt.setup(e)
			desc := e.node.cts[121].cfg["description"]

			err := e.in.Repair(t.Context(), 121, tt.o)

			require.EqualError(t, err, refusal(tt.node))
			require.Equal(t, desc, e.node.cts[121].cfg["description"])
			require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
			require.Equal(t, 0, e.node.count("pveum user token"))
			require.Empty(t, e.node.inits)
			require.Empty(t, e.node.pushes)
			require.False(t, e.node.cts[121].running)
		})
	}
}

// A manifest rebuilt from the marks cannot name a NoAccess line, which
// carries none: the repair says so.
func TestARecoverSaysWhichNoAccessLinesItCannotKnow(t *testing.T) {
	e := installed(t)
	e.restored(101, true)
	e.lose()
	e.node.users = append(e.node.users, &fakeUser{ID: "ops@pve", Enabled: true})
	e.node.grant("/", "user", "ops@pve", "NoAccess")

	require.NoError(t, e.in.Repair(t.Context(), 101, Options{Yes: true, Recover: true, CloudflareToken: cfToken}), e.ask.text())

	require.Contains(t, e.ask.text(), "warn: NoAccess for ops@pve on / carries no mark of the installer: the manifest rebuilt from "+
		"the marks leaves it out, and uninstall leaves it")
	require.Nil(t, e.bootstrapAppliance()["noAccess"])
}

// A restore to another VMID takes over the NoAccess lines above the container
// the installer added, but not the one on the container it was made from.
func TestTheManifestOfARestoreKeepsTheNoAccessLinesAboveIt(t *testing.T) {
	raw := `{"node":"pve1","appliance":{"vmid":100,"node":"pve1","token":"pco@pve!vm100","pool":"pco","noAccess":[` +
		`{"principal":"ops@pve","path":"/","role":"NoAccess"},` +
		`{"principal":"vmops@pve","path":"/vms/100","role":"NoAccess"},` +
		`{"principal":"pools@pve!ci","path":"/pool/pco","role":"NoAccess"}]}}`

	m, named, problems := checkManifest([]byte(raw), 101, true)

	require.NotNil(t, m)
	require.Equal(t, 100, named)
	require.Equal(t, []setup.NoAccessLine{
		{Principal: "ops@pve", Path: "/", Role: "NoAccess"},
		{Principal: "pools@pve!ci", Path: "/pool/pco", Role: "NoAccess"},
	}, m.Appliance.NoAccess)
	require.Equal(t, []string{"it is the manifest of lxc/100, which lxc/101 was restored from: its token, its grants and its " +
		"NoAccess lines on /vms/100 are that one's"}, problems)
}

func indexOf(t *testing.T, lines []string, line string) int {
	t.Helper()
	for i, l := range lines {
		if l == line {
			return i
		}
	}
	t.Fatalf("%q did not run; ran:\n%s", line, strings.Join(lines, "\n"))
	return -1
}
