package applianceinstall

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/setup"
)

func uninstallOptions() UninstallOptions {
	return UninstallOptions{Yes: true, KeepCloudflare: true, KeepTemplate: true}
}

func TestUninstallRemovesWhatTheInstallerMade(t *testing.T) {
	e := installed(t)

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Equal(t, []string{
		"pvesh get /nodes/pve1/lxc/100/config --current 1 --output-format json",
		"pct status 100",
		"pct pull 100 /var/lib/pco/manifest.json <run>/manifest.json",
		"pveum user list --output-format json",
		"pveum user token list pco@pve --output-format json",
		"pveum role list --output-format json",
		"pveum acl list --output-format json",
		"pveum pool list --output-format json",
		"pvesh get /pools --poolid pco --output-format json",
		"pvesh get /cluster/options --output-format json",
		"pvesh get /nodes/pve1/storage --output-format json",
		"pvesh get /nodes/pve1/storage/local/content --content vztmpl --output-format json",
		"pct set 100 --protection 0",
		"pct stop 100",
		"pct destroy 100 --purge 1",
		// The token before its user, which would leave its secret behind.
		"pveum user token remove pco@pve vm100",
		"pveum acl list --output-format json",
		"pveum user delete pco@pve",
		"pveum role delete PCO",
		"pveum pool delete pco",
		"pvesh get /cluster/options --output-format json",
		"pvesh set /cluster/options --registered-tags admin-only",
	}, withRun(e.node.ran, e.runDir))
	e.takenBack()
	require.Contains(t, e.node.volumes, "local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst", "kept by default")
	require.Contains(t, e.ask.text(), "(the template local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst stays; --keep-template=false removes it)")
}

func TestUninstallKeepsTheUserWhileItsTokenIsThere(t *testing.T) {
	e := installed(t)
	e.node.on("pveum user token remove", func(context.Context, []string) (string, error) {
		return "", errors.New("the cluster filesystem is busy")
	})

	err := e.in.Uninstall(t.Context(), 100, uninstallOptions())

	require.ErrorContains(t, err, "user pco@pve and role PCO are kept until it is gone")
	require.ErrorContains(t, err, "run it again to finish the rest")
	require.Empty(t, e.node.leaked)
	require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
	require.NotNil(t, e.node.roles["PCO"])

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())
	e.takenBack()
}

func TestUninstallRemovesTheTemplateWhenTold(t *testing.T) {
	e := installed(t)
	o := uninstallOptions()
	o.KeepTemplate = false

	require.NoError(t, e.in.Uninstall(t.Context(), 100, o), e.ask.text())

	require.Equal(t, "pvesm free local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst", e.node.ran[len(e.node.ran)-1])
	require.Empty(t, e.node.volumes)
}

// --keep-template=false frees the template the installer downloaded for the
// appliance, which its manifest names, and no other of pco: one kept for a
// rebuild, or one the install found on the storage, is the admin's.
func TestUninstallFreesOnlyTheTemplateItDownloaded(t *testing.T) {
	e := installed(t)
	older := "local:vztmpl/pco-appliance_1.1.0_amd64.tar.zst"
	e.node.volumes[older] = "/var/lib/vz/template/cache/pco-appliance_1.1.0_amd64.tar.zst"
	o := uninstallOptions()
	o.KeepTemplate = false

	require.NoError(t, e.in.Uninstall(t.Context(), 100, o), e.ask.text())

	require.Equal(t, 1, e.node.count("pvesm free"))
	require.Contains(t, e.node.ran, "pvesm free local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst")
	require.Equal(t, []string{older}, slices.Collect(maps.Keys(e.node.volumes)))
	require.Contains(t, e.ask.text(), "(kept: the template "+older+", which the manifest of lxc/100 does not name as the installer's)")

	e = newEnv(t)
	file := filepath.Join(t.TempDir(), "pco-appliance_1.2.3_amd64.tar.zst")
	require.NoError(t, os.WriteFile(file, e.node.urls[ReleaseBase(testVersion)+"/pco-appliance_1.2.3_amd64.tar.zst"], 0o644))
	e.node.volumes["local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst"] = file
	e.install(e.options())

	require.NoError(t, e.in.Uninstall(t.Context(), 100, o), e.ask.text())

	require.Equal(t, 0, e.node.count("pvesm free"))
	require.Contains(t, e.node.volumes, "local:vztmpl/pco-appliance_1.2.3_amd64.tar.zst")
}

