package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// unit is a systemd unit file: the values of each key, by section.
type unit map[string]map[string][]string

// parseUnit reads the sections, the key=value lines and the comments of a unit
// file, which is all the unit of pco has.
func parseUnit(t *testing.T, path string) unit {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	u := unit{}
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			u[section] = map[string][]string{}
		default:
			key, value, ok := strings.Cut(line, "=")
			require.True(t, ok, "not a key=value line: %q", line)
			require.NotEmpty(t, section, "a setting outside of a section: %q", line)
			u[section][key] = append(u[section][key], value)
		}
	}
	require.NoError(t, sc.Err())
	return u
}

func (u unit) one(t *testing.T, section, key string) string {
	t.Helper()
	values := u[section][key]
	require.Len(t, values, 1, "[%s] %s", section, key)
	return values[0]
}

func TestTheUnitOfTheDaemon(t *testing.T) {
	u := parseUnit(t, filepath.Join("..", "..", "packaging", "systemd", "pco.service"))

	require.Equal(t, "notify", u.one(t, "Service", "Type"), "the daemon sends READY=1")
	require.Equal(t, "/usr/bin/pco daemon", u.one(t, "Service", "ExecStart"))
	require.Equal(t, "pco", u.one(t, "Service", "RuntimeDirectory"), "the directory of the socket")
	require.Equal(t, "0750", u.one(t, "Service", "RuntimeDirectoryMode"))
	require.Equal(t, "pco", u.one(t, "Service", "StateDirectory"), "the local state directory")
	require.Equal(t, "0700", u.one(t, "Service", "StateDirectoryMode"))
	require.Equal(t, "150", u.one(t, "Service", "TimeoutStartSec"), "longer than the 2 minutes the daemon waits for the cluster filesystem at boot")
	require.Equal(t, "always", u.one(t, "Service", "Restart"))
	require.Equal(t, "0", u.one(t, "Service", "LimitCORE"), "a core dump of the daemon would hold the tokens")
	require.Equal(t, "invisible", u.one(t, "Service", "ProtectProc"))
	require.Equal(t, "multi-user.target", u.one(t, "Install", "WantedBy"))
}

// The daemon starts after the cluster filesystem and Proxmox and does not
// order itself before the start of the guests, those managed by HA included:
// they would wait for its start, up to TimeoutStartSec, and its first cycle
// would find every one of them stopped.
func TestTheDaemonDoesNotHoldTheGuestsBack(t *testing.T) {
	u := parseUnit(t, filepath.Join("..", "..", "packaging", "systemd", "pco.service"))

	require.Equal(t, "network-online.target pve-cluster.service pveproxy.service", u.one(t, "Unit", "After"))
	for _, before := range u["Unit"]["Before"] {
		require.NotContains(t, strings.Fields(before), "pve-guests.service")
		require.NotContains(t, strings.Fields(before), "pve-ha-lrm.service")
	}
}
