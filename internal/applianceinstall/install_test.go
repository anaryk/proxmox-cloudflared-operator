package applianceinstall

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

func TestAFreshInstallRunsExactlyTheseCommands(t *testing.T) {
	e := newEnv(t)

	e.install(e.options())

	sum := strings.Fields(mustRead(t, e.checksums))[0]
	tmpl := "pco-appliance_" + testVersion + "_amd64.tar.zst"
	upid := "UPID:pve1:00000001:00000000:00000000:download:" + tmpl + ":root@pam:"
	require.Equal(t, []string{
		"pveversion",
		"dpkg --print-architecture",
		"mountpoint -q " + e.pveDir,
		"ip -4 -o addr show scope global",
		"pvesh get /cluster/status --output-format json",
		"pvesh get /nodes/pve1/storage --output-format json",
		"pvesh get /nodes/pve1/network --output-format json",
		"pvesh get /access/acl --output-format json",
		"pvesh get /access/users --full 1 --output-format json",
		"pvesh get /access/groups --output-format json",
		"pvesh get /access/roles --output-format json",
		"pvesh get /cluster/resources --type vm --output-format json",
		"pvesh get /cluster/nextid --output-format json",
		"pct status 100",
		"qm status 100",
		"pvesh get /cluster/firewall/options --output-format json",
		// 2. the template, downloaded and checked by Proxmox; the task says how
		// it ended
		"pvesh get /nodes/pve1/storage/local/content --content vztmpl --output-format json",
		"pvesh create /nodes/pve1/storage/local/download-url --content vztmpl --filename " + tmpl +
			" --url https://github.com/anaryk/proxmox-cloudflared-operator/releases/download/v1.2.3/" + tmpl +
			" --checksum " + sum + " --checksum-algorithm sha256",
		"pvesh get /nodes/pve1/tasks/" + upid + "/status --output-format json",
		"pvesh get /nodes/pve1/tasks/" + upid + "/status --output-format json",
		// 3. the pool
		"pveum pool list --output-format json",
		"pveum pool add pco --comment pco appliances",
		// 4. the container
		"pct create 100 local:vztmpl/" + tmpl + " --unprivileged 1 --features nesting=1 --ostype debian --hostname pco" +
			" --cores 1 --memory 768 --swap 0 --rootfs local-zfs:4 --mp0 local-zfs:1,mp=/var/lib/pco,backup=0" +
			" --net0 name=eth0,bridge=vmbr0,ip=dhcp --onboot 1 --startup order=1,up=20 --pool pco" +
			" --tags pco-appliance;pco-combined --description pco appliance vm100, installed 2026-10-01 by pco appliance install",
		// 5. role, user, token
		"pveum role list --output-format json",
		"pveum role add PCO --privs VM.Audit,Sys.Audit,VM.GuestAgent.Audit,SDN.Audit,Pool.Audit",
		"pveum user list --output-format json",
		"pveum user add pco@pve --comment pco operator",
		"pveum acl list --output-format json",
		"pveum acl modify / --users pco@pve --roles PCO",
		"pveum user list --output-format json",
		"pveum user token list pco@pve --output-format json",
		"pveum user token add pco@pve vm100 --privsep 1 --comment pco appliance vm100 --output-format json",
		"pveum acl modify / --tokens pco@pve!vm100 --roles PCO",
		// 6. the gate tags
		"pvesh get /cluster/options --output-format json",
		"pvesh set /cluster/options --registered-tags admin-only;cf-tunnel;cf-tunnel-managed",
		// 7. start, boot, version
		"pct status 100",
		"pct start 100",
		"pct exec 100 --keep-env 0 -- timeout 90 systemctl is-system-running --wait",
		"pct exec 100 --keep-env 0 -- systemctl list-units --state=failed --plain --no-legend --no-pager",
		"pct exec 100 --keep-env 0 -- pco version",
		// 8. the CA, the marker and the bootstrap, then init
		"pct push 100 " + filepath.Join(e.pveDir, "pve-root-ca.pem") + " /var/lib/pco/pve-ca.pem --perms 0644",
		"pct push 100 <run>/volume /var/lib/pco/.volume --perms 0600",
		"pvesh get /nodes/pve1/lxc/100/config --current 1 --output-format json",
		"pct push 100 <run>/bootstrap.json /var/lib/pco/bootstrap.json --perms 0600",
		"pct exec 100 --keep-env 0 -- pco appliance init --bootstrap /var/lib/pco/bootstrap.json",
		// 9. protection last
		"pct set 100 --protection 1",
	}, withRun(e.node.ran, e.runDir))

	ct := e.node.cts[100]
	require.Equal(t, "1", ct.cfg["protection"])
	require.True(t, ct.running)
	require.Equal(t, []int{100}, e.node.pools["pco"].Members)
	require.Equal(t, "pco appliances", e.node.pools["pco"].Comment)
	require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
	require.True(t, e.node.has("/", "user", "pco@pve", "PCO"))
	require.True(t, e.node.has("/", "token", "pco@pve!vm100", "PCO"))
	require.Equal(t, []string{"admin-only", "cf-tunnel", "cf-tunnel-managed"}, e.node.tags)
	require.Equal(t, []string{"100 /var/lib/pco/pve-ca.pem 0644", "100 /var/lib/pco/.volume 0600", "100 /var/lib/pco/bootstrap.json 0600"}, e.node.pushes)
	require.NotContains(t, ct.files, bootstrapFile, "init consumed the bootstrap")

	// The journal directory is empty and nothing is left under /run.
	require.Empty(t, entries(t, e.journals))
	require.Empty(t, entries(t, e.runDir))
	text := e.ask.text()
	for _, want := range []string{
		"pco appliance lxc/100 is installed on pve1, in pool pco",
		"pct exec 100 -- pco status",
		"pct exec 100 -- pco credential add",
		"snapshots, clones and storage replication of lxc/100 carry its secrets",
		"the API at 192.0.2.10:8006 verifies as pve1 against the cluster CA",
	} {
		require.Contains(t, text, want)
	}
}