func TestUninstallKeepsWhatAnotherInstallUses(t *testing.T) {
	e := installed(t)
	// The host install's token, which needs user and role.
	e.node.addToken("pco@pve", "pco", "", false)

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Empty(t, e.node.cts)
	require.Equal(t, []string{"pco"}, e.node.tokenNames("pco@pve"))
	require.NotNil(t, e.node.roles["PCO"])
	require.True(t, e.node.has("/", "user", "pco@pve", "PCO"))
	require.False(t, e.node.has("/", "token", "pco@pve!vm100", "PCO"))
	require.Contains(t, e.ask.text(), "(kept: user pco@pve and role PCO, which pco@pve!pco use)")
	require.Empty(t, e.node.pools, "the pool is empty")
}

func TestUninstallKeepsTheTagsOfAHostInstall(t *testing.T) {
	e := installed(t)
	e.node.addToken("pco@pve", "pco", "", false)
	writeHostInstall(t, e)

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Equal(t, []string{"admin-only", "cf-tunnel", "cf-tunnel-managed"}, e.node.tags)
	require.Contains(t, e.ask.text(), "which the host install of pco on this node uses")
}

// The manifest comes out of the appliance and is not trusted: only the
// shapes the installer writes count, and an object goes only when Proxmox
// shows pco's mark on it.
func TestUninstallTrustsNoManifest(t *testing.T) {
	for _, tt := range []struct {
		name    string
		change  func(m map[string]any)
		warning string
	}{
		{"a token of root@pam", func(m map[string]any) { m["appliance"].(map[string]any)["token"] = "root@pam!pco" },
			`token "root@pam!pco" is not the token of lxc/100`},
		{"another VMID", func(m map[string]any) { m["appliance"].(map[string]any)["vmid"] = 9999 },
			"it is the manifest of lxc/9999, not of lxc/100"},
		{"a foreign volume", func(m map[string]any) {
			m["appliance"].(map[string]any)["template"] = "local:vztmpl/debian-13-standard_13.1-1_amd64.tar.zst"
		},
			`volume "local:vztmpl/debian-13-standard_13.1-1_amd64.tar.zst" is not a template of pco`},
		{"a tag of the admin", func(m map[string]any) { m["registeredTags"] = []any{"cf-tunnel", "admin only"} },
			`tag "admin only" is no tag`},
		{"a file on the node", func(m map[string]any) { m["webTLS"] = []any{"/etc/ssl/private/key.pem"} },
			"it names packages, files or units on the node"},
		{"a field no manifest has", func(m map[string]any) { m["users"] = []any{"root@pam"} },
			`it holds "users", which no manifest has`},
		{"more than 64 KiB", func(m map[string]any) { m["node"] = strings.Repeat("a", 70<<10) },
			"it is larger than 64 KiB"},
		{"a NoAccess line for root@pam", func(m map[string]any) {
			m["appliance"].(map[string]any)["noAccess"] = []any{map[string]any{"principal": "root@pam", "path": "/", "role": "NoAccess"}}
		}, `a NoAccess line: principal "root@pam" is never one the installer denies`},
		{"a line of another role", func(m map[string]any) {
			m["appliance"].(map[string]any)["noAccess"] = []any{map[string]any{"principal": "root@pam!pco", "path": "/", "role": "Administrator"}}
		}, `a NoAccess line: role "Administrator" on /: the installer adds NoAccess only`},
		{"a NoAccess line on another path", func(m map[string]any) {
			m["appliance"].(map[string]any)["noAccess"] = []any{map[string]any{"principal": "root@pam!pco", "path": "/storage/local", "role": "NoAccess"}}
		}, `a NoAccess line: path "/storage/local": the installer adds NoAccess on /, /vms, /pool, /pool/pco and /vms/100 only`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := installed(t)
			e.node.volumes["local:vztmpl/debian-13-standard_13.1-1_amd64.tar.zst"] = "/var/lib/vz/template/cache/debian-13-standard_13.1-1_amd64.tar.zst"
			e.node.addToken("root@pam", "pco", "", false)
			e.node.grant("/", "user", "root@pam", "NoAccess")
			e.node.grant("/", "token", "root@pam!pco", "Administrator")
			e.node.grant("/storage/local", "token", "root@pam!pco", "NoAccess")
			ct := e.node.cts[100]
			var m map[string]any
			require.NoError(t, json.Unmarshal(ct.files[manifestFile].data, &m))
			tt.change(m)
			data, err := json.Marshal(m)
			require.NoError(t, err)
			ct.files[manifestFile] = fakeFile{data: data, perms: "0600"}
			o := uninstallOptions()
			o.KeepTemplate = false

			require.NoError(t, e.in.Uninstall(t.Context(), 100, o), e.ask.text())

			require.Contains(t, e.ask.text(), tt.warning)
			for _, line := range withRun(e.node.ran, e.runDir) {
				require.NotContains(t, line, "root@pam", "nothing of root@pam is touched")
				require.NotContains(t, line, "debian-13", "nothing of the admin's volumes is touched")
			}
			require.Equal(t, []string{"pco"}, e.node.tokenNames("root@pam"))
			require.True(t, e.node.has("/", "user", "root@pam", "NoAccess"))
			require.True(t, e.node.has("/", "token", "root@pam!pco", "Administrator"))
			require.True(t, e.node.has("/storage/local", "token", "root@pam!pco", "NoAccess"))
			require.Contains(t, e.node.volumes, "local:vztmpl/debian-13-standard_13.1-1_amd64.tar.zst")
			require.Empty(t, e.node.cts)
			require.Equal(t, []string{"root@pam"}, e.node.userIDs())
		})
	}
}

