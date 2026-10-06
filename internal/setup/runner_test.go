package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLookPathUsesOnlyTheDirsGiven(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(first, "tool"), []byte("#!/bin/sh\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(second, "tool"), []byte("#!/bin/sh\n"), 0o755))

	path, err := lookPath("tool", []string{first, second})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(second, "tool"), path, "a file that cannot be run is no command")

	_, err = lookPath("other", []string{first, second})
	require.ErrorIs(t, err, ErrCommandNotFound)

	_, err = lookPath(filepath.Join(first, "missing"), nil)
	require.ErrorIs(t, err, ErrCommandNotFound)

	path, err = lookPath(filepath.Join(second, "tool"), nil)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(second, "tool"), path)

	_, err = lookPath("./tool", []string{second})
	require.Error(t, err, "a relative path depends on where pco runs")
}

func TestCommandsGetAFixedEnvironment(t *testing.T) {
	t.Setenv("PATH", "/home/someone/bin")
	t.Setenv("SECRET_OF_THE_CALLER", "x")

	require.Equal(t, []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LC_ALL=C",
		"DEBIAN_FRONTEND=noninteractive",
	}, commandEnv())
}

func TestCommandErrorsCarryCleanStderr(t *testing.T) {
	exit := errors.New("exit status 2")

	err := commandError("pveum", []string{"user", "add", "pco@pve"}, exit,
		"\x1b[31mcreate user failed:\x1b[0m\n\tuser exists\u202e\n\n")
	require.ErrorIs(t, err, exit)
	require.EqualError(t, err, "pveum user add pco@pve: exit status 2: [31mcreate user failed:[0m; user exists")

	require.EqualError(t, commandError("true", nil, exit, " \n"), "true: exit status 2")

	long := commandError("x", nil, exit, strings.Repeat("é", maxStderr))
	require.LessOrEqual(t, len(long.Error()), len("x: exit status 2: ")+maxStderr+len("..."))
	require.True(t, strings.HasSuffix(long.Error(), "é..."), "a character is not cut in two")
}

func TestParsePVEVersion(t *testing.T) {
	for _, tt := range []struct {
		out          string
		major, minor int
		text         string
		supported    bool
	}{
		{"pve-manager/9.2.21/0123abcd (running kernel: 6.14.8-2-pve)\n", 9, 2, "9.2.21", true},
		{"pve-manager/8.4.1/2a5fa54a8503f96d (running kernel: 6.8.12-10-pve)", 8, 4, "8.4.1", true},
		{"pve-manager/8.3.5/dac3aa88bac3f300", 8, 3, "8.3.5", false},
		{"pve-manager/7.4-17/513c62be", 7, 4, "7.4-17", false},
		{"pve-manager/10.0.0/ffff", 10, 0, "10.0.0", false},
	} {
		t.Run(tt.text, func(t *testing.T) {
			v, err := ParsePVEVersion(tt.out)
			require.NoError(t, err)
			require.Equal(t, PVEVersion{major: tt.major, minor: tt.minor, text: tt.text}, v)
			require.Equal(t, tt.supported, v.Supported())
		})
	}
	_, err := ParsePVEVersion("proxmox-ve: 9.0.0\n")
	require.ErrorContains(t, err, "not a version of pve-manager")
}

func TestSetupMakesAPrivilegeSeparatedTokenAnew(t *testing.T) {
	e := newTestEnv(t)
	e.installUnit("pco.service")
	e.script(freshInstall()...)
	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))

	e.script(preflight("9.0.10"), roleKept(),
		[]call{
			{line: "pveum user list --output-format json", out: usersWith},
			// The grant does not propagate, so it does not reach the guests.
			{line: "pveum acl list --output-format json", out: `[{"path":"/","type":"user","ugid":"pco@pve","roleid":"PCO","propagate":"0"}]`},
			{line: "pveum acl modify / --users pco@pve --roles PCO"},
			{line: "pveum user token list pco@pve --output-format json", out: `[{"tokenid":"pco","privsep":"1"}]`},
			{line: "pveum user token remove pco@pve pco"},
			tokenAdd("pve-secret-not-separated"),
		},
		tagsKept(), cloudflaredKept(), serviceRestarted())
	require.NoError(t, e.setup(Options{Yes: true, CloudflareToken: cfToken, Node: testNode}))
	e.done()

	require.Equal(t, "pve-secret-not-separated", e.pveToken().Secret.Reveal())
	e.requireShown("privilege separated")
}