func TestTheBootstrapIsTheContainers(t *testing.T) {
	e := newEnv(t)
	// An address the node holds only at run time is in no answer of the API;
	// the bootstrap carries it, so that the appliance never publishes it.
	e.node.addrs = append(e.node.addrs, "vmbr1 10.92.0.2/24")

	e.install(e.options())

	b := e.bootstrap(0)
	require.Equal(t, "install", b["mode"])
	require.EqualValues(t, 1, b["schemaVersion"])
	require.EqualValues(t, 100, b["vmid"])
	require.Equal(t, "pve1", b["node"])
	require.Equal(t, []any{"bc:24:11:00:00:64"}, b["macs"])
	require.Equal(t, []any{map[string]any{"address": "192.0.2.10:8006", "serverName": "pve1"}}, b["endpoints"])
	require.Equal(t, []any{"192.0.2.10", "10.92.0.2"}, b["nodeAddrs"])
	require.Equal(t, "cf-tunnel", b["gateTag"])
	require.Equal(t, "", b["cloudflareToken"])
	tok := b["pveToken"].(map[string]any)
	require.Equal(t, "pco@pve!vm100", tok["tokenId"])
	require.Equal(t, pveSecret+"1", tok["secret"])

	var m setup.Manifest
	raw, err := json.Marshal(b["manifest"])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, setup.Manifest{
		Node: "pve1", InstalledAt: m.InstalledAt,
		CreatedRole: true, CreatedUser: true, CreatedToken: true, GrantedACL: true,
		RegisteredTags: []string{"cf-tunnel", "cf-tunnel-managed"},
		Appliance: &setup.ApplianceManifest{
			VMID: 100, Node: "pve1", Token: "pco@pve!vm100", Pool: "pco", CreatedPool: true,
			Template: "local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst",
		},
	}, m)
	require.Equal(t, t0, m.InstalledAt.UTC())
}

// A gate tag Proxmox or the store would refuse fails before anything is made,
// not at step tags or in init after the container was.
func TestAGateTagIsCheckedFirst(t *testing.T) {
	const want = `--gate-tag "Cf Tunnel": want lower-case letters, digits and the characters _ - + ., the first one not - + or a dot`
	e := newEnv(t)
	o := e.options()
	o.GateTag = "Cf Tunnel"

	require.EqualError(t, e.in.Install(t.Context(), o), want)
	require.Empty(t, e.node.ran)
	e.nothingMade()

	e = installed(t)
	require.EqualError(t, e.in.Repair(t.Context(), 100, Options{Yes: true, GateTag: "Cf Tunnel"}), want)
	require.Empty(t, e.node.ran)
}

// --api-ca by a relative path is kept by its absolute one: a resumed run, a
// repair, may run from another directory.
func TestTheAPICAIsKeptByItsAbsolutePath(t *testing.T) {
	e := newEnv(t)
	e.presented = e.certs.custom
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "custom-ca.pem"), e.certs.customCA, 0o644))
	t.Chdir(dir)
	o := e.options()
	o.APICA = "custom-ca.pem"
	e.node.killAt = "pveum acl modify / --tokens pco@pve!vm100"
	require.PanicsWithValue(t, errKilled, func() { _ = e.in.Install(t.Context(), o) })
	t.Chdir(t.TempDir())

	require.NoError(t, e.in.Install(t.Context(), Options{Resume: e.journal(), Yes: true}), e.ask.text())

	require.Contains(t, e.node.ran, "pct push 100 "+filepath.Join(dir, "custom-ca.pem")+" /var/lib/pco/pve-ca.pem --perms 0644")
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// withRun writes the run's directory under /run as <run>.
func withRun(lines []string, runDir string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		for _, f := range strings.Fields(l) {
			if strings.HasPrefix(f, runDir+"/pco-appliance-install-") {
				l = strings.Replace(l, filepath.Dir(f), "<run>", 1)
			}
		}
		out[i] = l
	}
	return out
}