func TestUninstallRefusesAContainerWithoutTheMark(t *testing.T) {
	e := installed(t)
	e.node.cts[100].cfg["description"] = "a guest of an admin\n"

	err := e.in.Uninstall(t.Context(), 100, uninstallOptions())

	require.ErrorContains(t, err, "lxc/100 is not a pco appliance (its description lacks the mark of the installer): nothing was removed")
	require.Equal(t, 0, e.node.count("pct destroy"))
	require.Equal(t, 0, e.node.count("pveum user token remove"))
}

// What the install has at Cloudflare is asked about as pco uninstall asks:
// --yes alone never decides it while there is something there.
func TestUninstallAsksAboutCloudflare(t *testing.T) {
	objects := []string{"DNS record CNAME app.example.com in zone example.com", "tunnel pco-0123456789ab (0000) in account acc1"}
	t.Run("--yes alone", func(t *testing.T) {
		e := installed(t)
		e.node.cfObjects = objects

		err := e.in.Uninstall(t.Context(), 100, UninstallOptions{Yes: true, KeepTemplate: true})

		require.ErrorContains(t, err, "the install of lxc/100 has DNS records or tunnels at Cloudflare, and the uninstall removes the "+
			"credentials that reach them: say what becomes of them with --purge-cloudflare, which deletes them, or --keep-cloudflare")
		require.NotNil(t, e.node.cts[100])
	})
	t.Run("--yes with nothing there", func(t *testing.T) {
		e := installed(t)

		require.NoError(t, e.in.Uninstall(t.Context(), 100, UninstallOptions{Yes: true, KeepTemplate: true}))
		require.Empty(t, e.node.purges)
	})
	t.Run("--purge-cloudflare, forwarding the API of tests", func(t *testing.T) {
		e := installed(t)
		e.node.cfObjects = objects

		require.NoError(t, e.in.Uninstall(t.Context(), 100, UninstallOptions{Yes: true, PurgeCloudflare: true, KeepTemplate: true,
			CloudflareAPI: "http://127.0.0.1:8787/client/v4"}))

		require.Equal(t, []string{"100 PCO_CLOUDFLARE_API_URL=http://127.0.0.1:8787/client/v4"}, e.node.purges)
		purge := indexOf(t, e.node.ran, "pct exec 100 --keep-env 0 -- env PCO_CLOUDFLARE_API_URL=http://127.0.0.1:8787/client/v4 pco appliance purge")
		require.Less(t, purge, indexOf(t, e.node.ran, "pct destroy 100 --purge 1"), "through the container, before it goes")
	})
	t.Run("at the question", func(t *testing.T) {
		e := installed(t)
		e.node.cfObjects = objects
		e.ask.answers = []answer{{"Remove the appliance lxc/100", true}, {"Delete these at Cloudflare as well?", true}}

		require.NoError(t, e.in.Uninstall(t.Context(), 100, UninstallOptions{KeepTemplate: true}))

		require.Len(t, e.node.purges, 1)
		require.Contains(t, e.ask.text(), "DNS record CNAME app.example.com in zone example.com")
	})
	t.Run("at the question, declined", func(t *testing.T) {
		e := installed(t)
		e.node.cfObjects = objects
		e.ask.answers = []answer{{"Remove the appliance lxc/100", true}, {"Delete these at Cloudflare as well?", false}}

		require.NoError(t, e.in.Uninstall(t.Context(), 100, UninstallOptions{KeepTemplate: true}))

		require.Empty(t, e.node.purges)
		require.Empty(t, e.node.cts)
	})
	t.Run("not removed at the question", func(t *testing.T) {
		e := installed(t)
		e.ask.answers = []answer{{"Remove the appliance lxc/100", false}}

		require.ErrorIs(t, e.in.Uninstall(t.Context(), 100, UninstallOptions{KeepTemplate: true}), setup.ErrAborted)
		require.NotNil(t, e.node.cts[100])
	})
	t.Run("a stopped container with --yes", func(t *testing.T) {
		e := installed(t)
		e.node.cts[100].running = false

		err := e.in.Uninstall(t.Context(), 100, UninstallOptions{Yes: true, KeepTemplate: true})

		require.ErrorContains(t, err, "lxc/100 is stopped, so what its install has at Cloudflare cannot be looked at")
		require.NotNil(t, e.node.cts[100])
	})
	t.Run("a stopped container with --purge-cloudflare", func(t *testing.T) {
		e := installed(t)
		e.node.cts[100].running = false

		err := e.in.Uninstall(t.Context(), 100, UninstallOptions{Yes: true, PurgeCloudflare: true, KeepTemplate: true})

		require.ErrorContains(t, err, "--purge-cloudflare needs lxc/100 running, as its credentials are in it")
	})
}

