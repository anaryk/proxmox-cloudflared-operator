package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/anaryk/proxmox-cloudflared-operator/internal/applianceinstall"
)

// These tests stop before the installer reaches the node: none of them may
// run a command on the machine that runs them.

func TestApplianceInstallTakesItsOptionsFromTheFlags(t *testing.T) {
	a, cmd, _, errOut := commandWith(unreadable{t}, false)
	a.getenv = func(name string) string {
		if name == cloudflareURLVar {
			return "http://127.0.0.1:8787/client/v4"
		}
		return ""
	}
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("cf-token-from-file\n"), 0o600))
	f := installFlags{o: applianceinstall.Options{Yes: true, Storage: "local-zfs", VMID: 120}, tokenFile: token, noTags: true, keepTmpl: true}

	in, o, err := a.nodeInstaller(cmd, &f)

	require.NoError(t, err)
	require.NotNil(t, in)
	no := false
	require.Equal(t, applianceinstall.Options{
		Yes: true, Storage: "local-zfs", VMID: 120, KeepTemplate: true, RegisterTags: &no,
		CloudflareToken: "cf-token-from-file", CloudflareAPI: "http://127.0.0.1:8787/client/v4",
	}, o)
	require.Contains(t, errOut.String(), "warning: the Cloudflare API is overridden to http://127.0.0.1:8787/client/v4")
}

// The Cloudflare API of tests is checked as pco setup checks it, before
// anything else.
func TestApplianceNodeCommandsRefuseAnInvalidCloudflareAPI(t *testing.T) {
	for _, args := range [][]string{
		{"appliance", "install", "--yes"},
		{"appliance", "repair", "--vmid", "120", "--yes"},
		{"appliance", "uninstall", "--vmid", "120", "--yes"},
	} {
		t.Run(args[1], func(t *testing.T) {
			r := newRunner(t, filepath.Join(t.TempDir(), "pco", "pco.sock"))
			r.env.getenv = func(name string) string {
				if name == cloudflareURLVar {
					return "http://192.0.2.1:8787/client/v4"
				}
				return ""
			}

			res := r.runReader(unreadable{t}, args...)

			require.ErrorIs(t, res.err, errOverride)
		})
	}
}

func TestApplianceNodeCommandsRefuseInsideTheAppliance(t *testing.T) {
	for _, args := range [][]string{
		{"appliance", "install", "--yes"},
		{"appliance", "repair", "--vmid", "120", "--yes"},
		{"appliance", "uninstall", "--vmid", "120", "--yes"},
		{"appliance", "grant-network", "--vmid", "120", "--bridge", "vmbr1", "--yes"},
		{"appliance", "revoke-network", "--vmid", "120", "--bridge", "vmbr1", "--yes"},
	} {
		t.Run(args[1], func(t *testing.T) {
			r := newRunner(t, filepath.Join(t.TempDir(), "pco", "pco.sock"))
			r.env.profileFile = filepath.Join(t.TempDir(), "profile")
			require.NoError(t, os.WriteFile(r.env.profileFile, []byte("appliance\n"), 0o600))

			res := r.runReader(unreadable{t}, args...)

			require.EqualError(t, res.err, "pco appliance "+args[1]+" runs on the Proxmox VE node, not inside the appliance")
		})
	}
}

func TestApplianceCommandsOfTheInstallerRefuseOnTheHost(t *testing.T) {
	for _, args := range [][]string{
		{"appliance", "purge"},
		{"appliance", "manifest", "add", "--zone", "localnetwork", "--vnet", "vmbr1"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			r := newRunner(t, filepath.Join(t.TempDir(), "pco", "pco.sock"))

			res := r.runReader(unreadable{t}, args...)

			require.ErrorContains(t, res.err, "runs inside the appliance")
		})
	}
}

// Every command of the installer says what it does and shows how it is
// used; those it runs inside the appliance itself are hidden.
func TestApplianceInstallerCommandsAreDocumented(t *testing.T) {
	root := newRootCmdWith(testEnv())
	appliance, _, err := root.Find([]string{"appliance"})
	require.NoError(t, err)
	byName := map[string]*cobra.Command{}
	for _, c := range appliance.Commands() {
		byName[c.Name()] = c
	}
	for _, name := range []string{"install", "repair", "uninstall", "grant-network", "revoke-network"} {
		c := byName[name]
		require.NotNil(t, c, name)
		require.False(t, c.Hidden, name)
		require.NotEmpty(t, c.Long, name)
		require.NotEmpty(t, c.Example, name)
		lines := strings.Split(c.Example, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "pco ") {
				require.True(t, strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#"), "%s: %q has no comment above it", name, line)
				require.Contains(t, line, "pco appliance "+name)
			}
		}
	}
	for _, name := range []string{"purge", "manifest"} {
		require.True(t, byName[name].Hidden, name)
	}
}
