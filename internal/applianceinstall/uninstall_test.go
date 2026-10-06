package applianceinstall

import (
	"context"
	"encoding/json"
	"errors"
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
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := installed(t)
			e.node.volumes["local:vztmpl/debian-13-standard_13.1-1_amd64.tar.zst"] = "/var/lib/vz/template/cache/debian-13-standard_13.1-1_amd64.tar.zst"
			e.node.addToken("root@pam", "pco", "", false)
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

// A restore uninstalled while the appliance it was made from stays: its
// token, user, role and pool stay too.
func TestUninstallOfARestoreLeavesTheOriginal(t *testing.T) {
	e := installed(t)
	e.restored(101, true)
	require.NoError(t, e.in.Repair(t.Context(), 101, Options{Yes: true, Recover: true, CloudflareToken: cfToken}))
	require.NoError(t, e.in.Uninstall(t.Context(), 101, uninstallOptions()), e.ask.text())

	require.Nil(t, e.node.cts[101])
	require.NotNil(t, e.node.cts[100])
	require.Equal(t, []string{"vm100"}, e.node.tokenNames("pco@pve"))
	require.NotNil(t, e.node.roles["PCO"])
	require.Equal(t, []int{100}, e.node.pools["pco"].Members)
	require.True(t, e.node.has("/", "token", "pco@pve!vm100", "PCO"))
}

func TestUninstallOfAContainerThatIsGone(t *testing.T) {
	e := installed(t)
	e.node.cts[100].cfg["protection"] = "0"
	e.node.cts[100].running = false
	delete(e.node.cts, 100)
	e.node.pools["pco"].Members = nil

	require.NoError(t, e.in.Uninstall(t.Context(), 100, uninstallOptions()), e.ask.text())

	require.Empty(t, e.node.users[1:], "the marked user goes")
	require.Equal(t, []string{"admin-only", "cf-tunnel", "cf-tunnel-managed"}, e.node.tags, "without the manifest, no tag is the installer's")
	require.False(t, slices.ContainsFunc(e.node.ran, func(l string) bool { return strings.HasPrefix(l, "pct ") && !strings.HasPrefix(l, "pct status") }))
}

func writeHostInstall(t *testing.T, e *testEnv) {
	t.Helper()
	require.NoError(t, mkdirAll(e.pveDir+"/pco/meta"))
	require.NoError(t, writeFile(e.pveDir+"/pco/meta/install.json", `{"id":"0123456789ab"}`))
}