// A copy removed while the appliance it was made from stays: its token, user,
// role and pool stay too, and what is at Cloudflare, which is the original's.
func TestUninstallOfACopyLeavesTheOriginal(t *testing.T) {
	e := installed(t)
	e.cloned()
	e.node.cts[121].running = true
	e.node.cfObjects = []string{"tunnel pco-0123456789ab (0000) in account acc1"}

	require.NoError(t, e.in.Uninstall(t.Context(), 121, UninstallOptions{Yes: true, KeepTemplate: true}), e.ask.text())

	require.Contains(t, e.ask.text(), "(it is a copy of lxc/100, which is still there, on node pve1: what is at Cloudflare is that one's, and stays)")
	require.Nil(t, e.node.cts[121])
	require.NotNil(t, e.node.cts[100])
	require.Equal(t, 0, e.node.count("pct exec 121 --keep-env 0 -- pco appliance purge"), "nothing is asked of Cloudflare through the copy")
	require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
	require.NotNil(t, e.node.roles["PCO"])
	require.Equal(t, []int{100}, e.node.pools["pco"].Members)
	require.True(t, e.node.has("/", "token", "pco@pve!vm100", "PCO"))

	e = installed(t)
	e.cloned()
	e.node.cts[121].running = true

	err := e.in.Uninstall(t.Context(), 121, UninstallOptions{Yes: true, PurgeCloudflare: true, KeepTemplate: true})

	require.EqualError(t, err, "lxc/121 is a copy of lxc/100, which is still there: what is at Cloudflare is the install of "+
		"lxc/100, which --purge-cloudflare would delete under it; remove the copy without it; nothing was removed")
	require.NotNil(t, e.node.cts[121])
	require.Empty(t, e.node.purges)
}

