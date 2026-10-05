package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var webUnitPath = filepath.Join("..", "..", "packaging", "systemd", "pco-web.service")

// The unit of the web interface, line by line: its sandbox is what stands
// between a compromised pco-web and the node, so no line changes unnoticed.
func TestTheUnitOfTheWebInterface(t *testing.T) {
	b, err := os.ReadFile(webUnitPath)
	require.NoError(t, err)

	require.Equal(t, `[Unit]
Description=pco web interface
Documentation=https://github.com/anaryk/proxmox-cloudflared-operator
After=network-online.target pve-cluster.service pco.service
Wants=network-online.target

[Service]
Type=notify
User=pco-web
Group=pco-web
EnvironmentFile=-/etc/default/pco-web
ExecStart=/usr/bin/pco web
LoadCredential=tls.crt:/etc/pco/web/tls.crt
LoadCredential=tls.key:/etc/pco/web/tls.key
LoadCredential=pveproxy.crt:/etc/pco/web/pveproxy.crt
Restart=always
RestartSec=2
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ReadWritePaths=-/run/pco
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
SystemCallErrorNumber=EPERM
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ProtectProc=invisible
ProcSubset=pid
UMask=0077

[Install]
WantedBy=multi-user.target
`, string(b))
}

// Go raises RLIMIT_NOFILE through prlimit64 as it starts, which @resources
// filters: without an error number for filtered calls, systemd kills the
// process with SIGSYS before it serves anything.
func TestTheWebInterfaceSurvivesItsSyscallFilter(t *testing.T) {
	u := parseUnit(t, webUnitPath)

	require.Equal(t, []string{"@system-service", "~@privileged @resources"}, u["Service"]["SystemCallFilter"])
	require.Equal(t, "EPERM", u.one(t, "Service", "SystemCallErrorNumber"))
}

// The credentials link into /etc/pve, which pmxcfs mounts: the unit starts
// after it.
func TestTheWebInterfaceStartsAfterTheClusterFilesystem(t *testing.T) {
	u := parseUnit(t, webUnitPath)

	require.Contains(t, strings.Fields(u.one(t, "Unit", "After")), "pve-cluster.service")
}

// The daemon answers pco-web on its socket only while this exact file exists;
// the package must install it there, with the user it runs as.
func TestThePackageInstallsTheUnitOfTheWebInterface(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	require.NoError(t, err)
	require.Contains(t, string(b), "      - src: packaging/systemd/pco-web.service\n"+
		"        dst: /usr/lib/systemd/system/pco-web.service\n")

	users, err := os.ReadFile(filepath.Join("..", "..", "packaging", "sysusers.d", "pco.conf"))
	require.NoError(t, err)
	require.Contains(t, strings.Split(string(users), "\n"), `u pco-web - "pco web interface" - -`)
}
