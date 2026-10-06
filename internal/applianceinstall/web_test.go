package applianceinstall

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTheWebInterfaceListensOnTheAddressOfIP(t *testing.T) {
	e := newEnv(t)
	o := e.options()
	o.IP = "192.0.2.20/24,gw=192.0.2.1"

	e.install(o)

	ct := e.node.cts[100]
	require.Equal(t, "192.0.2.20\n", string(ct.files["/etc/pco/net0"].data))
	require.Contains(t, string(ct.files["/etc/default/pco-web"].data), "\nPCO_WEB_LISTEN=192.0.2.20:8643\n")
	require.Contains(t, e.ask.text(), "web interface: https://192.0.2.20:8643/")
}

func TestTheInstallerWaitsForTheLeaseAndTheCertificate(t *testing.T) {
	e := newEnv(t)
	noAddress := func(context.Context, []string) (string, error) { return `[{"ifname":"eth0","addr_info":[]}]`, nil }
	notYet := func(context.Context, []string) (string, error) {
		return "", exitError{1, "the web interface has no certificate yet"}
	}
	for range 3 {
		e.node.on("pct exec 100 --keep-env 0 -- ip -j -4 addr show dev eth0", noAddress)
		e.node.on("pct exec 100 --keep-env 0 -- pco web cert", notYet)
	}

	e.install(e.options())

	require.Equal(t, 4, e.node.count("pct exec 100 --keep-env 0 -- ip -j -4 addr show dev eth0"))
	require.Equal(t, 4, e.node.count("pct exec 100 --keep-env 0 -- pco web cert"))
	require.True(t, e.node.cts[100].web)
	require.Contains(t, e.ask.text(), fakeFingerprint)
}

func TestWithoutAnAddressOfNet0TheInstallIsTakenBack(t *testing.T) {
	e := newEnv(t)
	for range webTries {
		e.node.on("pct exec 100 --keep-env 0 -- ip -j -4 addr show dev eth0", func(context.Context, []string) (string, error) {
			return `[{"ifname":"eth0","addr_info":[{"family":"inet","local":"169.254.3.4","prefixlen":16,"scope":"link"}]}]`, nil
		})
	}

	err := e.in.Install(e.t.Context(), e.options())

	require.ErrorContains(t, err, "eth0 of lxc/100 has no IPv4 address after 30 s: give it one with --ip, or check the DHCP server on bridge vmbr0")
	require.Empty(t, e.node.cts, "the container is taken back")
}

func TestGlobalIPv4(t *testing.T) {
	for _, tt := range []struct {
		out, want string
	}{
		{`[{"addr_info":[{"family":"inet","local":"10.92.0.150","prefixlen":24,"scope":"global"}]}]`, "10.92.0.150"},
		{`[{"addr_info":[{"family":"inet","local":"169.254.1.1","scope":"link"},{"family":"inet","local":"10.92.0.151","scope":"global"}]}]`, "10.92.0.151"},
		{`[{"addr_info":[]}]`, ""},
		{`[]`, ""},
		{`Device "eth0" does not exist.`, ""},
	} {
		got, ok := globalIPv4(tt.out)
		require.Equal(t, tt.want != "", ok, tt.out)
		if ok {
			require.Equal(t, tt.want, got.String())
		}
	}
	require.Equal(t, fakeFingerprint, fingerprintOf("mode         self-signed: a key\nSHA-256      "+fakeFingerprint+"\n"))
	require.Empty(t, fingerprintOf("no certificate\n"))
}