func TestUninstallOfAContainerThatIsGone(t *testing.T) {
	e := installed(t)
	e.node.cts[100].cfg["protection"] = "0"
	e.node.cts[100].running = false
	delete(e.node.cts, 100)
	e.node.pools["pco"].Members = nil

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Contains(t, e.node.ran, "pvesh get /cluster/resources --type vm --output-format json", "no node of the cluster has it")
	require.Empty(t, e.node.users[1:], "the marked user goes")
	require.Equal(t, []string{"admin-only", "cf-tunnel", "cf-tunnel-managed"}, e.node.tags, "without the manifest, no tag is the installer's")
	require.False(t, slices.ContainsFunc(e.node.ran, func(l string) bool { return strings.HasPrefix(l, "pct ") && !strings.HasPrefix(l, "pct status") }))
}

// migrated moves the container vmid to another node of the cluster, as pct
// migrate does: this node no longer has its configuration, the cluster still
// lists it.
func (e *testEnv) migrated(vmid int, node string) {
	e.t.Helper()
	require.NotNil(e.t, e.node.cts[vmid])
	delete(e.node.cts, vmid)
	e.node.elsewhere[vmid] = node
}

// Only a container that is in no node's list is gone: on another node, the
// uninstall would take the token of an appliance that runs.
func TestUninstallOfAContainerOnAnotherNodeIsRefused(t *testing.T) {
	e := installed(t)
	e.migrated(100, "pve2")

	err := e.in.Uninstall(t.Context(), 100, uninstallOptions())

	require.EqualError(t, err, "lxc/100 is on node pve2, not on pve1: run the uninstall there; nothing was removed")
	require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
	require.Equal(t, 0, e.node.count("pveum user token remove"))
	require.Equal(t, 0, e.node.count("pveum acl delete"))
	require.Empty(t, entries(t, e.journals))
}

// deniedTwice is a node with appliance 100 installed with --deny-access: ops@pve
// holds Permissions.Modify on /, vmops@pve PVEVMUser on the appliance, and
// the installer added NoAccess for each.
func deniedTwice(t *testing.T) *testEnv {
	t.Helper()
	e := newEnv(t)
	e.node.roles["PermAdmin"] = []string{"Permissions.Modify", "Sys.Audit"}
	e.node.users = append(e.node.users, &fakeUser{ID: "ops@pve", Enabled: true}, &fakeUser{ID: "vmops@pve", Enabled: true})
	e.node.grant("/", "user", "ops@pve", "PermAdmin")
	e.node.grant("/vms/100", "user", "vmops@pve", "PVEVMUser")
	o := e.options()
	o.DenyAccess = true
	e.install(o)
	require.True(t, e.node.has("/", "user", "ops@pve", "NoAccess"))
	require.True(t, e.node.has("/vms/100", "user", "vmops@pve", "NoAccess"))
	e.node.ran = nil
	return e
}

// The NoAccess lines the installer added are its own: uninstall takes back
// those the manifest names while they are as it made them, and says why it
// leaves the others.
func TestUninstallTakesBackTheNoAccessLines(t *testing.T) {
	t.Run("as the installer made them", func(t *testing.T) {
		e := deniedTwice(t)

		require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

		require.Contains(t, e.ask.text(), "info:   the line NoAccess for ops@pve on /, which the installer added")
		require.Contains(t, e.node.ran, "pveum acl delete / --users ops@pve --roles NoAccess")
		require.False(t, e.node.has("/", "user", "ops@pve", "NoAccess"))
		require.False(t, e.node.has("/vms/100", "user", "vmops@pve", "NoAccess"), "it went with the container")
		require.Equal(t, 0, e.node.count("pveum acl delete /vms/100"))
		require.True(t, e.node.has("/", "user", "ops@pve", "PermAdmin"), "the admin's grant stays")
	})
	for _, tt := range []struct {
		name   string
		change func(e *testEnv)
		says   string
	}{
		{"one an admin changed", func(e *testEnv) {
			i := slices.Index(e.node.acl, fakeACL{Path: "/", Type: "user", UGID: "ops@pve", Role: "NoAccess"})
			e.node.acl[i].NoPropagate = true
		}, "(kept: NoAccess for ops@pve on /, which no longer reaches below / as the installer made it)"},
		{"one that is gone", func(e *testEnv) {
			e.node.acl = slices.DeleteFunc(e.node.acl, func(a fakeACL) bool { return a.UGID == "ops@pve" && a.Role == "NoAccess" })
		}, "(NoAccess for ops@pve on /, which the installer added, is not there any more)"},
		{"while another appliance is there", func(e *testEnv) {
			e.node.addToken("pco@pve", "vm200", "pco appliance vm200", true)
		}, "(kept: NoAccess for ops@pve on /, which keeps ops@pve out of the appliance lxc/200 as well)"},
		{"a description without the record of the lines", func(e *testEnv) {
			e.node.cts[100].cfg["description"] = description(100, t0, nil)
		}, "(kept: NoAccess for ops@pve on /, which nothing of lxc/100 names as added by the installer: " +
			"if an install added it, pveum acl delete / --users ops@pve --roles NoAccess takes it back)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := deniedTwice(t)
			tt.change(e)

			require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

			require.Contains(t, e.ask.text(), tt.says)
			require.Equal(t, 0, e.node.count("pveum acl delete / --users ops@pve"))
			require.Nil(t, e.node.cts[100])
		})
	}
}

// The description of the container is the node's note of the lines the
// installer added.
func TestTheDescriptionOfTheContainerRecordsTheNoAccessLines(t *testing.T) {
	e := deniedTwice(t)

	require.Equal(t, "pco appliance vm100, installed 2026-10-01 by pco appliance install\n"+
		"NoAccess for ops@pve on /\n"+
		"NoAccess for vmops@pve on /vms/100\n", e.node.cts[100].cfg["description"])
}

func TestParseDescription(t *testing.T) {
	good := "pco appliance vm100, installed 2026-10-01 by pco appliance install"
	vmid, lines, ok := parseDescription(good + "\nNoAccess for ops@pve on /\nNoAccess for ops@pve!ci on /vms/100\n")
	require.True(t, ok)
	require.Equal(t, 100, vmid)
	require.Equal(t, []setup.NoAccessLine{
		{Principal: "ops@pve", Path: "/", Role: "NoAccess"},
		{Principal: "ops@pve!ci", Path: "/vms/100", Role: "NoAccess"},
	}, lines)

	for name, desc := range map[string]string{
		"a note of the admin":    good + "\nbacked up on Fridays",
		"a line of another path": good + "\nNoAccess for ops@pve on /storage/local",
		"root@pam":               good + "\nNoAccess for root@pam on /",
		"pco@pve":                good + "\nNoAccess for pco@pve!vm100 on /",
		"the path of another VM": good + "\nNoAccess for ops@pve on /vms/101",
		"no mark":                "NoAccess for ops@pve on /",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, ok := parseDescription(desc)
			require.False(t, ok)
		})
	}
}

// A stopped container, whose manifest cannot be read, has the node's note: the
// lines it names are taken back with it.
func TestUninstallOfAStoppedContainerTakesBackTheLinesItsDescriptionNames(t *testing.T) {
	e := deniedTwice(t)
	e.node.cts[100].running = false

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Contains(t, e.ask.text(), "info:   the line NoAccess for ops@pve on /, which the installer added")
	require.False(t, e.node.has("/", "user", "ops@pve", "NoAccess"))
	require.Nil(t, e.node.cts[100])
}

// The manifest comes out of the appliance: a line it names that the
// description does not is not the installer's, and the admin's own line stays;
// a line the description names that the manifest does not stays too, as the
// two disagree.
func TestUninstallTakesBackOnlyTheLinesTheDescriptionAndTheManifestName(t *testing.T) {
	e := deniedTwice(t)
	e.node.users = append(e.node.users, &fakeUser{ID: "audit@pve", Enabled: true})
	e.node.grant("/", "user", "audit@pve", "NoAccess")
	ct := e.node.cts[100]
	var m map[string]any
	require.NoError(t, json.Unmarshal(ct.files[manifestFile].data, &m))
	m["appliance"].(map[string]any)["noAccess"] = []any{
		map[string]any{"principal": "audit@pve", "path": "/", "role": "NoAccess"},
		map[string]any{"principal": "vmops@pve", "path": "/vms/100", "role": "NoAccess"},
	}
	data, err := json.Marshal(m)
	require.NoError(t, err)
	ct.files[manifestFile] = fakeFile{data: data, perms: "0600"}

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.True(t, e.node.has("/", "user", "audit@pve", "NoAccess"), "the admin's line stays")
	require.True(t, e.node.has("/", "user", "ops@pve", "NoAccess"), "the manifest does not name it")
	require.Equal(t, 0, e.node.count("pveum acl delete / --users"))
	require.Contains(t, e.ask.text(), "(kept: NoAccess for ops@pve on /, which the description of lxc/100 names and its manifest does not)")
	require.Contains(t, e.ask.text(), "(kept: NoAccess for audit@pve on /, which nothing of lxc/100 names as added by the installer: "+
		"if an install added it, pveum acl delete / --users audit@pve --roles NoAccess takes it back)")
	require.Nil(t, e.node.cts[100])
}

// A restore keeps the lines above its container in its description, and
// drops the one on the container it was made from.
func TestARestoreKeepsTheLinesAboveItInItsDescription(t *testing.T) {
	e := deniedTwice(t)
	e.restored(101, true)
	e.lose()

	require.NoError(t, e.in.Repair(t.Context(), 101, Options{Yes: true, Recover: true, CloudflareToken: cfToken}), e.ask.text())

	require.Equal(t, "pco appliance vm101, installed 2026-10-01 by pco appliance install\nNoAccess for ops@pve on /",
		e.node.cts[101].cfg["description"])
}

// A run taken back takes back its NoAccess lines too.
func TestAFailureTakesBackTheNoAccessLines(t *testing.T) {
	e := newEnv(t)
	e.node.roles["PermAdmin"] = []string{"Permissions.Modify", "Sys.Audit"}
	e.node.users = append(e.node.users, &fakeUser{ID: "ops@pve", Enabled: true})
	e.node.grant("/", "user", "ops@pve", "PermAdmin")
	e.node.on("pct start 100", func(context.Context, []string) (string, error) { return "", errors.New("it broke") })
	o := e.options()
	o.DenyAccess = true

	err := e.in.Install(t.Context(), o)

	require.ErrorContains(t, err, "it broke; what the run made is taken back")
	require.False(t, e.node.has("/", "user", "ops@pve", "NoAccess"))
	require.True(t, e.node.has("/", "user", "ops@pve", "PermAdmin"))
	require.Empty(t, e.node.cts)
	require.Empty(t, entries(t, e.journals))
}

func writeHostInstall(t *testing.T, e *testEnv) {
	t.Helper()
	require.NoError(t, mkdirAll(e.pveDir+"/pco/meta"))
	require.NoError(t, writeFile(e.pveDir+"/pco/meta/install.json", `{"id":"0123456789ab"}`))
}
